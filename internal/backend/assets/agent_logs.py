"""Bounded native pane diagnostics; never dump configuration or credentials."""
import base64
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import tempfile

LIMIT = 65536


def sanitize(text, secrets):
    # Strip terminal control sequences before matching secrets, including OSC links.
    text = re.sub(r'\x1b\][^\x07]*(?:\x07|\x1b\\)', '', text)
    text = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', text)
    for secret in sorted(set(secrets), key=len, reverse=True):
        if secret:
            text = text.replace(secret, '[REDACTED]')
    text = re.sub(r'(?i)(authorization\s*[:=]\s*)(?:basic|bearer)\s+[^\s"\']+', r'\1[REDACTED]', text)
    text = re.sub(r'\bsk-[A-Za-z0-9_-]{12,}', '[REDACTED]', text)
    return ''.join(c for c in text if c in '\n\t' or ord(c) >= 32)[-LIMIT:]


def bounded_read(path):
    with pathlib.Path(path).open('rb') as source:
        raw = source.read(1024*1024+1)
    if len(raw)>1024*1024:
        raise ValueError('diagnostic input too large')
    return raw.decode()


def collect(ident, lines):
    import tomllib
    if not re.fullmatch('[a-f0-9]{32}', ident) or not 1 <= lines <= 2000:
        raise ValueError('invalid log request')
    state=pathlib.Path('/opt/hrx/deployments')/ident
    saved=json.loads(bounded_read(state/'agent.json'))
    if saved.get('deployment')!=ident or saved.get('session')!='hrx-'+ident:
        raise ValueError('invalid agent binding')
    home=pathlib.Path('/home/agent')
    lease=json.loads(bounded_read(home/'.config/hrx/lease.json'))
    secrets=[lease.get('key',''),lease.get('claude_code_oauth_token',''),
             bounded_read(home/'.config/pitput/remote.token').strip()]
    h=hashlib.sha256(saved['identity'].encode()).hexdigest()
    for board in saved.get('boards',[]):
        path=home/'.local/state/hrx/pitput'/h/'boards'/board/'state'/h/'state.json'
        if not path.exists(): continue
        mapping=json.loads(bounded_read(path)).get('mapping',{})
        if not mapping.get('config_path'): continue
        config=tomllib.loads(bounded_read(mapping['config_path']))
        password_path=config.get('opencode_password_file')
        if password_path and pathlib.Path(password_path).exists():
            password=bounded_read(password_path).strip()
            secrets += [password,base64.b64encode(('opencode:'+password).encode()).decode()]
    with tempfile.TemporaryFile() as output:
        result=subprocess.run(['runuser','-u','agent','--','tmux','capture-pane','-p','-S','-'+str(lines),'-t','hrx-'+ident],stdout=output,stderr=subprocess.DEVNULL,timeout=15)
        output.seek(0)
        captured=output.read(1024*1024+1)
        if len(captured)>1024*1024: raise ValueError('pane output too large')
    source='pane'
    if result.returncode==0:
        raw=captured.decode(errors='replace')
    elif (state/'previous-pane.log').exists():
        raw=bounded_read(state/'previous-pane.log');source='previous-pane'
    else:
        raise ValueError('no retained native pane')
    return {'deployment':ident,'source':source,'text':sanitize(raw,secrets),
            'truncated':len(raw)>LIMIT,'lines_requested':lines}


if __name__=='__main__':
    try:
        print(json.dumps(collect(sys.argv[1],int(sys.argv[2]))))
    except Exception:
        print('native diagnostics unavailable; private input suppressed',file=sys.stderr)
        sys.exit(1)
