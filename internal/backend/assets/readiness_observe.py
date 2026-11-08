"""Read-only, credential-free native observation for the live acceptance test."""
import base64
import hashlib
import json
import pathlib
import sys
import subprocess
import urllib.request

def native_binding_matches(session, mapping, config, board):
    directory = session.get('location', {}).get('directory')
    if not directory or not session.get('id') or not mapping.get('thread_id'):
        return False
    return (session['id'] == mapping['thread_id'] and not session.get('parentID')
            and session.get('time', {}).get('archived') is None
            and str(pathlib.Path(directory).resolve()) == mapping.get('project_directory')
            == config.get('opencode_project_directory')
            and mapping.get('board') == '/b/' + board)


def claude_transcript_evidence(records, session, project):
    calls={};actions=[];bound=False
    for record in records:
        if record.get('type') not in {'user','assistant'}:continue
        if record.get('sessionId')!=session or record.get('cwd')!=project or record.get('isSidechain'):
            raise ValueError('Claude transcript binding mismatch')
        bound=True
        content=record.get('message',{}).get('content',[])
        if not isinstance(content,list):continue
        for part in content:
            if not isinstance(part,dict):continue
            if part.get('type')=='tool_use' and part.get('name')=='Bash':
                calls[part['id']]=part.get('input',{}).get('command','')
            if part.get('type')=='tool_result' and not part.get('is_error',False):
                command=calls.get(part.get('tool_use_id'),'')
                for label,needle in [('read','attention read'),('post',' post '),('ack','bridge ack'),('ack','attention ack')]:
                    if 'pitput' in command and needle in command:actions.append(label)
    return bound,actions


def observe_claude(mapping,state,path,identity,board,result):
    session=mapping.get('thread_id','');home=pathlib.Path(mapping['claude_home'])
    if mapping.get('agent_id')!=identity or mapping.get('board')!='/b/'+board:
        return
    index=json.loads(pathlib.Path('/home/agent/.local/state/hrx/sessions/index.json').read_text())
    matching=[r for r in index.get('sessions',[]) if r.get('native_id')==session and r.get('harness')=='claude'
              and r.get('agent_identity')==identity and r.get('project')=='/home/agent/workspace'
              and r.get('claude_config_dir')==str(home)]
    active=json.loads((path.parent.parent/'claude-active-generation').read_text())
    if len(matching)!=1 or active!={'session':session,'generation':mapping.get('claude_task_generation')}:
        return
    transcripts=list((home/'projects').glob('*/'+session+'.jsonl'))
    if len(transcripts)!=1 or transcripts[0].stat().st_size>16*1024*1024:return
    records=[]
    with transcripts[0].open() as source:
        for line in source:
            if len(line)>1024*1024:raise ValueError('oversized transcript record')
            records.append(json.loads(line))
    bound,actions=claude_transcript_evidence(records,session,'/home/agent/workspace')
    result['native_binding']=bound
    result['native_actions']=actions
    result['handled_events']=[{'event_key':r['event_key'],'path':r['path']} for r in state.get('handled',[])]


def observe_codex(mapping, state, identity, board, result, home=pathlib.Path("/home/agent")):
    session = mapping.get('thread_id', '')
    if mapping.get('agent_id') != identity or mapping.get('board') != '/b/' + board:
        return
    index = json.loads((home / '.local/state/hrx/sessions/index.json').read_text())
    matching = [r for r in index.get('sessions', []) if r.get('native_id') == session
                and r.get('harness') == 'codex' and r.get('agent_identity') == identity
                and r.get('project') == str(home / 'workspace')]
    directory = home / '.local/state/hrx/pitput' / hashlib.sha256(identity.encode()).hexdigest() / session / board
    if mapping.get('codex_binary') != str(directory / 'codex-queue'):
        return
    binding = json.loads((directory / 'native-observer.json').read_text())
    if len(matching) != 1 or binding.get('session') != session or binding.get('project') != str(home / 'workspace'):
        return
    observed = subprocess.run(['/usr/local/bin/hrx', '_codex-observe'], input=json.dumps(binding),
                              capture_output=True, text=True, timeout=12)
    if observed.returncode or len(observed.stdout) > 1024 * 1024:
        raise ValueError('native observation unavailable')
    evidence = json.loads(observed.stdout)
    result['native_binding'] = evidence['native_binding']
    result['native_actions'] = evidence['native_actions']
    result['handled_events'] = [{'event_key':r['event_key'],'path':r['path']} for r in state.get('handled',[])]


