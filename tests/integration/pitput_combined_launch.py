"""Disposable installed Pitput + production HRX owned Codex combined-attention proof."""

import argparse
import hashlib
import http.server
import importlib.util
import json
import os
import re
from pathlib import Path
import shlex
import subprocess
import sys
import threading
import time
import unittest

from pitput_exchange import PitputIntegration, pitput, private
import pitput_targeted_launch as ownership

ROOT = Path(__file__).resolve().parents[2]
PROVENANCE = None


def load_adapter():
    source = Path(os.environ['HRX_PITPUT_OVERRIDE_WORKDIR']) / 'source/scripts/compact_codex_adapter.py'
    spec = importlib.util.spec_from_file_location('combined_codex_adapter', source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class Provider:
    def __init__(self, cwd):
        self.module = load_adapter()
        self.engine = self.module.CodexCompactAdapter(str(cwd), python=Path(sys.executable),
                                                     body_byte_limit=128, reply_to_asks=True)
        self.config = None
        self.source = None
        self.snapshot = None
        self.seen = []
        self.completed = []
        self.completion_facts = {}
        self.errors = []
        self.engine.expectation_loader = self.resolve
        provider = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                try:
                    size = int(self.headers.get('Content-Length', '0'))
                    if not 0 < size <= 1000000:
                        raise ValueError('bounded_request_required')
                    request = json.loads(self.rfile.read(size))
                    reply = provider.engine.respond(provider.module.normalize(request))
                    if isinstance(reply, str) and provider.engine.facts()['completed']:
                        kind = provider.seen[-1].header.source
                        provider.completed.append(kind)
                        provider.completion_facts[kind] = provider.engine.facts()
                        reply += '\nfixture-combined-complete:' + kind
                    events = provider.module.response_events(reply, 'combined-fixture')
                    body = ''.join('data: ' + json.dumps(event) + '\n\n' for event in events).encode()
                    self.send_response(200)
                    self.send_header('Content-Type', 'text/event-stream')
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                except Exception as error:
                    code = str(error)
                    allowed = {
                        'invalid_fixture_native_tool_result',
                        'missing_ambiguous_or_oversized_fixture_envelope',
                        'invalid_fixture_envelope', 'fixture_helper_failed',
                    }
                    provider.errors.append(code if code in allowed else type(error).__name__)
                    self.send_response(500)
                    self.end_headers()

        self.server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.server.server_port}'

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)

    def resolve(self, text):
        from ysg_mail import codex_bridge as bridge
        from ysg_mail.attention_delivery import MessageSnapshot
        from ysg_mail.attention_protocol import Metadata, Receipt
        config = bridge._load_config(self.config)
        store = bridge.Store(config, config.agents[0])
        metadata = Metadata.model_validate_json(
            (store.directory / 'attention-metadata.json').read_bytes())
        receipt = Receipt.model_validate_json(
            (store.directory / 'attention-presentation.json').read_bytes())
        event = bridge._event(metadata.event, store.agent)
        if receipt.presentation.text != text or receipt.header.combined_delivery_id != event['event_key']:
            raise ValueError('exact_combined_notice_required')
        bridge._validate_binding(store, event, metadata.origin)
        if receipt.header.source == 'dm':
            source = event['combined']['attention']
            expected = MessageSnapshot(service_id=event['cursor'].split(':')[0], source='dm',
                                       handle=self.source['message'],
                                       event_key=self.source['event_key'],
                                       sequence=source['inbox_seq'], sender='test/controller/agent',
                                       body=self.source['body'])
        elif receipt.header.source == 'ask':
            notice = event['combined']['notice']['selection']['notice']
            expected = MessageSnapshot(service_id=notice['service_id'], source='ask',
                                       handle=notice['ask_id'], event_key=notice['notice_id'],
                                       sequence=notice['ask_version'], sender=notice['actor'],
                                       body=self.snapshot.model_dump_json())
        else:
            raise ValueError('unexpected_fixture_source')
        self.seen.append(receipt)
        return receipt, expected


