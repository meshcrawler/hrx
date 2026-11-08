"""Pure tests against installed Pitput's real policy/floor schema; no service I/O.

Run with the same Pitput source/wheel on PYTHONPATH as the native fixture. This
module requires those contracts intentionally; it never replaces them with mocks.
"""

import copy
import json
import unittest

from pydantic import ValidationError
from pitput_combined_acceptance_support import policy_transition_payload, policy_transition_selection
from ysg_mail.attention_floor import FloorContext, explain_floor
from ysg_mail.attention_selection import match_board
from ysg_mail.board_filters import MonitorSpec
from ysg_mail.monitoring_policy import PolicyChange, Selection
from ysg_mail.typed_asks import AskScope

OWNER = 'test/combined/agent'
SENDER = 'test/controller/agent'


def legacy_selection():
    spec = MonitorSpec(path='/b/exchange', mentions=[OWNER], exclude_authors=[OWNER])
    return Selection(board_path=spec.path, board_filter_json=spec.model_dump_json(),
                     dm='consume', asks=True).model_dump(mode='json')


class PolicySchemaTests(unittest.TestCase):
    def test_original_fixture_payload_reproduces_service_validation_error(self):
        invalid = {**legacy_selection(), 'attention_policy_json': json.dumps({'mode': 'enabled'})}
        with self.assertRaisesRegex(ValidationError, 'attention_rules_cannot_mix_legacy_filters'):
            Selection.model_validate(invalid)

    def test_preserve_mute_unmute_withdraw_use_valid_payloads_without_actor(self):
        initial = legacy_selection()
        current = {'version': 2, 'selection': initial}
        for mode, action in [(None, 'preserve'), ('muted', 'preserve'),
                             ('enabled', 'preserve'), (None, 'withdraw')]:
            before = copy.deepcopy(current)
            payload = policy_transition_payload(current, owner=OWNER, mode=mode, action=action)
            self.assertEqual(current, before)
            self.assertNotIn('actor', payload)  # Authentication supplies actor at the service.
            parsed = PolicyChange.model_validate({**payload, 'actor': OWNER})
            self.assertEqual(parsed.pending_action, action)
            self.assertEqual(parsed.expected_version, current['version'])
            selection = Selection.model_validate(payload['selection'])
            self.assertEqual((selection.board_path, selection.dm, selection.asks),
                             ('/b/exchange', 'consume', True))
            if mode is None:
                self.assertEqual(payload['selection'], current['selection'])
            else:
                self.assertEqual(selection.attention_policy().mode, mode)
                self.assertEqual(selection.attention_policy().rules, ())
                self.assertEqual(selection.delivery_enabled(), mode == 'enabled')
            current = {'version': current['version'] + 1, 'selection': payload['selection']}
        self.assertNotIn('attention_policy_json', initial)

    def test_floor_migration_preserves_targeted_board_matching(self):
        legacy = Selection.model_validate(legacy_selection())
        floor = Selection.model_validate(policy_transition_selection(
            legacy_selection(), owner=OWNER, mode='enabled'))
        for author, title, body, selected in [
            (SENDER, 'ordinary', 'ordinary shared traffic', False),
            (SENDER, 'ordinary', 'HOLD: unrelated control', False),
            (SENDER, 'ordinary', '@' + OWNER + ' addressed input', True),
            (SENDER, '@' + OWNER.upper(), 'addressed title', True),
            (OWNER, 'ordinary', '@' + OWNER + ' self-authored', False),
            (SENDER, 'ordinary', '@' + OWNER + '-other near match', False),
            (SENDER, 'ordinary', '@' + OWNER + '/other near match', False),
            (SENDER, 'ordinary', 'prefix@' + OWNER + ' near match', False),
        ]:
            with self.subTest(author=author, title=title, body=body):
                item = {'path': '/b/exchange/topic/1', 'event_type': 'post',
                        'author': author, 'title': title, 'body': body}
                self.assertEqual(bool(match_board(legacy, OWNER, item)), selected)
                self.assertEqual(bool(match_board(floor, OWNER, item)), selected)

    def test_incoming_dm_and_unresolved_ask_floor_remain_addressed(self):
        policy = Selection.model_validate(policy_transition_selection(
            legacy_selection(), owner=OWNER, mode='enabled')).attention_policy()
        dm = FloorContext(scope=AskScope(source='dm', resource='dc-aaaaaaaaaaaa',
                                        participants=tuple(sorted((OWNER, SENDER)))),
                          kind='dm', author=SENDER, recipient=OWNER)
        ask = FloorContext(scope=AskScope(source='board', resource='/b/exchange/topic'),
                           kind='ask', author=SENDER, recipient=OWNER, ask_status='open')
        for event in (dm, ask):
            self.assertTrue(explain_floor(policy, event, owner=OWNER, authorized=True).selected)
            muted = policy.model_copy(update={'mode': 'muted'})
            self.assertFalse(explain_floor(muted, event, owner=OWNER, authorized=True).selected)

    def test_unknown_legacy_selectors_are_not_discarded(self):
        for field, value in [('regex', ['important']), ('followed_threads', ['/b/exchange/topic']),
                             ('mentions', ['another/recipient/agent']), ('exclude_authors', [])]:
            selection = legacy_selection()
            spec = json.loads(selection['board_filter_json'])
            spec[field] = value
            selection['board_filter_json'] = json.dumps(spec)
            with self.assertRaisesRegex(ValueError, 'requires_exact_owner_target'):
                policy_transition_selection(selection, owner=OWNER, mode='muted')


if __name__ == '__main__':
    unittest.main()
