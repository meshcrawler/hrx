import importlib.util
import pathlib
import types
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("yesod_register", pathlib.Path(__file__).with_name("yesod_register.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class RegistrationTests(unittest.TestCase):
    def test_new_identity_checks_roster_without_mutation(self):
        with mock.patch.object(module, "command", return_value='[]') as command:
            with mock.patch.dict(module.os.environ, {"YESOD_MAIL_IDENTITY": "parent/agent/name"}):
                agent = module.allocate("codex", "glm-5p3")
        self.assertIn("/hrx-codex/", agent)
        self.assertEqual(command.call_args_list[0].args[0], ["roster", "--json"])
        self.assertEqual(command.call_count, 1)
        self.assertNotIn("YESOD_MAIL_IDENTITY", command.call_args_list[0].args[1])

    def test_collision_retries_without_reusing_existing_address(self):
        occupied = '[{"addr":"laptop/hrx-codex/project-first"}]'
        with mock.patch.object(module, "command", side_effect=[occupied, '', '']), mock.patch.object(module.socket, "gethostname", return_value="laptop"), mock.patch.object(module.os, "getcwd", return_value="/project"), mock.patch.object(module.uuid, "uuid4", side_effect=[types.SimpleNamespace(hex="first"), types.SimpleNamespace(hex="second")]):
            self.assertEqual(module.allocate("codex", "model"), "laptop/hrx-codex/project-second")

    def test_resume_selects_inactive_identity_without_mutation(self):
        with mock.patch.object(module, "command", return_value='[]') as command:
            self.assertEqual(module.allocate("claude", "model", "host/hrx-claude/session"), "host/hrx-claude/session")
        self.assertEqual(command.call_count, 1)

    def test_active_identity_selection_defers_ownership_to_atomic_claim(self):
        with mock.patch.object(module, "command", return_value='[{"addr":"host/hrx-codex/old"}]') as command:
            self.assertEqual(module.allocate("codex", "model", "host/hrx-codex/old"), "host/hrx-codex/old")
        self.assertEqual(command.call_count, 1)

    def test_malformed_roster_never_registers(self):
        for result in ('{}', '[{}]', 'not json'):
            with self.subTest(result=result), mock.patch.object(module, "command", return_value=result) as command:
                with self.assertRaises((RuntimeError, ValueError)):
                    module.allocate("opencode", "model")
                self.assertEqual(command.call_count, 1)


if __name__ == "__main__":
    unittest.main()