def main():
    import tomllib
    identity,board=sys.argv[1:3]
    terminated=False
    if len(sys.argv)>3:
        native=subprocess.run(['runuser','-u','agent','--','tmux','list-panes','-t','hrx-'+sys.argv[3],'-F','#{pane_dead}'],capture_output=True,text=True)
        terminated=native.returncode!=0 or native.stdout.splitlines()==['1']
    h=hashlib.sha256(identity.encode()).hexdigest()
    path=pathlib.Path('/home/agent/.local/state/hrx/pitput')/h/'boards'/board/'state'/h/'state.json'
    result={'bound':False,'native_binding':False}
    if path.exists():
        state=json.loads(path.read_text());mapping=state.get('mapping',{})
        cursor_path=path.parent/'cursor.json'
        cursor=json.loads(cursor_path.read_text()) if cursor_path.exists() else {}
        result={'bound':mapping.get('agent_id')==identity and bool(mapping.get('thread_id')) and bool(cursor.get('monitor_id')),
                'session':mapping.get('thread_id'),'monitor':cursor.get('monitor_id'),'pending':bool(state.get('pending')),
                'handled':[r['event_key'] for r in state.get('handled',[])], 'native_actions':[]}
        if result['bound'] and mapping.get('backend')=='claude-task':
            try:observe_claude(mapping,state,path,identity,board,result)
            except Exception:result['native_observation_unavailable']=True
        if result['bound'] and mapping.get('codex_binary') and mapping.get('backend', 'codex') == 'codex':
            try:observe_codex(mapping,state,identity,board,result)
            except Exception:result['native_observation_unavailable']=True
        if result['bound'] and mapping.get('config_path'):
            config=tomllib.loads(pathlib.Path(mapping['config_path']).read_text())
            if config.get('backend')=='opencode':
                password=pathlib.Path(config['opencode_password_file']).read_text().strip()
                req=urllib.request.Request(config['opencode_url']+'/api/session/'+mapping['thread_id']+'/message?order=desc&limit=100',headers={'Authorization':'Basic '+base64.b64encode(('opencode:'+password).encode()).decode()})
                try:
                    with urllib.request.urlopen(req,timeout=10) as response:messages=json.load(response)['data']
                    session_req=urllib.request.Request(config['opencode_url']+'/api/session/'+mapping['thread_id'],headers=dict(req.header_items()))
                    with urllib.request.urlopen(session_req,timeout=10) as response:session=json.load(response)['data']
                    result['native_binding']=native_binding_matches(session,mapping,config,board)
                    result['handled_events']=[{'event_key':r['event_key'],'path':r['path']} for r in state.get('handled',[])]
                    result['provider_retries'] = [
                        {'attempt': m['retry'].get('attempt'), 'status': m['retry'].get('error', {}).get('status')}
                        for m in messages if isinstance(m.get('retry'), dict)
                        and type(m['retry'].get('attempt')) is int
                        and type(m['retry'].get('error', {}).get('status')) is int
                    ]
                    for message in messages:
                        for part in message.get('content',[]):
                            st=part.get('state',{})
                            if part.get('type')=='tool' and part.get('name')=='shell' and st.get('status')=='completed' and st.get('metadata',{}).get('exit')==0:
                                command=st.get('input',{}).get('command','')
                                for label,needle in [('read','attention read'),('post',' post '),('ack','bridge ack'),('ack','attention ack')]:
                                    if 'pitput' in command and needle in command:result['native_actions'].append(label)
                except Exception:
                    result['native_observation_unavailable']=True
    result['terminated']=terminated
    print(json.dumps(result))


if __name__ == '__main__':
    main()
