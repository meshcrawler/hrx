"""Configure and start one native HRX agent; input is private controller JSON."""
import datetime
import ipaddress
import json
import os
import pathlib
import pwd
import re
import shlex
import subprocess
import sys
import tempfile


def validate(data):
    if not re.fullmatch('[a-f0-9]{32}',data.get('deployment','')):
        raise ValueError('invalid deployment')
    if 'readiness_only' in data and type(data['readiness_only']) is not bool:
        raise ValueError('readiness_only must be boolean')
    if data.get('harness') not in {'opencode','codex','claude'}:
        raise ValueError('invalid harness')
    if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:/-]{0,239}',data.get('model','')):
        raise ValueError('invalid model')
    if not re.fullmatch('[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+',data.get('identity','')):
        raise ValueError('invalid identity')
    if not data.get('boards') or any(not re.fullmatch('[A-Za-z0-9][A-Za-z0-9_-]*',b) for b in data['boards']):
        raise ValueError('explicit boards required')
    for name in ['gateway_url']:
        if not data.get(name,'').startswith('https://'):
            raise ValueError('HTTPS gateway required')
    if not data.get('pitput',{}).get('url','').startswith('https://') or not data['pitput'].get('token'):
        raise ValueError('scoped Pitput profile required')
    for hostname,address in data.get('hosts',{}).items():
        if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9.-]{0,252}',hostname):
            raise ValueError('invalid configured hostname')
        ipaddress.ip_address(address)
    lease=data.get('lease',{})
    if not lease.get('key'):
        raise ValueError('validated gateway lease required')
    expiry=datetime.datetime.fromisoformat(lease.get('expires_at','').replace('Z','+00:00'))
    if expiry <= datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=30):
        raise ValueError('lease expired or too close to expiry')
    if data['harness']=='claude' and not lease.get('claude_code_oauth_token'):
        raise ValueError('Claude OAuth lease required')
    if data['harness']=='codex' and not isinstance(data.get('codex_catalog',{}).get('models'),list):
        raise ValueError('pinned Codex model catalog required')


def launch_argv(data,workspace):
    args=['/usr/local/bin/hrx','--profile','remote','--yolo','--pitput','--pitput-no-dm',
          '--agent-identity',data['identity'],'--pitput-targeted']
    for board in data['boards']:
        args+=['--pitput-board',board]
    for thread in data.get('threads',[]):
        args+=['--pitput-follow-thread',thread]
    if data['harness']=='claude':
        args+=['--claude-config-dir','/home/agent/.config/hrx/claude-native','--claude-init','theme,trust,bypass']
    if data.get('readiness_only') and data['harness']=='opencode':
        return args
    prompt=data['initial_prompt']
    args+=['--']+(['--prompt',prompt] if data['harness']=='opencode' else [prompt])
    return args


def private_file(path,content,user):
    path=pathlib.Path(path)
    # Verify every existing parent below the owned home before root writes through it.
    for parent in [path,*path.parents]:
        if parent.is_symlink():
            raise ValueError('symlink in managed configuration path')
        if str(parent)=='/home/agent':
            break
    path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    fd,temporary_name=tempfile.mkstemp(prefix='.hrx-',dir=path.parent)
    temporary=pathlib.Path(temporary_name)
    with os.fdopen(fd,'w') as f:
        f.write(content);f.flush();os.fsync(f.fileno())
    os.chown(temporary,user.pw_uid,user.pw_gid)
    os.replace(temporary,path)
    parent=path.parent
    while str(parent).startswith('/home/agent/'):
        os.chown(parent,user.pw_uid,user.pw_gid);parent.chmod(0o700);parent=parent.parent


def native_session_alive(runuser, session):
    result = subprocess.run(runuser + ['tmux', 'list-panes', '-t', session,
                            '-F', '#{pane_dead}'], capture_output=True, text=True)
    if result.returncode:
        return False
    states = result.stdout.splitlines()
    if len(states) != 1 or states[0] not in {'0', '1'}:
        raise ValueError('unexpected native session pane layout; inspect before retrying')
    return states[0] == '0'


def launcher(args):
    return '''import datetime,json,os
os.environ['PATH']='/usr/local/bin:/usr/bin:/bin'
lease=json.load(open('/home/agent/.config/hrx/lease.json'))
expiry=datetime.datetime.fromisoformat(lease['expires_at'].replace('Z','+00:00'))
remaining=int((expiry-datetime.datetime.now(datetime.timezone.utc)).total_seconds())-15
if remaining<1:raise SystemExit('credential lease expired')
if lease.get('claude_code_oauth_token'):os.environ['CLAUDE_CODE_OAUTH_TOKEN']=lease['claude_code_oauth_token']
os.chdir('/home/agent/workspace')
args=ARGS
os.execv('/usr/bin/timeout',['timeout','--signal=TERM','--kill-after=15s',str(remaining)]+args)
'''.replace('ARGS',repr(args))

