"""Real adapter + canonical receipts, no provider/process/helper/service executes."""

import copy
import hashlib
import importlib.util
import json
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace

import ysg_mail
from pitput_combined_acceptance_support import MatrixProvider, OwnedReadResumeWitness
from ysg_mail.attention_delivery import AttentionHeader, MessageSnapshot, read_fragment, render_attention
from ysg_mail.attention_protocol import Receipt

spec = importlib.util.spec_from_file_location('read_resume_adapter',
    Path(ysg_mail.__file__).resolve().parents[2] / 'scripts/compact_codex_adapter.py')
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


def setup():
    header = AttentionHeader(source='dm', sender='test/controller/agent', reason='incoming_dm',
        message='dm-aaaaaaaaaaaa', conversation='dc-aaaaaaaaaaaa',
        combined_delivery_id='delivery-' + 'd' * 64)
    handle, owner = 'at-' + 'a' * 28, 'test/combined/agent'
    receipt = Receipt(binding='b' * 64, epoch=0, owner=owner, handle=handle, header=header,
        presentation=render_attention(header, handle, owner=owner, include_protocol=True))
    snapshot = MessageSnapshot(service_id='service', source='dm', handle=header.message,
        event_key='source-event', sequence=12, sender=header.sender, body='é🙂 trailing HOLD. ' * 20)
    engine = adapter.CodexCompactAdapter('/fixture', body_byte_limit=64)
    engine.inner.expect(receipt, snapshot)
    request = adapter.normalize({'input': [{'type': 'message', 'role': 'user',
        'content': receipt.presentation.text}], 'tools': [{'type': 'function', 'name': 'exec_command',
        'parameters': {'properties': {key: {} for key in
            ('cmd', 'workdir', 'yield_time_ms', 'max_output_tokens')}}}]})
    first = engine.respond(request)
    record = {'delivery_id': receipt.delivery_id, 'bridge_delivery_id': receipt.delivery_id,
              'payload_sha256': hashlib.sha256(receipt.presentation.text.encode()).hexdigest(),
              'attempt': 1, 'session': 'owned-thread', 'source_key': 'source-event',
              'read_json_utf8_bytes': 0, 'completed': False}
    boundary = {'phase': 'after-admission', 'helper_invoked': True, 'helper_exit': 0,
        'owned_outcome': {'status': 'admitted', 'attempt': 1, 'delivery_id': receipt.delivery_id,
                         'payload_sha256': record['payload_sha256'], 'response_ordinal': 49}}
    arguments = dict(phase='after-admission', boundary=boundary,
        stopped={'owner_stopped': True, 'thread': record['session']}, record=record,
        receipt=receipt, pending=first['call_id'], offset=0, validated_read_bytes=0, operation='read')
    return SimpleNamespace(**locals())


