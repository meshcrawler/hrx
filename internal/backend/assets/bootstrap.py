"""Install a verified offline runtime bundle inside an owned fresh Debian LXC."""
import hashlib
import json
import os
import pathlib
import platform
import pwd
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


CODEX_RESOURCES = {'codex-code-mode-host': 'bin/codex-code-mode-host',
                   'bwrap': 'codex-resources/bwrap', 'rg': 'codex-path/rg'}


class InvalidBundle(Exception):
    pass


def receive(stream, root):
    root = pathlib.Path(root)
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.TemporaryDirectory(prefix='.receive-', dir=root) as temporary:
        stage = pathlib.Path(temporary)
        with tarfile.open(fileobj=stream, mode='r|') as archive:
            first = archive.next()
            if first is None or first.name != 'manifest.json' or not first.isfile() or first.size > 65536:
                raise InvalidBundle('first member must be a bounded manifest')
            raw = archive.extractfile(first).read()
            manifest = json.loads(raw)
            if manifest.get('schema') != 1 or manifest.get('harness') not in {'codex', 'claude', 'opencode'}:
                raise InvalidBundle('unsupported runtime manifest')
            artifacts = manifest.get('artifacts')
            if not isinstance(artifacts, list) or not 3 <= len(artifacts) <= 128:
                raise InvalidBundle('invalid artifact count')
            expected = {}
            for a in artifacts:
                if (not isinstance(a, dict) or set(a) != {'name', 'kind', 'version', 'sha256', 'size'}
                    or not isinstance(a['name'], str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9_.-]{0,199}', a['name'])
                    or a['name'] == 'manifest.json' or a['name'] in expected
                    or a['kind'] not in {'hrx', 'harness', 'wheel', 'codex-resource'}
                    or not isinstance(a['version'], str) or not a['version']
                    or not isinstance(a['sha256'], str) or not re.fullmatch('[a-f0-9]{64}', a['sha256'])
                    or type(a['size']) is not int or not 0 < a['size'] <= 512*1024*1024):
                    raise InvalidBundle('invalid artifact specification')
                if a['kind'] == 'wheel' and not a['name'].endswith('.whl'):
                    raise InvalidBundle('wheel artifact must have a wheel filename')
                if a['kind'] == 'codex-resource' and (manifest['harness'] != 'codex' or a['name'] not in CODEX_RESOURCES):
                    raise InvalidBundle('unsupported Codex companion artifact')
                expected[a['name']] = a
            if (sum(a['size'] for a in artifacts) > 1024*1024*1024
                or sum(a['kind'] == 'hrx' for a in artifacts) != 1
                or sum(a['kind'] == 'harness' for a in artifacts) != 1
                or not any(a['kind'] == 'wheel' and a['name'].startswith('pitput-') for a in artifacts)):
                raise InvalidBundle('incomplete runtime or oversized bundle')
            if manifest['harness'] == 'codex' and {a['name'] for a in artifacts if a['kind'] == 'codex-resource'} != set(CODEX_RESOURCES):
                raise InvalidBundle('Codex bundle requires pinned companion executables')
            seen = set()
            for member in archive:
                # tarfile iteration includes the first member already returned by next().
                if member is first:
                    continue
                a = expected.get(member.name)
                if a is None or member.name in seen or not member.isfile() or member.size != a['size']:
                    raise InvalidBundle('unexpected, duplicate or non-regular archive member')
                digest = hashlib.sha256()
                with archive.extractfile(member) as source, open(stage/member.name, 'xb') as dest:
                    while True:
                        chunk = source.read(1024*1024)
                        if not chunk:
                            break
                        digest.update(chunk)
                        dest.write(chunk)
                    dest.flush()
                    os.fsync(dest.fileno())
                if digest.hexdigest() != a['sha256']:
                    raise InvalidBundle('artifact digest mismatch')
                seen.add(member.name)
            if seen != set(expected):
                raise InvalidBundle('incomplete artifact stream')
        ident = hashlib.sha256(raw).hexdigest()
        destination = root/ident
        if destination.exists():
            # Verify retained artifacts too; a marker alone is not integrity evidence.
            for a in artifacts:
                path = destination/a['name']
                if path.is_symlink() or not path.is_file():
                    raise InvalidBundle('existing artifact cache was modified')
                digest = hashlib.sha256()
                with open(path, 'rb') as source:
                    for chunk in iter(lambda: source.read(1024*1024), b''):
                        digest.update(chunk)
                if digest.hexdigest() != a['sha256']:
                    raise InvalidBundle('existing artifact cache was modified')
        else:
            (stage/'manifest.json').write_bytes(raw)
            os.rename(stage, destination)
        return destination, manifest


