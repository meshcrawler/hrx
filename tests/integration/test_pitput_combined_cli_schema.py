"""Real parser/request-model bindings for cases seven/eight; all I/O is mocked."""

import contextlib
import copy
import io
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch
from urllib.parse import parse_qs

import pitput_combined_acceptance as acceptance
from ysg_mail import ask_cli
from ysg_mail.agent_enrollment import normalize_boards
from ysg_mail.ask_notice_requests import NoticeScanRequest
from ysg_mail.board_filters import MonitorSpec
from ysg_mail.combined_contracts import (
    CombinedAckRequest,
    CombinedPendingRequest,
    CombinedReadRequest,
)
from ysg_mail.pitput import parser
from ysg_mail.typed_asks import ChangeAsk


class FixtureCLIContractTests(unittest.TestCase):
    def test_answer_text_flag_reproduces_parser_error(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
            parser().parse_args(['ask', 'answer', 'ask-' + 'a' * 64, '--expected-version', '1',
                                 '--text', 'Old invalid fixture flag', '--request-id', 'close-unseen'])
        self.assertEqual(error.exception.code, 2)

    def test_actual_case_seven_commands_parse_and_build_typed_changes(self):
        case = acceptance.FrozenAcceptance('runTest')
        ids = ['ask-' + character * 64 for character in 'abc']
        case.controller, case.prefix = {}, ['--agent', acceptance.SENDER]
        case.command, case.post, case.arm_ask = Mock(), Mock(return_value={}), Mock()
        case.ask = Mock(side_effect=[{'ask_id': value} for value in ids])
        case.provider = SimpleNamespace(expected={(ids[0], 1): {}, (ids[1], 1): {}}, records=[])
        case.oracle = SimpleNamespace(case_seven=Mock())

        def handled(labels):
            case.provider.records.extend({'label': label} for label in labels)

        case.await_handled = handled
        changes = []

        def command(environment, *arguments):
            args = parser().parse_args(list(arguments))
            self.assertEqual(args.agent, acceptance.SENDER)

            def request(method, path, **kwargs):
                if path == '/v1/identity':
                    return {'capabilities': ['typed-asks.v1']}
                self.assertEqual((method, path), ('POST', '/v1/asks/' + args.ask_id + '/events'))
                changes.append(ChangeAsk.model_validate(kwargs['json']))
                return {'accepted': True}

            ask_cli.run(SimpleNamespace(request=request), args)

        with patch.object(acceptance, 'pitput', command):
            case.stale_and_resolution()
        self.assertEqual([(change.kind, change.expected_version, change.text, change.request_id)
                          for change in changes], [
            ('answer', 1, 'Closed before selection.', 'close-unseen'),
            ('withdraw', 1, 'Withdraw before selection.', 'supersede-unseen'),
            ('answer', 2, 'Engaged resolution remains visible.', 'engaged-answer'),
        ])

    def test_actual_case_eight_join_takeovers_and_privacy_payloads_match_contracts(self):
        case = acceptance.FrozenAcceptance('runTest')
        original = {'source': 'dm', 'delivery_id': 'delivery-' + 'd' * 64}
        source = {'message': 'dm-aaaaaaaaaaaa', 'event_key': 'private-event'}
        initial_spec = MonitorSpec(path='/b/exchange', dm='consume', dm_session='a' * 32,
                                   mentions=[acceptance.OWNER], exclude_authors=[acceptance.OWNER])
        initial_consumer = {'session': 'a' * 32, 'generation': 1}
        case.cursor = {'spec': initial_spec.model_dump(), 'consumer': initial_consumer}
        case.spool, case.token, case.viewer_token = Path('/mock/spool'), 'owner-token', 'viewer-token'
        case.command, case.await_handled, case.wait = Mock(), Mock(), Mock()
        case.dm, case.scan = Mock(return_value=source), Mock(return_value={'delivery': original})
        case.agent_env = Mock(return_value={'HOME': '/mock/outsider'})
        case.oracle = SimpleNamespace(case_eight=Mock())
        state = {'revoked': False, 'generation': 1}
        puts, probes, joins = [], [], []

        @contextlib.contextmanager
        def database():
            def execute(sql, values):
                if 'SET revoked_at=clock_timestamp()' in sql:
                    state['revoked'] = True
                elif 'SET revoked_at=NULL' in sql:
                    state['revoked'] = False
                return SimpleNamespace(fetchone=lambda: {'n': 0})
            yield SimpleNamespace(execute=execute)

        case.database = database

        def request(operation, *, method='POST', payload=None, token=None, allow_denied=False):
            if method == 'PUT':
                spec = MonitorSpec.model_validate(payload)
                reason = parse_qs(operation.lstrip('?'))['dm_takeover_reason'][0]
                self.assertTrue(reason.strip())
                self.assertLessEqual(len(reason), 256)
                self.assertEqual((spec.path, spec.dm), ('/b/exchange', 'consume'))
                puts.append(spec)
                state['generation'] += 1
                return SimpleNamespace(json=lambda: {'consumer': {
                    'session': spec.dm_session, 'generation': state['generation']}})
            endpoint = operation.split('/')[-1]
            model = {'scan': NoticeScanRequest, 'read': CombinedReadRequest,
                     'pending': CombinedPendingRequest, 'ack': CombinedAckRequest}[endpoint]
            parsed = model.model_validate(payload)
            probes.append((endpoint, copy.deepcopy(payload)))
            if endpoint != 'scan':
                self.assertEqual(parsed.delivery_id, original['delivery_id'])
            self.assertTrue(allow_denied)
            status = (401 if state['revoked'] else 404 if token in ('outsider-token', 'viewer-token')
                      else 409)
            return SimpleNamespace(status_code=status, text='')

        case.request = request

        def command(environment, *arguments):
            args = parser().parse_args(list(arguments))
            joins.append(args)
            self.assertEqual(args.command, 'join')
            self.assertEqual(args.identity, 'test/outsider/agent')
            self.assertEqual(normalize_boards(args.boards), ['exchange'])

        with (patch.object(acceptance, 'pitput', command),
              patch.object(acceptance, 'private'),
              patch.object(Path, 'read_text', return_value='outsider-token')):
            case.privacy()
        self.assertEqual(len(joins), 1)
        self.assertEqual(len(puts), 2)
        self.assertNotEqual(puts[0].dm_session, initial_spec.dm_session)
        self.assertEqual(puts[1], initial_spec)
        self.assertEqual(len(probes), 16)
        self.assertTrue(all(payload['consumer'] == initial_consumer for _, payload in probes[:4]))
        self.assertTrue(all(payload['consumer']['generation'] == 2 for _, payload in probes[4:]))
        self.assertEqual([endpoint for endpoint, _ in probes[:4]], ['read', 'scan', 'pending', 'ack'])


if __name__ == '__main__':
    unittest.main()