class ReadResumeSchemaTests(unittest.TestCase):
    def test_exact_user_last_pending_read_repro_and_witnessed_new_physical_read(self):
        f = setup()
        self.assertIsNone(f.engine.retry_interrupted_read(f.request))
        with self.assertRaisesRegex(ValueError, 'fixture_tool_result_still_pending'):
            f.engine.respond(f.request)  # Exact7357 shape: no function-call output exists.
        provider = MatrixProvider.__new__(MatrixProvider)
        provider.engine, provider._active_record = f.engine, f.record
        provider._read_parts, provider.seen = [], [f.receipt]
        provider.expected = {'source-event': {'snapshot': f.snapshot}}
        provider.module = adapter
        provider.tool_diagnostics = []
        provider.ack_retry_authorizer = None
        provider.attention_release = threading.Event()
        provider.attention_release.set()
        provider.arm_read_resume('after-admission', f.boundary, f.arguments['stopped'])
        observed = provider._observe_response(f.engine.respond)
        retried = observed(f.request)
        self.assertNotEqual(retried['call_id'], f.first['call_id'])
        responder = f.engine.inner.responder
        self.assertEqual((responder.offset, responder.fragments, provider._read_parts), (0, 0, []))
        self.assertFalse(responder.completed)
        self.assertNotIn('helper_ack', f.record)
        self.assertEqual(len(provider.seen), 1)  # No second notification/expectation.
        fragment = read_fragment(f.snapshot, body_byte_limit=64)
        tool_request = {**f.request, 'messages': [{'role': 'tool', 'tool_call_id': retried['call_id'],
            'content': adapter.opencode.envelope.encode(retried['call_id'], 0,
                                                       fragment.model_dump_json())}]}
        observed(tool_request)
        self.assertEqual(responder.offset, fragment.end_byte)
        self.assertEqual(provider._read_parts, [fragment.body_utf8.encode()])
        self.assertFalse(responder.completed)
        self.assertNotIn('helper_ack', f.record)

    def test_queue_response_witness_and_real_adapter_single_read_guard(self):
        f = setup()
        arguments = dict(f.arguments)
        arguments['phase'] = 'queue_response'
        arguments['boundary'] = {**f.boundary['owned_outcome'], 'phase': 'queue_response',
                                 'thread_id': f.record['session']}
        witness = OwnedReadResumeWitness(**arguments)
        reply = f.engine.retry_interrupted_read(f.request, authorize_user_replay=witness)
        self.assertNotEqual(reply['call_id'], f.first['call_id'])
        with self.assertRaisesRegex(ValueError, 'owned_read_resume_witness_mismatch'):
            f.engine.retry_interrupted_read(f.request, authorize_user_replay=witness)
        for operation in ('ack', 'reply'):
            with self.subTest(operation=operation):
                f = setup()
                f.engine.inner.responder.operation = operation
                def denied(*unused):
                    self.fail('non-READ must never consult recovery authorization')
                self.assertIsNone(f.engine.retry_interrupted_read(
                    f.request, authorize_user_replay=denied))

    def test_missing_wrong_boundary_stop_offset_or_operation_cannot_arm(self):
        for fault in ('missing-stop', 'wrong-thread', 'not-admitted', 'ordinal', 'delivery',
                      'digest', 'unknown-offset', 'offset', 'ack', 'reply', 'unknown-boundary'):
            with self.subTest(fault=fault):
                f = setup()
                arguments = copy.deepcopy(f.arguments)
                if fault == 'missing-stop':
                    arguments['stopped'] = {}
                elif fault == 'wrong-thread':
                    arguments['stopped']['thread'] = 'foreign-thread'
                elif fault == 'not-admitted':
                    arguments['boundary']['owned_outcome']['status'] = 'uncertain'
                elif fault == 'ordinal':
                    arguments['boundary']['owned_outcome']['response_ordinal'] = 0
                elif fault == 'delivery':
                    arguments['boundary']['owned_outcome']['delivery_id'] = 'foreign-delivery'
                elif fault == 'digest':
                    arguments['boundary']['owned_outcome']['payload_sha256'] = 'f' * 64
                elif fault == 'unknown-offset':
                    arguments['offset'] = None
                elif fault == 'offset':
                    arguments['offset'] = 1
                elif fault in ('ack', 'reply'):
                    arguments['operation'] = fault
                else:
                    arguments['phase'] = 'before-admission'
                with self.assertRaisesRegex(ValueError, 'owned_read_resume_witness_invalid'):
                    OwnedReadResumeWitness(**arguments)

    def test_repeated_changed_cross_delivery_or_physical_call_replay_is_denied(self):
        for fault in ('repeated', 'text', 'receipt', 'call', 'offset'):
            with self.subTest(fault=fault):
                f = setup()
                witness = OwnedReadResumeWitness(**f.arguments)
                receipt, pending, offset, text = f.receipt, f.first['call_id'], 0, f.receipt.presentation.text
                if fault == 'repeated':
                    witness(receipt, pending, offset, text)
                elif fault == 'text':
                    text += ' changed'
                elif fault == 'receipt':
                    receipt = receipt.model_copy(update={'binding': 'f' * 64})
                elif fault == 'call':
                    pending = 'unknown-call'
                else:
                    offset = 1
                with self.assertRaisesRegex(ValueError, 'owned_read_resume_witness_mismatch'):
                    witness(receipt, pending, offset, text)


if __name__ == '__main__':
    unittest.main()
