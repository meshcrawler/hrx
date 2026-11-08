"""Backend fixture contracts with mocked tools only; no processes or sockets."""

import ast
import copy
import json
import hashlib
from pathlib import Path
import re
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock
from unittest.mock import patch

import pitput_backend_acceptance as acceptance
import pitput_backend_provider as provider
import pitput_combined_acceptance_support as support


def text(value):
    return value if isinstance(value, str) else ''


def request(content):
    return {'system': 'owned system config', 'tools': [{'name': 'Bash'}],
            'messages': [{'role': 'user', 'content': content}]}


class BackendFixtureTests(unittest.TestCase):
    def test_all_request_facts_distinguish_tools_omitted_continuation_without_rerouting(self):
        call = 'private exact pending call'
        digest = hashlib.sha256(call.encode()).hexdigest()
        request = {'messages': [{'role': 'user', 'content': 'private prompt'},
                                {'role': 'tool', 'tool_call_id': call, 'content': 'private native output'}]}
        facts = provider.native_request_facts(request, digest)
        self.assertEqual((facts['routing_class'], facts['tools_present'], facts['last_role'],
                          facts['tool_result_count'], facts['last_tool_result_count'], facts['matches_emitted_call']),
                         ('metadata', False, 'tool', 1, 1, True))
        self.assertEqual(facts['last_call_sha256'], digest)
        self.assertNotIn('private', json.dumps(facts))
        request['tools'] = [{'function': {'name': 'shell'}}]
        self.assertEqual(provider.native_request_facts(request, digest)['routing_class'], 'responder')
        malicious = {'messages': [{'role': ['private role'], 'content': 'private arbitrary command'}]}
        self.assertEqual(provider.native_request_facts(malicious)['last_role'], 'other')
        self.assertNotIn('private', json.dumps(provider.native_request_facts(malicious)))

    def test_open_code_records_metadata_and_rejected_requests_in_same_bounded_diagnostic_stream(self):
        value = provider.BackendProvider.__new__(provider.BackendProvider)
        value.active_record = None
        value.request_count, value.request_diagnostics = 0, []
        value._request_lock = threading.Lock()
        for request in ({'messages': [{'role': 'user', 'content': 'private'}]}, None,
                        {'tools': [{'function': {'name': 'shell'}}], 'messages': []}):
            value.record_open_code_request(request)
        self.assertEqual(value.request_count, 3)
        self.assertEqual([row['routing_class'] for row in value.request_diagnostics],
                         ['metadata', 'rejected_or_unparsed', 'responder'])
        self.assertNotIn('private', json.dumps(value.request_diagnostics))

    def muted_policy_case(self, scenario=None, backend='claude'):
        case = acceptance.BackendAcceptance('runTest')
        case.backend, case.thread = backend, 'same-session'
        original = {'source': 'dm', 'delivery_id': 'delivery-original',
                    'attention': {'event_key': 'original-source'}}
        state = {'admission_evidence_ids': [], 'ack_count': 0, 'pending': copy.deepcopy(original)}
        counter = SimpleNamespace(total=0)
        counter.snapshot = lambda: (counter.total, 0)
        canonical = {'text': 'exact owned primer', 'session': case.thread, 'exact_session_binding': True}
        canonical['sha256'] = hashlib.sha256(canonical['text'].encode()).hexdigest()
        case.provider = SimpleNamespace(records=[], counter=counter, initialization_records=[],
                                        initialization_loader=lambda: canonical)
        old = {'session': case.thread, 'generation': 'old-generation'}
        case.result = {'native_generation_bindings': [old]}
        case.muted_source_state = Mock(side_effect=lambda _: copy.deepcopy(state))

        def mutate(phase):
            if scenario == 'source-' + phase:
                case.provider.records.append({'label': 'must-not-deliver'})
            if scenario == 'pending-' + phase:
                state['pending']['delivery_id'] = 'changed-pending'
            if scenario == 'admission-' + phase:
                state['admission_evidence_ids'].append('new-admission')
            if scenario == 'ack-' + phase:
                state['ack_count'] += 1

        def command(action):
            if action == 'resume':
                counter.total += 3
                case.result['native_generation_bindings'].append({
                    'session': 'foreign' if scenario == 'wrong-generation' else case.thread,
                    'generation': 'new-generation', 'predecessor_generation': old['generation'],
                    'single_combined_worker': True, 'native_system_binding': True, 'preserved_state': True})
                case.provider.initialization_records.append({
                    'session': case.thread, 'primer_sha256': 'wrong' if scenario == 'wrong-init' else canonical['sha256'],
                    'utf8_bytes': len(canonical['text'].encode())})
                mutate('startup')
            return {'transport_healthy': True, 'controller_healthy': True, 'thread': case.thread,
                    'native_idle': True, 'pending': None}

        case.command = Mock(side_effect=command)

        def window(name, used_counter, observe, receipt):
            self.assertEqual(name, 'idle-after-recovery-full-primer')
            self.assertIs(used_counter, counter)  # The unfiltered global counter.
            mutate('settling')
            observe()
            baseline = counter.total
            if scenario == 'quiet-request':
                counter.total += 1
            mutate('window')
            observe()
            receipt.update(status='PASS', requests_at_start=baseline, requests_at_baseline=baseline,
                           requests_at_end=counter.total, window_elapsed_seconds=30)

        return case, original, SimpleNamespace(observe_window=window)

    def test_default_muted_resume_keeps_existing_codex_counter_semantics(self):
        case = acceptance.FrozenAcceptance('runTest')
        case.command = Mock()
        self.assertEqual(case.resume_muted_policy({'delivery_id': 'original'}, 99), 99)
        case.command.assert_called_once_with('resume')

    def test_backend_muted_resume_records_startup_and_global_quiet_window_separately(self):
        for backend in ('claude', 'opencode'):
            case, original, idle = self.muted_policy_case(backend=backend)
            with patch.object(acceptance, 'source_module', return_value=idle):
                self.assertEqual(case.resume_muted_policy(original, 0), 3)
            facts = case.result['policy_muted_resume']
            self.assertEqual((facts['status'], facts['requests_before_resume'],
                              facts['requests_at_healthy_baseline'], facts['requests_after_window']),
                             ('PASS', 0, 3, 3))
            self.assertEqual(facts['startup_interval_requests'], 3)
            self.assertEqual(facts['source_records_before'], facts['source_records_after'])
            case.provider.records.append({'label': 'late muted source'})
            with self.assertRaises(AssertionError):
                case._muted_policy_guard()  # Remains active through shared extra3s/stop.

    def test_muted_delivery_during_startup_or_settling_fails_even_if_later_quiet(self):
        for scenario in ('source-startup', 'source-settling', 'source-window', 'pending-startup',
                         'pending-settling', 'admission-startup', 'ack-settling', 'quiet-request'):
            with self.subTest(scenario=scenario):
                case, original, idle = self.muted_policy_case(scenario)
                with patch.object(acceptance, 'source_module', return_value=idle), self.assertRaises(AssertionError):
                    case.resume_muted_policy(original, 0)

    def test_muted_resume_rejects_wrong_fresh_generation_or_owned_initialization(self):
        for scenario, backend in (('wrong-generation', 'claude'), ('wrong-init', 'opencode')):
            with self.subTest(scenario=scenario):
                case, original, idle = self.muted_policy_case(scenario, backend)
                with patch.object(acceptance, 'source_module', return_value=idle), self.assertRaises(AssertionError):
                    case.resume_muted_policy(original, 0)

    def test_emitted_native_reply_facts_are_hashes_and_do_not_claim_transport_acceptance(self):
        reply = {'tool_calls': [{'id': 'private call', 'function': {'name': 'shell',
                                                                 'arguments': 'private command credential'}}]}
        facts = provider.native_reply_delivery(reply, 'read')
        self.assertEqual((facts['kind'], facts['operation'], facts['tool']), ('tool', 'read', 'shell'))
        self.assertFalse(facts['http_write_returned'])
        self.assertEqual(facts['call_sha256'], hashlib.sha256(b'private call').hexdigest())
        self.assertNotIn('private', json.dumps(facts))

    def test_failure_control_publishes_one_read_only_request_with_five_second_budget(self):
        owner = acceptance.OpenCodeOwner.__new__(acceptance.OpenCodeOwner)
        owner.session, owner.step, owner.lifecycle_observations = 'ses_exact', 0, []
        owner.dead = Mock(return_value=False)
        with tempfile.TemporaryDirectory() as temporary:
            owner.case = SimpleNamespace(control=Path(temporary))
            commands = []

            def publish(path, text):
                commands.append(json.loads(text))
                path.with_name('result-0000.json').write_text(json.dumps({
                    'thread': 'ses_exact', 'failure_observation': {'status': 'observed'}}))

            with (patch.object(support, 'publish_control', publish),
                  patch.object(acceptance.time, 'time', return_value=1000),
                  patch.object(acceptance.time, 'monotonic', return_value=50)):
                value = owner.control('failure-observe', timeout=5, call_sha256='a' * 64)
            self.assertEqual(value['failure_observation']['status'], 'observed')
            self.assertEqual(commands, [{'action': 'failure-observe', 'deadline_unix_ms': 1005000,
                                         'call_sha256': 'a' * 64}])

    def test_failure_control_timeout_and_projection_error_cannot_replace_primary_failure(self):
        owner = acceptance.OpenCodeOwner.__new__(acceptance.OpenCodeOwner)
        owner.session, owner.step, owner.lifecycle_observations = 'ses_exact', 0, []
        owner.dead = Mock(return_value=False)
        with tempfile.TemporaryDirectory() as temporary:
            owner.case = SimpleNamespace(control=Path(temporary))
            with (patch.object(support, 'publish_control'),
                  patch.object(acceptance.time, 'monotonic', side_effect=[0, 6]),
                  patch.object(acceptance.time, 'sleep', side_effect=AssertionError('deadline ignored'))):
                with self.assertRaisesRegex(RuntimeError, 'control_failed'):
                    owner.control('failure-observe', timeout=5)
        case = acceptance.BackendAcceptance('runTest')
        case.owner = owner
        owner.failure_observation = Mock(side_effect=ValueError('private credential'))
        try:
            raise AssertionError('mandatory native handling failed')
        except AssertionError as primary:
            result = case.collect_backend_failure()
            self.assertEqual(str(primary), 'mandatory native handling failed')
        self.assertEqual(result, {'status': 'unavailable', 'error_type': 'other'})
        self.assertNotIn('private', json.dumps(result))

    def test_claude_child_environment_preserves_both_native_helper_authority_gates(self):
        with patch.dict(acceptance.os.environ, {'UNRELATED_AMBIENT_SECRET': 'private'}, clear=True):
            env = acceptance.authorized_claude_fixture_environment({'PATH': '/isolated/bin', 'HOME': '/isolated/home'}, Path('/isolated/home'))
        self.assertEqual(env['CI'], 'true')
        self.assertEqual(env['PITPUT_NATIVE_FIXTURE_EXECUTION_AUTHORIZED'], '1')
        self.assertEqual(env['PATH'], '/isolated/home/bin' + acceptance.os.pathsep + '/isolated/bin')
        self.assertNotIn('UNRELATED_AMBIENT_SECRET', env)

    def test_backend_versions_use_exact_real_toolchain_banners(self):
        pins = json.loads((acceptance.ROOT / 'tests/compatibility/ci/toolchain.json').read_text())
        for backend in ('opencode', 'claude'):
            with self.subTest(backend=backend):
                acceptance.verify_backend_version(backend, pins[backend])
                for changed in ('2.1.2', pins[backend] + ' modified', ''):
                    with self.assertRaisesRegex(SystemExit, 'pinned_backend_version_required'):
                        acceptance.verify_backend_version(backend, changed)

    def test_local_native_entrypoint_is_blocked_before_any_runtime_or_provenance(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'report.xml'
            with (patch.object(acceptance.sys, 'argv', ['fixture', '--backend', 'claude', '--junit', str(output)]),
                  patch.dict(acceptance.os.environ, {}, clear=True),
                  patch.object(acceptance.ownership, 'pitput_provenance') as provenance,
                  patch.object(acceptance.subprocess, 'check_output') as process):
                self.assertEqual(acceptance.main(), 2)
                provenance.assert_not_called()
                process.assert_not_called()
            record = json.loads(output.with_suffix('.json').read_text())
            self.assertEqual(record['status'], 'BLOCKED')
            self.assertEqual(output.read_text().count('<testcase'), 9)

    def script(self):
        state = SimpleNamespace(errors=[], task_diagnostics=[], tool_diagnostics=[], counter=Mock(), native_fault=lambda: None,
                                respond=Mock(return_value='complete'))
        base = SimpleNamespace(text_of=text,
                               latest_dialogue_message=lambda value: value['messages'][-1],
                               tag=lambda value, name: (re.search('<' + name + '>(.*?)</' + name + '>', value).group(1)
                                                        if re.search('<' + name + '>(.*?)</' + name + '>', value) else None))
        binding = Mock(return_value=(Path('/private/@combined/bridge.toml'), 'owner/dev/agent', 'native-session'))
        return provider.ClaudeTaskScript(state, base, binding), state, binding

    def test_claude_uses_owned_task_output_and_correlated_native_tools(self):
        script, state, binding = self.script()
        armed = script.decide(request('start'))
        binding.assert_called_with('owned system config')
        self.assertTrue(armed['tool']['input']['run_in_background'])
        self.assertIn('bridge claude-task', armed['tool']['input']['command'])
        self.assertNotIn('codex', json.dumps(armed))
        result = [{'type': 'tool_result', 'tool_use_id': armed['tool']['id'],
                   'content': 'ID: owned_task output written to: /private/output'}]
        script.base.text_of = lambda value: value if isinstance(value, str) else ''
        self.assertIn('text', script.decide(request(result)))
        notice = ('<task-notification><task-id>owned_task</task-id><output-file>/private/output</output-file>'
                  '<status>completed</status></task-notification>')
        event = script.decide(request(notice))
        self.assertEqual(event['tool']['input']['command'], 'cat /private/output')
        state.respond.assert_not_called()  # A task notification never reads the source itself.
        state.respond.return_value = {'tool_calls': [{'id': 'exact_read_call', 'function': {
            'arguments': json.dumps({'command': 'fixed read command', 'workdir': '/outside checkout'})}}]}
        read = script.decide(request([{'type': 'tool_result', 'tool_use_id': event['tool']['id'],
                                      'content': 'exact immutable presentation'}]))
        self.assertEqual(read['tool']['id'], 'exact_read_call')
        self.assertIn("cd '/outside checkout'", read['tool']['input']['command'])
        state.respond.return_value = 'completed real tools'
        again = script.decide(request([{'type': 'tool_result', 'tool_use_id': 'exact_read_call',
                                       'content': 'actual native envelope'}]))
        self.assertTrue(again['tool']['input']['run_in_background'])
        forwarded = state.respond.call_args.args[0]['messages'][0]
        self.assertEqual((forwarded['role'], forwarded['tool_call_id']), ('tool', 'exact_read_call'))
        self.assertEqual(state.errors, [])

    def test_foreign_or_failed_task_cannot_read_any_output(self):
        script, state, _ = self.script()
        script.task = ('owned', '/private/owned')
        result = script.decide(request('<task-notification><task-id>foreign</task-id>'
                                       '<output-file>/private/secret</output-file><status>completed</status></task-notification>'))
        self.assertNotIn('tool', result)
        self.assertEqual(state.errors, ['claude_foreign_or_failed_task_notification'])
        state.respond.assert_not_called()

    def test_native_tool_correlation_failure_categories_remain_strict_and_body_free(self):
        cases = [([], 'claude_task_tool_result_count'),
                 ([{'type': 'tool_result', 'tool_use_id': 'foreign', 'content': 'private credential'}], 'claude_task_tool_result_id'),
                 ([{'type': 'tool_result', 'tool_use_id': 'expected', 'is_error': True, 'content': 'private native error'}], 'claude_task_tool_result_native_error')]
        for results, expected in cases:
            script, state, _ = self.script()
            script.pending = ('expected', 'native-tool')
            reply = script.decide(request(results))
            self.assertNotIn('tool', reply)
            self.assertEqual(state.errors, [expected])
            self.assertNotIn('private', json.dumps(state.tool_diagnostics))
            self.assertEqual(script.pending, ('expected', 'native-tool'))
            state.respond.assert_not_called()

    def test_owned_initialization_is_exact_and_separate_from_source_handling(self):
        value = provider.BackendProvider.__new__(provider.BackendProvider)
        primer = 'private exact owned initialization'
        value.module = SimpleNamespace(core=SimpleNamespace(CompactResponder=SimpleNamespace(_text=lambda text: text)))
        value.initialization_loader = lambda: {'session': 'ses_original', 'text': primer,
                                              'sha256': hashlib.sha256(primer.encode()).hexdigest(),
                                              'exact_session_binding': True}
        value.initialization_records = []
        value.initialization_session = None
        value.engine = Mock()
        value.engine.responder.call_id = None
        value.engine.responder.completed = False
        value.engine.respond.return_value = 'ordinary native reply'
        value.engine.facts.return_value = {'completed': False}
        value.attention_release = threading.Event()
        value.attention_release.set()
        reply = value.respond({'messages': [{'role': 'user', 'content': primer}]})
        self.assertIn('initialization received', reply)
        value.engine.respond.assert_not_called()
        self.assertEqual(len(value.initialization_records), 1)
        self.assertNotIn('private', json.dumps(value.initialization_records))
        value.respond({'messages': [{'role': 'user', 'content': primer + ' modified'}]})
        value.engine.respond.assert_called_once()
        self.assertEqual(len(value.initialization_records), 1)
        value.initialization_session = 'ses_foreign'
        with self.assertRaisesRegex(ValueError, 'owned_initialization_binding_mismatch'):
            value.respond({'messages': [{'role': 'user', 'content': primer}]})

    def test_owned_init_preserves_only_exact_pending_actual_suffix(self):
        call = 'pending-call'
        suffix = [{'role': 'assistant', 'tool_calls': [{'id': call, 'function': {'name': 'shell'}}]},
                  {'role': 'tool', 'tool_call_id': call, 'content': 'actual private envelope'},
                  {'role': 'user', 'content': 'exact canonical owned init'}]
        actual = {'tools': [{'name': 'shell'}], 'messages': [
            {'role': 'system', 'content': 'private system'},
            {'role': 'user', 'content': 'original notification'}, *suffix]}
        forwarded = provider.owned_initialization_tool_suffix(actual, call)
        self.assertEqual(forwarded['messages'], actual['messages'][:-1])
        self.assertIs(forwarded['messages'][-1], suffix[1])
        self.assertEqual(len(actual['messages']), 5)
        invalid = []
        for index, field, value in [(0, 'role', 'user'), (1, 'tool_call_id', 'foreign')]:
            changed = copy.deepcopy(suffix)
            changed[index][field] = value
            invalid.append(changed)
        duplicate_call = copy.deepcopy(suffix)
        duplicate_call[0]['tool_calls'].append(copy.deepcopy(duplicate_call[0]['tool_calls'][0]))
        invalid.extend([duplicate_call, suffix[1:], suffix[:2] + [suffix[1], suffix[2]],
                        suffix + [{'role': 'user', 'content': 'later init'}]])
        stale = copy.deepcopy(suffix)
        stale[0]['tool_calls'][0]['id'] = 'stale'
        invalid.append(stale)
        for messages in invalid:
            with self.subTest(messages=messages), self.assertRaises(ValueError):
                provider.owned_initialization_tool_suffix({'messages': messages}, call)

    def test_owned_init_routes_actual_result_through_existing_strict_engine(self):
        value = provider.BackendProvider.__new__(provider.BackendProvider)
        primer = 'canonical initialization'
        value.module = SimpleNamespace(
            core=SimpleNamespace(CompactResponder=SimpleNamespace(_text=lambda value: value)),
            envelope=SimpleNamespace(decode=Mock(return_value={'stdout': '{}'})))
        value.initialization_loader = lambda: {'session': 'ses_original', 'text': primer,
            'sha256': hashlib.sha256(primer.encode()).hexdigest(), 'exact_session_binding': True}
        value.initialization_records = []
        value.initialization_session = None
        value.engine = Mock()
        value.engine.responder.call_id = 'exact-call'
        value.engine.responder.completed = False
        value.engine.responder.operation = 'native-read-validation'
        value.engine.responder._text = lambda value: value
        value.engine.respond.return_value = {'next': 'strict engine reply'}
        value.ack_retry_authorizer = None
        actual = {'tools': [{'name': 'shell'}], 'messages': [
            {'role': 'system', 'content': 'system'},
            {'role': 'user', 'content': 'notification'},
            {'role': 'assistant', 'tool_calls': [{'id': 'exact-call'}]},
            {'role': 'tool', 'tool_call_id': 'exact-call', 'content': 'actual-envelope'},
            {'role': 'user', 'content': primer}]}
        self.assertEqual(value.respond(actual), {'next': 'strict engine reply'})
        value.module.envelope.decode.assert_called_once_with('actual-envelope', 'exact-call')
        value.engine.respond.assert_called_once_with({**actual, 'messages': actual['messages'][:-1]})
        self.assertTrue(value.initialization_records[0]['pending_result_consumed'])
        self.assertNotIn('actual-envelope', json.dumps(value.initialization_records))

    def test_native_provenance_accounts_uncertain_predecessor_and_exact_ack_ids(self):
        case = acceptance.BackendAcceptance('test_frozen_matrix')
        case.result = {}
        base = {'delivery_id': 'delivery', 'payload_sha256': 'digest',
                'source_event_key': 'event', 'source_monitor': 'monitor'}
        attempts = [{**base, 'record_id': 'old', 'attempt': 1, 'phase': 'uncertain', 'report_digest': 'a'},
                    {**base, 'record_id': 'new', 'attempt': 2, 'phase': 'admitted', 'report_digest': 'b'}]
        facts = {'entries': [], 'retired_attempts': 0, 'attempts': attempts,
                 'acknowledged': [{**item, 'attribution': 'known'} for item in attempts]}
        case.publication_audit = lambda: facts
        case.provider = SimpleNamespace(records=[{**base, 'native_publication_observation': {
            'matching_attempts': [{'record_id': 'old', 'phase_at_observation': 'enqueueing'}]}}])
        def wait(condition, *_args, **_kwargs):
            self.assertTrue(condition())
        case.wait = wait
        self.assertIs(case.native_publication_receipts(), facts)
        self.assertEqual(case.provider.records[0]['positive_callback_record_ids'], ['new'])
        self.assertEqual(attempts[0]['phase'], 'uncertain')
        for change in ('missing', 'foreign', 'digest', 'enqueueing', 'no-positive'):
            changed = copy.deepcopy(facts)
            if change == 'missing':
                changed['acknowledged'].pop()
            elif change == 'foreign':
                changed['acknowledged'].append({'record_id': 'foreign'})
            elif change == 'digest':
                changed['acknowledged'][0]['report_digest'] = 'wrong'
            elif change == 'enqueueing':
                changed['attempts'][0]['phase'] = 'enqueueing'
            else:
                changed['attempts'][1]['phase'] = 'uncertain'
                changed['acknowledged'][1]['phase'] = 'uncertain'
            case.publication_audit = lambda: changed
            with self.subTest(change=change), self.assertRaises(AssertionError):
                case.native_publication_receipts()

    def test_retained_crash_witness_requires_actual_exact_suffix_and_native_proof(self):
        original = {'messages': [{'role': 'system', 'content': 'private system'},
                                {'role': 'user', 'content': 'private actual notification'}]}
        actual = {**original, 'messages': [*original['messages'], {'role': 'user', 'content': 'canonical init'}]}
        binding = {'owner': 'owner', 'session': 'ses_original', 'delivery_id': 'delivery-original',
                   'receipt_binding': 'generation-current', 'epoch': 1,
                   'payload_sha256': 'payload', 'source_envelope_sha256': 'source'}
        proof = {'session': binding['session'], 'exact_root_project_binding': True,
                 'target_message_id': 'msg_' + binding['delivery_id'], 'history': {
                     'target_id_occurrences': 1,
                     'target_message': {'status': 'observed', 'text_sha256': 'payload'}}}
        def witness():
            return {'binding': copy.deepcopy(binding), 'request': copy.deepcopy(original),
                    'authorized': True, 'consumed': False}
        owned = witness()
        result = provider.retained_crash_suffix(actual, owned, binding, proof)
        self.assertIs(result['messages'][-1], actual['messages'][-2])
        self.assertEqual(result['messages'], original['messages'])
        self.assertTrue(owned['consumed'])
        with self.assertRaisesRegex(ValueError, 'consumed'):
            provider.retained_crash_suffix(actual, owned, binding, proof)
        for change in ('prefix', 'foreign', 'missing', 'duplicate', 'old-generation', 'session',
                       'pending', 'unauthorized', 'native-missing', 'native-digest', 'native-duplicate'):
            candidate, request, current, native = witness(), copy.deepcopy(actual), dict(binding), copy.deepcopy(proof)
            if change == 'prefix':
                request['messages'][0]['content'] = 'changed history'
            elif change == 'foreign':
                request['messages'][-2]['content'] = 'foreign notification'
            elif change == 'missing':
                request['messages'].pop(-2)
            elif change == 'duplicate':
                request['messages'].insert(-1, copy.deepcopy(request['messages'][-2]))
            elif change == 'old-generation':
                current['receipt_binding'] = 'retired-generation'
            elif change == 'session':
                current['session'] = 'ses_foreign'
            elif change == 'pending':
                current['source_envelope_sha256'] = 'changed source'
            elif change == 'unauthorized':
                candidate['authorized'] = False
            elif change == 'native-missing':
                native['history']['target_message']['status'] = 'unavailable'
            elif change == 'native-digest':
                native['history']['target_message']['text_sha256'] = 'foreign'
            else:
                native['history']['target_id_occurrences'] = 2
            with self.subTest(change=change), self.assertRaises(ValueError):
                provider.retained_crash_suffix(request, candidate, current, native)
            self.assertFalse(candidate['consumed'])

    def test_verified_owned_init_consumes_actual_retained_notification_once(self):
        value = provider.BackendProvider.__new__(provider.BackendProvider)
        primer = 'canonical owned init'
        notification = 'private actual source notification'
        binding = {'owner': 'owner', 'session': 'ses_original', 'delivery_id': 'delivery-original',
                   'payload_sha256': hashlib.sha256(notification.encode()).hexdigest()}
        original = {'tools': [{'name': 'shell'}], 'messages': [
            {'role': 'system', 'content': 'private history'}, {'role': 'user', 'content': notification}]}
        value.module = SimpleNamespace(core=SimpleNamespace(CompactResponder=SimpleNamespace(_text=text)))
        value.crash_binding = lambda: binding
        value.crash_capture_requested = True
        value.crash_witness = None
        value.initialization_loader = lambda: {'session': 'ses_original', 'text': primer,
            'sha256': hashlib.sha256(primer.encode()).hexdigest(), 'exact_session_binding': True,
            'exact_instruction_binding': True, 'instruction_root': '/owned/cache',
            'instruction_path': '/owned/cache/hrx/yesod-instructions-123.md'}
        value.capture_crash_candidate(original)
        with self.assertRaisesRegex(ValueError, 'duplicate_candidate'):
            value.capture_crash_candidate(original)
        value.crash_witness['authorized'] = True
        value.crash_witness_records = []
        value.crash_witness_diagnostics = []
        value.crash_resume_proof = lambda: {
            'session': binding['session'], 'exact_root_project_binding': True,
            'target_message_id': 'msg_' + binding['delivery_id'], 'history': {
                'target_id_occurrences': 1, 'target_message': {
                    'status': 'observed', 'text_sha256': binding['payload_sha256']}}}
        value.initialization_loader = lambda: {'session': 'ses_original', 'text': primer,
            'sha256': hashlib.sha256(primer.encode()).hexdigest(), 'exact_session_binding': True}
        value.initialization_session = None
        value.initialization_records = []
        value.attention_release = threading.Event()
        value.attention_release.set()
        value.engine = Mock()
        value.engine.responder.call_id = None
        value.engine.responder.completed = False
        value.engine.respond.return_value = 'strict engine actual retained request'
        value.engine.facts.return_value = {'completed': False}
        actual = {**original, 'messages': [*original['messages'], {'role': 'user', 'content': primer}]}
        self.assertEqual(value.respond(actual), 'strict engine actual retained request')
        value.engine.respond.assert_called_once_with(original)
        self.assertTrue(value.crash_witness_records[0]['consumed_once'])
        self.assertNotIn('private', json.dumps(value.crash_witness_records))
        value.respond(actual)  # A later init cannot consume the old witness again.
        value.engine.respond.assert_called_once()
        self.assertEqual(len(value.crash_witness_records), 1)

    def test_exact_owned_instruction_update_matches_only_complete_source_equivalent_transform(self):
        primer = 'canonical & <owned> primer'
        binding = {'session': 'ses_original', 'delivery_id': 'delivery-original', 'payload_sha256': 'payload'}
        old = {'session': binding['session'], 'text': primer,
               'sha256': hashlib.sha256(primer.encode()).hexdigest(), 'exact_session_binding': True,
               'exact_instruction_binding': True, 'instruction_root': '/owned/cache',
               'instruction_path': '/owned/cache/hrx/yesod-instructions-123.md'}
        new = {**old, 'instruction_path': '/owned/cache/hrx/yesod-instructions-456.md'}
        # Direct pinned renderUpdate + wrapSystemUpdate + string-user merge.
        append = ('\n<system-update>\nThe instructions from /owned/cache/hrx/yesod-instructions-123.md'
                  ' no longer apply.\n\nNew instructions apply from:\nInstructions from: '
                  '/owned/cache/hrx/yesod-instructions-456.md\ncanonical &amp; &lt;owned&gt; primer'
                  '\n</system-update>')
        self.assertEqual(provider.exact_owned_instruction_append(old, new, binding), append)
        before = [{'role': 'system', 'content': 'private history'},
                  {'role': 'user', 'content': 'private actual notification', 'name': 'owned'}]
        actual = {'messages': [before[0], {**before[1], 'content': before[1]['content'] + append},
                               {'role': 'user', 'content': primer}]}
        proof = {'session': binding['session'], 'exact_root_project_binding': True,
                 'target_message_id': 'msg_' + binding['delivery_id'], 'history': {
                     'target_id_occurrences': 1,
                     'target_message': {'status': 'observed', 'text_sha256': 'payload'}}}
        def witness():
            return {'binding': binding, 'request': {'messages': copy.deepcopy(before)},
                    'instruction': copy.deepcopy(old), 'authorized': True, 'consumed': False}
        owned = witness()
        forwarded = provider.retained_crash_suffix(actual, owned, binding, proof, initialization=new)
        self.assertEqual(forwarded['messages'], before)
        self.assertEqual(actual['messages'][-2]['content'], before[-1]['content'] + append)
        self.assertTrue(owned['verified_transform']['complete_transform_verified'])
        self.assertNotIn('/owned/', json.dumps(owned['verified_transform']))
        with self.assertRaisesRegex(ValueError, 'consumed'):
            provider.retained_crash_suffix(actual, owned, binding, proof, initialization=new)
        for change in ('absent-metadata', 'absent-old', 'bad-old-hash', 'bad-new-hash', 'untrusted-path', 'forged-path', 'outside-root', 'different-root', 'same-path', 'wrong-primer',
                       'wrong-session', 'wrapper', 'escaping', 'payload', 'suffix', 'preceding', 'metadata', 'final-init'):
            candidate, request, current = witness(), copy.deepcopy(actual), copy.deepcopy(new)
            if change == 'absent-metadata': current = None
            elif change == 'absent-old': candidate.pop('instruction')
            elif change == 'bad-old-hash': candidate['instruction']['sha256'] = 'forged'
            elif change == 'bad-new-hash': current['sha256'] = 'forged'
            elif change == 'untrusted-path': current['exact_instruction_binding'] = False
            elif change == 'forged-path': current['instruction_path'] = '/owned/cache/hrx/yesod-instructions-999.md'
            elif change == 'outside-root': current['instruction_path'] = '/foreign/yesod-instructions-456.md'
            elif change == 'different-root': current['instruction_root'] = '/foreign/cache'
            elif change == 'same-path': current['instruction_path'] = old['instruction_path']
            elif change == 'wrong-primer':
                current['text'] += ' modified'
                current['sha256'] = hashlib.sha256(current['text'].encode()).hexdigest()
            elif change == 'wrong-session': current['session'] = 'ses_foreign'
            elif change == 'wrapper': request['messages'][-2]['content'] = request['messages'][-2]['content'].replace('system-update', 'other-update')
            elif change == 'escaping': request['messages'][-2]['content'] = request['messages'][-2]['content'].replace('&amp;', '&')
            elif change == 'payload': request['messages'][-2]['content'] = 'changed notification' + append
            elif change == 'suffix': request['messages'][-2]['content'] += ' extra'
            elif change == 'preceding': request['messages'][0]['content'] += ' changed'
            elif change == 'metadata': request['messages'][-2]['name'] = 'foreign'
            else: request['messages'][-1]['content'] = 'arbitrary init'
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, '^crash_witness_actual_suffix_changed$'):
                provider.retained_crash_suffix(request, candidate, binding, proof, initialization=current)
            self.assertFalse(candidate['consumed'])

    def test_crash_mismatch_diagnostics_distinguish_text_representation_without_private_values(self):
        def extract(content):
            if isinstance(content, str):
                return content
            if isinstance(content, list) and all(isinstance(item, dict) and item.get('type') == 'text'
                                                and isinstance(item.get('text'), str) for item in content):
                return '\n'.join(item['text'] for item in content)
            raise ValueError('private arbitrary extraction error')
        before = [{'role': 'system', 'content': 'private system'},
                  {'role': 'user', 'content': 'private notification'}]
        baseline = [*copy.deepcopy(before), {'role': 'user', 'content': 'private canonical init'}]
        for change in ('representation', 'text', 'unknown-key', 'tool-call', 'missing', 'malicious-block'):
            actual = copy.deepcopy(baseline)
            if change == 'representation':
                actual[0]['content'] = [{'type': 'text', 'text': before[0]['content']}]
            elif change == 'text':
                actual[0]['content'] = 'private changed system'
            elif change == 'unknown-key':
                actual[1]['private arbitrary field'] = {'credential': 'private secret'}
            elif change == 'tool-call':
                actual[1]['tool_calls'] = [{'id': 'private-id', 'function': {'name': 'private-command'}}]
            elif change == 'missing':
                actual.pop(0)
            else:
                actual[0]['content'] = [{'type': 'private arbitrary type', 'text': 'private hidden'}] * 40
            facts = provider.crash_suffix_mismatch_facts(actual, before, extract)
            self.assertNotIn('private', json.dumps(facts))
            mismatch = facts['first_mismatch']
            self.assertIn(mismatch['expected_role'], ('system', 'user'))
            self.assertRegex(mismatch['expected_sha256'], '^[0-9a-f]{64}$')
            self.assertTrue(set(mismatch['changed_fields']) <= {
                'role', 'content', 'tool_calls', 'tool_call_id', 'name', 'reasoning_content', 'providerOptions'})
            if change == 'representation':
                self.assertEqual(mismatch['index'], 0)
                self.assertFalse(facts['system_equal'])
                self.assertTrue(facts['notification_message_equal'])
                self.assertTrue(mismatch['text_equal'])
                self.assertEqual(mismatch['expected_content']['type'], 'string')
                self.assertEqual(mismatch['actual_content']['type'], 'list')
                self.assertEqual(mismatch['actual_content']['block_types']['text'], 1)
            elif change == 'text':
                self.assertFalse(mismatch['text_equal'])
                self.assertEqual(mismatch['actual_content']['type'], 'string')
            elif change == 'unknown-key':
                self.assertTrue(facts['system_equal'])
                self.assertEqual(mismatch['changed_fields'], [])
                self.assertTrue(mismatch['other_fields_changed'])
            elif change == 'malicious-block':
                self.assertEqual(mismatch['actual_content']['block_types']['other'], 32)
                self.assertTrue(mismatch['actual_content']['blocks_truncated'])
                self.assertEqual(mismatch['actual_content']['text_status'], 'unsupported')

    def test_crash_diagnostics_do_not_change_rejection_or_consume_and_are_bounded(self):
        before = [{'role': 'user', 'content': 'private actual notification'}]
        binding = {'session': 'ses_original', 'delivery_id': 'delivery-original', 'payload_sha256': 'payload'}
        proof = {'session': binding['session'], 'exact_root_project_binding': True,
                 'target_message_id': 'msg_' + binding['delivery_id'], 'history': {
                     'target_id_occurrences': 1,
                     'target_message': {'status': 'observed', 'text_sha256': 'payload'}}}
        witness = {'binding': binding, 'request': {'messages': before}, 'authorized': True, 'consumed': False}
        actual = {'messages': [{'role': 'user', 'content': 'private changed'},
                               {'role': 'user', 'content': 'private canonical init'}]}
        diagnostics = []
        for _ in range(6):
            with self.assertRaisesRegex(ValueError, '^crash_witness_actual_suffix_changed$'):
                provider.retained_crash_suffix(actual, witness, binding, proof, diagnostics=diagnostics,
                                              text_extract=Mock(side_effect=ValueError('private parser error')))
        self.assertFalse(witness['consumed'])
        self.assertEqual(len(diagnostics), 4)
        self.assertNotIn('private', json.dumps(diagnostics))
        self.assertEqual(diagnostics[0]['first_mismatch']['actual_content']['text_status'], 'unsupported')
        with patch.object(provider, 'crash_suffix_mismatch_facts', side_effect=RuntimeError('private fault')):
            fallback = []
            with self.assertRaisesRegex(ValueError, '^crash_witness_actual_suffix_changed$'):
                provider.retained_crash_suffix(actual, witness, binding, proof, diagnostics=fallback)
        self.assertEqual(fallback, [{'status': 'unavailable'}])
        self.assertFalse(witness['consumed'])

    def test_crash_witness_requires_callback_stop_and_exact_before_stop_history(self):
        value = provider.BackendProvider.__new__(provider.BackendProvider)
        binding = {'owner': 'owner', 'session': 'ses_original', 'delivery_id': 'delivery-original',
                   'payload_sha256': 'payload'}
        value.crash_binding = lambda: binding
        proof = {'stage': 'before_stop', 'session': binding['session'], 'exact_root_project_binding': True,
                 'target_message_id': 'msg_' + binding['delivery_id'], 'history': {
                     'target_id_occurrences': 1,
                     'target_message': {'status': 'observed', 'text_sha256': 'payload'}}}
        boundary = {'phase': 'after-admission', 'native_submit_returned': True,
                    'owner': 'owner', 'session': binding['session'], 'delivery_id': binding['delivery_id']}
        stopped = {'native_stop_verified': True, 'transport_healthy': False,
                   'controller_healthy': False, 'native_lifecycle': [proof]}
        for change in ('success', 'no-callback', 'not-stopped', 'wrong-owner', 'no-history'):
            value.crash_witness = {'binding': binding, 'authorized': False}
            current, result = dict(boundary), copy.deepcopy(stopped)
            if change == 'no-callback': current['native_submit_returned'] = False
            if change == 'not-stopped': result['native_stop_verified'] = False
            if change == 'wrong-owner': current['owner'] = 'foreign'
            if change == 'no-history': result['native_lifecycle'] = []
            if change == 'success':
                value.authorize_crash_witness(current, result)
                self.assertTrue(value.crash_witness['authorized'])
            else:
                with self.subTest(change=change), self.assertRaises(ValueError):
                    value.authorize_crash_witness(current, result)
                self.assertFalse(value.crash_witness['authorized'])

    def test_startup_diagnostics_export_only_known_stage_and_failure_codes(self):
        self.assertEqual(acceptance.safe_startup_failure({'stage': 'client_start', 'code': 'unbound_attachment_prompt'}),
                         {'stage': 'client_start', 'code': 'unbound_attachment_prompt'})
        value = acceptance.safe_startup_failure({'stage': 'private_credential', 'code': 'private_body'})
        self.assertNotIn('private', json.dumps(value))

    def test_shutdown_retires_known_task_before_native_failure_notification(self):
        script, state, _ = self.script()
        script.task = ('owned', '/private/owned')
        script.retire_current_task()
        # Native termination need not include an output-file. An exact known
        # predecessor is ignored, never read or rearmed during shutdown.
        for status in ('failed', 'killed', 'completed'):
            result = script.decide(request('<task-notification><task-id>owned</task-id>'
                                           '<status>' + status + '</status></task-notification>'))
            self.assertNotIn('tool', result)
        state.respond.assert_not_called()
        self.assertEqual(state.errors, [])
        self.assertTrue(all(value['known_retired_task'] for value in state.task_diagnostics))
        self.assertNotIn('/private', json.dumps(state.task_diagnostics))
        # A failed task that was never retired still cannot supply output.
        script.task = ('current', '/private/current')
        result = script.decide(request('<task-notification><task-id>current</task-id>'
                                       '<output-file>/private/current</output-file><status>failed</status></task-notification>'))
        self.assertNotIn('tool', result)
        self.assertEqual(state.errors, ['claude_foreign_or_failed_task_notification'])

    def test_verified_resumed_generation_rearms_once_without_reading_retired_output(self):
        script, state, binding = self.script()
        script.task = ('old', '/private/retired')
        script.retire_current_task()
        script.task = None
        script.fresh_generation_allowed = True
        notice = '<task-notification><task-id>old</task-id></task-notification>'
        result = script.decide(request(notice))
        binding.assert_called_once_with('owned system config')
        self.assertTrue(result['tool']['input']['run_in_background'])
        self.assertFalse(script.fresh_generation_allowed)
        pending = script.pending
        again = script.decide(request(notice))
        self.assertNotIn('tool', again)
        self.assertEqual(script.pending, pending)
        self.assertEqual(binding.call_count, 1)
        state.respond.assert_not_called()
        self.assertEqual(state.errors, [])

    def test_resumed_generation_cannot_change_identity_state_or_reuse_generation(self):
        previous = {'backend': 'claude-task', 'claude_home': '/private/home',
                    'state_directory': '/private/shared-state',
                    'claude_task_generation': '12345678-1234-4123-8123-123456789abc',
                    'agents': [{'agent_id': 'owner/dev/agent', 'thread_id': 'same-session', 'combined': True}]}
        current = {**previous, 'claude_task_generation': '22345678-1234-4123-8123-123456789abc'}
        provider.validate_claude_generation(current, 'owner/dev/agent', 'same-session', previous)
        cases = [previous, {**current, 'state_directory': '/other/state'},
                 {**current, 'claude_home': '/other/home'}, {**current, 'agents': []}]
        for field, value in (('agent_id', 'foreign/dev/agent'), ('thread_id', 'foreign-session'), ('combined', False)):
            changed = copy.deepcopy(current)
            changed['agents'][0][field] = value
            cases.append(changed)
        for changed in cases:
            with self.assertRaises(ValueError):
                provider.validate_claude_generation(changed, 'owner/dev/agent', 'same-session', previous)

    def test_failed_fresh_generation_binding_cannot_rearm_retired_task(self):
        script, state, binding = self.script()
        script.retired_tasks.add('old')
        script.fresh_generation_allowed = True
        binding.side_effect = ValueError('claude_combined_predecessor_config_reused')
        result = script.decide(request('<task-notification><task-id>old</task-id></task-notification>'))
        self.assertNotIn('tool', result)
        self.assertIsNone(script.pending)
        state.respond.assert_not_called()
        self.assertEqual(state.errors, ['claude_combined_predecessor_config_reused'])

    def test_owner_binds_new_config_in_native_system_before_publishing_it_to_provider(self):
        with tempfile.TemporaryDirectory() as temporary:
            owner = acceptance.ClaudeOwner.__new__(acceptance.ClaudeOwner)
            owner.home, owner.session = Path(temporary), 'same-session'
            owner.case = SimpleNamespace(provider=SimpleNamespace(config=None), result={})
            owner.config = None
            owner.previous_config = Path(temporary) / 'old-config'
            owner.previous_generation = {'claude_task_generation': '12345678-1234-4123-8123-123456789abc',
                                         'state_directory': '/private/shared', 'claude_home': '/private/home'}
            root = owner.home / 'state/hrx/pitput' / hashlib.sha256(acceptance.OWNER.encode()).hexdigest()
            path = root / '.claude-launch-new/same-session/@combined/bridge.toml'
            path.parent.mkdir(parents=True)
            path.write_text('backend="claude-task"\nclaude_home="/private/home"\n'
                            'state_directory="/private/shared"\n'
                            'claude_task_generation="22345678-1234-4123-8123-123456789abc"\n'
                            '[[agents]]\nagent_id=' + json.dumps(acceptance.OWNER) + '\n'
                            'thread_id="same-session"\ncombined=true\n')
            with self.assertRaisesRegex(ValueError, 'system_binding_missing'):
                owner.binding('unrelated native system')
            self.assertIsNone(owner.case.provider.config)
            config, _, _ = owner.binding('Pitput attention protocol attention-v1. ' + str(path))
            self.assertEqual(config, path)
            self.assertEqual(owner.case.provider.config, path)
            self.assertEqual(len(owner.case.result['native_generation_bindings']), 1)

    def test_owner_publishes_retirement_before_signaling_supervisor(self):
        script, state, _ = self.script()
        script.task = ('owned', '/private/owned')
        owner = acceptance.ClaudeOwner.__new__(acceptance.ClaudeOwner)
        owner.script, owner.process, owner.summary = script, Mock(), Path('/private/summary')
        owner.terminal = owner.master = None

        def stop(process, summary, evidence):
            self.assertIn('owned', script.retired_tasks)
            result = script.decide(request('<task-notification><task-id>owned</task-id>'
                                           '<status>killed</status></task-notification>'))
            self.assertNotIn('tool', result)
            return 'clean'

        owner.base = SimpleNamespace(stop_supervised=Mock(side_effect=stop))
        owner.stop()
        self.assertTrue(owner.stopped)
        self.assertIsNone(owner.process)
        self.assertEqual(state.errors, [])

    def test_unplanned_controller_exit_is_failure_but_explicit_stop_is_observable(self):
        case = acceptance.BackendAcceptance('runTest')
        case.thread = 'session'
        case.intentionally_stopped = case.expected_fault = False
        case.owner = Mock()
        case.owner.observe.return_value = {'transport_healthy': True, 'controller_healthy': False, 'native_idle': True}
        case.bridge_pending = Mock(return_value=None)
        with self.assertRaisesRegex(AssertionError, 'consumer unexpectedly exited'):
            case.command('observe')
        case.command('stop')
        self.assertTrue(case.intentionally_stopped)

    def test_actual_fault_wrapper_binds_original_owner_session_and_delivery(self):
        for phase in ('before-admission', 'after-admission'):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source = acceptance.FAULT_CLI.format(python='/fixture/python', control=str(root))
                tree = ast.parse(source)
                function = next(node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == 'fault')
                fake_os = SimpleNamespace(_exit=Mock(side_effect=SystemExit))
                namespace = {'root': root, 'json': json, 'os': fake_os}
                exec(compile(ast.Module(body=[function], type_ignores=[]), '<fault-wrapper>', 'exec'), namespace)
                (root / 'native-fault.json').write_text(json.dumps({'phase': phase}))
                submit = Mock(return_value=SimpleNamespace(status='success'))
                store = SimpleNamespace(agent=SimpleNamespace(agent_id='owner/dev/agent', thread_id='native'))
                with self.assertRaises(SystemExit):
                    namespace['fault'](submit, ('exact payload',), store, 'original-delivery')
                self.assertEqual(submit.call_count, int(phase == 'after-admission'))
                marker = json.loads((root / 'native-fault-entered.json').read_text())
                self.assertEqual((marker['owner'], marker['session'], marker['delivery_id']),
                                 ('owner/dev/agent', 'native', 'original-delivery'))


if __name__ == '__main__':
    unittest.main()
