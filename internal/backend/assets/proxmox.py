"""HRX host-side LXC lifecycle. JSON in/out; no credentials in this protocol."""
import fcntl
import hashlib
import ipaddress
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import time


class Refused(Exception):
    pass


def invoke(argv):
    p = subprocess.run(argv, capture_output=True, text=True, timeout=180, umask=0o022)
    if p.returncode:
        raise Refused('host command failed: ' + argv[0] + '; inspect protected host diagnostics')
    return p.stdout


def validate(request):
    expected = {'operation', 'deployment', 'spec'}
    if request.get('operation') == 'guest':
        expected.add('command')
        command = request.get('command')
        if not isinstance(command, list) or not command or any(not isinstance(a, str) or '\x00' in a for a in command):
            raise Refused('invalid guest command')
    if set(request) != expected:
        raise Refused('unknown or missing request fields')
    if request['operation'] not in {'ensure', 'inspect', 'start', 'stop', 'destroy', 'guest'}:
        raise Refused('unsupported lifecycle operation')
    if not isinstance(request['deployment'], str) or not re.fullmatch('[a-f0-9]{32}', request['deployment']):
        raise Refused('deployment must be a 128-bit lowercase hex ID')
    spec = request['spec']
    if not isinstance(spec, dict) or set(spec) - {'relay'} != {'template', 'storage', 'bridge', 'cores', 'memory_mib', 'disk_gib', 'tags', 'launch_digest'}:
        raise Refused('unknown or missing container specification fields')
    if 'relay' in spec:
        relay = spec['relay']
        if not isinstance(relay, dict) or set(relay) != {'listen_address','upstream_address','upstream_port'}:
            raise Refused('invalid relay configuration')
        for field in ['listen_address','upstream_address']:
            try:
                address = ipaddress.IPv4Address(relay[field])
                if address.is_unspecified or address.is_multicast or address.is_loopback:
                    raise ValueError()
            except (ValueError, TypeError):
                raise Refused('relay requires explicit unicast IPv4 addresses')
        if type(relay['upstream_port']) is not int or not 1 <= relay['upstream_port'] <= 65535:
            raise Refused('invalid upstream relay port')
    for key in ('storage', 'bridge'):
        if not isinstance(spec[key], str) or not re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}', spec[key]):
            raise Refused('invalid ' + key)
    if not isinstance(spec['template'], str) or not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9_-]*:vztmpl/[a-zA-Z0-9][a-zA-Z0-9_.-]*', spec['template']):
        raise Refused('invalid template volume')
    for key, maximum in [('cores', 256), ('memory_mib', 1048576), ('disk_gib', 16384)]:
        if type(spec[key]) is not int or not 1 <= spec[key] <= maximum:
            raise Refused('invalid resource limit: ' + key)
    if not isinstance(spec['tags'], list) or len(spec['tags']) > 32 or any(not isinstance(t, str) or not re.fullmatch('[a-z0-9][a-z0-9_-]{0,63}', t) for t in spec['tags']):
        raise Refused('invalid tags')
    if not isinstance(spec['launch_digest'], str) or not re.fullmatch('[a-f0-9]{64}', spec['launch_digest']):
        raise Refused('launch digest must bind the complete desired agent configuration')
    return hashlib.sha256(json.dumps(spec, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def save(path, record):
    fd, name = tempfile.mkstemp(prefix='.write-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(record, f, sort_keys=True)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


RELAY_SOURCE = """import socket,socketserver,select,threading
settings=SETTINGS
slots=threading.BoundedSemaphore(32)
class Handler(socketserver.BaseRequestHandler):
 def handle(self):
  if self.client_address[0]!=settings['guest'] or not slots.acquire(blocking=False):return
  try:
   with socket.create_connection((settings['upstream_address'],settings['upstream_port']),timeout=15) as upstream:
    upstream.settimeout(None)
    peers=[self.request,upstream]
    while True:
     ready,_,_=select.select(peers,[],[],300)
     if not ready:return
     for source in ready:
      data=source.recv(65536)
      if not data:return
      (upstream if source is self.request else self.request).sendall(data)
  except OSError:pass
  finally:slots.release()
class Server(socketserver.ThreadingTCPServer):
 allow_reuse_address=True
 daemon_threads=True
with Server((settings['listen_address'],settings['port']),Handler) as server:server.serve_forever()
"""


class Host:
    def __init__(self, root, run=invoke, guest=None):
        self.root = pathlib.Path(root)
        self.run = run
        self.guest = guest

    def json(self, argv):
        return json.loads(self.run(argv + ['--output-format', 'json']))

    def resource(self, vmid):
        rows = self.json(['pvesh', 'get', '/cluster/resources', '--type', 'vm'])
        return next((r for r in rows if r.get('vmid') == vmid), None)

    def owned(self, record):
        resource = self.resource(record['vmid'])
        if resource is None:
            return None
        if resource.get('type') != 'lxc' or resource.get('node') != record['node']:
            raise Refused('reserved VMID belongs to a different resource')
        config = self.json(['pvesh', 'get', f"/nodes/{record['node']}/lxc/{record['vmid']}/config"])
        if config.get('description', '').removesuffix('\n') != record['marker'] or config.get('unprivileged') != 1:
            raise Refused('container ownership or isolation mismatch; refusing mutation')
        if config.get('lock'):
            raise Refused('owned guest is locked by a Proxmox operation; observe the task before retry')
        status = self.json(['pvesh', 'get', f"/nodes/{record['node']}/lxc/{record['vmid']}/status/current"])
        return dict(resource, status=status['status'])

    def relay(self, record, spec, running):
        if 'relay' not in spec:
            return
        unit = 'hrx-relay-' + record['deployment'] + '.service'
        script = self.root / ('relay-' + record['deployment'] + '.py')
        state = self.run(['systemctl', 'show', unit, '--property=ActiveState', '--value']).strip()
        if not running:
            if state not in {'', 'inactive', 'failed'}:
                self.run(['systemctl', 'stop', unit])
            script.unlink(missing_ok=True)
            record.pop('relay_port', None)
            return
        port = 20000 + record['vmid']
        if not 20000 <= port <= 65535:
            raise Refused('VMID exceeds managed relay port range')
        recovered = False
        for attempt in range(30):
            rows = json.loads(self.run(['pct','exec',str(record['vmid']),'--','ip','-j','-4','addr','show','dev','eth0']))
            addresses = [a['local'] for row in rows for a in row.get('addr_info',[]) if a.get('scope')=='global']
            if addresses:
                break
            if not recovered:
                # Debian's DHCP startup can finish after receiving no offers and
                # leave no client running. Retry only that terminal service state;
                # never restart in-flight initialization or an addressed guest.
                network = self.run(['pct','exec',str(record['vmid']),'--','systemctl','show','networking',
                                    '--property=ActiveState','--property=SubState'])
                if set(network.splitlines()) == {'ActiveState=active','SubState=exited'}:
                    probe = "import pathlib\nactive=False\nfor p in pathlib.Path('/proc').glob('[0-9]*/comm'):\n try: active |= p.read_text().strip() in {'dhclient','dhcpcd','udhcpc'}\n except FileNotFoundError: pass\nprint('active' if active else 'absent')"
                    client = self.run(['pct','exec',str(record['vmid']),'--','python3','-c',probe]).strip()
                    if client == 'absent':
                        self.run(['pct','exec',str(record['vmid']),'--','systemctl','restart','networking'])
                        recovered = True
            time.sleep(1)
        if len(addresses) != 1:
            raise Refused('relay requires one observed guest IPv4 address; DHCP observation deadline or ambiguous addresses')
        guest = str(ipaddress.IPv4Address(addresses[0]))
        settings = dict(spec['relay'], guest=guest, port=port)
        source = RELAY_SOURCE.replace('SETTINGS', repr(settings))
        if state == 'active' and script.exists() and script.read_text() == source:
            record['relay_port'] = port
            return
        if state not in {'', 'inactive', 'failed'}:
            self.run(['systemctl','stop',unit])
        script.write_text(source)
        script.chmod(0o600)
        self.run(['systemd-run','--quiet','--collect','--unit='+unit,
                  '--property=NoNewPrivileges=yes','--property=ProtectSystem=strict',
                  '--property=ProtectHome=yes','--property=PrivateTmp=yes',
                  '/usr/bin/python3',str(script)])
        if self.run(['systemctl','show',unit,'--property=ActiveState','--value']).strip() != 'active':
            raise Refused('managed gateway relay failed to start')
        record['relay_port'] = port

    def release_network(self, record):
        # Release only this owned guest's DHCP lease, while its NIC still exists.
        # Stopping/destroying an LXC without release otherwise consumes the pool
        # until lease expiry. This never edits the server's shared lease database.
        script = "import pathlib,subprocess\np=pathlib.Path('/var/lib/dhcp/dhclient.eth0.leases')\nif p.exists(): subprocess.run(['/sbin/dhclient','-r','-pf','/run/dhclient.eth0.pid','-lf',str(p),'eth0'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20)"
        self.run(['pct','exec',str(record['vmid']),'--','python3','-c',script])

    def handle(self, request):
        digest = validate(request)
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        info = self.root.lstat()
        if self.root.is_symlink() or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise Refused('host state directory must be private and owned by the executor')
        with open(self.root / (request['deployment'] + '.lock'), 'a') as lock:
            # Per-deployment mutation lock permits independent guests to bootstrap concurrently.
            fcntl.flock(lock, fcntl.LOCK_EX)
            return self.reconcile(request, digest)

    def reconcile(self, request, digest):
        ident, spec, operation = request['deployment'], request['spec'], request['operation']
        path = self.root / (ident + '.json')
        if path.exists():
            record = json.loads(path.read_text())
            if record['digest'] != digest or record['deployment'] != ident:
                raise Refused('existing deployment has a different desired specification')
        elif operation != 'ensure':
            raise Refused('unknown deployment; lifecycle commands never adopt a guest')
        else:
            with open(self.root / 'allocation.lock', 'a') as allocation_lock:
                fcntl.flock(allocation_lock, fcntl.LOCK_EX)
                # Ask Proxmox for the node rather than trusting SSH alias/hostname spelling.
                nodes = self.json(['pvesh', 'get', '/cluster/status'])
                local = [n['name'] for n in nodes if n.get('type') == 'node' and n.get('local') == 1]
                if len(local) != 1:
                    raise Refused('cannot identify exactly one local Proxmox node')
                node = local[0]
                reserved = {json.loads(p.read_text())['vmid'] for p in self.root.glob('*.json')}
                occupied = {r['vmid'] for r in self.json(['pvesh', 'get', '/cluster/resources', '--type', 'vm'])}
                vmid = next((candidate for candidate in range(3000, 3100)
                             if candidate not in reserved and candidate not in occupied), None)
                if vmid is None:
                    raise Refused('HRX VMID range 3000–3099 exhausted; no container allocated')
                record = {'schema': 1, 'deployment': ident, 'digest': digest, 'vmid': vmid,
                          'node': node, 'phase': 'reserved', 'marker': 'hrx:v1:' + ident + ':' + digest}
                save(path, record)  # Allocation intent is durable before pct can create anything.
        if record['phase'] == 'destroyed':
            if operation not in {'inspect', 'destroy'}:
                raise Refused('deployment was destroyed; use a new deployment ID')
            return record
        resource = self.owned(record)
        if operation == 'inspect':
            return dict(record, container_status=resource.get('status', 'unknown') if resource else 'absent')
        if record['phase'] == 'destroying' and operation != 'destroy':
            raise Refused('destruction is in progress; reconcile destroy before any other mutation')
        if operation == 'ensure' and resource is None:
            if record['phase'] not in {'reserved', 'creating'}:
                raise Refused('owned container disappeared; refusing to recreate it silently')
            record['phase'] = 'creating'
            save(path, record)
            self.run(['pct', 'create', str(record['vmid']), spec['template'],
                      '--hostname', 'hrx-' + ident[:12], '--unprivileged', '1',
                      '--cores', str(spec['cores']), '--memory', str(spec['memory_mib']), '--swap', '0',
                      '--rootfs', spec['storage'] + ':' + str(spec['disk_gib']),
                      '--net0', 'name=eth0,bridge=' + spec['bridge'] + ',ip=dhcp,firewall=1',
                      '--onboot', '0', '--tags', ';'.join(sorted(set(spec['tags'] + ['hrx-managed']))),
                      '--description', record['marker']])
            resource = self.owned(record)
            if resource is None:
                raise Refused('create returned without an observable owned guest')
        if resource is None:
            if operation == 'destroy' and record['phase'] == 'destroying':
                record['phase'] = 'destroyed'
                record['container_status'] = 'absent'
                save(path, record)
                return record
            raise Refused('owned container is absent; inspect protected host diagnostics')
        if operation == 'guest':
            if resource.get('status') != 'running':
                raise Refused('guest execution requires a running owned container')
            if self.guest is None:
                raise Refused('guest stream is unavailable')
            output = self.guest(['pct', 'exec', str(record['vmid']), '--'] + request['command'])
            return dict(record, guest_output=output or '')
        if operation == 'destroy':
            self.relay(record, spec, False)
            record['phase'] = 'destroying'
            save(path, record)
            if resource.get('status') != 'stopped':
                self.release_network(record)
                self.run(['pct', 'stop', str(record['vmid'])])
            self.owned(record)  # Recheck ownership before the destructive operation.
            self.run(['pct', 'destroy', str(record['vmid']), '--purge', '1'])
            for _ in range(30):
                if self.resource(record['vmid']) is None:
                    break
                time.sleep(1)
            else:
                raise Refused('destroy returned but VMID remains observable; reconcile later')
            record['phase'] = 'destroyed'
            record['container_status'] = 'absent'
        else:
            if operation == 'stop':
                self.relay(record, spec, False)
            desired = {'start': 'running', 'stop': 'stopped'}.get(operation)
            if desired and resource.get('status') != desired:
                if operation == 'stop':
                    self.release_network(record)
                self.run(['pct', operation, str(record['vmid'])])
                resource = self.owned(record)
                if resource is None or resource.get('status') != desired:
                    raise Refused('lifecycle operation has not reached requested state')
            if operation == 'start':
                self.relay(record, spec, True)
            record['phase'] = 'created'
            record['container_status'] = resource.get('status', 'unknown')
        save(path, record)
        return record


def stream_guest(argv):
    # stdin's buffered reader may already hold bytes after the JSON header.
    # Copy through it rather than passing the underlying descriptor to pct.
    output = tempfile.TemporaryFile()
    process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=output,
                               stderr=subprocess.DEVNULL, umask=0o022)
    try:
        try:
            shutil.copyfileobj(sys.stdin.buffer, process.stdin, 1024 * 1024)
        finally:
            process.stdin.close()
        if process.wait(timeout=900):
            raise Refused('guest command failed; inspect guest diagnostics')
        output.seek(0)
        data = output.read(1024*1024+1)
        if len(data)>1024*1024:
            raise Refused('guest observation exceeds output limit')
        return data.decode('utf-8')
    except BaseException:
        process.kill()
        process.wait()
        raise
    finally:
        output.close()


def main():
    os.umask(0o077)
    try:
        raw = sys.stdin.buffer.readline(65537)
        if len(raw) > 65536:
            raise Refused('request exceeds limit')
        result = Host('/var/lib/hrx/proxmox', guest=stream_guest).handle(json.loads(raw))
        print(json.dumps({'ok': True, 'deployment': result}))
    except Refused as error:
        print(json.dumps({'ok': False, 'error': str(error)}))
        return 0
    except Exception:
        print(json.dumps({'ok': False, 'error': 'host operation failed; reconcile existing deployment before retry'}))
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
