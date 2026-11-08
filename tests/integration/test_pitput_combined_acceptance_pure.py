"""Pure artifact/preflight checks. No server, native, database or installed CLI starts."""

import ast
import contextlib
import copy
import hashlib
import io
import json
import tempfile
import threading
import unittest
from datetime import UTC, datetime
from pathlib import Path
from types import ModuleType, SimpleNamespace
from unittest.mock import Mock, patch
from uuid import uuid4
from xml.etree import ElementTree as ET

import pitput_combined_acceptance as acceptance
import pitput_combined_acceptance_support as support
from pitput_combined_acceptance_support import (
    ADMISSION_FAULT_WRAPPER,
    ExpectedOwnedCancellation,
    MatrixProvider,
)


class EvidenceTests(unittest.TestCase):
    def test_failure_observation_is_bounded_and_cannot_replace_primary_assertion(self):
        case = acceptance.FrozenAcceptance('runTest')
        case.thread, case.step = 'thread', 0
        case.leader_done = Mock(return_value=False)
        case.wait = Mock(side_effect=AssertionError('normal long wait forbidden for diagnostics'))
        with tempfile.TemporaryDirectory() as directory:
            case.control = Path(directory)
            with patch.object(acceptance, 'publish_control', side_effect=ValueError('private secret')):
                try:
                    raise AssertionError('mandatory handling failure')
                except AssertionError as primary:
                    result = case.failure_observation()
                    self.assertEqual(str(primary), 'mandatory handling failure')
            self.assertEqual(result['native'], {'status': 'unavailable', 'error_type': 'ValueError'})
            self.assertNotIn('private', json.dumps(result))
            with (patch.object(acceptance, 'publish_control'),
                  patch.object(acceptance.time, 'monotonic', side_effect=[0, 6]),
                  patch.object(acceptance.time, 'sleep', side_effect=AssertionError('deadline ignored'))):
                result = case.failure_observation()
            self.assertEqual(result['native']['status'], 'deadline_or_owner_stopped')
            case.wait.assert_not_called()

    def test_failure_observation_collects_existing_owned_read_before_cleanup(self):
        case = acceptance.FrozenAcceptance('runTest')
        case.thread, case.step = 'thread', 0
        case.leader_done = Mock(return_value=False)
        case.last_transport_observation = {'native_idle': False, 'observation_deferred': True}
        with tempfile.TemporaryDirectory() as directory:
            case.control = Path(directory)

            def publish(path, text):
                self.assertEqual(json.loads(text), {'action': 'failure-observe'})
                result = path.with_name(path.name.replace('command-', 'result-'))
                result.write_text(json.dumps({'thread': 'thread', 'failure_observation': {
                    'status': 'observed', 'thread_status': 'active', 'last_turns': []}}))

            with patch.object(acceptance, 'publish_control', publish):
                result = case.failure_observation()
        self.assertEqual(result['native']['thread_status'], 'active')
        self.assertTrue(result['last_transport']['observation_deferred'])

    def test_provider_reply_and_transport_facts_never_echo_native_payload(self):
        reply = {'type': 'function_call', 'name': 'exec_command', 'call_id': 'private-call',
                 'arguments': '{"cmd":"private command credential"}'}
        facts = support.provider_reply_facts(reply, 'read')
        self.assertEqual((facts['kind'], facts['tool'], facts['operation']),
                         ('tool', 'exec_command', 'read'))
        self.assertFalse(facts['http_write_returned'])
        self.assertNotIn('private', json.dumps(facts))
        observed = support.safe_transport_observation({'native_idle': False,
            'pending': {'delivery': 'notified', 'attempts': 0, 'ack_requested': False,
                        'body': 'private body', 'command': 'private command'},
            'unknown': 'private credential'})
        self.assertEqual(observed['pending']['delivery'], 'notified')
        self.assertNotIn('private', json.dumps(observed))

    def test_case_seven_resolution_call_sites_use_explicit_reason_flag(self):
        tree = ast.parse(Path(acceptance.__file__).read_text())
        method = next(node for node in ast.walk(tree)
                      if isinstance(node, ast.FunctionDef) and node.name == 'stale_and_resolution')
        calls = []
        for node in ast.walk(method):
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == 'pitput':
                words = [value.value for value in node.args
                         if isinstance(value, ast.Constant) and isinstance(value.value, str)]
                if words[:2] in (['ask', 'answer'], ['ask', 'withdraw']):
                    self.assertIn('--reason', words)
                    self.assertNotIn('--text', words)
                    calls.append(words)
        self.assertEqual(len(calls), 3)

    def test_notification_attempt_comes_from_exact_prepared_owned_admission_not_metadata(self):
        # Mirrors the actual Metadata contract: version/origin/event/header only.
        metadata = SimpleNamespace(version=1, origin='service', event={}, header={})
        self.assertFalse(hasattr(metadata, 'attempt'))
        receipt = SimpleNamespace(delivery_id='stable-native-delivery',
                                  presentation=SimpleNamespace(text='exact generated notice'))
        prepared = SimpleNamespace(receipt=receipt, phase='enqueueing', attempt=2)
        self.assertEqual(support.bound_admission_attempt(
            prepared, receipt, 'exact generated notice'), 2)
        for change in ({'attempt': None}, {'attempt': 0}, {'attempt': True},
                       {'phase': 'prepared'}, {'receipt': SimpleNamespace(delivery_id='other')}):
            with self.subTest(change=change):
                invalid = SimpleNamespace(**(vars(prepared) | change))
                with self.assertRaises(ValueError):
                    support.bound_admission_attempt(invalid, receipt, 'exact generated notice')
        with self.assertRaises(ValueError):
            support.bound_admission_attempt(prepared, receipt, 'changed notification')

    def ack_loss_fixture(self):
        delivery = {'delivery_id': 'combined-1', 'source': 'dm', 'attention': {'event_key': 'dm-1'}}
        event = {'event_key': 'combined-1', 'cursor': 'service:session:30', 'seq': 30,
                 'combined': delivery}
        record = {'source': 'dm', 'delivery_id': 'combined-1', 'bridge_delivery_id': 'native-1',
            'payload_sha256': 'payload-digest', 'attempt': 1, 'completed': False,
            'expected_read_sha256': 'read-digest', 'read_sha256': 'read-digest',
            'read_body_utf8_bytes': 42, 'read_json_utf8_bytes': 882,
            'bridge_event_sha256': support.json_digest(event),
            'selected_envelope_sha256': support.json_digest(delivery),
            'ack_command_issued': {'call_id': 'call-exact-ack', 'arguments_sha256': 'command-digest',
                                   'owner': acceptance.OWNER, 'delivery_id': 'combined-1'}}
        record['ack_command_issued']['call_id'] = 'call_' + hashlib.sha256(b'native-1:ack:42').hexdigest()[:24]
        admission = {'status': 'admitted', 'delivery_id': 'native-1', 'payload_sha256': 'payload-digest',
                     'attempt': 1, 'response_ordinal': 49}
        acknowledgment = {'record': {'protocol': 'combined-attention.v1',
            'delivery_id': 'combined-1', 'request_id': 'handle-combined-1',
            'reason': 'Explicit owning-bridge handling acknowledgment', 'status': 'acknowledged'},
            'delivery': delivery}
        return {'record': record, 'phase': 'after-admission',
                'boundary': {'phase': 'after-admission', 'owned_outcome': admission},
                'tombstones': [event], 'acknowledgments': [acknowledgment],
                'owner': acceptance.OWNER}

    def test_lost_ack_requires_positive_exact_chain_without_fabricating_provider_result(self):
        fixture = self.ack_loss_fixture()
        evidence = support.lost_ack_evidence(**fixture)
        self.assertEqual(evidence['status'], 'receipt_lost_reconciled')
        self.assertTrue(evidence['service_ack_observed'])
        self.assertFalse(evidence['provider_receipt_observed'])
        self.assertFalse(fixture['record']['completed'])
        self.assertNotIn('helper_ack', fixture['record'])
        fixture['phase'] = 'queue_response'
        fixture['boundary'] = dict(fixture['boundary']['owned_outcome'], phase='queue_response')
        self.assertEqual(support.lost_ack_evidence(**fixture)['handling_effects'], 1)

    def test_lost_ack_rejects_absence_uncertainty_mismatched_reads_commands_and_receipts(self):
        changes = [
            lambda f: f.update(phase='before-admission'),
            lambda f: f.update(tombstones=[]),
            lambda f: f.update(acknowledgments=[]),
            lambda f: f['acknowledgments'].append(copy.deepcopy(f['acknowledgments'][0])),
            lambda f: f['boundary']['owned_outcome'].update(response_ordinal=0),
            lambda f: f['boundary']['owned_outcome'].update(status='uncertain'),
            lambda f: f['boundary']['owned_outcome'].update(delivery_id='other'),
            lambda f: f['boundary']['owned_outcome'].update(payload_sha256='other'),
            lambda f: f['record'].update(read_sha256='other'),
            lambda f: f['record'].update(read_body_utf8_bytes=0),
            lambda f: f['record'].pop('ack_command_issued'),
            lambda f: f['record']['ack_command_issued'].update(owner='external'),
            lambda f: f['record']['ack_command_issued'].update(call_id='unrelated-actual-call'),
            lambda f: f['record']['ack_command_issued'].update(delivery_id='other'),
            lambda f: f['tombstones'][0].update(cursor='other'),
            lambda f: f['acknowledgments'][0]['record'].update(request_id='other'),
            lambda f: f['acknowledgments'][0]['delivery'].update(source='board'),
        ]
        for change in changes:
            with self.subTest(change=change):
                fixture = self.ack_loss_fixture()
                change(fixture)
                with self.assertRaises(ValueError):
                    support.lost_ack_evidence(**fixture)

    def test_reconciled_retirement_allows_next_expectation_without_completed_or_tool_receipt(self):
        fixture = self.ack_loss_fixture()
        record = fixture['record']
        responder = SimpleNamespace(completed=False, operation='ack', call_id=record['ack_command_issued']['call_id'],
                                    expected=object(), snapshot=object())
        provider = MatrixProvider.__new__(MatrixProvider)
        provider.engine = SimpleNamespace(inner=SimpleNamespace(responder=responder))
        provider.counter = SimpleNamespace(snapshot=lambda: (10, 0))
        provider._active_record = record
        provider._read_parts = [b'validated']
        provider.retire_reconciled_ack(record, support.lost_ack_evidence(**fixture))
        self.assertFalse(record['completed'])
        self.assertNotIn('helper_ack', record)
        self.assertFalse(responder.completed)
        self.assertIsNone(responder.expected)
        self.assertIsNone(responder.call_id)
        self.assertEqual(provider._read_parts, [])
        with self.assertRaises(ValueError):
            provider.retire_reconciled_ack(record, record['receipt_lost_reconciled'])

    def test_policy_fixture_preserves_selection_then_explicitly_migrates_mute(self):
        selection = {'board_path': '/b/exchange', 'dm': 'consume', 'asks': True,
                     'board_filter_json': json.dumps({'path': '/b/exchange', 'after': 17,
                         'mentions': [acceptance.OWNER], 'exclude_authors': [acceptance.OWNER],
                         'threads': [], 'followed_threads': [], 'regex': []})}
        initial = json.dumps(selection, sort_keys=True)
        current = {'version': 2, 'selection': selection}
        preserve = support.policy_transition_payload(current, owner=acceptance.OWNER)
        self.assertEqual(preserve['selection'], selection)
        self.assertNotIn('attention_policy_json', preserve['selection'])
        muted = support.policy_transition_payload(current, owner=acceptance.OWNER, mode='muted')
        self.assertEqual(json.loads(muted['selection']['attention_policy_json']),
                         {'mode': 'muted', 'rules': []})
        spec = json.loads(muted['selection']['board_filter_json'])
        self.assertEqual((spec['mentions'], spec['exclude_authors'], spec['after']), ([], [], 17))
        enabled = support.policy_transition_payload({'version': 3, 'selection': muted['selection']},
                                                   owner=acceptance.OWNER, mode='enabled')
        withdrawn = support.policy_transition_payload(
            {'version': 4, 'selection': enabled['selection']}, owner=acceptance.OWNER,
            action='withdraw')
        self.assertEqual(withdrawn['selection'], enabled['selection'])
        self.assertEqual(withdrawn['pending_action'], 'withdraw')
        self.assertEqual(json.dumps(selection, sort_keys=True), initial)

    def test_control_reader_sees_only_complete_record(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'command.json'

            def interrupted_write(path, text):
                path.write_text('{')
                self.assertFalse(target.exists())
                path.write_text(text)
                path.chmod(0o600)

            with patch.object(support, 'private', interrupted_write):
                support.publish_control(target, '{"action":"observe"}')
            self.assertEqual(json.loads(target.read_text()), {'action': 'observe'})
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_missing_prerequisite_fails_blocked_and_reports_all_nine_without_skip(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'acceptance.xml'
            with (patch.dict(acceptance.os.environ, {}, clear=True),
                  patch.object(acceptance.sys, 'argv', ['acceptance', '--junit', str(output)]),
                  patch.object(acceptance.subprocess, 'run', side_effect=AssertionError('process forbidden')),
                  patch.object(acceptance.subprocess, 'check_output', side_effect=AssertionError('process forbidden')),
                  contextlib.redirect_stderr(io.StringIO())):
                self.assertEqual(acceptance.main(), 2)
            report = json.loads(output.with_suffix('.json').read_text())
            self.assertEqual(report['status'], 'BLOCKED')
            cases = ET.parse(output).getroot().findall('testcase')
            self.assertEqual(len(cases), 9)
            self.assertTrue(all(case.find('failure') is not None for case in cases))
            self.assertFalse(any(case.find('skipped') is not None for case in cases))

    def test_partial_results_cannot_hide_unrun_mandatory_cases(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'partial.xml'
            acceptance.write_reports(output, {'status': 'FAIL', 'cases': {'1': {'status': 'PASS'}}})
            cases = ET.parse(output).getroot().findall('testcase')
            self.assertEqual(len(cases), 9)
            self.assertEqual(sum(case.find('failure') is not None for case in cases), 8)

    def test_overall_cleanup_failure_cannot_publish_all_green_junit(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'cleanup.xml'
            acceptance.write_reports(output, {'status': 'FAIL', 'cases': {
                str(index): {'status': 'PASS'} for index in range(1, 10)
            }})
            suite = ET.parse(output).getroot()
            self.assertEqual(suite.get('failures'), '1')
            self.assertEqual(len(suite.findall('testcase/failure')), 1)

    def test_paused_request_canceled_after_owned_stop_cannot_start_adapter_tools(self):
        provider = MatrixProvider.__new__(MatrixProvider)
        provider.attention_release = Mock()
        provider.attention_release.is_set.return_value = False
        provider.attention_release.wait.return_value = True
        provider.attention_seen = Mock()
        provider.cancel_paused_attention = True
        respond = Mock(side_effect=AssertionError('adapter must not run after owned stop'))
        observed = provider._observe_response(respond)
        with self.assertRaises(ExpectedOwnedCancellation):
            observed({'messages': [{'role': 'user', 'content': 'known generated notice'}]})
        respond.assert_not_called()
        provider.attention_seen.set.assert_called_once()

    def tool_provider(self, *, retry=None, decoded=None):
        provider = MatrixProvider.__new__(MatrixProvider)
        provider._active_record = {'read_json_utf8_bytes': 10, 'completed': False}
        provider._read_parts = [b'already validated']
        provider.tool_diagnostics = []
        provider.ack_retry_authorizer = None
        responder = SimpleNamespace(call_id='call_pending', operation='read', _text=lambda text: text)
        provider.engine = SimpleNamespace(
            inner=SimpleNamespace(responder=responder),
            retry_interrupted_read=Mock(return_value=retry),
            retry_faulted_ack=Mock(return_value=None),
            tool_diagnostic=Mock(return_value={'native_abort_marker': False}),
            read_recovery_events=[{'reason': 'restored_validated_read_result', 'offset': 64}],
            facts=lambda: {'completed': False},
        )
        provider.module = SimpleNamespace(opencode=SimpleNamespace(envelope=SimpleNamespace(
            decode=Mock(return_value=decoded or {'stdout': '{"body_utf8":"new bytes"}'}))))
        return provider

    def test_recovered_read_intent_does_not_count_old_result_as_new_read_or_ack(self):
        provider = self.tool_provider(retry={'type': 'function_call', 'call_id': 'call_retry'})
        respond = Mock(side_effect=AssertionError('old result must not advance common controller'))
        before = list(provider._read_parts)
        reply = provider._observe_response(respond)({'messages': [
            {'role': 'tool', 'tool_call_id': 'call_old', 'content': 'old native result'}]})
        self.assertEqual(reply['call_id'], 'call_retry')
        self.assertEqual(provider._read_parts, before)
        self.assertEqual(provider._active_record['read_json_utf8_bytes'], 10)
        self.assertFalse(provider._active_record['completed'])
        self.assertNotIn('helper_ack', provider._active_record)
        provider.module.opencode.envelope.decode.assert_not_called()
        respond.assert_not_called()

    def test_read_bytes_are_observed_only_after_exact_common_validation(self):
        provider = self.tool_provider()
        request = {'messages': [{'role': 'tool', 'tool_call_id': 'call_pending', 'content': 'frame'}]}
        failed = Mock(side_effect=ValueError('fixture_read_fragment_mismatch'))
        with self.assertRaisesRegex(ValueError, 'fixture_read_fragment_mismatch'):
            provider._observe_response(failed)(request)
        self.assertEqual(provider._read_parts, [b'already validated'])
        self.assertEqual(provider._active_record['read_json_utf8_bytes'], 10)
        provider._observe_response(Mock(return_value={'type': 'function_call'}))(request)
        self.assertEqual(provider._read_parts, [b'already validated', b'new bytes'])
        self.assertEqual(provider._active_record['read_json_utf8_bytes'],
                         10 + len('{"body_utf8":"new bytes"}'.encode()))

    def test_exact_validated_final_read_records_ack_intent_without_ack_result(self):
        for wrong_call in (False, True):
            with self.subTest(wrong_call=wrong_call):
                provider = self.tool_provider()
                provider._active_record.update(source_key='source', delivery_id='combined-1')
                provider.expected = {'source': {'snapshot': SimpleNamespace(body='already validatednew bytes')}}
                responder = provider.engine.inner.responder
                responder.expected = SimpleNamespace(owner=acceptance.OWNER)
                def emit_ack(request, responder=responder, wrong_call=wrong_call):
                    responder.operation = 'ack'
                    responder.call_id = 'exact-issued-ack'
                    return {'type': 'function_call', 'call_id': 'wrong' if wrong_call else responder.call_id,
                            'arguments': '{"command":"fixed installed CLI ACK"}'}
                request = {'messages': [{'role': 'tool', 'tool_call_id': 'call_pending', 'content': 'frame'}]}
                if wrong_call:
                    with self.assertRaisesRegex(ValueError, 'explicit_ack_requires_exact_validated_reads'):
                        provider._observe_response(emit_ack)(request)
                    self.assertNotIn('ack_command_issued', provider._active_record)
                else:
                    provider._observe_response(emit_ack)(request)
                    record = provider._active_record
                    self.assertEqual(record['ack_command_issued']['call_id'], 'exact-issued-ack')
                    self.assertEqual(record['read_sha256'], hashlib.sha256(
                        b'already validatednew bytes').hexdigest())
                    self.assertFalse(record['completed'])
                    self.assertNotIn('helper_ack', record)

    def test_tool_failure_diagnostic_ring_keeps_current_failure_without_payload(self):
        provider = self.tool_provider()
        provider.tool_diagnostics = [{'old': index} for index in range(64)]
        provider.engine.tool_diagnostic.return_value = {
            'native_abort_marker': True, 'call_matches_pending': True,
            'content_utf8_bytes': 7, 'pending_envelope_count': 0,
        }
        provider.module.opencode.envelope.decode.side_effect = ValueError('fixture_helper_failed')
        request = {'messages': [{'role': 'tool', 'tool_call_id': 'call_pending',
                                 'content': 'SECRET TOOL PAYLOAD'}]}
        with self.assertRaisesRegex(ValueError, 'fixture_helper_failed'):
            provider._observe_response(Mock())(request)
        self.assertEqual(len(provider.tool_diagnostics), 64)
        self.assertTrue(provider.tool_diagnostics[-1]['native_abort_marker'])
        self.assertNotIn('SECRET TOOL PAYLOAD', json.dumps(provider.tool_diagnostics))

    def ack_proxy(self, phase):
        proxy = support.AckFaultProxy.__new__(support.AckFaultProxy)
        proxy.lock = threading.Lock()
        proxy.restored, proxy.hit = threading.Event(), threading.Event()
        proxy.hit.set()
        proxy.recovery_consumed = False
        delivery = 'delivery-' + 'a' * 64
        proxy.observation = {'phase': phase, 'delivery_id': delivery,
                             'request_id': 'handle-' + delivery,
                             'forwarded': phase != 'before-ack-commit'}
        if phase != 'before-ack-commit':
            proxy.observation.update(http_status=200, acknowledgment={
                'protocol': 'combined-attention.v1', 'delivery_id': delivery,
                'request_id': 'handle-' + delivery, 'status': 'acknowledged',
            })
        return proxy, delivery

    def test_each_ack_fault_authorizes_once_only_after_readiness_and_exact_intent(self):
        for phase in ('before-ack-commit', 'after-ack-commit', 'lost-ack-response'):
            with self.subTest(phase=phase):
                proxy, delivery = self.ack_proxy(phase)
                with patch.object(proxy.restored, 'wait', return_value=False):
                    with self.assertRaisesRegex(ValueError, 'restore_deadline'):
                        proxy.authorize_failed_ack(delivery)
                self.assertFalse(proxy.recovery_consumed)
                with self.assertRaisesRegex(ValueError, 'fault_target_mismatch'):
                    proxy.authorize_failed_ack('different-delivery')
                proxy.mark_restored(phase)
                self.assertEqual(proxy.authorize_failed_ack(delivery), phase)
                with self.assertRaisesRegex(ValueError, 'authorization_consumed'):
                    proxy.authorize_failed_ack(delivery)

    def test_after_commit_authorization_requires_actual_captured_success(self):
        proxy, delivery = self.ack_proxy('after-ack-commit')
        proxy.observation['http_status'] = 503
        proxy.mark_restored('after-ack-commit')
        with self.assertRaisesRegex(ValueError, 'fault_target_mismatch'):
            proxy.authorize_failed_ack(delivery)
        self.assertFalse(proxy.recovery_consumed)

    def test_faulted_ack_retry_never_records_failed_receipt_as_handling(self):
        provider = self.tool_provider()
        provider.engine.inner.responder.operation = 'ack'
        provider.ack_retry_authorizer = Mock(return_value='before-ack-commit')
        provider.engine.retry_faulted_ack.return_value = {'type': 'function_call', 'call_id': 'retry'}
        provider.engine.ack_recovery_events = [{'phase': 'before-ack-commit',
                                              'ack_receipt_observed': False}]
        respond = Mock(side_effect=AssertionError('failed ACK must not advance responder'))
        result = provider._observe_response(respond)({'messages': [
            {'role': 'tool', 'tool_call_id': 'call_pending', 'content': 'canonical failed ACK'}]})
        self.assertEqual(result['call_id'], 'retry')
        self.assertFalse(provider._active_record['completed'])
        self.assertNotIn('helper_ack', provider._active_record)
        self.assertEqual(provider._active_record['read_json_utf8_bytes'], 10)
        provider.module.opencode.envelope.decode.assert_not_called()
        respond.assert_not_called()

    def test_post_arms_from_authoritative_read_sequence_not_write_receipt_guess(self):
        case = acceptance.FrozenAcceptance('test_frozen_matrix')
        case.cursor = {'service_id': 'service'}
        case.result = {'source_manifest': []}
        case.controller, case.prefix = {}, []
        case.provider = Mock()
        message_path = '/b/exchange/source/47'
        captured = {}

        def remote(env, *args):
            if args[0] == 'create':
                captured['body'] = args[args.index('--body') + 1]
                return {'path': message_path, 'event_key': 'source-event'}  # No seq in write receipt.
            self.assertEqual(args, ('read', message_path))
            captured['exact_read'] = True
            return [{'path': message_path, 'event_key': 'source-event',
                     'seq': 47, 'body': captured['body']}]

        # The standard HRX Python gate has no installed Pitput. This stub tests
        # only fixture source binding, never schema validation or native acceptance.
        package, delivery = ModuleType('ysg_mail'), ModuleType('ysg_mail.attention_delivery')
        package.__path__ = []
        delivery.MessageSnapshot = lambda **fields: SimpleNamespace(**fields)
        with (patch.object(acceptance, 'pitput', remote), patch.dict(acceptance.sys.modules, {
            'ysg_mail': package, 'ysg_mail.attention_delivery': delivery,
        })):
            case.post('source')
        self.assertTrue(captured['exact_read'])
        snapshot = case.provider.arm.call_args.args[2]
        self.assertEqual((snapshot.event_key, snapshot.sequence), ('source-event', 47))
        self.assertEqual(case.result['source_manifest'][0]['sequence'], 47)

    def test_diagnostic_projection_omits_body_policy_text_and_non_json_database_types(self):
        secret = 'untrusted source body must not enter evidence'
        pending = {'delivery_id': 'delivery-id', 'source': 'board', 'body': secret,
                   'selected_at': datetime.now(UTC), 'scope_id': uuid4(),
                   'attention': {'event_key': 'event-id', 'snippet': secret, 'body': secret}}
        projected = acceptance.FrozenAcceptance.source_ids(pending)
        encoded = json.dumps(projected)
        self.assertNotIn(secret, encoded)
        self.assertEqual(projected['attention'], {'event_key': 'event-id'})
        selection = acceptance.FrozenAcceptance.selection_flags({
            'board_path': '/b/exchange', 'dm': 'consume', 'asks': True,
            'reason': secret, 'board_filter_json': json.dumps({
                'path': '/b/exchange', 'after': 0, 'mentions': ['test/combined/agent'],
                'regex': [secret],
            }),
        })
        self.assertNotIn(secret, json.dumps(selection))
        self.assertEqual(selection['board_filter']['regex_count'], 1)

    def test_generated_admission_wrapper_is_valid_python(self):
        ast.parse(ADMISSION_FAULT_WRAPPER)


if __name__ == '__main__':
    unittest.main()
