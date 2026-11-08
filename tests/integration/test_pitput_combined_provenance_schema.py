"""Real publication/persistence schemas with mock SQL only; no service or native I/O."""

import asyncio
import contextlib
import copy
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import pitput_combined_acceptance as acceptance
from pitput_combined_acceptance_support import json_digest, persisted_payload_matches
from ysg_mail import postgres_monitoring
from ysg_mail.attention_delivery import AttentionHeader, render_attention
from ysg_mail.attention_protocol import Admission, Receipt
from ysg_mail.combined_client import Acknowledgment
from ysg_mail.combined_contracts import CombinedDelivery
from ysg_mail.monitoring_native import submission_evidence
from ysg_mail.monitoring_policy import (
    AttentionEvidence,
    Boundary,
    PolicyChange,
    PolicyRevision,
    ReadRange,
    Selection,
    SourceEvent,
    TypedReadEvidence,
    TypedSourceReference,
)
from ysg_mail.monitoring_publication import Publication
from ysg_mail.monitoring_receipts import publication_evidence

OWNER = acceptance.OWNER


def persisted_fixture(source='board'):
    typed = dict(ask_id='ask-' + 'a' * 64, ask_version=1, notice_id='ask-notice-' + 'b' * 64)
    header = AttentionHeader(source=source, sender=acceptance.SENDER,
                             reason={'board': 'mention', 'dm': 'incoming_dm',
                                     'ask': 'addressed_ask'}[source],
                             **(typed if source == 'ask' else
                                {'message': 'dm-aaaaaaaaaaaa', 'conversation': 'dc-aaaaaaaaaaaa'}
                                if source == 'dm' else {}))
    handle = 'at-' + 'a' * 28
    receipt = Receipt(binding='b' * 64, epoch=0, owner=OWNER, handle=handle, header=header,
                      presentation=render_attention(header, handle, owner=OWNER,
                                                    include_protocol=True))
    admission = Admission(receipt=receipt, phase='admitted', attempt=2, admission_epoch=3)
    reference = (TypedSourceReference(service_id='service', event_key=typed['notice_id'],
                                     ask_id=typed['ask_id'], ask_version=1, snapshot_version='version1')
                 if source == 'ask' else SourceEvent(service_id='service', source=source,
                     event_key='source-event', sequence=1, version='immutable'))
    selected = AttentionEvidence(evidence_id='original-selection', policy_version=1,
        event=reference, disposition='selected', actor=OWNER, reason='Original source policy',
        matched_rules=('addressed',))
    publication = Publication(origin='https://service.example', service_id='service',
        monitor_id='monitor', source=source, event_key=reference.event_key, admission=admission)
    native_facts = publication_evidence(publication, owner=OWNER, selection=selected, header=header)
    read = AttentionEvidence(evidence_id='exact-source-read', policy_version=1, event=reference,
        disposition='read', actor=OWNER, reason='Exact source fetch',
        **({'typed_read': TypedReadEvidence(status='current', current_version=1, request_id='read-one')}
           if source == 'ask' else {'read_range': ReadRange(start=1, end=1,
                                                           source_version='immutable')}))
    stored = [selected.model_dump(mode='json')]

    async def execute(sql, values):
        assert sql.startswith('INSERT INTO pitput_attention_evidence')
        stored.append(values[4].obj)

    async def persist():
        connection = SimpleNamespace(execute=execute)
        repository = postgres_monitoring.MonitoringRepository()
        with patch.object(postgres_monitoring, 'PresentationRepository',
                          return_value=SimpleNamespace(put=AsyncMock())):
            for fact in (*native_facts, read):
                await repository._append_fact(connection, OWNER, 'scope', fact,
                                               selection_id=selected.evidence_id)

    asyncio.run(persist())
    submission = submission_evidence(admission)
    body = 'é🙂'.encode()
    digest = hashlib.sha256(body).hexdigest()
    event = {'source': source, 'source_event_key': reference.event_key,
             'delivery_id': 'delivery-' + 'd' * 64, 'source_monitor': 'monitor',
             'native_publication_attempt_id': submission.attempt_id,
             'notification_utf8_bytes': len(receipt.presentation.text.encode()),
             'payload_sha256': native_facts[-1].payload_sha256,
             'read_sha256': digest, 'expected_read_sha256': digest,
             'read_body_utf8_bytes': len(body), 'expected_read_body_utf8_bytes': len(body),
             'read_json_utf8_bytes': 100, 'completed': True}
    return event, selected, stored


