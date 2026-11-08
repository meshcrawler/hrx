"""Launch policy checks without native processes, PTYs or database fixtures."""
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from conversation import Agent


class ConversationCommandTests(unittest.TestCase):
    def test_restart_after_unclean_stop_is_refused_before_allocating_resources(self):
        for stops in ([False], [True, False], [False, True]):
            with self.subTest(stops=stops), patch("conversation.sys.platform", "linux"), \
                    patch("conversation.pty.openpty") as terminal, \
                    patch("conversation.subprocess.Popen") as process:
                agent = self.agent("claude")
                agent.clean_stops = stops
                with self.assertRaisesRegex(RuntimeError, "cleanup was not proven clean"):
                    agent.start("continue", resume=True)
                terminal.assert_not_called()
                process.assert_not_called()

    def agent(self, harness):
        agent = Agent.__new__(Agent)
        agent.runner = SimpleNamespace(hrx="/test/hrx")
        agent.cell = {"harness": harness, "route": "test-route", "reasoning": None}
        agent.identity = "test/conversation/agent"
        agent.session_id = "saved-session"
        agent.env = {"CLAUDE_CONFIG_DIR": "/isolated/home/claude"}
        return agent

    def test_fresh_claude_uses_product_initialization_before_native_arguments(self):
        args = self.agent("claude").command("challenge")
        hrx_args, native_args = args[:args.index("--")], args[args.index("--") + 1:]
        self.assertEqual(hrx_args[hrx_args.index("--claude-config-dir") + 1],
                         "/isolated/home/claude")
        self.assertEqual(hrx_args[hrx_args.index("--claude-init") + 1], "theme,trust,bypass")
        self.assertEqual(native_args, ["challenge"])

    def test_claude_resume_restores_saved_configuration_without_initializing(self):
        agent = self.agent("claude")
        agent.env = {}
        args = agent.command("continue", resume=True)
        self.assertEqual(args[args.index("--resume") + 1], "saved-session")
        self.assertIn("--no-yolo", args)
        self.assertNotIn("--claude-init", args)
        self.assertNotIn("--claude-config-dir", args)

    def test_other_harnesses_do_not_receive_claude_configuration(self):
        for harness in ("codex", "opencode"):
            with self.subTest(harness=harness):
                agent = self.agent(harness)
                agent.env = {}
                args = agent.command("challenge")
                self.assertNotIn("--claude-init", args)
                self.assertNotIn("--claude-config-dir", args)
                self.assertEqual(args[args.index("--") + 1:],
                                 ["--prompt", "challenge"] if harness == "opencode" else ["challenge"])
