"""Frozen #30 matrix on installed Pitput and the production HRX Codex receive owner.

Run only in the existing disposable GitLab native image. No paid provider exists.
An incomplete or failed case prevents overall PASS and retains sanitized evidence.
"""

import argparse
import hashlib
import json
import os
import re
import secrets
import shlex
import subprocess
import sys
import time
import tomllib
import unittest
from pathlib import Path
from xml.etree import ElementTree as ET

import pitput_targeted_launch as ownership
from pitput_combined_acceptance_support import (
    ADMISSION_FAULT_WRAPPER,
    AckFaultProxy,
    MatrixProvider,
    persisted_payload_matches,
    lost_ack_evidence,
    policy_transition_payload,
    publish_control,
    safe_transport_observation,
    source_module,
)
from pitput_combined_launch import CombinedLaunch
from pitput_exchange import pitput, private

ROOT = Path(__file__).resolve().parents[2]
OWNER = 'test/combined/agent'
SENDER = 'test/controller/agent'


class FrozenAcceptance(CombinedLaunch):
    result = {'status': 'NOT_RUN', 'cases': {}}
    proxy = None
    viewer_token = secrets.token_hex(32)

    @classmethod
    def start_server(cls):
        text = cls.server_config.read_text()
        if 'test/viewer/agent' not in text:
            private(cls.server_config, text + '\n[server.agents."test/viewer/agent"]\n'
                    + 'token_sha256 = "' + hashlib.sha256(cls.viewer_token.encode()).hexdigest()
                    + '"\nboards = ["*"]\ncan_write = false\n')
        super().start_server()
        if cls.proxy is None:
            cls.proxy = AckFaultProxy(cls.url)
            cls.addClassCleanup(cls.proxy.close)
            cls.url = cls.proxy.url

    def request(self, operation, *, method='POST', payload=None, token=None, allow_denied=False):
        import httpx

        headers = {'Authorization': 'Bearer ' + (self.token if token is None else token)}
        url = self.url + '/v1/monitors/' + self.cursor['monitor_id'] + (operation if operation.startswith('?') else '/' + operation)
        response = httpx.request(method, url, headers=headers, json=payload, timeout=15)
        if not allow_denied:
            self.assertEqual(response.status_code, 200, 'service request failed: ' + operation)
        return response

    def scan(self):
        for _ in range(100):
            value = self.request('combined/scan', payload={'consumer': self.cursor['consumer']}).json()
            if value['status'] != 'continue':
                return value
        self.fail('bounded combined scan did not complete')

    def refresh_binding(self):
        config = tomllib.loads(self.provider.config.read_text())
        directory = Path(config['state_directory']) / hashlib.sha256(OWNER.encode()).hexdigest()
        self.spool = directory
        self.cursor = json.loads((directory / 'cursor.json').read_text())
        self.token = (Path(self.env_target['HOME']) /
                      '.config/pitput/agent-base/test/combined/agent/token').read_text().strip()

    def wait(self, condition, code, timeout=90):
        deadline = time.monotonic() + timeout
        while not condition():
            self.assertFalse(self.provider.errors, 'scripted provider error')
            self.assertFalse(self.leader_done(), 'production owner exited')
            self.assertLess(time.monotonic(), deadline, code)
            time.sleep(0.1)

    def command(self, action):
        step = self.step
        self.step += 1
        publish_control(self.control / f'command-{step:04d}.json', json.dumps({'action': action}))
        path = self.control / f'result-{step:04d}.json'
        self.wait(path.exists, 'production owner phase: ' + action, timeout=90)
        result = json.loads(path.read_text())
        self.assertEqual(result['thread'], self.thread)
        if action == 'observe':
            self.last_transport_observation = safe_transport_observation(result)
            self.last_transport_observation_at = time.monotonic()
        if action == 'resume':
            self.refresh_binding()
        return result

    def failure_observation(self):
        """Five-second failure-only read, never a handling retry or new assertion."""
        result = {'last_transport': getattr(self, 'last_transport_observation', None)}
        if hasattr(self, 'last_transport_observation_at'):
            result['last_transport_age_seconds'] = max(
                0, time.monotonic() - self.last_transport_observation_at)
        try:
            if self.leader_done():
                result['native'] = {'status': 'owner_unavailable'}
                return result
            step = self.step
            self.step += 1
            publish_control(self.control / f'command-{step:04d}.json',
                            json.dumps({'action': 'failure-observe'}))
            path = self.control / f'result-{step:04d}.json'
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if path.exists():
                    if path.stat().st_size > 65536:
                        raise ValueError('failure_projection_size')
                    observed = json.loads(path.read_text())
                    if observed.get('thread') != self.thread:
                        raise ValueError('failure_projection_binding')
                    result['native'] = observed['failure_observation']
                    return result
                if self.leader_done():
                    break
                time.sleep(0.05)
            result['native'] = {'status': 'deadline_or_owner_stopped'}
        except Exception as error:
            # Diagnostics never replace the primary failure or quote peer errors.
            result['native'] = {'status': 'unavailable', 'error_type': type(error).__name__}
        return result

    def post(self, label, *, addressed=True):
        from ysg_mail.attention_delivery import MessageSnapshot
        body = (('@' + OWNER + ' ') if addressed else '') + label + ' é🙂 ' + 'context ' * 36 + 'HOLD.'
        row = pitput(self.controller, *self.prefix, 'create', '/b/exchange/' + label,
                     '--author', SENDER, '--title', label, '--body', body, '--request-id', label)
        # The write receipt names the exact message; source history supplies its
        # authoritative sequence. Never infer a global source position from a path.
        read = pitput(self.controller, *self.prefix, 'read', row['path'])
        self.assertEqual(len(read), 1, 'created board source unavailable')
        source = read[0]
        self.assertEqual((source['event_key'], source['path']), (row['event_key'], row['path']))
        self.assertTrue(source['body'].encode() == body.encode(), 'created board bytes changed')
        snapshot = MessageSnapshot(service_id=self.cursor['service_id'], source='board',
                                   handle=row['path'], event_key=row['event_key'],
                                   sequence=source['seq'], sender=SENDER, body=body)
        self.result['source_manifest'].append({
            'label': label, 'source': 'board', 'source_event_key': row['event_key'],
            'path': row['path'], 'sequence': snapshot.sequence, 'addressed': addressed,
            'body_sha256': hashlib.sha256(body.encode()).hexdigest(), 'body_utf8_bytes': len(body.encode()),
        })
        if addressed:
            self.provider.arm(row['event_key'], label, snapshot)
        else:
            self.provider.whole_source.append(body.encode())
        return row

    def dm(self, label):
        from ysg_mail.attention_delivery import MessageSnapshot
        body = label + ' é🙂 ' + 'private context ' * 25 + 'HOLD.'
        row = pitput(self.controller, *self.prefix, 'dm', 'send', OWNER,
                     '--body', body, '--request-id', label)
        # Inbox sequence is recipient-local and comes from the real send receipt.
        # It is independently confirmed against the selected attention before native read.
        inbox = pitput(self.env_target, '--agent', OWNER, 'dm', 'inbox')
        sequence = next(item['inbox_seq'] for item in inbox if item['message'] == row['message'])
        self.result['source_manifest'].append({
            'label': label, 'source': 'dm', 'source_event_key': row['event_key'],
            'message': row['message'], 'sequence': sequence,
            'body_sha256': hashlib.sha256(body.encode()).hexdigest(), 'body_utf8_bytes': len(body.encode()),
        })
        self.provider.arm(row['event_key'], label, MessageSnapshot(
            service_id=self.cursor['service_id'], source='dm', handle=row['message'],
            event_key=row['event_key'], sequence=sequence, sender=SENDER, body=body))
        return row

    def ask(self, label, source):
        row = pitput(self.controller, *self.prefix, 'ask', 'create', '--source', 'board',
                     '--event-key', source['event_key'], '--to', OWNER,
                     '--question', label + ': review é🙂 and HOLD.', '--due', '2030-01-01T00:00:00Z',
                     '--request-id', label)
        self.arm_ask(row['ask_id'], 1, label)
        return row

    def arm_ask(self, ask_id, version, label):
        from ysg_mail.ask_snapshot import AskSnapshot
        from ysg_mail.attention_delivery import MessageSnapshot
        raw = pitput(self.controller, *self.prefix, 'ask', 'show', ask_id, '--version', str(version))
        raw.pop('snapshot_version', None)
        snapshot = AskSnapshot.model_validate_json(json.dumps(raw))
        # The typed notice ID/actor are source-selector fields, bound in resolve().
        self.provider.expected[(ask_id, version)] = {
            'label': label, 'typed': snapshot,
            'snapshot': MessageSnapshot(service_id=self.cursor['service_id'], source='ask',
                                        handle=ask_id, event_key=ask_id, sequence=version,
                                        sender=snapshot.event.command.actor if snapshot.event else SENDER,
                                        body=snapshot.model_dump_json()),
        }
        raw_bytes = snapshot.model_dump_json().encode()
        self.provider.whole_source.append(raw_bytes)
        self.result['source_manifest'].append({
            'label': label, 'source': 'ask', 'ask_id': ask_id, 'version': version,
            'snapshot_version': snapshot.snapshot_version,
            'body_sha256': hashlib.sha256(raw_bytes).hexdigest(), 'body_utf8_bytes': len(raw_bytes),
        })

    def handled(self, label):
        return [r for r in self.provider.records if r['label'] == label and (r['completed'] or r.get('receipt_lost_reconciled'))]

    def reconcile_crash_ack(self, label, phase, boundary, observed):
        if not observed['native_idle'] or observed.get('pending') is not None:
            return
        candidates = [r for r in self.provider.records if r['label'] == label]
        if len(candidates) != 1 or candidates[0]['completed'] or candidates[0].get('receipt_lost_reconciled'):
            return
        record = candidates[0]
        if not record.get('ack_command_issued') or self.provider.counter.snapshot()[1] != 0:
            return
        state = json.loads((self.spool / 'state.json').read_text())
        if not any(item.get('event_key') == record['delivery_id'] for item in state['handled']):
            return
        with self.database() as connection:
            acknowledgments = connection.execute(
                'SELECT record,delivery FROM pitput_combined_acknowledgments '
                'WHERE owner=%s AND delivery_id=%s', (OWNER, record['delivery_id']),
            ).fetchall()
        evidence = lost_ack_evidence(record, phase=phase, boundary=boundary,
            tombstones=state['handled'], acknowledgments=acknowledgments, owner=OWNER)
        self.provider.retire_reconciled_ack(record, evidence)

    def await_handled(self, labels, timeout=90, *, crash_ack=None):
        next_observation = 0.0
        def handled_with_transport_observation():
            nonlocal next_observation
            if time.monotonic() >= next_observation:
                observed = self.command('observe')  # Read-only health, never ACK or inference.
                if crash_ack is not None:
                    self.reconcile_crash_ack(labels[0], *crash_ack, observed)
                next_observation = time.monotonic() + 2
            return all(len(self.handled(label)) == 1 for label in labels)
        self.wait(handled_with_transport_observation,
                  'mandatory source not handled exactly once', timeout=timeout)
        def committed_and_idle():
            observed = self.command('observe')
            return observed.get('pending') is None and observed['native_idle']
        self.wait(committed_and_idle, 'explicit source ACK/native completion not committed', timeout=45)

    def quiet(self, generation):
        idle = source_module('native_quiescence')
        receipt = {}
        # Reuse the frozen observer's cadence, settle deadline and scheduler tolerance.
        name = 'idle-after-first-primer' if generation == 'fresh' else 'idle-after-recovery-full-primer'
        def observe():
            value = self.command('observe')
            return {'native_alive': value['transport_healthy'],
                    'bridges_alive': value['controller_healthy'],
                    'same_native_process': value['thread'] == self.thread,
                    'completed': value['native_idle'] and value['pending'] is None}
        idle.observe_window(name, self.provider.counter, observe, receipt)
        self.result['idle_windows'][generation] = receipt

    def case(self, number, action):
        self.result['active_case'] = number
        observed = self.result['cases'][str(number)] = {
            'status': 'RUNNING', 'before': self.safe_snapshot(),
        }
        try:
            facts = action()
        except Exception as error:
            observed['status'] = 'BLOCKED' if str(error).startswith('BLOCKED:') else 'FAIL'
            observed['after'] = self.safe_snapshot()
            raise
        observed.update(status='PASS', observations=facts, after=self.safe_snapshot())

    def test_frozen_matrix(self):
        oracle = source_module('combined_acceptance_oracles')
        self.oracle = oracle
        self.result = type(self).result = {'status': 'RUNNING', 'cases': {}, 'idle_windows': {},
                                          'provider': 'deterministic-loopback-only',
                                          'scope': 'production-owned-Codex/Linux; scripted tool proof',
                                          'source_manifest': []}
        self.env_target = self.agent_env('combined')
        home = Path(self.env_target['HOME'])
        self.control = home / 'control'
        self.control.mkdir(mode=0o700)
        unrelated = home / 'unrelated-cwd'
        unrelated.mkdir(mode=0o700)
        self.step = 0
        private(home / 'bin/pitput', f'#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput "$@"\n')
        (home / 'bin/pitput').chmod(0o700)
        binary, hrx = self.root / 'hrx-native.test', self.root / 'hrx'
        subprocess.run(['go', 'test', '-c', '-race', '-o', str(binary), './internal/app'], cwd=ROOT,
                       check=True, timeout=300)
        subprocess.run(['go', 'build', '-o', str(hrx), './cmd/hrx'], cwd=ROOT, check=True, timeout=120)
        admission = home / 'bin/admission-fault'
        private(admission, ADMISSION_FAULT_WRAPPER)
        admission.chmod(0o700)
        self.provider = MatrixProvider(unrelated)
        self.addCleanup(self.provider.close)
        self.env_target.update(
            PATH=str(home / 'bin') + os.pathsep + self.env['PATH'], CI='true',
            PITPUT_NATIVE_FIXTURE_EXECUTION_AUTHORIZED='1', HRX_NATIVE_SESSION_TESTS='1',
            HRX_PITPUT_INTEGRATION='1', HRX_PITPUT_COMBINED_ACCEPTANCE='1',
            HRX_PITPUT_COMBINED_CONTROL=str(self.control), HRX_PITPUT_COMBINED_PROVIDER=self.provider.url,
            HRX_PITPUT_ADMISSION_BINARY=str(admission), HRX_PITPUT_REAL_ADMISSION_BINARY=str(hrx))
        log = (self.root / 'acceptance-native.log').open('w+')
        self.addCleanup(log.close)
        summary = self.control / 'supervisor.json'
        process = ownership.start_owned(
            [str(binary), '-test.run=^TestNativePitputCombinedAcceptanceCodex$',
             '-test.timeout=16m', '-test.v'], summary=summary, env=self.env_target, cwd=ROOT, log=log)
        self.leader_done = lambda: (not ownership.supervisor_running(process)
                                   or Path(str(summary) + '.main').exists())
        try:
            self.wait((self.control / 'acceptance.ready').exists, 'production combined startup', 120)
            ready = json.loads((self.control / 'acceptance.ready').read_text())
            self.thread = ready['thread']
            self.provider.config = Path(ready['config'])
            self.refresh_binding()
            self.case(1, self.sources_and_quiet)
            self.case(2, self.fairness)
            self.case(3, self.exact_read_reply_ack)
            self.case(4, self.recovery)
            self.case(5, self.crashes)
            self.case(6, self.policy)
            self.case(7, self.stale_and_resolution)
            self.case(8, self.privacy)
            self.case(9, self.idle)
            self.provenance_chain()
            self.command('done')
            self.wait(self.leader_done, 'production completion', 45)
            self.result['status'] = 'PASS'
        finally:
            if self.result['status'] != 'PASS':
                self.result['failure_observation'] = self.failure_observation()
            cleanup = ownership.stop_owned(process, summary)
            self.result['owned_cleanup'] = cleanup
            if cleanup['classification'] != 'clean':
                self.result['status'] = 'FAIL'
            self.result['events'] = self.provider.records
            self.result['provider_errors'] = self.provider.errors
            self.result['native_tool_diagnostics'] = self.provider.tool_diagnostics
            self.result['final_source_state'] = self.safe_snapshot()
            sizes = {'whole_source': sum(map(len, self.provider.whole_source)),
                     'delivered': sum(r['notification_utf8_bytes'] for r in self.provider.records),
                     'exact_reads': sum(r['read_json_utf8_bytes'] for r in self.provider.records)}
            self.result['bytes'] = {'utf8_bytes': sizes,
                                    'utf8_div_4_token_estimates': {key: size / 4 for key, size in sizes.items()},
                                    'token_estimate_label': 'UTF-8 bytes / 4; not measured model tokens',
                                    'whole_source_baseline': 'same labeled transcript including ordinary discussion',
                                    'savings_threshold': None}
            self.result['provider_requests'], self.result['provider_active'] = self.provider.counter.snapshot()
            self.result['expected_owned_provider_cancellations'] = self.provider.expected_attention_cancellations
            self.result['expected_busy_provider_cancellations'] = self.provider.expected_busy_cancellations
            if self.result['status'] != 'PASS':
                self.result['status'] = 'FAIL'
            log.seek(0)
            output = log.read()
            sensitive = [entry['snapshot'].body for entry in self.provider.expected.values()]
            sensitive += [getattr(self, 'token', ''), self.viewer_token, self.enrollment_token]
            sanitized, leaked = ownership.sanitize(output, sensitive)
            self.result['diagnostics'] = [
                re.sub(r'/tmp/[^\s:]+', '[owned-path]', line)[:512]
                for line in sanitized.splitlines()
                if re.search(r'(--- FAIL|Error:|\.go:[0-9]+:)', line)
            ][-12:]
            self.result['fixture_content_leaked'] = leaked
            if leaked:
                self.result['status'] = 'FAIL'
            for entry in self.provider.expected.values():
                self.assertFalse(entry['snapshot'].body in output, 'source content leaked into native log')
            self.assertEqual(cleanup['classification'], 'clean', 'owned cleanup failed')

    def sources_and_quiet(self):
        self.post('ordinary-board', addressed=False)
        self.quiet('fresh')
        self.command('stop')
        board = self.post('matching-board')
        self.dm('incoming-dm')
        self.ask('addressed-ask', board)
        self.command('resume')
        self.await_handled(['matching-board', 'incoming-dm', 'addressed-ask'])
        wakeups = {'ordinary-board': []}
        for source, label in [('board', 'matching-board'), ('dm', 'incoming-dm'), ('ask', 'addressed-ask')]:
            wakeups[source] = [r['session'] for r in self.handled(label)]
        self.oracle.case_one(wakeups, self.thread)
        return {'wakeups': wakeups}

    def fairness(self):
        self.command('stop')
        labels = []
        for index in range(8):
            board = self.post(f'fair-board-{index}')
            self.dm(f'fair-dm-{index}')
            self.ask(f'fair-ask-{index}', board)
            labels.extend([f'fair-board-{index}', f'fair-dm-{index}', f'fair-ask-{index}'])
        start = len(self.provider.records)
        self.command('resume')
        self.await_handled(labels, timeout=180)
        order = [r['source'] for r in self.provider.records[start:]]
        # All three queues had >=8 eligible events: first12 have continuous eligibility.
        self.oracle.case_two(order[:12])
        return {'first_twelve_acknowledged_sources': order[:12], 'total_handled': len(order)}

    def exact_read_reply_ack(self):
        ask = self.provider.expected[next(key for key, item in self.provider.expected.items()
                                          if item['label'] == 'addressed-ask')]['snapshot'].handle
        final = pitput(self.controller, *self.prefix, 'ask', 'show', ask)
        self.assertEqual((final['status'], final['version']), ('replied', 2))
        records = self.provider.records
        self.assertTrue(all(r['read_sha256'] == r['expected_read_sha256'] for r in records))
        self.assertTrue(all(r.get('helper_ack', {}).get('acknowledged') is True for r in records))
        # Observe no new reply/version or inference after explicit ACKs settle.
        before = len(records)
        self.quiet('resumed')
        self.assertEqual(len(self.provider.records), before)
        again = pitput(self.controller, *self.prefix, 'ask', 'show', ask)
        self.assertEqual((again['status'], again['version']), ('replied', 2))
        return {'exact_byte_reads': len(records), 'recipient_status': again['status'],
                'recipient_version': again['version'], 'ack_only_new_deliveries': 0,
                'helper_working_directory': 'outside-checkout'}

    def recovery(self):
        self.provider.busy.clear()
        self.provider.busy_seen.clear()
        self.command('busy')
        self.wait(self.provider.busy_seen.is_set, 'foreground turn did not reach provider')
        self.dm('recovery-pending')
        self.wait(lambda: self.scan()['delivery'] is not None, 'busy target lost pending')
        original = self.scan()['delivery']
        observed = {'busy': original}
        self.server.terminate()
        self.server.wait(15)
        self.start_server()
        observed['server-restart'] = self.scan()['delivery']
        self.command('stop')
        observed['bridge-restart'] = self.scan()['delivery']
        self.provider.busy.set()
        observed['session-resume'] = self.scan()['delivery']
        self.oracle.case_four(original, observed)
        self.command('resume')
        self.await_handled(['recovery-pending'])
        self.assertEqual(self.handled('recovery-pending')[0]['delivery_id'], original['delivery_id'])
        return {'original_delivery': original, 'recovered_envelopes': observed,
                'eventual_handling_effects': len(self.handled('recovery-pending'))}

    def crashes(self):
        results = {}
        self.result['crash_observations'] = results
        for phase in ('before-admission', 'after-admission', 'before-ack-commit',
                      'after-ack-commit', 'lost-ack-response'):
            label = 'crash-' + phase
            self.provider.ack_retry_authorizer = None
            self.provider.read_resume_authorizer = None
            if phase in ('before-admission', 'after-admission'):
                (self.control / 'fault-entered.json').unlink(missing_ok=True)
                publish_control(self.control / 'admission-fault.json', json.dumps({'phase': phase}))
            else:
                self.proxy.arm(phase)
                self.provider.ack_retry_authorizer = self.proxy.authorize_failed_ack
            self.dm(label)
            if phase in ('before-admission', 'after-admission'):
                self.wait((self.control / 'fault-entered.json').exists, 'admission fault not reached')
                boundary = json.loads((self.control / 'fault-entered.json').read_text())
                if phase == 'after-admission':
                    self.assertEqual(boundary.get('owned_outcome', {}).get('status'), 'admitted',
                                     'BLOCKED: after-admission fault was not reached')
                else:
                    self.assertFalse(boundary['helper_invoked'])
                stopped = self.command('stop')
                if phase == 'after-admission':
                    self.provider.arm_read_resume(phase, boundary, stopped)
                self.command('resume')
            else:
                self.wait(self.proxy.hit.is_set, 'ACK fault not reached')
                boundary = self.proxy.observation
                if phase != 'before-ack-commit':
                    self.assertEqual(boundary['http_status'], 200)
                    self.assertEqual(boundary['acknowledgment']['status'], 'acknowledged')
                # Restart the real server after the captured transaction boundary.
                self.server.terminate()
                self.server.wait(15)
                self.start_server()
                self.proxy.mark_restored(phase)
            results[phase] = {'boundary': boundary, 'mandatory_work': 1}
            try:
                self.await_handled([label], crash_ack=(phase, boundary)
                                   if phase == 'after-admission' else None)
            finally:
                result = self.scan()
                results[phase].update(pending=int(result['delivery'] is not None),
                                      handling_effects=len(self.handled(label)),
                                      final_handling_effects=len(self.handled(label)))
                for filename in ('attention-admission.json', 'state.json'):
                    path = self.spool / filename
                    if path.exists():
                        raw = json.loads(path.read_text())
                        results[phase][filename] = {key: raw.get(key) for key in
                                                   ('phase', 'attempt', 'owned_outcome', 'last_error')}
            self.assertEqual(len(self.handled(label)), 1)
        self.provider.ack_retry_authorizer = None
        self.provider.read_resume_authorizer = None
        self.oracle.case_five(results)
        owner_crashes = {}
        self.result['owner_crash_observations'] = owner_crashes
        for phase in ('prepared', 'queue_response'):
            label = 'owner-crash-' + phase
            (self.control / 'owner-fault-entered.json').unlink(missing_ok=True)
            if phase == 'queue_response':
                self.provider.cancel_paused_attention = False
                self.provider.attention_seen.clear()
                self.provider.attention_release.clear()
            publish_control(self.control / 'owner-fault.json', json.dumps({'phase': phase}))
            baseline_requests = self.provider.counter.snapshot()[0]
            self.dm(label)
            self.wait((self.control / 'owner-fault-entered.json').exists,
                      'actual owned crash boundary not reached')
            point = json.loads((self.control / 'owner-fault-entered.json').read_text())
            self.assertEqual((point['phase'], point['thread_id'], point['attempt']),
                             (phase, self.thread, 1))
            self.assertEqual(point['response_ordinal'] == 0, phase == 'prepared')
            self.assertEqual(self.handled(label), [], 'native handled before required crash pause')
            stopped = self.command('stop')
            self.provider.cancel_paused_attention = True
            self.provider.attention_release.set()
            self.wait(lambda: self.provider.counter.snapshot()[1] == 0,
                      'canceled owned provider request not settled', timeout=15)
            if phase == 'queue_response':
                self.provider.arm_read_resume(phase, point, stopped)
            owner_crashes[phase] = {'boundary': point, 'mandatory_work': 1,
                                   'requests_before_resume': self.provider.counter.snapshot()[0] - baseline_requests,
                                   'handling_effects_before_resume': len(self.handled(label))}
            if phase == 'prepared':
                self.assertEqual(owner_crashes[phase]['requests_before_resume'], 0)
            self.command('resume')
            try:
                self.await_handled([label], crash_ack=(phase, point)
                                   if phase == 'queue_response' else None)
            finally:
                owner_crashes[phase]['final_handling_effects'] = len(self.handled(label))
                owner_crashes[phase]['source_pending'] = self.persisted_pending()
                saved = self.spool / 'attention-admission.json'
                if saved.exists():
                    raw = json.loads(saved.read_text())
                    owner_crashes[phase]['recovered_admission'] = {
                        key: raw.get(key) for key in ('phase', 'attempt', 'owned_outcome')
                    }
            handled = self.handled(label)[0]
            self.assertEqual((handled['bridge_delivery_id'], handled['payload_sha256']),
                             (point['delivery_id'], point['payload_sha256']))
            admission = json.loads((self.spool / 'attention-admission.json').read_text())
            evidence = admission.get('owned_outcome', {})
            reconciled = handled.get('receipt_lost_reconciled')
            if reconciled:
                owner_crashes[phase]['receipt_lost_reconciled'] = reconciled
                self.assertEqual(phase, 'queue_response')
            else:
                self.assertEqual((admission['phase'], evidence.get('status')), ('admitted', 'admitted'))
            if phase == 'prepared':
                self.assertEqual(admission['attempt'], 2, 'not-sent proof must precede explicit next attempt')
            elif not reconciled:
                self.assertEqual(admission['attempt'], 1, 'ambiguous admission must never be resubmitted')
                self.assertEqual(evidence['response_ordinal'], 0)
                self.assertIn(evidence['recovery_source'], ('queue', 'history'))
                self.assertGreater(evidence['recovery_ordinal'], 0)
            native_turns = self.command('continuation-observe')['native_turns']
            original_turns = [turn for turn in native_turns
                              if point['delivery_id'] in turn['client_ids']]
            self.assertEqual(sum(turn['client_ids'].count(point['delivery_id'])
                                 for turn in native_turns), 1,
                             'original notification/client ID must never be resent')
            self.assertEqual(len(original_turns), 1)
            owner_crashes[phase]['original_native_turn_id'] = original_turns[0]['id']
            owner_crashes[phase]['original_notification_count'] = 1
            if phase == 'queue_response':
                original_index = native_turns.index(original_turns[0])
                later_turns = native_turns[original_index + 1:]
                if reconciled:
                    self.assertIn(original_turns[0]['status'], ('completed', 'interrupted'))
                    self.assertEqual(later_turns, [], 'ACKed original work requires no continuation')
                    owner_crashes[phase]['continuation_not_required'] = True
                elif original_turns[0]['status'] == 'interrupted':
                    self.assertEqual(len(later_turns), 1, 'one bounded empty continuation required')
                    self.assertEqual(later_turns[0]['client_ids'], [])
                    self.assertEqual(later_turns[0]['status'], 'completed')
                    self.assertNotEqual(later_turns[0]['id'], original_turns[0]['id'])
                    owner_crashes[phase]['new_continuation_turn_id'] = later_turns[0]['id']
                else:
                    # The same crash boundary can leave a durable queued item.
                    # Its actual cold-resume completion needs no empty turn.
                    self.assertEqual(original_turns[0]['status'], 'completed')
                    self.assertEqual(later_turns, [])
                    owner_crashes[phase]['continuation_not_required'] = True
            owner_crashes[phase]['owned_outcome'] = evidence
            self.assertEqual(owner_crashes[phase]['final_handling_effects'], 1)
            self.provider.read_resume_authorizer = None
        return {'transport_faults': results, 'owner_crashes': owner_crashes}

    @staticmethod
    def source_ids(value):
        """Only stable routing IDs/cursors; never free-form source/policy text."""
        if not isinstance(value, dict):
            return None
        fields = ('source', 'delivery_id', 'source_monitor', 'event_key', 'seq', 'inbox_seq',
                  'path', 'message', 'conversation', 'cursor', 'notice_id', 'ask_id',
                  'ask_version', 'policy_version', 'scope_id')
        result = {}
        for key in fields:
            item = value.get(key)
            if type(item) in (int, bool) or item is None:
                if key in value:
                    result[key] = item
            elif isinstance(item, str):
                result[key] = item if len(item) <= 512 else '[overlong-id]'
        if isinstance(value.get('attention'), dict):
            result['attention'] = FrozenAcceptance.source_ids(value['attention'])
        if isinstance(value.get('notice'), dict):
            selected = value['notice'].get('selection') or {}
            result['typed_selection'] = FrozenAcceptance.source_ids(selected)
            result['typed_notice'] = FrozenAcceptance.source_ids(selected.get('notice'))
        return result

    @staticmethod
    def selection_flags(value):
        if not isinstance(value, dict):
            return None
        result = {key: value.get(key) for key in ('board_path', 'dm', 'asks')
                  if type(value.get(key)) in (str, bool) or value.get(key) is None}
        raw = value.get('board_filter_json')
        if isinstance(raw, str):
            spec = json.loads(raw)
            result['board_filter'] = {'path': spec.get('path'), 'after': spec.get('after'),
                                      'mentions': spec.get('mentions', [])[:8],
                                      'regex_count': len(spec.get('regex', [])),
                                      'followed_thread_count': len(spec.get('followed_threads', []))}
        raw = value.get('attention_policy_json')
        if isinstance(raw, str):
            policy = json.loads(raw)
            result['delivery_mode'] = policy.get('mode')
            result['rule_count'] = len(policy.get('rules', []))
        return result

    def safe_snapshot(self):
        """Fixed body-free IDs/state, bounded and JSON-safe; diagnostic failures never mask a case."""
        try:
            raw_policy = self.request('policy', method='GET').json()
            current = raw_policy.get('current_revision') or {}
            boundary = current.get('boundary') or {}
            policy = {'scope_id': raw_policy.get('scope_id'), 'version': current.get('version'),
                      'selection': self.selection_flags(current.get('selection')),
                      'board_after': boundary.get('board_after'), 'dm_after': boundary.get('dm_after'),
                      'ask_after': boundary.get('ask_after')}
            with self.database() as connection:
                row = connection.execute(
                    'SELECT spec,after_seq,pending,last_source,last_ack '
                    'FROM pitput_service_monitors WHERE owner=%s AND id=%s',
                    (OWNER, self.cursor['monitor_id']),
                ).fetchone()
                combined = connection.execute(
                    'SELECT pending,last_group,message_streak FROM pitput_combined_delivery WHERE owner=%s',
                    (OWNER,),
                ).fetchone() or {}
            spec = row.get('spec') or {}
            monitor = {'after_seq': row.get('after_seq'), 'last_source': row.get('last_source'),
                       'last_ack': row.get('last_ack'), 'pending': self.source_ids(row.get('pending')),
                       'spec': {'path': spec.get('path'), 'after': spec.get('after'),
                                'dm': spec.get('dm'), 'mentions': spec.get('mentions', [])[:8],
                                'regex_count': len(spec.get('regex', []))}}
            state = json.loads((self.spool / 'state.json').read_text())
            pending = state.get('pending') or {}
            snapshot = {'policy': policy, 'monitor': monitor,
                        'combined': {'last_group': combined.get('last_group'),
                                     'message_streak': combined.get('message_streak'),
                                     'pending': self.source_ids(combined.get('pending'))},
                        'consumer_generation': self.cursor['consumer']['generation'],
                        'native_session': self.thread,
                        'bridge': {'pending': bool(pending), 'delivery': pending.get('delivery'),
                                   'event_key': (pending.get('event') or {}).get('event_key'),
                                   'ack_requested': pending.get('ack_requested'),
                                   'attempts': pending.get('attempts'),
                                   'handled_count': len(state.get('handled', [])),
                                   'last_error': state.get('last_error')}}
            # Diagnostics must not turn a primary failure into an artifact encoding failure.
            json.dumps(snapshot)
            return snapshot
        except Exception as error:
            return {'unavailable': type(error).__name__}

    def database(self):
        import psycopg
        from psycopg.rows import dict_row
        return psycopg.connect(host=str(self.pg_socket), dbname='postgres', user='fixture',
                               row_factory=dict_row)

    def persisted_pending(self):
        with self.database() as connection:
            return connection.execute('SELECT pending FROM pitput_combined_delivery WHERE owner=%s',
                                      (OWNER,)).fetchone()['pending']

    def change_policy(self, *, mode=None, action='preserve'):
        from ysg_mail.monitoring_policy import Selection

        current = self.request('policy', method='GET').json()['current_revision']
        payload = policy_transition_payload(current, owner=OWNER, mode=mode, action=action)
        Selection.model_validate(payload['selection'])
        return self.request('policies', payload=payload).json()

    def resume_muted_policy(self, original, before):
        # Existing Codex owner has no resumed initialization inference. Backends
        # with an explicit native startup handshake may protect/measure it here.
        self.command('resume')
        return before

    def policy(self):
        self.command('stop')
        self.dm('policy-preserve')
        original = self.scan()['delivery']
        self.assertIsNotNone(original)
        self.change_policy()
        preserved = self.scan()['delivery']
        self.change_policy(mode='muted')
        self.assertEqual(self.scan()['status'], 'suppressed')
        muted = self.persisted_pending()
        before = self.provider.counter.snapshot()[0]
        before = self.resume_muted_policy(original, before)
        # The actual running bridge repeatedly sees suppression; no native input.
        for _ in range(3):
            self.command('observe')
            time.sleep(1)
        after = self.provider.counter.snapshot()[0]
        self.command('stop')
        self.change_policy(mode='enabled')
        unmuted = self.scan()['delivery']
        self.assertEqual(original, unmuted)
        self.command('resume')
        self.await_handled(['policy-preserve'])
        self.command('stop')
        self.dm('policy-withdraw')
        withdrawn = self.scan()['delivery']
        self.change_policy(action='withdraw')
        denied = self.request('combined/ack', payload={
            'consumer': self.cursor['consumer'], 'delivery_id': withdrawn['delivery_id'],
            'request_id': 'withdrawn-must-not-ack', 'reason': 'Probe withdrawn delivery',
        }, allow_denied=True)
        self.assertEqual(denied.status_code, 409)
        with self.database() as connection:
            facts = connection.execute(
                # Same-scope board/DM withdrawal is selection-linked evidence;
                # foreign-scope and typed withdrawals have their own append-only facts.
                "SELECT combined_envelope FROM ("
                "SELECT owner,combined_envelope FROM pitput_attention_evidence "
                "WHERE record->>'disposition'='withdrawn' UNION ALL "
                "SELECT owner,combined_envelope FROM pitput_pending_withdrawals UNION ALL "
                "SELECT owner,combined_envelope FROM pitput_ask_notice_withdrawals"
                ") AS withdrawals WHERE owner=%s "
                "AND combined_envelope->>'delivery_id'=%s", (OWNER, withdrawn['delivery_id']),
            ).fetchall()
        self.assertEqual(len(facts), 1)
        self.assertEqual(facts[0]['combined_envelope'], withdrawn)
        self.command('resume')
        self.wait(lambda: self.command('observe')['pending'] is None, 'withdrawal did not clear slot')
        self.assertEqual(self.handled('policy-withdraw'), [])
        # Attribution is compared as the full original selected envelope, not current policy.
        self.oracle.case_six(original, preserved=preserved, muted=muted, unmuted=unmuted,
                             mute_wakeups=after-before, original_attribution=withdrawn,
                             withdrawal_attribution=facts[0]['combined_envelope'],
                             withdrawn_deliveries=len(self.handled('policy-withdraw')),
                             withdrawn_ack_successes=int(denied.status_code == 200))
        return {'preserved_delivery_id': original['delivery_id'], 'mute_request_delta': after-before,
                'withdrawn_original_envelope': withdrawn, 'withdrawn_ack_http_status': denied.status_code}

    def stale_and_resolution(self):
        self.command('stop')
        source = self.post('stale-source', addressed=False)
        closed = self.ask('closed-before-selection', source)
        pitput(self.controller, *self.prefix, 'ask', 'answer', closed['ask_id'],
               '--expected-version', '1', '--reason', 'Closed before selection.',
               '--request-id', 'close-unseen')
        # Neither old nor closed-unseen snapshots are authorized provider expectations.
        self.provider.expected.pop((closed['ask_id'], 1))
        superseded = self.ask('superseded-before-selection', source)
        pitput(self.controller, *self.prefix, 'ask', 'withdraw', superseded['ask_id'],
               '--expected-version', '1', '--reason', 'Withdraw before selection.',
               '--request-id', 'supersede-unseen')
        self.provider.expected.pop((superseded['ask_id'], 1))
        self.command('resume')
        # Establish actual engagement via recipient native exact-read/reply/ACK, then resolve.
        engaged = self.ask('engaged-open', source)
        self.await_handled(['engaged-open'])
        before = len(self.provider.records)
        pitput(self.controller, *self.prefix, 'ask', 'answer', engaged['ask_id'],
               '--expected-version', '2', '--reason', 'Engaged resolution remains visible.',
               '--request-id', 'engaged-answer')
        self.arm_ask(engaged['ask_id'], 3, 'engaged-resolution')
        self.await_handled(['engaged-resolution'])
        observed = [r['label'] for r in self.provider.records[before:]]
        self.oracle.case_seven(observed)
        self.assertFalse(any(r['label'] in {'closed-before-selection', 'superseded-before-selection'}
                             for r in self.provider.records))
        return {'closed_ask_id': closed['ask_id'], 'superseded_ask_id': superseded['ask_id'],
                'delivered_labels_after_resolution': observed}

    def privacy(self):
        self.command('stop')
        source = self.dm('private-probe-target')
        original = self.scan()['delivery']
        self.assertEqual(original['source'], 'dm')
        old_consumer = self.cursor['consumer']
        spec = {**self.cursor['spec'], 'dm_session': secrets.token_hex(16)}
        registration = self.request('?dm_takeover_reason=Frozen%20acceptance%20fence',
                                    method='PUT', payload=spec).json()
        self.cursor['consumer'] = registration['consumer']
        outsider = self.agent_env('outsider')
        pitput(outsider, 'join', 'exchange', '--identity', 'test/outsider/agent')
        outsider_token = (Path(outsider['HOME']) /
                          '.config/pitput/agent-base/test/outsider/agent/token').read_text().strip()
        probes = {}
        contracts = {}
        for actor in ('stale-consumer', 'revoked-credential', 'third-identity', 'wildcard-viewer'):
            if actor == 'revoked-credential':
                with self.database() as connection:
                    connection.execute('UPDATE pitput_service_agents SET revoked_at=clock_timestamp() WHERE identity=%s', (OWNER,))
            try:
                for operation, endpoint in [('read', 'read'), ('consume', 'scan'), ('pending', 'pending'), ('ack', 'ack')]:
                    payload = {'consumer': old_consumer if actor == 'stale-consumer' else self.cursor['consumer']}
                    if endpoint in {'read', 'ack', 'pending'}:
                        payload['delivery_id'] = original['delivery_id']
                    if endpoint in {'read', 'ack'}:
                        payload['request_id'] = 'denied-' + actor + '-' + operation
                    if endpoint == 'ack':
                        payload['reason'] = 'Unauthorized privacy probe must be denied'
                    token = outsider_token if actor == 'third-identity' else (self.viewer_token if actor == 'wildcard-viewer' else self.token)
                    response = self.request('combined/' + endpoint, payload=payload, token=token, allow_denied=True)
                    expected_status = 401 if actor == 'revoked-credential' else 409 if actor == 'stale-consumer' else 404
                    contracts[(actor, operation)] = {expected_status}
                    private_tokens = [original['delivery_id'], source['message'], source['event_key'], 'private context']
                    leaked = sum(len(value.encode()) for value in private_tokens if value in response.text)
                    with self.database() as connection:
                        effects = connection.execute('SELECT count(*) AS n FROM pitput_combined_acknowledgments WHERE owner=%s AND delivery_id=%s', (OWNER, original['delivery_id'])).fetchone()['n']
                    probes[(actor, operation)] = {'http_status': response.status_code,
                                                  'private_bytes': leaked, 'effects': effects}
            finally:
                if actor == 'revoked-credential':
                    with self.database() as connection:
                        connection.execute('UPDATE pitput_service_agents SET revoked_at=NULL WHERE identity=%s', (OWNER,))
        self.oracle.case_eight(probes, denied_statuses=contracts)
        self.assertEqual(self.scan()['delivery'], original)
        # Restore the production bridge's exact registered session with the new fenced generation.
        registration = self.request('?dm_takeover_reason=Restore%20acceptance%20owner',
                                    method='PUT', payload=self.cursor['spec']).json()
        self.cursor['consumer'] = registration['consumer']
        private(self.spool / 'cursor.json', json.dumps(self.cursor))
        self.command('resume')
        self.await_handled(['private-probe-target'])
        return {'probes': [{'actor': actor, 'operation': operation, **value}
                           for (actor, operation), value in probes.items()],
                'original_pending_unchanged': True}

    def native_publication_receipts(self):
        queue = self.spool / 'monitor-publications.json'
        def published():
            state = json.loads(queue.read_text()) if queue.exists() else {}
            return (not state.get('entries', [1])
                    and len(state.get('acknowledged', [])) >= len(self.provider.records))
        self.wait(published, 'native evidence outbox did not drain', timeout=60)
        return json.loads(queue.read_text())

    def provenance_chain(self):
        # Exact, fixture-bounded reads of existing evidence; no new audit API.
        # Native reporting is asynchronous, so await its durable outbox first.
        state = self.native_publication_receipts()
        self.assertTrue(all(item['attribution'] == 'known' for item in state['acknowledged']))
        self.result['provenance_chain'] = []
        for event in self.provider.records:
            self.assertTrue(event['completed'] or event.get('receipt_lost_reconciled'))
            with self.database() as connection:
                selection = connection.execute(
                    'SELECT scope_id,policy_version,record FROM pitput_attention_selections '
                    'WHERE owner=%s AND source=%s AND event_key=%s',
                    (OWNER, event['source'], event['source_event_key']),
                ).fetchall()
                self.assertEqual(len(selection), 1, 'source must have one original selection')
                selected = selection[0]
                policy = connection.execute(
                    'SELECT record FROM pitput_monitor_policy_revisions '
                    'WHERE owner=%s AND scope_id=%s AND version=%s',
                    (OWNER, selected['scope_id'], selected['policy_version']),
                ).fetchone()
                self.assertIsNotNone(policy, 'original policy missing')
                facts = connection.execute(
                    "SELECT record FROM pitput_attention_evidence WHERE owner=%s AND scope_id=%s "
                    "AND record->'event'->>'event_key'=%s ORDER BY ordinal",
                    (OWNER, selected['scope_id'], event['source_event_key']),
                ).fetchall()
                ack = connection.execute(
                    'SELECT record,delivery FROM pitput_combined_acknowledgments '
                    'WHERE owner=%s AND delivery_id=%s', (OWNER, event['delivery_id']),
                ).fetchall()
            facts = [row['record'] for row in facts]
            dispositions = [fact['disposition'] for fact in facts]
            self.assertTrue({'selected', 'notification_attempt', 'notification_accepted', 'read'}
                            <= set(dispositions), 'source/policy/delivery/read evidence incomplete')
            self.assertTrue(all(fact['policy_version'] == selected['policy_version'] for fact in facts))
            accepted = [fact for fact in facts if fact['disposition'] == 'notification_accepted']
            self.assertTrue(any(persisted_payload_matches(fact, event) for fact in accepted),
                            'delivered payload not bound to native accepted evidence')
            self.assertEqual(len(ack), 1, 'combined handling effect must commit once')
            self.assertEqual(ack[0]['delivery']['source_monitor'], event['source_monitor'])
            self.assertEqual(ack[0]['record']['status'], 'acknowledged')
            if event['source'] == 'ask':
                self.assertEqual(ack[0]['delivery']['notice']['selection']['scope_id'],
                                 str(selected['scope_id']))
            self.result['provenance_chain'].append({
                'source': event['source'], 'source_event_key': event['source_event_key'],
                'delivery_id': event['delivery_id'], 'scope_id': str(selected['scope_id']),
                'policy_version': selected['policy_version'],
                'evidence_ids': [fact['evidence_id'] for fact in facts],
                'attempt_ids': [fact['attempt_id'] for fact in accepted],
                'payload_sha256': event['payload_sha256'], 'read_sha256': event['read_sha256'],
                'dispositions': dispositions, 'ack_request_id': ack[0]['record']['request_id'],
                'handling_effects': len(ack),
            })

    def idle(self):
        self.assertEqual(set(self.result['idle_windows']), {'fresh', 'resumed'})
        for receipt in self.result['idle_windows'].values():
            self.assertEqual(receipt['status'], 'PASS')
            self.assertGreaterEqual(receipt['window_elapsed_seconds'], 30)
            self.assertEqual(receipt['requests_at_baseline'], receipt['requests_at_end'])
        return self.result['idle_windows']


