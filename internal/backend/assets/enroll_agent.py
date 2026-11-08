"""Controller-only enrollment. stdout is private and must never be logged."""
import json
import sys
from ysg_mail.agent_enrollment import join, load_agent_settings, _credential


def enroll(request):
    identity = request['identity']
    receipt = join(identity, request['boards'])
    remote = load_agent_settings(identity).remote
    profile = {'url': str(remote.url), 'token': _credential(remote.token_file)}
    if remote.ca_file is not None:
        profile['ca_pem'] = remote.ca_file.read_text()
    return {'identity': receipt['identity'],
            'boards': [b.removeprefix('/b/') for b in receipt['boards']],
            'pitput': profile}


if __name__ == '__main__':
    try:
        request = json.loads(sys.stdin.buffer.read(65537))
        print(json.dumps(enroll(request)))
    except Exception:
        print('scoped agent enrollment failed; private diagnostics suppressed', file=sys.stderr)
        sys.exit(1)
