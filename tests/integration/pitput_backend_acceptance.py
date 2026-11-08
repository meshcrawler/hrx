"""Frozen combined service cases through real OpenCode/Claude ingress on GitLab.

Backend runtimes and PostgreSQL must come from the existing disposable native
image. This entrypoint never substitutes Codex or permits a local native run.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import pty
import subprocess
import sys
import time
import tomllib
import unittest
import re
import uuid

from pitput_backend_provider import BackendProvider, ClaudeTaskScript, validate_claude_generation
from pitput_combined_acceptance import FrozenAcceptance, OWNER, write_reports
from pitput_combined_acceptance_support import source_module
from pitput_exchange import pitput, private
import pitput_targeted_launch as ownership

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tests/compatibility'))


def safe_startup_failure(value):
    codes = {'unbound_attachment_prompt', 'owned_primer_configuration', 'owned_primer_path_binding',
             'owned_primer_file', 'attachment_binding', 'attachment_ambiguous', 'attachment_early_exit',
             'observation_binding', 'observation_shape', 'observation_read', 'native_fixture_failure'}
    if (isinstance(value, dict) and set(value) == {'stage', 'code'}
            and value['stage'] in {'setup', 'client_start', 'fresh_client_started'} and value['code'] in codes):
        return value
    return {'stage': 'setup', 'code': 'native_fixture_failure'}


def authorized_claude_fixture_environment(env, home):
    # Runner.configure deliberately rebuilds an isolated environment. Carry the
    # explicit native-helper gate into that child, not ambient user variables.
    return {**env, 'PATH': str(home / 'bin') + os.pathsep + env['PATH'], 'SHELL': '/bin/bash',
            'DISABLE_AUTOUPDATER': '1', 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC': '1',
            'HRX_CLAUDE_ATTENTION_AUTHORIZED': '1', 'CI': 'true',
            'PITPUT_NATIVE_FIXTURE_EXECUTION_AUTHORIZED': '1'}

# Faults live only in this disposable CLI wrapper, never in product configuration.
# The real bridge dispatch and real native submit still run. A task-output return
# remains publication evidence; it is never relabeled model consumption.
FAULT_CLI = '''#!{python}
import json,os
from pathlib import Path
from ysg_mail import codex_bridge as bridge, attention_protocol as protocol
from ysg_mail.pitput import main
root=Path({control!r})
def fault(submit,args,store,delivery):
 mode=root/'native-fault.json'
 if not mode.exists(): return submit(*args)
 phase=json.loads(mode.read_text())['phase']
 mode.unlink()
 if phase=='after-admission':
  result=submit(*args)
  if result.status!='success': raise RuntimeError('fault_admission_not_successful')
 record={{'phase':phase,'native_submit_returned':phase=='after-admission',
          'owner':store.agent.agent_id,'session':store.agent.thread_id,'delivery_id':delivery}}
 temporary=root/'native-fault-entered.tmp'
 temporary.write_text(json.dumps(record))
 temporary.chmod(0o600)
 temporary.replace(root/'native-fault-entered.json')
 os._exit(0 if phase=='after-admission' else 73)
original=bridge._submit_attention
bridge._submit_attention=lambda store,text,delivery,stop:fault(original,(store,text,delivery,stop),store,delivery)
dispatch=protocol.dispatch
def task_dispatch(store,event,header,submit):
 if store.config.backend!='claude-task': return dispatch(store,event,header,submit)
 return dispatch(store,event,header,lambda text,delivery:fault(submit,(text,delivery),store,delivery))
protocol.dispatch=task_dispatch
main()
'''


class OpenCodeOwner:
    def __init__(self, case, home):
        self.case, self.home, self.step = case, home, 0
        self.process = self.log = None
        self.lifecycle_observations = []
        case.result['native_lifecycle_observations'] = self.lifecycle_observations
        case.owner = self
        self.stopped = False
        self.provider_server = case.provider.open_code_server()
        binary = case.root / 'backend-native.test'
        subprocess.run(['go', 'test', '-c', '-race', '-o', str(binary), './internal/app'],
                       cwd=ROOT, check=True, capture_output=True, timeout=300)
        env = {**case.env_target, 'HRX_PITPUT_BACKEND_ACCEPTANCE': 'opencode',
               'HRX_PITPUT_COMBINED_CONTROL': str(case.control),
               'HRX_PITPUT_COMBINED_PROVIDER': self.provider_server,
               'HRX_NATIVE_SESSION_TESTS': '1', 'HRX_PITPUT_INTEGRATION': '1'}
        self.log = (home / 'backend-native.log').open('wb')
        self.summary = case.control / 'supervisor.json'
        self.process = ownership.start_owned(
            [str(binary), '-test.run=^TestNativePitputCombinedAcceptanceOpenCode$', '-test.timeout=20m', '-test.v'],
            summary=self.summary, env=env, cwd=ROOT, log=self.log)
        ready = case.control / 'backend.ready'
        deadline = time.monotonic() + 120
        while not ready.exists():
            if self.dead() or time.monotonic() > deadline:
                failure = case.control / 'backend-failure.json'
                if failure.exists():
                    value = json.loads(failure.read_text())
                    case.result['startup_failure'] = safe_startup_failure(value)
                raise RuntimeError('BLOCKED: production_opencode_startup')
            time.sleep(0.1)
        value = json.loads(ready.read_text())
        self.config, self.session = Path(value['config']), value['thread']
        if value.get('fixture_permission_mode') != 'explicit_yolo':
            raise RuntimeError('explicit_fixture_permissions_missing')
        case.result['fixture_permission_mode'] = value['fixture_permission_mode']
        if case.provider.initialization_session is not None and case.provider.initialization_session != self.session:
            raise RuntimeError('owned_initialization_binding_mismatch')
        self.lifecycle_observations.extend(value['native_lifecycle'])

    def dead(self):
        return self.process is None or not ownership.supervisor_running(self.process) or Path(str(self.summary) + '.main').exists()

    def control(self, action, *, timeout=90, call_sha256=None):
        from pitput_combined_acceptance_support import publish_control
        step = self.step
        self.step += 1
        command = {'action': action}
        deadline = time.monotonic() + timeout
        if action == 'failure-observe':
            command.update(deadline_unix_ms=int((time.time() + timeout) * 1000), call_sha256=call_sha256 or '')
        publish_control(self.case.control / f'command-{step:04d}.json', json.dumps(command))
        path = self.case.control / f'result-{step:04d}.json'
        while not path.exists():
            if self.dead() or time.monotonic() > deadline:
                raise RuntimeError('production_opencode_control_failed')
            time.sleep(0.1)
        value = json.loads(path.read_text())
        if value['thread'] != self.session:
            raise RuntimeError('native_session_changed')
        self.lifecycle_observations.extend(value.get('native_lifecycle', []))
        return value

    def failure_observation(self):
        record = self.case.provider.active_record or {}
        call_hash = record.get('last_provider_reply', {}).get('call_sha256')
        if call_hash is not None and not re.fullmatch('[0-9a-f]{64}', call_hash):
            return {'status': 'unavailable'}
        return self.control('failure-observe', timeout=5, call_sha256=call_hash)['failure_observation']

    def stop(self):
        self.last_stop_result = self.control('stop')
        self.stopped = True

    def resume(self):
        self.control('resume')
        self.stopped = False

    def observe(self):
        value = self.control('observe')
        value['native_idle'] = value['native_idle'] and self.case.provider.counter.snapshot()[1] == 0
        return value

    def restart_native(self):
        self.stop()
        self.resume()  # HRX recreates its private server and rotates credentials.

    def busy(self):
        self.control('busy')

    def close(self):
        if self.process is None:
            if self.log is not None:
                self.log.close()
            return {'classification': 'not_started'}
        if not self.dead():
            self.control('done')
        receipt = ownership.stop_owned(self.process, self.summary)
        self.log.close()
        if receipt['classification'] != 'clean':
            raise RuntimeError('opencode_owned_cleanup_not_clean')
        return receipt


class ClaudeOwner:
    def __init__(self, case, home, hrx):
        import claude_attention as base
        from gateway import Gateway
        from run import Runner

        self.case, self.home, self.base = case, home, base
        self.hrx, self.session = hrx, str(uuid.uuid4())
        self.process = self.master = self.terminal = None
        self.responder = self.gateway = None
        case.owner = self
        self.stopped = False
        self.config = None
        self.previous_config = self.previous_generation = None
        self.script = ClaudeTaskScript(case.provider, base, self.binding)
        self.responder = base.Responder(self.script)
        self.policy = {'max_requests': 1000, 'wall_seconds': 1800, 'cell_seconds': 1800,
                       'request_seconds': 90, 'max_request_bytes': 4000000,
                       'max_output_tokens': 4096, 'concurrency': 4, 'routes': [base.ROUTE]}
        gateway_root = home / 'gateway'
        gateway_root.mkdir(mode=0o700)
        self.gateway = Gateway(gateway_root, self.responder.url, 'loopback-fixture',
                               base.fixture_catalog(datetime.now(timezone.utc)), self.policy)
        runner = Runner(hrx, self.gateway, self.policy)
        self.project = home / 'project'
        self.project.mkdir(mode=0o700)
        env = runner.configure(home, self.project, {'harness': 'claude', 'route': base.ROUTE, 'reasoning': None})
        self.env = authorized_claude_fixture_environment(env, home)
        self.resume()

    def binding(self, system=None):
        root = self.home / 'state/hrx/pitput' / hashlib.sha256(OWNER.encode()).hexdigest()
        matches = list(root.glob('.claude-launch-*/' + self.session + '/@combined/bridge.toml'))
        if self.config is None:
            if system is None or 'Pitput attention protocol attention-v1.' not in system:
                raise ValueError('claude_combined_system_binding_missing')
            matches = [path for path in matches if str(path) in system]
            if len(matches) != 1:
                raise ValueError('claude_combined_config_missing_or_ambiguous')
            if matches[0] == self.previous_config:
                raise ValueError('claude_combined_predecessor_config_reused')
            config = tomllib.loads(matches[0].read_text())
            validate_claude_generation(config, OWNER, self.session, self.previous_generation)
            self.config = matches[0]
            self.case.provider.config = self.config
            self.case.result.setdefault('native_generation_bindings', []).append({
                'session': self.session, 'generation': config['claude_task_generation'],
                'predecessor_generation': self.previous_generation['claude_task_generation'] if self.previous_generation else None,
                'single_combined_worker': True, 'native_system_binding': True,
                'preserved_state': self.previous_generation is None or config['state_directory'] == self.previous_generation['state_directory'],
            })
        return self.config, OWNER, self.session

    def resume(self):
        if self.process is not None:
            raise RuntimeError('duplicate_backend_consumer')
        old = self.config
        self.previous_config = old
        self.previous_generation = tomllib.loads(old.read_text()) if old is not None else None
        self.config = None
        self.script.retire_current_task()
        self.script.task = self.script.pending = None
        self.script.fresh_generation_allowed = old is not None
        summary = self.home / ('supervisor-' + uuid.uuid4().hex + '.json')
        self.summary = summary
        args = [sys.executable, '-I', str(self.base.SUPERVISOR), '--summary', str(summary), '--',
                str(self.hrx), 'claude', '--model', self.base.ROUTE, '--agent-identity', OWNER,
                '--pitput', '--pitput-board', 'exchange', '--pitput-combined-preview',
                *([] if old else ['--claude-config-dir', self.env['CLAUDE_CONFIG_DIR'],
                                 '--claude-init', 'theme,trust,bypass']),
                *(['--resume', self.session] if old else []), '--',
                *([] if old else ['--session-id', self.session]),
                'Monitor the authorized combined Pitput sources.']
        self.master, slave = pty.openpty()
        try:
            self.process = subprocess.Popen(args, cwd=self.project, env=self.env,
                                            stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        finally:
            os.close(slave)
        self.terminal = self.base.Terminal(self.master, self.process)
        self.stopped = False
        deadline = time.monotonic() + 180
        while self.script.task is None:
            if self.process.poll() is not None or time.monotonic() >= deadline or self.script.violations:
                raise RuntimeError('BLOCKED: claude_combined_task_not_armed')
            time.sleep(0.1)
        self.config, _, _ = self.binding()
        self.case.provider.config = self.config

    def stop(self):
        if self.process is None:
            return
        self.script.retire_current_task()
        evidence = {'signals': []}
        if self.base.stop_supervised(self.process, self.summary, evidence) != 'clean':
            raise RuntimeError('claude_owned_cleanup_not_clean')
        self.process = None
        self.stopped = True
        if self.terminal is not None:
            self.terminal.close()
            self.terminal = None
        if self.master is not None:
            os.close(self.master)
            self.master = None

    def observe(self):
        alive = self.process is not None and self.process.poll() is None
        return {'transport_healthy': alive, 'controller_healthy': alive,
                'native_idle': alive and self.script.task is not None and self.script.pending is None}

    def dead(self):
        return not self.stopped and (self.process is None or self.process.poll() is not None)

    def restart_native(self):
        self.stop()
        self.resume()

    def busy(self):
        os.write(self.master, b'fixture-authorized-busy-turn\r')

    def close(self):
        self.stop()
        if self.responder is not None:
            self.responder.close()
        if self.gateway is not None:
            self.gateway.close()
        return {'bridges': 'PASS', 'native': 'PASS'}


class BackendAcceptance(FrozenAcceptance):
    backend = None

    def publication_audit(self):
        from ysg_mail import codex_bridge as bridge
        from ysg_mail.native_publication import audit
        config = bridge._load_config(self.provider.config)
        return audit(bridge.Store(config, config.agents[0]))

    def native_publication_receipts(self):
        def published():
            facts = self.publication_audit()
            self.result['native_publication_outbox'] = facts
            acknowledged = {item['record_id']: item for item in facts['acknowledged']}
            return (not facts['entries'] and facts['retired_attempts'] == 0
                    and bool(facts['attempts'])
                    and set(acknowledged) == {item['record_id'] for item in facts['attempts']}
                    and all(
                        attempt['phase'] != 'enqueueing'
                        and attempt['record_id'] in acknowledged
                        and all(acknowledged[attempt['record_id']][field] == attempt[field]
                                for field in ('phase', 'report_digest'))
                        and acknowledged[attempt['record_id']]['attribution'] == 'known'
                        for attempt in facts['attempts']))
        self.wait(published, 'actual native publication attempts not fully acknowledged', timeout=60)
        facts = self.result['native_publication_outbox']
        actual = {item['record_id']: item for item in facts['attempts']}
        for event in self.provider.records:
            observed = event['native_publication_observation']['matching_attempts']
            self.assertTrue(observed, 'native observation has no real prior attempt')
            self.assertTrue(all(item['record_id'] in actual for item in observed),
                            'observed native attempt disappeared from bounded transcript')
            binding_fields = ('delivery_id', 'payload_sha256', 'source_event_key', 'source_monitor')
            self.assertTrue(all(all(actual[item['record_id']][field] == event[field]
                                        for field in binding_fields) for item in observed),
                            'observed attempt is not bound to the same immutable source payload')
            positive = [item['record_id'] for item in facts['attempts'] if item['phase'] == 'admitted'
                        and item['delivery_id'] == event['delivery_id']
                        and item['payload_sha256'] == event['payload_sha256']
                        and item['source_event_key'] == event['source_event_key']
                        and item['source_monitor'] == event['source_monitor']]
            self.assertTrue(positive, 'same immutable delivery/payload has no actual positive callback')
            event['positive_callback_record_ids'] = positive
        # The inherited per-source policy/selection/payload/read/ACK assertions
        # still run. No uncertain predecessor is required or relabeled admitted.
        return facts

    def collect_backend_failure(self):
        if not isinstance(self.owner, OpenCodeOwner):
            return {'status': 'not_applicable'}
        try:
            return self.owner.failure_observation()
        except Exception as error:
            name = type(error).__name__
            return {'status': 'unavailable', 'error_type': name if name in ('RuntimeError', 'OSError', 'AssertionError') else 'other'}

    def muted_source_state(self, original):
        source_key = (original['notice']['selection']['notice']['notice_id'] if original['source'] == 'ask'
                      else original['attention']['event_key'])
        with self.database() as connection:
            facts = connection.execute(
                "SELECT record FROM pitput_attention_evidence WHERE owner=%s "
                "AND record->'event'->>'event_key'=%s "
                "AND record->>'disposition' IN ('notification_attempt','notification_accepted')",
                (OWNER, source_key),
            ).fetchall()
            acknowledgments = connection.execute(
                'SELECT count(*) AS n FROM pitput_combined_acknowledgments WHERE owner=%s AND delivery_id=%s',
                (OWNER, original['delivery_id']),
            ).fetchone()['n']
            pending = connection.execute('SELECT pending FROM pitput_combined_delivery WHERE owner=%s',
                                         (OWNER,)).fetchone()['pending']
        return {'admission_evidence_ids': sorted(row['record']['evidence_id'] for row in facts),
                'ack_count': acknowledgments, 'pending': pending}

    def muted_startup_binding(self, generations_before, initializations_before):
        if self.backend == 'claude':
            generations = self.result.get('native_generation_bindings', [])
            self.assertGreater(generations_before, 0, 'missing predecessor generation binding')
            self.assertEqual(len(generations), generations_before + 1, 'one fresh generation must be bound')
            old, current = generations[generations_before - 1], generations[-1]
            self.assertEqual(current['session'], self.thread)
            self.assertNotEqual(current['generation'], old['generation'])
            self.assertEqual(current['predecessor_generation'], old['generation'])
            self.assertTrue(all(current.get(key) is True for key in
                                ('single_combined_worker', 'native_system_binding', 'preserved_state')))
            return {'backend': 'claude', 'generation_binding': current}
        self.assertEqual(self.backend, 'opencode')
        records = self.provider.initialization_records[initializations_before:]
        self.assertEqual(len(records), 1, 'one exact owned initialization must complete')
        canonical = self.provider.initialization_loader()
        self.assertIsNotNone(canonical)
        digest = hashlib.sha256(canonical['text'].encode()).hexdigest()
        self.assertTrue(canonical.get('exact_session_binding') is True)
        self.assertEqual((canonical['session'], canonical['sha256']), (self.thread, digest))
        self.assertEqual((records[0]['session'], records[0]['primer_sha256'], records[0]['utf8_bytes']),
                         (self.thread, digest, len(canonical['text'].encode())))
        return {'backend': 'opencode', 'owned_initializations': records}

    def resume_muted_policy(self, original, before):
        source_records_before = len(self.provider.records)
        state_before = self.muted_source_state(original)
        self.assertEqual(state_before, {'admission_evidence_ids': [], 'ack_count': 0, 'pending': original})
        generations_before = len(self.result.get('native_generation_bindings', []))
        initializations_before = len(self.provider.initialization_records)
        observed = self.result['policy_muted_resume'] = {
            'status': 'RUNNING', 'requests_before_resume': before,
            'source_records_before': source_records_before, 'source_state_before': state_before,
            'startup_count_label': 'all global requests during verified native resume/settling; never subtracted from a measured window',
            'quiet_window': {},
        }

        def guard():
            self.assertEqual(len(self.provider.records), source_records_before,
                             'muted source reached native provider during startup or observation')
            state = self.muted_source_state(original)
            self.assertEqual(state, state_before, 'muted source admission, ACK or pending envelope changed')
            return state

        self._muted_policy_guard = guard
        super().resume_muted_policy(original, before)
        guard()  # Protect the entire startup interval, before any new baseline.
        observed['requests_after_resume'] = self.provider.counter.snapshot()[0]

        def observe():
            state = self.command('observe')
            guard()
            return {'native_alive': state['transport_healthy'], 'bridges_alive': state['controller_healthy'],
                    'same_native_process': state['thread'] == self.thread,
                    'completed': state['native_idle'] and state['pending'] is None}

        # Reuse the fixed recovery-primer window/cadence, stored separately from
        # the required fresh/resumed idle receipts and labeled as policy context.
        source_module('native_quiescence').observe_window(
            'idle-after-recovery-full-primer', self.provider.counter, observe, observed['quiet_window'])
        receipt = observed['quiet_window']
        self.assertEqual(receipt['status'], 'PASS')
        self.assertGreaterEqual(receipt['window_elapsed_seconds'], 30)
        self.assertEqual(receipt['requests_at_baseline'], receipt['requests_at_end'])
        self.assertGreaterEqual(receipt['requests_at_baseline'], before)
        observed['source_state_after'] = guard()
        observed['startup_binding'] = self.muted_startup_binding(generations_before, initializations_before)
        after = self.provider.counter.snapshot()[0]
        self.assertEqual(after, receipt['requests_at_end'])
        observed.update(status='PASS', requests_at_healthy_baseline=receipt['requests_at_baseline'],
                        requests_after_window=after,
                        startup_interval_requests=receipt['requests_at_baseline'] - before,
                        source_records_after=len(self.provider.records))
        return after  # The existing additional three-second/oracle check remains.

    def command(self, action):
        muted_guard = getattr(self, '_muted_policy_guard', None)
        if muted_guard is not None:
            muted_guard()
        if action == 'stop':
            self.owner.stop()
            self.intentionally_stopped = True
        elif action == 'resume':
            self.owner.resume()
            self.intentionally_stopped = False
            self.provider.config = self.owner.config
            self.refresh_binding()
        elif action == 'busy':
            self.owner.busy()
        elif action not in ('observe', 'done'):
            raise RuntimeError('BLOCKED: backend_control_' + action)
        result = self.owner.observe()
        if muted_guard is not None:
            muted_guard()
            if action == 'stop':
                self._muted_policy_guard = None
        if not self.intentionally_stopped and not self.expected_fault:
            self.assertTrue(result['transport_healthy'], 'native transport unexpectedly exited')
            self.assertTrue(result['controller_healthy'], 'backend consumer unexpectedly exited')
        result.update(thread=self.thread,
                      pending=self.bridge_pending())
        return result

    def bridge_pending(self):
        path = self.spool / 'state.json'
        if not path.exists():
            return None
        return json.loads(path.read_text()).get('pending')

    def recovery(self):
        self.provider.attention_seen.clear()
        self.provider.attention_release.clear()
        self.provider.busy.clear()
        self.provider.busy_seen.clear()
        self.command('busy')
        self.wait(self.provider.busy_seen.is_set, 'foreground turn did not reach provider')
        self.dm('recovery-pending')
        self.wait(lambda: self.scan()['delivery'] is not None, 'busy target lost pending')
        original = self.scan()['delivery']
        if self.backend == 'opencode':
            self.wait(lambda: (self.bridge_pending() or {}).get('delivery') == 'notified',
                      'busy native queue admission not observed')
        observed = {'busy': original}
        self.server.terminate()
        self.server.wait(15)
        self.start_server()
        observed['server-restart'] = self.scan()['delivery']
        self.command('stop')
        observed['bridge-restart'] = self.scan()['delivery']
        self.provider.busy.set()
        self.wait(lambda: self.provider.counter.snapshot()[1] == 0, 'stopped foreground request did not settle')
        self.command('resume')
        observed['session-resume'] = self.scan()['delivery']
        self.oracle.case_four(original, observed)
        self.assertEqual(self.handled('recovery-pending'), [])
        self.provider.attention_release.set()
        self.await_handled(['recovery-pending'])
        self.assertEqual(len(self.handled('recovery-pending')), 1)
        self.quiet('resumed')
        return {'original_delivery': original, 'recovered_envelopes': observed,
                'eventual_handling_effects': 1, 'native_session_preserved': self.owner.session == self.thread}

    def crashes(self):
        results = {}
        self.result['crash_observations'] = results
        for phase in ('before-admission', 'after-admission', 'before-ack-commit',
                      'after-ack-commit', 'lost-ack-response'):
            label = 'crash-' + phase
            self.expected_fault = True
            self.provider.ack_retry_authorizer = None
            if phase.endswith('admission'):
                (self.control / 'native-fault-entered.json').unlink(missing_ok=True)
                private(self.control / 'native-fault.json', json.dumps({'phase': phase}))
                if phase == 'after-admission':
                    self.provider.cancel_attention = False
                    if self.backend == 'opencode':
                        self.provider.crash_capture_requested = True
                    self.provider.attention_seen.clear()
                    self.provider.attention_release.clear()
            else:
                self.proxy.arm(phase)
                self.provider.ack_retry_authorizer = self.proxy.authorize_failed_ack
            self.dm(label)
            if phase.endswith('admission'):
                self.wait((self.control / 'native-fault-entered.json').exists, 'native admission fault not reached')
                boundary = json.loads((self.control / 'native-fault-entered.json').read_text())
                self.assertEqual(boundary['native_submit_returned'], phase == 'after-admission')
                self.assertEqual((boundary['owner'], boundary['session']), (OWNER, self.thread))
                if phase == 'after-admission':
                    self.wait(self.provider.attention_seen.is_set, 'admitted native input did not reach pause')
                original = self.scan()['delivery']
                self.assertIsNotNone(original)
                self.assertEqual(self.handled(label), [], 'handling occurred before pending crash boundary')
                self.command('stop')
                self.assertEqual(self.scan()['delivery'], original)
                if phase == 'after-admission':
                    if self.backend == 'opencode':
                        self.provider.authorize_crash_witness(boundary, self.owner.last_stop_result)
                    self.provider.cancel_attention = True
                    self.provider.attention_release.set()
                    self.wait(lambda: self.provider.counter.snapshot()[1] == 0, 'owned paused request did not settle')
                    self.provider.cancel_attention = False
                self.command('resume')
            else:
                self.wait(self.proxy.hit.is_set, 'ACK fault not reached')
                boundary = self.proxy.observation
                self.server.terminate()
                self.server.wait(15)
                self.start_server()
                self.proxy.mark_restored(phase)
            self.await_handled([label])
            self.expected_fault = False
            results[phase] = {'boundary': boundary, 'mandatory_work': 1,
                              'pending': int(self.scan()['delivery'] is not None),
                              'handling_effects': len(self.handled(label)),
                              'final_handling_effects': len(self.handled(label))}
        self.provider.ack_retry_authorizer = None
        self.oracle.case_five(results)
        return {'transport_faults': results, 'codex_owner_journal_crashes': {
            'applicability': 'not applicable',
            'reason': 'These backends retain private stable-message or generation-fenced task ingress; no Codex owner journal exists.'}}

    def test_frozen_matrix(self):
        self.oracle = source_module('combined_acceptance_oracles')
        self.result = type(self).result = {'status': 'RUNNING', 'cases': {}, 'idle_windows': {},
                                          'provider': 'deterministic-loopback-only',
                                          'backend': self.backend, 'source_manifest': []}
        self.env_target = self.agent_env('combined')
        home = Path(self.env_target['HOME'])
        self.control = home / 'control'
        self.control.mkdir(mode=0o700)
        unrelated = home / 'unrelated-cwd'
        unrelated.mkdir(mode=0o700)
        private(home / 'bin/pitput', FAULT_CLI.format(python=sys.executable, control=str(self.control)))
        (home / 'bin/pitput').chmod(0o700)
        self.env_target.update(PATH=str(home / 'bin') + os.pathsep + self.env['PATH'],
                               CI='true', CI_PROJECT_PATH='acme/pitput',
                               PITPUT_NATIVE_FIXTURE_EXECUTION_AUTHORIZED='1')
        pitput(self.env_target, 'join', 'exchange', '--identity', OWNER)
        self.provider = BackendProvider(unrelated)
        self.provider.crash_resume_proof = lambda: json.loads((self.control / 'native-resume-proof.json').read_text())
        self.provider.initialization_loader = lambda: json.loads((self.control / 'owned-primer.json').read_text()) if (self.control / 'owned-primer.json').exists() else None
        self.provider.native_fault = lambda: json.loads((self.control / 'native-fault-entered.json').read_text()) if self.expected_fault and (self.control / 'native-fault-entered.json').exists() else None
        self.owner = None
        self.intentionally_stopped = self.expected_fault = False
        self.leader_done = lambda: self.owner is not None and self.owner.dead()
        try:
            if self.backend == 'opencode':
                self.owner = OpenCodeOwner(self, home)
            else:
                hrx = self.root / 'hrx'
                subprocess.run(['go', 'build', '-o', str(hrx), './cmd/hrx'], cwd=ROOT, check=True, timeout=120)
                self.owner = ClaudeOwner(self, home, hrx)
            self.thread = self.owner.session
            self.provider.config = self.owner.config
            self.refresh_binding()
            for number, action in ((1, self.sources_and_quiet), (2, self.fairness),
                                   (3, self.exact_read_reply_ack), (4, self.recovery),
                                   (5, self.crashes), (6, self.policy),
                                   (7, self.stale_and_resolution), (8, self.privacy), (9, self.idle)):
                self.case(number, action)
            self.provenance_chain()
            self.result['status'] = 'PASS'
        finally:
            if self.owner is not None and self.provider.config is not None:
                try:
                    self.result['native_publication_outbox'] = self.publication_audit()
                except Exception:
                    self.result['native_publication_outbox'] = {'status': 'unavailable'}
            if self.result['status'] != 'PASS' and self.owner is not None:
                self.result['failure_native_observation'] = self.collect_backend_failure()
            self.provider.close()
            self.result.update(events=self.provider.records, provider_errors=self.provider.errors,
                               task_notification_diagnostics=self.provider.task_diagnostics,
                               tool_result_diagnostics=self.provider.tool_diagnostics,
                               owned_initialization_requests=self.provider.initialization_records,
                               crash_witnesses=self.provider.crash_witness_records,
                               crash_witness_diagnostics=self.provider.crash_witness_diagnostics)
            if self.backend == 'opencode':
                self.result.update(native_request_diagnostics=self.provider.request_diagnostics,
                                   native_request_count=self.provider.request_count)
            sizes = {'whole_source': sum(map(len, self.provider.whole_source)),
                     'delivered': sum(record['notification_utf8_bytes'] for record in self.provider.records),
                     'exact_reads': sum(record['read_json_utf8_bytes'] for record in self.provider.records)}
            self.result['bytes'] = {'utf8_bytes': sizes,
                                    'utf8_div_4_token_estimates': {key: value / 4 for key, value in sizes.items()},
                                    'token_estimate_label': 'UTF-8 bytes / 4; not measured model tokens',
                                    'whole_source_baseline': 'same applicable labeled transcript including ordinary discussion',
                                    'savings_threshold': None}
            if self.owner is not None:
                self.result['owned_cleanup'] = self.owner.close()
            if self.result['status'] != 'PASS':
                self.result['status'] = 'FAIL'


def verify_backend_version(backend, version):
    # Match the native-image preflight exactly, including the CLI's banner.
    pins = json.loads((ROOT / 'tests/compatibility/ci/toolchain.json').read_text())
    if backend not in ('opencode', 'claude') or version != pins[backend]:
        raise SystemExit('pinned_backend_version_required')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend', required=True, choices=['opencode', 'claude'])
    parser.add_argument('--junit', required=True, type=Path)
    args = parser.parse_args()
    record = {'status': 'BLOCKED', 'cases': {}}
    try:
        if os.environ.get('CI') != 'true' or os.environ.get('HRX_PITPUT_COMBINED_ACCEPTANCE_AUTHORIZED') != '1':
            raise SystemExit('disposable_gitlab_authorization_required')
        if not re.fullmatch('[0-9a-f]{40}', os.environ.get('HRX_PITPUT_SOURCE_SHA', '')):
            raise SystemExit('full_pitput_commit_required')
        provenance = ownership.pitput_provenance(entrypoint=Path(__file__).resolve())
        reason = ownership.blocked_reason(args.backend)
        if reason:
            raise SystemExit(reason)
        version = subprocess.check_output([args.backend, '--version'], text=True).strip()
        verify_backend_version(args.backend, version)
        postgres_version = subprocess.check_output(['pg_ctl', '--version'], text=True).strip()
        if not re.search(r'\b(?:1[7-9]|[2-9][0-9])\.', postgres_version):
            raise SystemExit('postgresql17_or_later_required')
        hrx_commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
        if os.environ.get('CI_COMMIT_SHA') != hrx_commit:
            raise SystemExit('exact_hrx_ci_commit_required')
    except (SystemExit, OSError, subprocess.SubprocessError) as error:
        record['reason'] = str(error) if isinstance(error, SystemExit) and re.fullmatch('[a-z0-9_]+', str(error)) else 'prerequisite_setup_failed'
        write_reports(args.junit, record)
        print('BLOCKED: ' + record['reason'], file=sys.stderr)
        return 2
    BackendAcceptance.backend = args.backend
    result = unittest.TextTestRunner(verbosity=2).run(unittest.TestSuite([BackendAcceptance('test_frozen_matrix')]))
    record = {**BackendAcceptance.result, 'pitput': provenance, 'native_version': version,
              'postgresql_version': postgres_version, 'hrx_commit': hrx_commit,
              'claim': 'scripted native transport/tools; no paid semantic proof'}
    record['fixture_cleanup'] = {
        'postgresql_stopped': getattr(BackendAcceptance, 'postgres_cleanup_verified', False),
        'service_processes_stopped': all(process.poll() is not None for process in getattr(BackendAcceptance, 'processes', [])),
        'disposable_root_removed': not getattr(BackendAcceptance, 'root', ROOT).exists(),
    }
    status = result.wasSuccessful() and record['status'] == 'PASS' and all(record['fixture_cleanup'].values())
    if not status and record['status'] == 'PASS':
        record['status'] = 'FAIL'
    write_reports(args.junit, record)
    return 0 if status else 1


if __name__ == '__main__':
    raise SystemExit(main())