class CombinedLaunch(PitputIntegration):
    result = {'status': 'NOT_RUN'}

    @classmethod
    def start_server(cls):
        text = cls.server_config.read_text()
        if 'combined_attention_preview' not in text:
            private(cls.server_config, text.replace('[server]\n',
                                                    '[server]\ncombined_attention_preview = true\n'))
        super().start_server()

    def test_combined(self):
        from ysg_mail.ask_snapshot import AskSnapshot
        env = self.agent_env('combined')
        home = Path(env['HOME'])
        control = home / 'control'
        control.mkdir(mode=0o700)
        unrelated = home / 'unrelated-cwd'
        unrelated.mkdir(mode=0o700)
        private(home / 'bin/pitput',
                f'#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput "$@"\n')
        (home / 'bin/pitput').chmod(0o700)
        binary = self.root / 'hrx-native.test'
        hrx = self.root / 'hrx'
        subprocess.run(['go', 'test', '-c', '-race', '-o', str(binary), './internal/app'],
                       cwd=ROOT, check=True, timeout=300)
        subprocess.run(['go', 'build', '-o', str(hrx), './cmd/hrx'],
                       cwd=ROOT, check=True, timeout=120)
        provider = Provider(unrelated)
        self.addCleanup(provider.close)
        env.update(PATH=str(home / 'bin') + os.pathsep + self.env['PATH'],
                   CI='true', PITPUT_NATIVE_FIXTURE_EXECUTION_AUTHORIZED='1',
                   HRX_NATIVE_SESSION_TESTS='1', HRX_PITPUT_INTEGRATION='1',
                   HRX_PITPUT_COMBINED_NATIVE='1', HRX_PITPUT_COMBINED_CONTROL=str(control),
                   HRX_PITPUT_COMBINED_PROVIDER=provider.url, HRX_PITPUT_ADMISSION_BINARY=str(hrx))
        log = (self.root / 'combined-native.log').open('w+')
        self.addCleanup(log.close)
        summary = control / 'supervisor.json'
        process = ownership.start_owned(
            [str(binary), '-test.run=^TestNativePitputCombinedLaunchCodex$', '-test.timeout=300s',
             '-test.v'], summary=summary, env=env, cwd=ROOT, log=log)
        cleanup = None
        stage = 'waiting_stopped_runtime'
        secrets = []

        def leader_done():
            return (not ownership.supervisor_running(process)
                    or Path(str(summary) + '.main').exists())

        try:
            self.assertTrue(ownership.wait_ready(control / 'stopped.ready',
                                                leader_done, timeout=120))
            stopped = json.loads((control / 'stopped.ready').read_text())
            provider.config = Path(stopped['config'])
            stage = 'create_private_source_and_ask'
            body = 'Synthetic owned controller source. ' * 10 + 'HOLD: do not deploy.'
            source = pitput(self.controller, *self.prefix, 'dm', 'send', 'test/combined/agent',
                            '--body', body, '--request-id', 'combined-source')
            provider.source = {**source, 'body': body}
            secrets.append(body)
            created = pitput(self.controller, *self.prefix, 'ask', 'create', '--source', 'dm',
                             '--event-key', source['event_key'], '--to', 'test/combined/agent',
                             '--question', 'Review the synthetic migration; HOLD deployment.',
                             '--due', '2030-01-01T00:00:00Z', '--request-id', 'combined-ask')
            raw = pitput(self.controller, *self.prefix, 'ask', 'show', created['ask_id'],
                         '--version', '1')
            raw.pop('snapshot_version', None)
            provider.snapshot = AskSnapshot.model_validate_json(json.dumps(raw))
            outsider = self.agent_env('outsider')
            pitput(outsider, 'join', 'exchange', '--identity', 'test/outsider/agent')
            denied = pitput(outsider, '--agent', 'test/outsider/agent', 'ask', 'show',
                            created['ask_id'], check=False)
            self.assertNotEqual(denied.returncode, 0)
            self.assertIn('remote_http_404', denied.stderr)
            seed = '''
import json,sys
from pathlib import Path
from ysg_mail import codex_bridge as bridge
from ysg_mail.combined_bridge import receive_next
config=bridge._load_config(Path(sys.argv[1]))
store=bridge.Store(config,config.agents[0])
assert receive_next(store)
with store.access() as (_,state):
 print(json.dumps({'delivery_id':state['pending']['event']['event_key']}))
'''
            stage = 'persist_pending_while_stopped'
            selected = subprocess.run([sys.executable, '-I', '-c', seed, str(provider.config)],
                                      env=env, cwd=unrelated, capture_output=True, text=True, timeout=30)
            self.assertEqual(selected.returncode, 0, 'pending selection failed')
            pending_id = json.loads(selected.stdout)['delivery_id']
            stage = 'resume_same_native_session'
            private(control / 'resume.go', '{}')
            self.assertTrue(ownership.wait_ready(control / 'resumed.ready',
                                                leader_done, timeout=90))
            stage = 'installed_helper_completion'
            deadline = time.monotonic() + 120
            while sorted(provider.completed) != ['ask', 'dm']:
                self.assertFalse(provider.errors, 'scripted provider failed')
                self.assertLessEqual(len(provider.completed), 2, 'duplicate source completion')
                self.assertFalse(leader_done(), 'native leader exited early')
                self.assertLess(time.monotonic(), deadline, 'native helper completion deadline')
                time.sleep(0.1)
            self.assertEqual(provider.seen[0].header.combined_delivery_id, pending_id)
            final = pitput(self.controller, *self.prefix, 'ask', 'show', created['ask_id'])
            self.assertEqual((final['status'], final['version']), ('replied', 2))
            self.assertTrue(provider.completion_facts['ask']['ask_reply_confirmed'])
            stage = 'native_publication_replay'
            state_root = json.loads(subprocess.run(
                [sys.executable, '-I', '-c',
                 'import json,sys,tomllib;print(json.dumps(tomllib.load(open(sys.argv[1],"rb"))["state_directory"]))',
                 str(provider.config)], env=env, capture_output=True, text=True, check=True).stdout)
            target = hashlib.sha256(b'test/combined/agent').hexdigest()
            queue = Path(state_root) / target / 'monitor-publications.json'
            deadline = time.monotonic() + 30
            while True:
                publications = json.loads(queue.read_text()) if queue.exists() else {}
                if (not publications.get('entries', [1])
                        and len(publications.get('acknowledged', [])) >= 2):
                    break
                self.assertLess(time.monotonic(), deadline, 'native report replay deadline')
                time.sleep(0.1)
            self.assertTrue(all(item['attribution'] == 'known'
                                for item in publications['acknowledged']))
            stage = 'native_post_tool_completion'
            private(control / 'done.go', '{}')
            self.assertTrue(ownership.wait_ready(control / 'native-receipt.json',
                                                leader_done, timeout=40))
            deadline = time.monotonic() + 45
            while not leader_done():
                self.assertLess(time.monotonic(), deadline, 'owned cleanup deadline')
                time.sleep(0.1)
            cleanup = ownership.stop_owned(process, summary)
            self.assertEqual(cleanup['classification'], 'clean')
            native = json.loads((control / 'native-receipt.json').read_text())
            self.assertEqual(native['status'], 'PASS')
            log.seek(0)
            output = log.read()
            self.assertNotIn(body, output)
            self.assertIn('--- PASS: TestNativePitputCombinedLaunchCodex', output)
            type(self).result = {'status': 'PASS', 'native': native,
                                 'sources': [item.header.source for item in provider.seen],
                                 'pending_delivery_preserved': True, 'privacy_denied': True,
                                 'installed_exact_reads': True, 'recipient_reply': True,
                                 'explicit_ack': True, 'ask_unresolved': True,
                                 'known_native_publications': True, 'owned_cleanup': cleanup}
        finally:
            type(self).result['provider_facts'] = provider.engine.facts()
            type(self).result['completed_sources'] = provider.completed
            type(self).result['completion_facts'] = provider.completion_facts
            type(self).result['seen_sources'] = [item.header.source for item in provider.seen]
            type(self).result['provider_errors'] = provider.errors
            if cleanup is None:
                cleanup = ownership.stop_owned(process, summary)
                type(self).result.update(status='FAIL', owned_cleanup=cleanup)
            if provider.config is not None:
                from ysg_mail import codex_bridge as bridge
                try:
                    config = bridge._load_config(provider.config)
                    store = bridge.Store(config, config.agents[0])
                    state = json.loads(store.state_file.read_text())
                    pending = state.get('pending') or {}
                    detail = {
                        'pending': bool(pending), 'delivery': pending.get('delivery'),
                        'ack_requested': pending.get('ack_requested'),
                        'attempts': pending.get('attempts'),
                        'handled_count': len(state.get('handled', [])),
                        'last_error': state.get('last_error'),
                    }
                    type(self).result['bridge_diagnostics'] = detail
                except Exception as error:
                    type(self).result['bridge_diagnostics'] = {'unavailable': type(error).__name__}
            type(self).result['python_stage'] = stage
            native_stage = control / 'native-stage.json'
            if native_stage.exists():
                value = json.loads(native_stage.read_text())
                type(self).result['native_stage'] = value.get('stage', 'unknown')
            log.seek(0)
            sanitized, leaked = ownership.sanitize(log.read(), secrets)
            lines = [re.sub(r'/tmp/[^\s:]+', '[owned-path]', line)
                     for line in sanitized.splitlines()
                     if re.search(r'(--- FAIL|Error:|\.go:[0-9]+:)', line)]
            type(self).result['diagnostics'] = lines[-12:]
            type(self).result['fixture_content_leaked'] = leaked


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--junit', type=Path, default=ROOT / 'artifacts/ci/pitput-combined.xml')
    args = parser.parse_args()
    if not os.environ.get('HRX_PITPUT_SOURCE_SHA'):
        raise SystemExit('BLOCKED: full HRX_PITPUT_SOURCE_SHA required')
    provenance = ownership.pitput_provenance(entrypoint=Path(__file__).resolve())
    reason = ownership.blocked_reason('codex')
    if reason:
        raise SystemExit('BLOCKED: ' + reason)
    result = unittest.TextTestRunner(verbosity=2).run(
        unittest.TestSuite([CombinedLaunch('test_combined')]))
    record = {**CombinedLaunch.result, 'pitput': provenance}
    args.junit.parent.mkdir(parents=True, exist_ok=True)
    args.junit.with_suffix('.json').write_text(json.dumps(record, indent=2) + '\n')
    status = result.wasSuccessful() and record['status'] == 'PASS'
    args.junit.write_text('<testsuite name="hrx-combined" tests="1" failures="' +
                          ('0' if status else '1') + '"><testcase name="owned-codex">' +
                          ('' if status else '<failure message="combined native proof failed"/>') +
                          '</testcase></testsuite>\n')
    return 0 if status else 1


if __name__ == '__main__':
    raise SystemExit(main())
