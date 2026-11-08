"""Pure monitor-wait contracts: fake CLI/clock, no process, network or database."""

import importlib.util
import json
import subprocess
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location(
    "pitput_exchange_wait_fixture",
    Path(__file__).parents[1] / "integration" / "pitput_exchange.py",
)
exchange = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(exchange)

MONITOR = ["--agent", "test/controller/agent", "monitor", "/b/exchange/durable-monitor",
           "--mention", "test/controller/agent", "--cursor-file", "/fixture/cursor.json",
           "--timeout", "0.2", "--once"]
EVENT = {
    "seq": 2, "event_key": "fixture-event", "path": "/b/exchange/durable-monitor/2",
    "author": "test/controller/agent", "reason": ["mention:test/controller/agent"],
    "cursor": "fixture-bound-exact-cursor", "trust": "untrusted_peer_content",
}


class Clock:
    def __init__(self):
        self.now = 0.0

    def read(self):
        return self.now

    def pause(self, seconds):
        self.now += seconds


def result(value=None, *, code=0, raw=None):
    return SimpleNamespace(returncode=code,
                           stdout=raw if raw is not None else json.dumps(value),
                           stderr="secret-bearing diagnostic must not be rendered")


class MonitorDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        # A missed mock is a test failure, never an accidental native/DB/process run.
        guard = patch.object(exchange.subprocess, "run",
                             side_effect=AssertionError("no real process allowed"))
        guard.start()
        self.addCleanup(guard.stop)
        self.env = {"HOME": "/isolated/fixture", "SECRET": "not-for-diagnostics"}

    def wait(self, execute, **kwargs):
        return exchange.wait_for_monitor_event(
            self.env, MONITOR, execute=execute, clock=self.clock.read,
            pause=self.clock.pause, **kwargs,
        )

    def test_first_quiet_polls_then_exact_event_without_ack_or_spec_change(self):
        replies = iter([result(raw=""), result(), result(EVENT)])
        calls = []

        def execute(env, *args, **kwargs):
            calls.append((env, args, kwargs))
            self.clock.now += 0.25
            return next(replies)

        self.assertEqual(self.wait(execute), EVENT)
        self.assertEqual(len(calls), 3)
        for env, args, kwargs in calls:
            self.assertIs(env, self.env)
            self.assertEqual(args, tuple(MONITOR))
            self.assertFalse(kwargs["check"])
            self.assertNotIn("--ack", args)
        self.assertEqual([round(call[2]["timeout"], 2) for call in calls],
                         [10.0, 9.7, 9.4])

    def test_all_quiet_polls_fail_at_overall_deadline_not_pass(self):
        calls = []

        def execute(env, *args, timeout, **kwargs):
            calls.append(timeout)
            self.clock.now += min(0.2, timeout)
            return result()

        with self.assertRaises(exchange.MonitorDeliveryError) as failure:
            self.wait(execute, wait_seconds=1.0)
        diagnostics = failure.exception.diagnostics
        self.assertEqual(diagnostics["reason"], "delivery_deadline")
        self.assertEqual(diagnostics["polls"], len(calls))
        self.assertGreater(diagnostics["polls"], 1)
        self.assertEqual(diagnostics["elapsed_seconds"], 1.0)
        self.assertTrue(all(0 < value <= 1 for value in calls))

    def test_late_event_is_not_allowed_past_overall_budget(self):
        def execute(*args, **kwargs):
            self.clock.now = 1.1
            return result(EVENT)

        with self.assertRaisesRegex(exchange.MonitorDeliveryError, "delivery_deadline"):
            self.wait(execute, wait_seconds=1.0)

    def test_subprocess_timeout_is_immediate_not_retried_and_redacted(self):
        execute = Mock(side_effect=subprocess.TimeoutExpired(
            ["private-command"], 1, output="secret output", stderr="secret stderr",
        ))
        with self.assertRaises(exchange.MonitorDeliveryError) as failure:
            self.wait(execute)
        self.assertEqual(failure.exception.diagnostics["reason"], "command_timeout")
        self.assertEqual(failure.exception.diagnostics["polls"], 1)
        self.assertNotIn("secret", str(failure.exception))
        self.assertNotIn("private-command", str(failure.exception))
        execute.assert_called_once()

    def test_nonzero_exit_fails_immediately_without_exposing_stdout_stderr(self):
        execute = Mock(return_value=result(code=2, raw="secret token/body"))
        with self.assertRaises(exchange.MonitorDeliveryError) as failure:
            self.wait(execute)
        self.assertEqual(failure.exception.diagnostics["reason"], "command_failed")
        self.assertEqual(failure.exception.diagnostics["exit_code"], 2)
        self.assertNotIn("secret", str(failure.exception))
        self.assertEqual(execute.call_count, 1)

    def test_command_exception_fails_immediately_with_class_only(self):
        execute = Mock(side_effect=OSError("secret /private/path"))
        with self.assertRaises(exchange.MonitorDeliveryError) as failure:
            self.wait(execute)
        self.assertEqual(failure.exception.diagnostics["error_class"], "OSError")
        self.assertNotIn("private", str(failure.exception))
        self.assertEqual(execute.call_count, 1)

    def test_malformed_json_is_not_a_quiet_poll(self):
        execute = Mock(return_value=result(raw="secret invalid-json"))
        with self.assertRaisesRegex(exchange.MonitorDeliveryError, "invalid_event_json") as failure:
            self.wait(execute)
        self.assertNotIn("secret", str(failure.exception))
        execute.assert_called_once()

    def test_malformed_metadata_is_not_a_quiet_poll(self):
        for event in ({}, [], "secret", {"cursor": "secret"},
                      {**EVENT, "seq": True}, {**EVENT, "cursor": ""},
                      {**EVENT, "trust": "trusted"}, {**EVENT, "body": "secret"},
                      {**EVENT, "reason": []}):
            with self.subTest(kind=type(event).__name__):
                execute = Mock(return_value=result(event))
                with self.assertRaisesRegex(exchange.MonitorDeliveryError,
                                            "invalid_event_schema") as failure:
                    self.wait(execute)
                self.assertNotIn("secret", str(failure.exception))
                execute.assert_called_once()

    def test_ack_command_is_never_accepted_as_positive_wait(self):
        for ack in ("--ack", "--ack=secret-cursor"):
            execute = Mock()
            with self.assertRaisesRegex(exchange.MonitorDeliveryError,
                                        "invalid_positive_monitor_spec"):
                exchange.wait_for_monitor_event(self.env, [*MONITOR, ack], execute=execute)
            execute.assert_not_called()

    def test_oversized_output_and_noninteger_status_are_not_retried(self):
        for reply in (result(raw="s" * 65537),
                      result(raw="\ud800"),
                      SimpleNamespace(returncode=True, stdout="{}", stderr="secret"),
                      SimpleNamespace(returncode=0, stdout=None, stderr="secret")):
            execute = Mock(return_value=reply)
            with self.assertRaises(exchange.MonitorDeliveryError):
                self.wait(execute)
            execute.assert_called_once()

    def test_nonmonotonic_clock_cannot_extend_deadline(self):
        def execute(*args, **kwargs):
            self.clock.now = -1
            return result(EVENT)

        with self.assertRaisesRegex(exchange.MonitorDeliveryError, "invalid_clock"):
            self.wait(execute)

    def test_invalid_wait_budget_starts_nothing(self):
        for budget in (0, -1, float("nan"), float("inf"), True, 61):
            execute = Mock()
            with self.assertRaisesRegex(exchange.MonitorDeliveryError, "invalid_wait_bounds"):
                self.wait(execute, wait_seconds=budget)
            execute.assert_not_called()

    def test_real_cli_wrapper_forwards_the_remaining_process_timeout(self):
        with patch.object(exchange, "run", return_value=result(EVENT)) as run:
            self.assertEqual(exchange.pitput(self.env, "unused", timeout=0.125), EVENT)
        self.assertEqual(run.call_args.kwargs["timeout"], 0.125)


if __name__ == "__main__":
    unittest.main()