class ProvenanceSchemaTests(unittest.TestCase):
    def test_real_persisted_notification_has_hash_and_bytes_not_payload_text(self):
        for source in ('board', 'dm', 'ask'):
            with self.subTest(source=source):
                event, selected, facts = persisted_fixture(source)
                accepted = next(fact for fact in facts
                                if fact['disposition'] == 'notification_accepted')
                self.assertNotIn('payload_utf8', accepted)
                with self.assertRaises(KeyError):
                    _ = accepted['payload_utf8']  # Original final-chain contract failure.
                self.assertEqual(accepted['payload_sha256'], event['payload_sha256'])
                self.assertEqual(accepted['payload_bytes'], event['notification_utf8_bytes'])
                self.assertEqual(accepted['attempt_id'], event['native_publication_attempt_id'])
                self.assertTrue(persisted_payload_matches(accepted, event))

    def test_hash_bytes_and_notification_reference_remain_exact(self):
        for fault in ('hash', 'bytes', 'non-integer-bytes', 'missing-attempt', 'not-accepted'):
            with self.subTest(fault=fault):
                event, selected, facts = persisted_fixture()
                facts = copy.deepcopy(facts)
                accepted = next(fact for fact in facts
                                if fact['disposition'] == 'notification_accepted')
                if fault == 'hash':
                    accepted['payload_sha256'] = 'f' * 64
                elif fault == 'bytes':
                    accepted['payload_bytes'] += 1
                elif fault == 'non-integer-bytes':
                    accepted['payload_bytes'] = str(accepted['payload_bytes'])
                elif fault == 'missing-attempt':
                    del accepted['attempt_id']
                else:
                    accepted['disposition'] = 'notification_attempt'
                if fault == 'missing-attempt':
                    with self.assertRaises(ValueError):
                        persisted_payload_matches(accepted, event)
                else:
                    self.assertFalse(persisted_payload_matches(accepted, event))

    def test_actual_final_chain_checks_real_policy_ack_and_envelope_fields(self):
        event, selected, facts = persisted_fixture()
        policy = PolicyRevision(version=1, change=PolicyChange(request_id='activate',
            expected_version=0, actor=OWNER, reason='Original policy',
            selection=Selection(board_path='/b/exchange', dm='consume', asks=True)),
            boundary=Boundary(service_id='service', board_after=0, dm_after=0, ask_after=0))
        envelope = CombinedDelivery(source='board', source_monitor='monitor',
            delivery_id=event['delivery_id'], attention={'event_key': event['source_event_key']})
        acknowledged = Acknowledgment(protocol='combined-attention.v1',
            delivery_id=envelope.delivery_id, request_id='exact-ack', reason='Handled exact source',
            status='acknowledged')
        event['selected_envelope_sha256'] = json_digest(envelope.model_dump(mode='json'))

        def execute(sql, values):
            rows = ([{'scope_id': 'scope', 'policy_version': 1, 'record': selected.model_dump(mode='json')}]
                    if 'FROM pitput_attention_selections' in sql else
                    [{'record': policy.model_dump(mode='json')}]
                    if 'FROM pitput_monitor_policy_revisions' in sql else
                    [{'record': fact} for fact in facts]
                    if 'FROM pitput_attention_evidence' in sql else
                    [{'record': acknowledged.model_dump(mode='json'),
                      'delivery': envelope.model_dump(mode='json')}])
            return SimpleNamespace(fetchall=lambda: rows, fetchone=lambda: rows[0])

        @contextlib.contextmanager
        def database():
            yield SimpleNamespace(execute=execute)

        case = acceptance.FrozenAcceptance('runTest')
        case.provider, case.result = SimpleNamespace(records=[event]), {}
        case.database = database
        case.wait = lambda condition, *args, **kwargs: self.assertTrue(condition())
        with tempfile.TemporaryDirectory() as directory:
            case.spool = Path(directory)
            (case.spool / 'monitor-publications.json').write_text(json.dumps({
                'entries': [], 'acknowledged': [{'attribution': 'known'}]}))
            case.provenance_chain()
        self.assertEqual(case.result['provenance_chain'][0]['ack_request_id'], 'exact-ack')
        self.assertEqual(case.result['provenance_chain'][0]['handling_effects'], 1)


if __name__ == '__main__':
    unittest.main()