def write_reports(path, record):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.with_suffix('.json').write_text(json.dumps(record, indent=2) + '\n')
    suite = ET.Element('testsuite', name='frozen-combined-acceptance', tests='9')
    failures = 0
    for number in range(1, 10):
        observed = record.get('cases', {}).get(str(number), {'status': 'BLOCKED'})
        case = ET.SubElement(suite, 'testcase', name='fixed-case-' + str(number))
        if observed['status'] != 'PASS':
            ET.SubElement(case, 'failure', message=observed['status'] + ': required case not proved')
            failures += 1
    if record.get('status') != 'PASS' and failures == 0:
        ET.SubElement(case, 'failure', message='FAIL: required overall provenance/cleanup gate failed')
        failures = 1
    suite.set('failures', str(failures))
    ET.ElementTree(suite).write(path, encoding='unicode')


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--junit', type=Path, default=ROOT / 'artifacts/ci/pitput-combined-acceptance.xml')
    args = parser.parse_args()
    record = {'status': 'BLOCKED', 'cases': {}, 'reason': 'prerequisite_not_checked'}
    try:
        if (os.environ.get('CI') != 'true'
                or os.environ.get('HRX_PITPUT_COMBINED_ACCEPTANCE_AUTHORIZED') != '1'):
            raise SystemExit('disposable_gitlab_authorization_required')
        sha = os.environ.get('HRX_PITPUT_SOURCE_SHA', '')
        if not re.fullmatch('[0-9a-f]{40}', sha):
            raise SystemExit('full_pitput_commit_required')
        provenance = ownership.pitput_provenance(entrypoint=Path(__file__).resolve())
        reason = ownership.blocked_reason('codex')
        if reason:
            raise SystemExit(reason)
        versions = {}
        for executable in ('codex', 'pg_ctl'):
            versions[executable] = subprocess.check_output([executable, '--version'], text=True).strip()
        if versions['codex'] != 'codex-cli 0.163.0':
            raise SystemExit('pinned_codex_version_required')
        if not re.search(r'\b(?:1[7-9]|[2-9][0-9])\.', versions['pg_ctl']):
            raise SystemExit('postgresql17_or_later_required')
        hrx_commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
        if os.environ.get('CI_COMMIT_SHA') != hrx_commit:
            raise SystemExit('exact_hrx_ci_commit_required')
    except (SystemExit, OSError, subprocess.SubprocessError) as error:
        # Only preflight-owned codes, never arbitrary subprocess stderr or profiles.
        code = str(error) if isinstance(error, SystemExit) and re.fullmatch('[a-z0-9_]+', str(error)) else 'prerequisite_setup_failed'
        record['reason'] = code
        write_reports(args.junit, record)
        print('BLOCKED: ' + code, file=sys.stderr)
        return 2
    outcome = unittest.TextTestRunner(verbosity=2).run(
        unittest.TestSuite([FrozenAcceptance('test_frozen_matrix')]))
    record = FrozenAcceptance.result
    record['fixture_cleanup'] = {
        'postgresql_stopped': getattr(FrozenAcceptance, 'postgres_cleanup_verified', False),
        'service_processes_stopped': all(process.poll() is not None
                                         for process in getattr(FrozenAcceptance, 'processes', [])),
        'disposable_root_removed': not getattr(FrozenAcceptance, 'root', ROOT).exists(),
    }
    record.update(pitput=provenance, native_versions=versions, hrx_commit=hrx_commit,
                  claim='scripted native transport/tool behavior; no model understanding or paid semantic proof')
    status = (outcome.wasSuccessful() and record['status'] == 'PASS'
              and set(record['cases']) == set(map(str, range(1, 10)))
              and all(record['fixture_cleanup'].values()))
    if not status and record['status'] == 'PASS':
        record['status'] = 'FAIL'
    write_reports(args.junit, record)
    return 0 if status else 1


if __name__ == '__main__':
    raise SystemExit(main())
