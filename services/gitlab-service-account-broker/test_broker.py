import concurrent.futures
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from urllib.request import Request, urlopen
from urllib.error import HTTPError
from http.server import ThreadingHTTPServer
from unittest.mock import patch

from broker import Broker, BrokerError, Handler


class BrokerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.secret = self.root / 'admin'
        self.secret.write_text('admin-secret')
        self.secret.chmod(0o600)
        self.client = 'client-secret-' + 'x' * 32
        self.config = dict(gitlab_url='https://gitlab.example', gitlab_token_file=str(self.secret),
                           state_dir=str(self.root / 'state'), group_id=7,
                           clients={'host/role/agent': hashlib.sha256(self.client.encode()).hexdigest()})
        self.broker = Broker(self.config)
        self.calls = []

    def api(self, path, data):
        self.calls.append((path, data))
        if path.endswith('service_accounts'):
            return dict(id=42, username=data['username'])
        return dict(user_id=42, token='service-token', expires_at=data['expires_at'])

    def test_create_reuse_restart_and_private_storage(self):
        with patch.object(self.broker, 'api', side_effect=self.api):
            first = self.broker.credentials('host/role/agent')
            self.assertEqual(first, self.broker.credentials('host/role/agent'))
        self.assertEqual(first, Broker(self.config).credentials('host/role/agent'))
        self.assertEqual(2, len(self.calls))
        self.assertEqual('/groups/7/service_accounts/42/personal_access_tokens', self.calls[1][0])
        self.assertEqual(0, self.broker.db.stat().st_mode & 0o077)

    def test_instance_endpoints(self):
        self.config.pop('group_id')
        self.config['state_dir'] = str(self.root / 'instance')
        broker = Broker(self.config)
        with patch.object(broker, 'api', side_effect=self.api):
            broker.credentials('host/role/agent')
        self.assertEqual('/users/42/personal_access_tokens', self.calls[1][0])

    def test_expiry_renews_token_without_new_account(self):
        with patch.object(self.broker, 'api', side_effect=self.api):
            self.broker.credentials('host/role/agent')
            with self.broker.connect() as db:
                db.execute('UPDATE accounts SET expires_at=?', (dt.date.today().isoformat(),))
            self.broker.credentials('host/role/agent')
        self.assertEqual(3, len(self.calls))
        self.assertTrue(self.calls[-1][0].endswith('personal_access_tokens'))

    def test_ambiguous_failure_is_not_retried(self):
        with patch.object(self.broker, 'api', side_effect=BrokerError('timeout')) as api:
            for _ in range(2):
                with self.assertRaises(BrokerError):
                    self.broker.credentials('host/role/agent')
            self.assertEqual(1, api.call_count)

    def test_concurrent_requests_mint_once(self):
        def get():
            try:
                return self.broker.credentials('host/role/agent')
            except BrokerError:
                return None  # Concurrent pending requests fail closed; retry after completion.
        with patch.object(self.broker, 'api', side_effect=self.api):
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                results = list(pool.map(lambda _: get(), range(16)))
        self.assertTrue(any(results))
        self.assertEqual(2, len(self.calls))

    def test_identity_authorization(self):
        self.assertTrue(self.broker.authorized('host/role/agent', self.client))
        self.assertFalse(self.broker.authorized('other', self.client))
        self.assertFalse(self.broker.authorized('host/role/agent', 'wrong'))

    def test_host_enrollment_is_scoped_idempotent_and_revocable(self):
        host_secret = 'host-secret-' + 'a' * 40
        client = 'b' * 64
        self.config['enrollment_clients'] = {'host': {
            'sha256': hashlib.sha256(host_secret.encode()).hexdigest(),
            'prefix': 'host/', 'profile': 'developer'}}
        broker = Broker(self.config)
        for identity in ['other/dev/agent', 'hostile/dev/agent', 'host/../agent', 'host//agent', '/host/dev/agent']:
            self.assertFalse(broker.enroll(identity, host_secret, client))
        self.assertFalse(broker.enroll('host/dev/agent', self.client, client))
        self.assertFalse(broker.enroll('host/role/agent', host_secret, client))
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            self.assertTrue(all(pool.map(lambda _: broker.enroll('host/dev/agent', host_secret, client), range(8))))
        self.assertFalse(broker.enroll('host/dev/agent', host_secret, 'c' * 64))
        self.assertTrue(broker.authorized('host/dev/agent', client))
        self.assertFalse(broker.authorized('host/dev/agent', host_secret))
        self.assertFalse(broker.authorized('host/role/agent', client))
        broker = Broker(self.config)
        with patch.object(broker, 'api', side_effect=self.api):
            result = broker.credentials('host/dev/agent')
        self.assertEqual(result['profile'], 'developer')
        self.assertIn('write_repository', result['scopes'])
        with broker.connect() as db:
            rows = db.execute('SELECT * FROM enrolled_clients').fetchall()
        self.assertNotIn(client, str(rows))
        self.assertNotIn(host_secret, str(rows))
        self.config['enrollment_clients'] = {}
        revoked = Broker(self.config)
        self.assertFalse(revoked.authorized('host/dev/agent', client))
        with self.assertRaises(BrokerError):
            revoked.credentials('host/dev/agent')

    def test_database_bound_to_issuer(self):
        for key, value in [('gitlab_url', 'https://other.example'), ('group_id', 8)]:
            with self.subTest(key=key), self.assertRaises(BrokerError):
                Broker(dict(self.config, **{key: value}))

    def test_profile_upgrade_reuses_account_and_replaces_cached_token(self):
        with patch.object(self.broker, 'api', side_effect=self.api):
            self.broker.credentials('host/role/agent')
        self.config['identity_profiles'] = {'host/role/agent': 'developer'}
        self.config['ssh_hosts'] = ['ssh.gitlab.example']
        broker = Broker(self.config)
        with patch.object(broker, 'api', side_effect=self.api):
            result = broker.credentials('host/role/agent')
            self.assertEqual(result, broker.credentials('host/role/agent'))
        self.assertEqual(3, len(self.calls))
        self.assertEqual(['api', 'write_repository'], self.calls[-1][1]['scopes'])
        self.assertEqual('developer', result['profile'])
        self.assertEqual(['ssh.gitlab.example'], result['ssh_hosts'])
        self.config['identity_profiles']['host/role/agent'] = 'observer'
        broker = Broker(self.config)
        with patch.object(broker, 'api', side_effect=self.api):
            result = broker.credentials('host/role/agent')
        self.assertEqual(['read_api', 'read_repository'], result['scopes'])
        self.assertEqual(4, len(self.calls))

    def test_legacy_database_migration_preserves_token(self):
        with patch.object(self.broker, 'api', side_effect=self.api):
            first = self.broker.credentials('host/role/agent')
        with self.broker.connect() as db:
            db.execute('ALTER TABLE accounts DROP COLUMN token_scopes')
            db.execute("UPDATE metadata SET value=? WHERE key='issuer'",
                       (json.dumps([self.broker.origin, self.broker.base, ['api']]),))
        broker = Broker(self.config)
        with patch.object(broker, 'api', side_effect=AssertionError('unexpected provisioning')):
            self.assertEqual(first, broker.credentials('host/role/agent'))

    def test_invalid_policy_rejected(self):
        for change in [{'identity_profiles': {'unknown': 'developer'}},
                       {'identity_profiles': {'host/role/agent': 'admin'}},
                       {'ssh_hosts': ['ssh.example/path']}, {'ssh_hosts': ['bad\nhost']}]:
            with self.subTest(change=change), self.assertRaises(BrokerError):
                Broker(dict(self.config, **change))

    def test_insecure_config_rejected(self):
        with self.assertRaises(BrokerError):
            Broker(dict(self.config, gitlab_url='http://gitlab.example'))
        self.secret.chmod(0o644)
        with self.assertRaises(BrokerError):
            Broker(self.config)

    def test_http_auth_redaction_and_reuse(self):
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        server.broker = self.broker
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        def stop():
            server.shutdown()
            server.server_close()
            thread.join()
        self.addCleanup(stop)
        url = 'http://127.0.0.1:%d/v1/credentials' % server.server_port
        def request(identity, bearer):
            return urlopen(Request(url, data=json.dumps({'identity': identity}).encode(),
                                   headers={'Authorization': 'Bearer ' + bearer}), timeout=3)
        with patch.object(self.broker, 'api', side_effect=self.api):
            for identity, bearer in [('other', self.client), ('host/role/agent', 'wrong')]:
                with self.assertRaises(HTTPError) as error:
                    request(identity, bearer)
                self.assertEqual(403, error.exception.code)
                error.exception.close()
            self.assertEqual([], self.calls)
            with self.assertRaises(HTTPError) as error:
                urlopen(Request(url, data=json.dumps({'identity': 'host/role/agent', 'profile': 'developer'}).encode(),
                                headers={'Authorization': 'Bearer ' + self.client}), timeout=3)
            self.assertEqual(400, error.exception.code)
            error.exception.close()
            self.assertEqual([], self.calls)
            with request('host/role/agent', self.client) as response:
                self.assertEqual('no-store', response.headers['Cache-Control'])
                self.assertEqual('service-token', json.load(response)['token'])

    def test_gitlab_http_contract(self):
        # Exercise the actual HTTP implementation against a local GitLab API fixture.
        from http.server import BaseHTTPRequestHandler
        seen = []
        owner = self
        class GitLab(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                seen.append((self.path, self.headers.get('PRIVATE-TOKEN'), self.headers.get('User-Agent')))
                data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                body = json.dumps(owner.api(self.path.removeprefix('/api/v4'), data)).encode()
                self.send_response(201)
                self.end_headers()
                self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1', 0), GitLab)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            # Only fixture transport uses HTTP; configuration validation requires HTTPS.
            self.broker.origin = 'http://127.0.0.1:%d' % server.server_port
            self.broker.credentials('host/role/agent')
            self.assertEqual(2, len(seen))
            self.assertTrue(all(auth == 'admin-secret' for _, auth, _ in seen))
            self.assertTrue(all(agent == 'gitlab-service-account-broker/0.1' for _, _, agent in seen))
            self.assertEqual('/api/v4/groups/7/service_accounts', seen[0][0])
        finally:
            server.shutdown()
            server.server_close()
            thread.join()



class FakeMembers:
    """In-memory GitLab members API: {(collection, encoded_path): {user_id: (level, expires_at)}}."""

    def __init__(self):
        self.members = {}
        self.calls = []
        self.fail = None  # (method, status) or (method, BrokerError) to inject once per call

    def __call__(self, method, path, data=None, timeout=None):
        self.calls.append((method, path, data))
        parts = path.split('/')  # ['', collection, encoded, 'members', user?]
        key = (parts[1], parts[2])
        table = self.members.setdefault(key, {})
        if self.fail and self.fail[0] == method:
            outcome = self.fail[1]
            if isinstance(outcome, Exception):
                if getattr(outcome, 'applied', False):
                    self.apply(method, table, parts, data)
                raise outcome
            return outcome, None
        return self.apply(method, table, parts, data)

    def apply(self, method, table, parts, data):
        if method == 'GET':
            uid = int(parts[4])
            if uid not in table:
                return 404, None
            level, expires = table[uid]
            return 200, {'id': uid, 'access_level': level, 'expires_at': expires}
        if method == 'POST':
            if data['user_id'] in table:
                return 409, None
            table[data['user_id']] = (data['access_level'], data['expires_at'])
            return 201, {'id': data['user_id']}
        uid = int(parts[4])
        if uid not in table:
            return 404, None
        if method == 'PUT':
            table[uid] = (data['access_level'], data['expires_at'])
            return 200, {'id': uid}
        del table[uid]
        return 204, None

    def writes(self):
        return [call for call in self.calls if call[0] != 'GET']


class MembershipTests(unittest.TestCase):
    setUp = BrokerTests.setUp
    api = BrokerTests.api

    def broker_with(self, memberships):
        self.config['identity_memberships'] = memberships
        broker = Broker(self.config)
        self.fake = getattr(self, 'fake', None) or FakeMembers()
        return broker

    def issue(self, broker):
        with patch.object(broker, 'api', side_effect=self.api), patch.object(broker, 'member_api', side_effect=self.fake):
            return broker.credentials('host/role/agent')

    def records(self, broker):
        with broker.connect() as db:
            return db.execute('SELECT kind,path,access_level,expires_at,pending_access_level FROM memberships').fetchall()

    def test_invalid_memberships_rejected(self):
        ok = {'project': 'acme/pitput', 'role': 'developer'}
        for value in [[], {'x': 'y'}, {'host/role/agent': ok}, {'host//': [ok]}, {'/host/': [ok]},
                      {'host/role/agent': [dict(ok, role='owner')]}, {'host/role/agent': [dict(ok, role='admin')]},
                      {'host/role/agent': [dict(ok, extra=1)]}, {'host/role/agent': [{'role': 'guest'}]},
                      {'host/role/agent': [{'project': 'a/b', 'group': 'a', 'role': 'guest'}]},
                      {'host/role/agent': [{'user': 'a/b', 'role': 'guest'}]},
                      {'host/role/agent': [{'project': 'pitput', 'role': 'guest'}]},
                      {'host/role/agent': [{'project': 'a/../b', 'role': 'guest'}]},
                      {'host/role/agent': [{'project': '/a/b', 'role': 'guest'}]},
                      {'host/role/agent': [{'project': 'a/b/', 'role': 'guest'}]},
                      {'host/role/agent': [{'project': 'a/b.git', 'role': 'guest'}]},
                      {'host/role/agent': [{'group': 'a%2Fb', 'role': 'guest'}]},
                      {'host/role/agent': [{'group': 7, 'role': 'guest'}]},
                      {'host/role/agent': [ok, dict(ok, role='guest')]}]:
            with self.subTest(value=value), self.assertRaises(BrokerError):
                Broker(dict(self.config, identity_memberships=value))
        Broker(dict(self.config, identity_memberships={
            'host/': [{'group': 'acme', 'role': 'guest'}], 'host/role/agent': [ok, {'group': 'a/b-c.d', 'role': 'maintainer'}]}))

    def test_precedence_exact_then_longest_prefix(self):
        broker = self.broker_with({
            'host/': [{'group': 'short', 'role': 'guest'}],
            'host/role/': [{'group': 'long', 'role': 'reporter'}],
            'host/role/exact': [],
            'host/role/agent': [{'project': 'a/exact', 'role': 'developer'}]})
        self.assertEqual({('project', 'a/exact'): 30}, broker.desired_memberships('host/role/agent'))
        self.assertEqual({}, broker.desired_memberships('host/role/exact'))
        self.assertEqual({('group', 'long'): 20}, broker.desired_memberships('host/role/other'))
        self.assertEqual({('group', 'short'): 10}, broker.desired_memberships('host/rolex/agent'))
        self.assertEqual({}, broker.desired_memberships('hostile/role/agent'))

    def test_grant_is_idempotent_recorded_and_expires_with_credential(self):
        broker = self.broker_with({'host/role/': [{'project': 'acme/pitput', 'role': 'maintainer'},
                                                  {'group': 'acme/sub', 'role': 'reporter'}]})
        first = self.issue(broker)
        self.assertEqual({'identity', 'username', 'user_id', 'token', 'expires_at', 'host', 'scopes', 'profile', 'ssh_hosts'},
                         set(first))
        self.assertEqual((40, first['expires_at']), self.fake.members[('projects', 'acme%2Fpitput')][42])
        self.assertEqual((20, first['expires_at']), self.fake.members[('groups', 'acme%2Fsub')][42])
        self.assertEqual(2, len(self.fake.writes()))
        self.assertIn(('POST', '/projects/acme%2Fpitput/members',
                       {'user_id': 42, 'access_level': 40, 'expires_at': first['expires_at']}), self.fake.calls)
        self.assertEqual(sorted([('group', 'acme/sub', 20, first['expires_at'], None),
                                 ('project', 'acme/pitput', 40, first['expires_at'], None)]),
                         sorted(self.records(broker)))
        self.assertEqual(first, self.issue(broker))
        self.assertEqual(2, len(self.fake.writes()))

    def test_renewal_extends_and_config_changes_downgrade_or_remove(self):
        rule = {'project': 'acme/pitput', 'role': 'maintainer'}
        broker = self.broker_with({'host/role/agent': [rule, {'group': 'acme', 'role': 'guest'}]})
        first = self.issue(broker)
        self.config['token_days'] = 20
        broker = Broker(self.config)
        with broker.connect() as db:
            db.execute('UPDATE accounts SET expires_at=?', (dt.date.today().isoformat(),))
        renewed = self.issue(broker)
        self.assertNotEqual(first['expires_at'], renewed['expires_at'])
        self.assertIn(('PUT', '/projects/acme%2Fpitput/members/42',
                       {'access_level': 40, 'expires_at': renewed['expires_at']}), self.fake.calls)
        self.assertEqual((40, renewed['expires_at']), self.fake.members[('projects', 'acme%2Fpitput')][42])
        broker = self.broker_with({'host/role/agent': [dict(rule, role='reporter')]})
        self.issue(broker)
        self.assertEqual((20, renewed['expires_at']), self.fake.members[('projects', 'acme%2Fpitput')][42])
        self.assertNotIn(42, self.fake.members[('groups', 'acme')])
        self.assertIn(('DELETE', '/groups/acme/members/42', None), self.fake.calls)
        broker = self.broker_with({})
        self.issue(broker)
        self.assertNotIn(42, self.fake.members[('projects', 'acme%2Fpitput')])
        self.assertEqual([], self.records(broker))

    def test_human_memberships_are_never_touched(self):
        project, group = ('projects', 'acme%2Fpitput'), ('groups', 'acme')
        self.fake = FakeMembers()
        self.fake.members[group] = {42: (30, None)}  # Added by a human before any rule existed.
        broker = self.broker_with({'host/role/agent': [{'project': 'acme/pitput', 'role': 'developer'},
                                                       {'group': 'acme', 'role': 'guest'}]})
        result = self.issue(broker)
        self.assertEqual((30, None), self.fake.members[group][42])
        self.assertEqual([('project', 'acme/pitput', 30, result['expires_at'], None)], self.records(broker))
        self.fake.members[project][42] = (40, '2099-01-01')  # A human later changes the broker's grant.
        broker = self.broker_with({})
        self.issue(broker)
        self.assertEqual((40, '2099-01-01'), self.fake.members[project][42])
        self.assertEqual((30, None), self.fake.members[group][42])
        self.assertEqual([], self.records(broker))
        self.assertFalse([c for c in self.fake.writes() if c[0] in {'PUT', 'DELETE'}])

    def test_gitlab_failure_still_issues_logs_nothing_secret_and_retries(self):
        broker = self.broker_with({'host/role/agent': [{'project': 'acme/pitput', 'role': 'developer'}]})
        self.fake.fail = ('POST', 500)
        with self.assertLogs('gitlab-service-account-broker', 'WARNING') as logs:
            result = self.issue(broker)
        self.assertEqual('service-token', result['token'])
        output = '\n'.join(logs.output)
        self.assertIn('HTTP 500', output)
        self.assertNotIn('service-token', output)
        self.assertNotIn('admin-secret', output)
        self.fake.fail = None
        self.issue(broker)
        self.assertEqual((30, result['expires_at']), self.fake.members[('projects', 'acme%2Fpitput')][42])

    def test_ambiguous_grant_stays_owned_and_refusal_claims_nothing(self):
        broker = self.broker_with({'host/role/agent': [{'project': 'acme/pitput', 'role': 'developer'}]})
        lost = BrokerError('GitLab membership request failed')
        lost.applied = True  # GitLab applied the POST but the response was lost.
        self.fake.fail = ('POST', lost)
        with self.assertLogs('gitlab-service-account-broker', 'WARNING'):
            self.issue(broker)
        self.fake.fail = None
        broker = self.broker_with({})
        self.issue(broker)
        self.assertNotIn(42, self.fake.members[('projects', 'acme%2Fpitput')])
        broker = self.broker_with({'host/role/agent': [{'project': 'acme/pitput', 'role': 'developer'}]})
        self.fake.fail = ('POST', 403)
        with self.assertLogs('gitlab-service-account-broker', 'WARNING'):
            self.issue(broker)
        self.assertEqual([], self.records(broker))

    def test_member_api_http_contract_and_redaction(self):
        from http.server import BaseHTTPRequestHandler
        seen = []
        class GitLab(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def handle_any(self):
                length = int(self.headers.get('Content-Length') or 0)
                body = json.loads(self.rfile.read(length)) if length else None
                seen.append((self.command, self.path, self.headers.get('PRIVATE-TOKEN'), body))
                if self.command == 'GET':
                    raw = b'{"message":"404 Not found leaked-secret"}'
                    self.send_response(404)
                else:
                    raw = b'leaked-secret'
                    self.send_response(500)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            do_GET = do_POST = do_PUT = do_DELETE = handle_any
        server = ThreadingHTTPServer(('127.0.0.1', 0), GitLab)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            broker = self.broker_with({'host/role/agent': [{'project': 'acme/pitput', 'role': 'developer'}]})
            broker.origin = 'http://127.0.0.1:%d' % server.server_port
            with self.assertLogs('gitlab-service-account-broker', 'WARNING') as logs:
                with patch.object(broker, 'api', side_effect=self.api):
                    result = broker.credentials('host/role/agent')
            self.assertEqual(['GET', 'POST'], [s[0] for s in seen])
            self.assertEqual('/api/v4/projects/acme%2Fpitput/members/42', seen[0][1])
            self.assertEqual('/api/v4/projects/acme%2Fpitput/members', seen[1][1])
            self.assertEqual({'user_id': 42, 'access_level': 30, 'expires_at': result['expires_at']}, seen[1][3])
            self.assertTrue(all(s[2] == 'admin-secret' for s in seen))
            output = '\n'.join(logs.output)
            self.assertNotIn('leaked-secret', output)
            self.assertNotIn('admin-secret', output)
            self.assertNotIn('service-token', output)
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == '__main__':
    unittest.main()