def configure(data):
    validate(data)
    user=pwd.getpwnam('agent')
    ident=data['deployment'];session='hrx-'+ident
    home=pathlib.Path('/home/agent');workspace=home/'workspace'
    state=pathlib.Path('/opt/hrx/deployments')/ident
    state.mkdir(parents=True,exist_ok=True)
    state.parent.chmod(0o755);state.chmod(0o755)
    marker=state/'agent.json'
    runuser=['runuser','-u','agent','--']
    alive=native_session_alive(runuser,session)
    if marker.exists():
        saved=json.loads(marker.read_text())
        if saved['identity']!=data['identity'] or saved['harness']!=data['harness']:
            raise ValueError('deployment identity mismatch')
        marker.chmod(0o644)  # Public identity marker; credentials live in separate private files.
        if alive:
            return dict(saved,status='starting_or_running',ready=False)
        raise ValueError('prior native launch stopped; inspect and explicitly resume it')
    if alive:
        raise ValueError('unrecorded native session; refusing adoption')
    if data.get('hosts'):
        hosts=pathlib.Path('/etc/hosts');lines=hosts.read_text().splitlines()
        for hostname,address in data['hosts'].items():
            existing=[line.split()[0] for line in lines if hostname in line.split('#',1)[0].split()[1:]]
            if existing and any(value!=address for value in existing):
                raise ValueError('configured host conflicts with existing guest mapping')
            if not existing:lines.append(address+' '+hostname)
        hosts.write_text('\n'.join(lines)+'\n')
    quote=json.dumps
    private_file(home/'.config/hrx/lease.json',json.dumps(data['lease']),user)
    config='[permissions]\nyolo = true\n[profiles.remote]\nharness = '+quote(data['harness'])+'\nmodel = '+quote(data['model'])+'\ncredential_file = "/home/agent/.config/hrx/lease.json"\n'
    if data.get('reasoning'):
        config+='reasoning = '+quote(data['reasoning'])+'\n'
    if data['harness']!='codex':
        config+='['+data['harness']+'.provider]\nbase_url = '+quote(data['gateway_url'])+'\nenv_key = "HRX_PROFILE_GATEWAY_KEY"\n'
    else:
        # Pin the native baseline: known catalog models make the TUI reassert
        # an explicit default tier even when no speed tiers are advertised.
        native='service_tier = "default"\nmodel_catalog_json = "/home/agent/.codex/models_cache.json"\nmodel = '+quote(data['model'])+'\nmodel_provider = "hrx_remote"\n[model_providers.hrx_remote]\nname = "HRX remote gateway"\nbase_url = '+quote(data['gateway_url'])+'\nwire_api = "responses"\nenv_key = "HRX_PROFILE_GATEWAY_KEY"\n'
        private_file(home/'.codex/config.toml',native,user)
        private_file(home/'.codex/models_cache.json',json.dumps(data['codex_catalog']),user)
    private_file(home/'.config/hrx/config.toml',config,user)
    private_file(home/'.config/pitput/remote.token',data['pitput']['token'],user)
    pitput='identity = '+quote(data['identity'])+'\n[remote]\nurl = '+quote(data['pitput']['url'])+'\ntoken_file = "/home/agent/.config/pitput/remote.token"\n'
    if data['pitput'].get('ca_pem'):
        private_file(home/'.config/pitput/remote-ca.pem',data['pitput']['ca_pem'],user)
        pitput+='ca_file = "/home/agent/.config/pitput/remote-ca.pem"\n'
    private_file(home/'.config/pitput/remote.toml',pitput,user)
    result=subprocess.run(runuser+['/usr/local/bin/pitput','agent','import','--config',str(home/'.config/pitput/remote.toml')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if result.returncode:
        raise ValueError('scoped Pitput profile import failed')
    if data.get('readiness_only') and data['harness'] in {'opencode','codex'}:
        private_file(workspace/'AGENTS.md',data['initial_prompt']+'\n',user)
    args=launch_argv(data,str(workspace))
    launch=launcher(args)
    (state/'launch.py').write_text(launch);(state/'launch.py').chmod(0o644)
    saved={'schema':1,'deployment':ident,'identity':data['identity'],'harness':data['harness'],'session':session,'ready':False,'boards':data['boards'],'threads':data.get('threads',[])}
    marker.write_text(json.dumps(saved))
    marker.chmod(0o644)
    result=subprocess.run(runuser+['tmux','new-session','-d','-s',session,'tmux set-option -w remain-on-exit on && exec python3 '+shlex.quote(str(state/'launch.py'))],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if result.returncode:
        raise ValueError('native tmux launch failed; inspect recorded deployment')
    return dict(saved,status='starting')


def resume_arguments(saved, mappings):
    harness=saved.get('harness')
    if harness not in {'opencode','claude','codex'}:
        raise ValueError('unsupported remote resume harness')
    boards=saved.get('boards',[])
    if not boards or len(mappings)!=len(boards):
        raise ValueError('saved board bindings required')
    sessions=set()
    for board,mapping in zip(boards,mappings):
        if mapping.get('agent_id')!=saved['identity'] or mapping.get('board')!='/b/'+board:
            raise ValueError('native resume binding mismatch')
        if harness=='opencode':
            valid=(mapping.get('project_directory')=='/home/agent/workspace'
                   and re.fullmatch('ses_[A-Za-z0-9]+',mapping.get('thread_id','')))
        elif harness=='codex':
            valid=(mapping.get('codex_home')=='/home/agent/.codex'
                   and re.fullmatch('[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',mapping.get('thread_id','')))
        else:
            valid=(mapping.get('backend')=='claude-task'
                   and mapping.get('claude_home')=='/home/agent/.config/hrx/claude-native'
                   and re.fullmatch('[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',mapping.get('thread_id','')))
        if not valid:raise ValueError('native resume binding mismatch')
        sessions.add(mapping['thread_id'])
    if len(sessions)!=1:raise ValueError('boards disagree on native resume session')
    data=dict(saved,readiness_only=True,initial_prompt='')
    args=launch_argv(data,'/home/agent/workspace')
    if '--' in args:args=args[:args.index('--')]
    args+=['--resume',sessions.pop()]
    if harness=='claude':
        args+=['--','Resume this exact session. Start the current HRX-owned background monitor tasks from the launch instructions and wait for notifications. Do not repeat completed assignments or posts; retain their work IDs.']
    return args


def resume(ident):
    import hashlib
    if not re.fullmatch('[a-f0-9]{32}',ident):
        raise ValueError('invalid deployment')
    state=pathlib.Path('/opt/hrx/deployments')/ident
    saved=json.loads((state/'agent.json').read_text())
    session='hrx-'+ident
    if saved.get('deployment')!=ident or saved.get('session')!=session:
        raise ValueError('saved deployment mismatch')
    runuser=['runuser','-u','agent','--']
    if native_session_alive(runuser,session):
        return dict(saved,status='starting_or_running',ready=False)
    lease=json.loads(pathlib.Path('/home/agent/.config/hrx/lease.json').read_text())
    expiry=datetime.datetime.fromisoformat(lease['expires_at'].replace('Z','+00:00'))
    if expiry <= datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=30):
        raise ValueError('lease expired; explicit credential replacement required')
    h=hashlib.sha256(saved['identity'].encode()).hexdigest()
    root=pathlib.Path('/home/agent/.local/state/hrx/pitput')/h/'boards'
    mappings=[json.loads((root/b/'state'/h/'state.json').read_text())['mapping'] for b in saved.get('boards',[])]
    args=resume_arguments(saved,mappings)
    # Preserve the previous pane before replacing a dead tmux session.
    captured=subprocess.run(runuser+['tmux','capture-pane','-p','-S','-2000','-t',session],capture_output=True,text=True)
    if captured.returncode==0:
        archive=state/'previous-pane.log'
        if archive.is_symlink(): raise ValueError('invalid diagnostic path')
        archive.write_text(captured.stdout);archive.chmod(0o600)
        subprocess.run(runuser+['tmux','kill-session','-t',session],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    launch=state/'resume.py'
    if launch.is_symlink(): raise ValueError('invalid resume path')
    launch.write_text(launcher(args));launch.chmod(0o644)
    result=subprocess.run(runuser+['tmux','new-session','-d','-s',session,'tmux set-option -w remain-on-exit on && exec python3 '+shlex.quote(str(launch))],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if result.returncode: raise ValueError('native resume failed')
    return dict(saved,status='starting',ready=False)


def replace_lease(data):
    ident=data.get('deployment','')
    if not re.fullmatch('[a-f0-9]{32}',ident):raise ValueError('invalid deployment')
    saved=json.loads((pathlib.Path('/opt/hrx/deployments')/ident/'agent.json').read_text())
    if saved.get('deployment')!=ident or saved.get('session')!='hrx-'+ident:
        raise ValueError('saved deployment mismatch')
    if native_session_alive(['runuser','-u','agent','--'],'hrx-'+ident):
        raise ValueError('stop native session before credential replacement')
    lease=data['lease']
    expiry=datetime.datetime.fromisoformat(lease['expires_at'].replace('Z','+00:00'))
    if not lease.get('key') or expiry<=datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=30):
        raise ValueError('replacement lease unavailable or expired')
    if saved['harness']=='claude' and not lease.get('claude_code_oauth_token'):
        raise ValueError('Claude OAuth required')
    private_file('/home/agent/.config/hrx/lease.json',json.dumps(lease),pwd.getpwnam('agent'))
    return {'deployment':ident,'credential_installed':True,'ready':False}


if __name__=='__main__':
    os.umask(0o077)
    try:
        raw=sys.stdin.buffer.read(1024*1024+1)
        if len(raw)>1024*1024:raise ValueError('configuration too large')
        data=json.loads(raw)
        if data.get('operation')=='replace-lease':result=replace_lease(data)
        elif data.get('operation')=='resume':result=resume(data['deployment'])
        else:result=configure(data)
        print(json.dumps(result))
    except Exception:
        print('agent configuration/start failed; private input suppressed',file=sys.stderr)
        sys.exit(1)