def install_binary(source, target, expected_digest):
    if target.is_symlink():
        raise InvalidBundle('runtime binary is a symlink')
    if target.exists():
        digest = hashlib.sha256()
        with target.open('rb') as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(chunk)
        if digest.hexdigest() != expected_digest:
            raise InvalidBundle('installed runtime binary was modified')
        return  # Do not rewrite an executable used by a live native session.
    # Never leave a partially copied executable at the committed runtime path.
    # A killed controller/guest can leave an unused temporary file; retry safely
    # installs a new one while retaining integrity checks for published binaries.
    fd, name = tempfile.mkstemp(prefix='.install-', dir=target.parent)
    os.close(fd)
    temporary = pathlib.Path(name)
    try:
        shutil.copyfile(source, temporary)
        temporary.chmod(0o755)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, target)
        directory_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        temporary.unlink(missing_ok=True)


def install_codex_resources(directory, runtime, manifest):
    for artifact in manifest['artifacts']:
        if artifact['kind'] != 'codex-resource':
            continue
        if manifest['harness'] != 'codex' or artifact['name'] not in CODEX_RESOURCES:
            raise InvalidBundle('unsupported Codex companion artifact')
        target = runtime / CODEX_RESOURCES[artifact['name']]
        if target.parent.is_symlink():
            raise InvalidBundle('companion directory is a symlink')
        target.parent.mkdir(parents=True, exist_ok=True)
        install_binary(directory / artifact['name'], target, artifact['sha256'])


def install(directory, manifest):
    if platform.system() != 'Linux' or platform.machine() != 'x86_64':
        raise InvalidBundle('this runtime bundle requires Linux x86_64')
    log_path = pathlib.Path('/var/log/hrx-bootstrap.log')
    with open(log_path, 'ab') as log:
        def run(args):
            result = subprocess.run(args, stdout=log, stderr=log, env=dict(os.environ, DEBIAN_FRONTEND='noninteractive'), timeout=600)
            if result.returncode:
                raise InvalidBundle('runtime installation failed; inspect /var/log/hrx-bootstrap.log')
        run(['apt-get', 'update', '-qq'])
        run(['apt-get', 'install', '-y', '--no-install-recommends', 'python3-venv', 'tmux', 'ca-certificates', 'curl', 'git'])
        try:
            user = pwd.getpwnam('agent')
        except KeyError:
            run(['useradd', '--create-home', '--shell', '/bin/bash', 'agent'])
            user = pwd.getpwnam('agent')
        if user.pw_uid == 0 or user.pw_dir != '/home/agent' or pathlib.Path('/home/agent').is_symlink():
            raise InvalidBundle('agent account does not have the expected private home')
        for name in ['.config', '.config/hrx', '.config/pitput', 'workspace']:
            path = pathlib.Path('/home/agent')/name
            if path.is_symlink():
                raise InvalidBundle('agent configuration directory is a symlink')
            path.mkdir(mode=0o700, parents=True, exist_ok=True)
            os.chown(path, user.pw_uid, user.pw_gid)
            path.chmod(0o700)
        runtime = pathlib.Path('/opt/hrx/runtimes')/directory.name
        runtime.mkdir(parents=True, exist_ok=True)
        run(['python3', '-m', 'venv', str(runtime/'venv')])
        wheels = [str(directory/a['name']) for a in manifest['artifacts'] if a['kind']=='wheel']
        run([str(runtime/'venv/bin/pip'), 'install', '--no-index', '--no-deps', *wheels])
        run([str(runtime/'venv/bin/pip'), 'check'])
        binaries = {'pitput': runtime/'venv/bin/pitput'}
        for a in manifest['artifacts']:
            if a['kind'] in {'hrx', 'harness'}:
                name = 'hrx' if a['kind']=='hrx' else manifest['harness']
                target = runtime/'bin/codex' if name == 'codex' else runtime/name
                target.parent.mkdir(parents=True, exist_ok=True)
                install_binary(directory/a['name'], target, a['sha256'])
                binaries[name] = target
        install_codex_resources(directory, runtime, manifest)
        for name, target in binaries.items():
            run([str(target), '--help' if name == 'pitput' else '--version'])
        run([str(runtime/'venv/bin/python'), '-c', 'import importlib.metadata; print(importlib.metadata.version("pitput"))'])
        # Publish only after all offline installation and version checks succeed.
        for name, target in binaries.items():
            link = pathlib.Path('/usr/local/bin')/name
            temporary = link.with_name('.hrx-'+name)
            temporary.unlink(missing_ok=True)
            temporary.symlink_to(target)
            os.replace(temporary, link)
        (runtime/'installed.json').write_text(json.dumps({'schema':1, 'bundle':directory.name,'harness':manifest['harness']}))


if __name__ == '__main__':
    os.umask(0o022)
    try:
        directory, manifest = receive(sys.stdin.buffer, '/opt/hrx/artifacts')
        install(directory, manifest)
    except Exception as error:
        print('HRX bootstrap failed: '+(str(error) if isinstance(error, InvalidBundle) else 'invalid bundle or installation error'), file=sys.stderr)
        sys.exit(1)
