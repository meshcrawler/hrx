"""Source tests for the Claude native attention proof: no native CLI, DB or network."""

import json
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import claude_attention as ca


STATE, TIMEOUT = "/tmp/run/agent-state", 20
NONCES = {"first": "a" * 32, "second": "b" * 32}
PRIMING = "...CLAUDE BACKGROUND DELIVERY... HRX has not started this monitor."


class SyntheticUsageTests(unittest.TestCase):
    def test_stream_and_nonstream_preserve_bounded_usage_for_text_and_tools(self):
        usage = {"input_tokens": 70000, "output_tokens": 7,
                 "cache_read_input_tokens": 12, "cache_creation_input_tokens": 13}
        for reply in ({"text": "synthetic"},
                      {"tool": {"id": "toolu_test", "name": "Bash", "input": {}}}):
            spec = dict(reply, usage=usage)
            frames = [json.loads(line[6:]) for line in ca.sse("fixture", spec).decode().splitlines()
                      if line.startswith("data: ")]
            self.assertEqual(frames[0]["message"]["usage"], usage)
            self.assertEqual(frames[-2]["usage"], {"output_tokens": 7})
            self.assertEqual(ca.message("fixture", spec)["usage"], usage)
            self.assertEqual(frames[-2]["delta"]["stop_reason"],
                             "tool_use" if "tool" in reply else "end_turn")
        copied = ca.synthetic_usage({"usage": usage})
        copied["input_tokens"] = 1
        self.assertEqual(usage["input_tokens"], 70000)

    def test_default_usage_retains_existing_plan(self):
        self.assertEqual(ca.synthetic_usage({}), {"input_tokens": 1, "output_tokens": 1})

    def test_invalid_usage_refused_by_both_wire_formats(self):
        invalid = [None, [], {}, {"input_tokens": 1},
                   {"input_tokens": 1, "output_tokens": 1, "unexpected": 0},
                   {"input_tokens": 900000, "output_tokens": 100001}]
        for field in ("input_tokens", "output_tokens", "cache_read_input_tokens",
                      "cache_creation_input_tokens"):
            for value in (True, False, -1, 1.0, "1", None, 1000001):
                invalid.append({"input_tokens": 1, "output_tokens": 1, field: value})
        for usage in invalid:
            for render in (ca.sse, ca.message):
                with self.subTest(usage=usage, render=render.__name__):
                    with self.assertRaisesRegex(ValueError, "invalid_synthetic_usage"):
                        render("fixture", {"text": "synthetic", "usage": usage})

    def test_handler_rejects_invalid_usage_before_http_success_without_socket(self):
        for stream in (False, True):
            script = Mock()
            script.decide.return_value = {"text": "synthetic", "usage": {"input_tokens": True}}
            server = Mock(server_address=("127.0.0.1", 1))
            with patch.object(ca.http.server, "ThreadingHTTPServer", return_value=server) as make, \
                    patch.object(ca.threading, "Thread"):
                ca.Responder(script)
            handler_type = make.call_args.args[1]
            handler = object.__new__(handler_type)
            request = json.dumps({"stream": stream}).encode()
            handler.path = "/v1/messages"
            handler.headers = {"Content-Length": str(len(request))}
            handler.rfile, handler.wfile = io.BytesIO(request), io.BytesIO()
            handler.send_response, handler.end_headers = Mock(), Mock()
            handler.do_POST()
            handler.send_response.assert_called_once_with(400)
            script.violate.assert_called_once_with("responder_invalid_request")
            self.assertEqual(handler.wfile.getvalue(), b"")


def event(seq, nonce=None):
    record = {"seq": seq, "event_key": f"key{seq}", "path": f"/b/exchange/attention/{seq}",
              "author": ca.CONTROLLER, "reason": ["mention"], "cursor": f"c:{seq}",
              "trust": "untrusted_peer_content"}
    if nonce:
        record["body"] = nonce
    return record


class Claude:
    """Plays Claude Code: runs each scripted tool call against a fake world and
    sends the next request exactly as the CLI would (tool_result or notification)."""

    def __init__(self, script, system=PRIMING):
        self.script, self.system, self.messages = script, system, [{"role": "user", "content": "go"}]
        self.tasks, self.queue, self.acked = 0, [], set()
        self.status = {"identity": ca.IDENTITY, "profile": "/p", "boards": [ca.BOARD], "can_write": True,
                       "status": "ready", "url": "http://127.0.0.1:1"}
        self.outbox = []

    def request(self, aux=False):
        if aux:
            return self.script.decide({"model": ca.ROUTE, "messages": [{"role": "user", "content": "title"}]})
        return self.script.decide({"model": ca.ROUTE, "system": self.system, "messages": self.messages,
                                   "tools": [{"name": "Bash"}, {"name": "Read"}]})

    def turn(self, spec):
        """Apply one response; return the follow-up request spec or None when idle."""
        if "tool" not in spec:
            return None
        tool = spec["tool"]
        self.messages.append({"role": "assistant", "content": [{"type": "tool_use", **tool}]})
        command = tool["input"]["command"]
        if tool["input"].get("run_in_background"):
            self.tasks += 1
            output = f"Command running in background with ID: task{self.tasks}. Output is being written to: " \
                     f"/tmp/tasks/task{self.tasks}.output. You will be notified when it completes."
        elif command.startswith("pitput join "):
            output = json.dumps({"identity": ca.IDENTITY, "boards": [ca.BOARD], "can_write": True})
        elif command.startswith("pitput agent status"):
            output = json.dumps(self.status)
        elif command.startswith("cat "):
            output = json.dumps(self.outbox[-1]) + "\n"
        elif " read " in command:
            seq = int(command.rsplit("/", 1)[1])
            output = json.dumps([{"seq": seq, "body": NONCES["first" if seq == 1 else "second"]}])
        else:
            cursor = command.rsplit("--ack ", 1)[1]
            self.acked.add(cursor)
            output = json.dumps({"acknowledged": cursor})
        self.messages.append({"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": tool["id"], "content": output, "is_error": False}]})
        return self.request()

    def run_until_idle(self, spec):
        while spec is not None:
            spec = self.turn(spec)

    def notify(self, record, task=None, body="", output_file=None, status="completed"):
        self.outbox.append(record)
        task = task or f"task{self.tasks}"
        output_file = output_file or f"/tmp/tasks/{task}.output"
        text = (f"<task-notification>\n<task-id>{task}</task-id>\n<output-file>{output_file}"
                f"</output-file>\n<status>{status}</status>\n<summary>done{body}</summary>\n</task-notification>")
        self.messages.append({"role": "user", "content": [{"type": "text", "text": text}]})
        self.run_until_idle(self.request())


def full_run(**overrides):
    script = ca.Script(STATE, TIMEOUT, NONCES)
    claude = Claude(script, **overrides)
    claude.run_until_idle(claude.request())
    claude.notify(event(1))           # first wake: read, re-arm without ACK
    claude.notify(event(1))           # replay of the unACKed event: ACK, re-arm
    claude.notify(event(2))           # second post: read, ACK, re-arm
    return script, claude


WINDOWS = [{"requests": 0, "main": 0, "auxiliary": 0, "cycles": 2, "seconds": 45}] * 2
CLEAN = {"drain_stopped": "PASS", "process_boundary": "PASS", "pitput_server": "PASS",
         "postgres_stopped": "PASS", "no_leftover_processes": "PASS"}


class ScriptTests(unittest.TestCase):
    def test_full_lifecycle_passes_every_check(self):
        script, claude = full_run()
        checks, status = ca.evaluate(script, WINDOWS, CLEAN)
        self.assertEqual(status, "PASS", checks)
        self.assertEqual(claude.acked, {"c:1", "c:2"})
        self.assertEqual(script.priming, ["system"])
        self.assertEqual([tool["purpose"] for tool in script.tools],
                         ["join", "status", "arm", "event", "read", "arm", "event", "ack", "arm",
                          "event", "read", "ack", "arm"])
        self.assertEqual(script.enrollment, True)

    def test_model_joins_and_checks_status_before_arming(self):
        # Job 4126: HRX enrolls only Codex/OpenCode; the primed Claude model must join itself.
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        first = claude.request()
        self.assertEqual(first["tool"]["input"]["command"], f"pitput join /b/{ca.BOARD} --identity {ca.IDENTITY}")
        second = claude.turn(first)
        self.assertEqual(second["tool"]["input"]["command"], f"pitput agent status --agent-id={ca.IDENTITY}")
        third = claude.turn(second)
        self.assertTrue(third["tool"]["input"].get("run_in_background"))

    def test_not_ready_enrollment_stops_before_arming(self):
        for status in ({"identity": ca.IDENTITY, "boards": [ca.BOARD], "status": "pending"},
                       {"identity": "other/agent/x", "boards": [ca.BOARD], "status": "ready"},
                       {"identity": ca.IDENTITY, "boards": ["restricted"], "status": "ready"}):
            with self.subTest(status=status):
                script = ca.Script(STATE, TIMEOUT, NONCES)
                claude = Claude(script)
                claude.status = status
                claude.run_until_idle(claude.request())
                self.assertIn("agent_not_ready_after_join", script.violations)
                self.assertNotIn("arm", [tool["purpose"] for tool in script.tools])

    def test_enrollment_status_parsing(self):
        ready = {"identity": ca.IDENTITY, "boards": [ca.BOARD], "status": "ready"}
        self.assertTrue(ca.enrollment_ready(json.dumps(ready)))
        self.assertTrue(ca.enrollment_ready(json.dumps(dict(ready, boards=[f"/b/{ca.BOARD}"]))))
        for bad in ("", "not json", "[]", json.dumps(dict(ready, boards="exchange"))):
            self.assertFalse(ca.enrollment_ready(bad))

    def test_notification_sizes_are_recorded(self):
        script, _ = full_run()
        for observation in script.events:
            self.assertGreater(observation["notification_bytes"], 0)
            self.assertGreater(observation["event_bytes"], 0)

    def test_priming_in_a_message_is_located_not_assumed(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        script.decide({"model": ca.ROUTE, "system": "plain", "tools": [{"name": "Bash"}],
                       "messages": [{"role": "user", "content": [{"type": "text", "text": PRIMING}]}]})
        self.assertEqual(script.priming, ["messages[0]"])

    def test_missing_priming_fails(self):
        script, _ = full_run(system="no priming here")
        self.assertIn("priming_absent", script.violations)
        self.assertEqual(ca.evaluate(script, WINDOWS, CLEAN)[1], "FAIL")

    def test_notification_carrying_the_post_body_fails(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        claude.notify(event(1), body=NONCES["first"])
        self.assertIn("notification_contains_post_body", script.violations)

    def test_event_output_with_a_body_is_not_metadata_only(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        claude.notify(event(1, NONCES["first"]))
        self.assertIn("event_not_metadata_only", script.violations)

    def test_notification_for_another_task_fails(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        claude.notify(event(1), task="someone-elses-task")
        self.assertIn("notification_not_for_armed_task", script.violations)

    def test_model_request_while_idle_is_a_violation(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        claude.request()  # Woken with neither a tool result nor a notification.
        self.assertIn("unexpected_request_while_idle", script.violations)

    def test_uncorrelated_tool_result_fails(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        spec = claude.request()
        claude.messages.append({"role": "assistant", "content": [{"type": "tool_use", **spec["tool"]}]})
        claude.messages.append({"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "toolu_other", "content": "x"}]})
        claude.request()
        self.assertIn("tool_result_correlation", script.violations)

    def test_auxiliary_requests_are_answered_counted_and_never_advance(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        self.assertEqual(claude.request(aux=True), {"text": "ok"})
        self.assertEqual(script.step, 0)
        self.assertEqual([r["class"] for r in script.records], ["auxiliary"])

    def test_unconfirmed_ack_stops_the_plan(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        script.event = event(1)
        script.events.append({})
        self.assertFalse(script.on_result("ack", '{"error": "stale"}'))
        self.assertIn("ack_not_confirmed", script.violations)

    def test_ack_schema_requires_the_exact_positive_cursor(self):
        handled = event(1)
        self.assertTrue(ca.ack_confirmed('{"acknowledged": "c:1"}\n', handled))
        for output in ('{"acknowledged": false}', '{"acknowledged": true}', '{"acknowledged": "c:2"}',
                       '{"acknowledged": "c:1", "error": "x"}', 'acknowledged c:1', '["c:1"]', '',
                       '{"acknowledged": null}', '{"note": "acknowledged c:1"}'):
            with self.subTest(output=output):
                self.assertFalse(ca.ack_confirmed(output, handled))
        self.assertFalse(ca.ack_confirmed('{"acknowledged": ""}', {"cursor": ""}))
        self.assertFalse(ca.ack_confirmed('{"acknowledged": "c:1"}', None))

    def wake_with(self, **notification):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        step = script.step
        claude.notify(event(1), **notification)
        return script, step

    def test_mismatched_output_file_is_never_read(self):
        script, step = self.wake_with(output_file="/tmp/tasks/other.output")
        self.assertIn("notification_output_file_mismatch", script.violations)
        self.assertEqual(script.step, step, "no tool may run after a mismatched notification")
        self.assertNotIn("event", [tool["purpose"] for tool in script.tools])

    def test_incomplete_task_notification_is_never_acted_on(self):
        for status in ("failed", "killed", "running", ""):
            with self.subTest(status=status):
                script, step = self.wake_with(status=status)
                self.assertIn("notification_not_completed", script.violations)
                self.assertEqual(script.step, step)

    def test_failed_task_gets_one_classified_diagnostic_read_then_stops(self):
        # Job 4090: the seeded launch armed, then the owned monitor task reported failed.
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        text = ("<task-notification>\n<task-id>task1</task-id>\n<output-file>/tmp/tasks/task1.output</output-file>"
                "\n<status>failed</status>\n<summary>Background command exited with exit code 2</summary>"
                "\n</task-notification>")
        claude.messages.append({"role": "user", "content": [{"type": "text", "text": text}]})
        spec = claude.request()
        self.assertEqual(spec["tool"]["input"]["command"], "cat /tmp/tasks/task1.output")
        self.assertNotIn("run_in_background", spec["tool"]["input"])
        step = script.step
        output = "pitput: remote_http_403 secret-detail\n"
        claude.messages.append({"role": "assistant", "content": [{"type": "tool_use", **spec["tool"]}]})
        claude.messages.append({"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": spec["tool"]["id"], "content": output, "is_error": False}]})
        self.assertEqual(claude.request(), {"text": "fixture stopped"})
        self.assertEqual(script.step, step, "a diagnostic read never advances the plan")
        observation = script.events[-1]
        self.assertEqual((observation["status"], observation["exit_code"]), ("failed", 2))
        self.assertEqual(observation["failure_classes"], ["pitput_http_403"])
        self.assertEqual(observation["failure_output_bytes"], len(output.encode()))
        self.assertIn("notification_not_completed", script.violations)
        self.assertNotIn("secret", json.dumps(script.events))
        self.assertEqual(ca.evaluate(script, WINDOWS, CLEAN)[1], "FAIL")

    def failed_wake(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        text = ("<task-notification>\n<task-id>task1</task-id>\n<output-file>/tmp/tasks/task1.output</output-file>"
                "\n<status>failed</status>\n<summary>exit code 1</summary>\n</task-notification>")
        claude.messages.append({"role": "user", "content": [{"type": "text", "text": text}]})
        spec = claude.request()

        def deliver():
            claude.messages.append({"role": "assistant", "content": [{"type": "tool_use", **spec["tool"]}]})
            claude.messages.append({"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": spec["tool"]["id"], "content": "pitput: agent_not_enrolled"}]})
            claude.request()
        return script, deliver

    def test_diagnostic_arriving_after_the_violation_is_collected_before_stop(self):
        import threading
        script, deliver = self.failed_wake()
        self.assertEqual((script.diagnostic, script.violations), ("pending", ["notification_not_completed"]))
        timer = threading.Timer(.3, deliver)
        timer.start()
        self.addCleanup(timer.cancel)
        with self.assertRaises(ca.Stop):
            ca.stop_on_violation(script)
        self.assertEqual(script.diagnostic, "collected")
        self.assertEqual(script.events[-1]["failure_classes"], ["pitput_agent_not_enrolled"])
        self.assertEqual(script.events[-1]["failure_diagnostic"], "collected")

    def test_drain_waits_for_a_wake_that_is_still_in_progress(self):
        # Review 876: the violation is visible before wake() sets "pending". An unlocked
        # check saw None, returned at once and then marked the diagnostic a timeout.
        import threading
        script = ca.Script(STATE, TIMEOUT, NONCES)
        script.events.append({})
        published = threading.Event()

        def slow_wake():
            with script.lock:
                script.violations.append("notification_not_completed")
                published.set()
                time.sleep(.3)
                script.diagnostic = "pending"

        def result():
            with script.lock:
                script.diagnostic = "collected"
                script.events[-1]["failure_diagnostic"] = "collected"

        wake = threading.Thread(target=slow_wake)
        wake.start()
        published.wait(5)
        timer = threading.Timer(.6, result)
        timer.start()
        self.addCleanup(timer.cancel)
        ca.drain_diagnostic(script, seconds=5)
        wake.join(5)
        self.assertEqual(script.diagnostic, "collected", "drain timed out before the wake finished")
        self.assertEqual(script.events[-1]["failure_diagnostic"], "collected")

    def test_diagnostic_that_never_returns_is_recorded_as_timeout(self):
        script, _ = self.failed_wake()
        ca.drain_diagnostic(script, seconds=.3)
        self.assertEqual((script.diagnostic, script.events[-1]["failure_diagnostic"]), ("timeout", "timeout"))
        self.assertNotIn("failure_classes", script.events[-1])
        self.assertEqual(ca.evaluate(script, WINDOWS, CLEAN)[1], "FAIL")

    def test_late_result_after_timeout_never_advances_the_plan(self):
        script, deliver = self.failed_wake()
        step = script.step
        ca.drain_diagnostic(script, seconds=.1)
        deliver()
        self.assertEqual((script.step, script.diagnostic), (step, "timeout"))

    def test_output_paths_are_shell_quoted(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        script.notified_file = "/tmp/tasks/a b;rm -rf x.output"
        spec = script.advance("event")
        self.assertEqual(spec["tool"]["input"]["command"], "cat '/tmp/tasks/a b;rm -rf x.output'")

    def test_failure_classes_are_allowlisted(self):
        cases = {"pitput: remote_http_401": ["pitput_http_401"],
                 "pitput: remote_http_500": ["pitput_http_other"],
                 "pitput: remote_connection_unavailable": ["pitput_connection_unavailable"],
                 "bash: pitput: command not found": ["command_not_found"],
                 "usage: pitput monitor ... error: unrecognized arguments: --x": ["pitput_usage_error"],
                 "Traceback (most recent call last):\nModuleNotFoundError: No module named 'ysg_mail'":
                     ["python_traceback", "module_not_found"],
                 "something else entirely": []}
        for output, expected in cases.items():
            with self.subTest(output=output):
                self.assertEqual(ca.failure_classes(output), expected)

    def test_idle_window_stops_as_fail_on_a_violation(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        script.violations.append("notification_not_completed")
        with patch.object(ca, "settle", lambda *a: 0), patch.object(ca, "cycles", lambda state: 0), \
                patch.object(ca.time, "sleep", lambda s: None), patch.object(ca, "drain_diagnostic", lambda s: None):
            with self.assertRaises(ca.Stop):
                ca.idle_window(script, STATE, TIMEOUT)

    def test_foreign_task_notification_is_never_acted_on(self):
        script, step = self.wake_with(task="someone-elses-task")
        self.assertIn("notification_not_for_armed_task", script.violations)
        self.assertEqual(script.step, step)

    def test_ack_uses_identical_filters_and_cursor_file(self):
        monitor = ca.monitor_args(STATE, TIMEOUT)
        self.assertEqual(ca.ack_command(STATE, TIMEOUT, "c:9"), " ".join(monitor + ["--ack", "c:9"]))
        arm = ca.arm_command(STATE, TIMEOUT)
        self.assertIn(" ".join(ca.monitor_args('"$state"', TIMEOUT)), arm)
        self.assertIn("--once", arm)


class EvaluationTests(unittest.TestCase):
    def test_any_idle_request_including_auxiliary_fails_quiescence(self):
        script, _ = full_run()
        noisy = [WINDOWS[0], {"requests": 1, "main": 0, "auxiliary": 1, "cycles": 2, "seconds": 45}]
        checks, status = ca.evaluate(script, noisy, CLEAN)
        self.assertFalse(checks["idle_windows_quiet"])
        self.assertEqual(status, "FAIL")

    def test_fewer_than_two_cycles_fails(self):
        script, _ = full_run()
        short = [WINDOWS[0], dict(WINDOWS[0], cycles=1)]
        self.assertFalse(ca.evaluate(script, short, CLEAN)[0]["idle_windows_quiet"])

    def test_cleanup_failure_fails(self):
        script, _ = full_run()
        self.assertEqual(ca.evaluate(script, WINDOWS, dict(CLEAN, no_leftover_processes="FAIL"))[1], "FAIL")
        self.assertEqual(ca.evaluate(script, WINDOWS, {})[1], "FAIL")

    def test_quiet_counts_every_request_by_class(self):
        records = [{"at": 1, "class": "main"}, {"at": 2, "class": "auxiliary"}, {"at": 9, "class": "main"}]
        self.assertEqual(ca.quiet(records, 0, 5), {"seconds": 5, "requests": 2, "main": 1, "auxiliary": 1})

    def test_incomplete_plan_fails(self):
        script = ca.Script(STATE, TIMEOUT, NONCES)
        claude = Claude(script)
        claude.run_until_idle(claude.request())
        self.assertEqual(ca.evaluate(script, WINDOWS, CLEAN)[1], "FAIL")


class TransportTests(unittest.TestCase):
    def test_streamed_tool_use_round_trips(self):
        spec = {"tool": {"id": "toolu_1", "name": "Bash", "input": {"command": "true", "run_in_background": True}}}
        frames = [line[6:] for line in ca.sse("m", spec).decode().splitlines() if line.startswith("data: ")]
        data = [json.loads(frame) for frame in frames]
        self.assertEqual([d["type"] for d in data], ["message_start", "content_block_start", "content_block_delta",
                                                     "content_block_stop", "message_delta", "message_stop"])
        self.assertEqual(json.loads(data[2]["delta"]["partial_json"]), spec["tool"]["input"])
        self.assertEqual(data[4]["delta"]["stop_reason"], "tool_use")

    def test_text_response_ends_the_turn(self):
        body = ca.message("m", {"text": "idle"})
        self.assertEqual((body["stop_reason"], body["content"][0]["text"]), ("end_turn", "idle"))

    def test_controller_never_acks(self):
        with self.assertRaises(AssertionError):
            ca.controller_pitput({}, "monitor", "/b/exchange", "--ack", "c:1")

    def test_fixture_catalog_is_fresh_for_hrx(self):
        # Job 4062: without generated_at HRX's catalog.Join refused the launch.
        from datetime import datetime, timezone
        now = datetime.now(timezone.utc)
        catalog = ca.fixture_catalog(now)
        self.assertEqual(datetime.fromisoformat(catalog["generated_at"]), now)
        self.assertEqual(catalog["schema_version"], 1)
        source = catalog["models"][0]["sources"][0]
        self.assertEqual((source["gateway_route"], source["details"]["harnesses"]),
                         (ca.ROUTE, {"claude": {"wire_api": "messages"}}))
        self.assertTrue(any(p["source"] == "gateway" and p["ok"] for p in catalog["coverage"]["providers"]))

    def test_hrx_launcher_errors_are_classified_before_screens(self):
        cases = {b"hrx: gateway catalog is stale or has an unsupported schema": "hrx_catalog_stale_or_schema",
                 b"Pitput enrollment failed; check machine enrollment authority": "hrx_pitput_enrollment",
                 b"--pitput requires the Pitput CLI": "hrx_pitput_cli_missing",
                 b"reasoning level is not declared for this model and harness": "hrx_route_unavailable",
                 b"gateway discovery did not succeed; refusing last-known routes": "hrx_gateway_discovery"}
        for text, name in cases.items():
            with self.subTest(name=name):
                self.assertEqual(ca.screen_class(b"\x1b[31m" + text + b" (trust this folder?)"), name)

    def test_launch_diagnostics_are_bounded_and_text_free(self):
        screen = b"secret-key sk-ant-123 /tmp/run/path gateway catalog is stale or has an unsupported schema"
        events = [{"http_status": 403, "local_limit": None}, {"http_status": 403, "local_limit": None},
                  {"http_status": 429, "local_limit": "request_budget"}]
        diagnostics = ca.launch_diagnostics({"status": 1}, events, screen, False, {"hrx": 1})
        self.assertEqual(diagnostics, {
            "screen_timeline": [], "hrx_exit": 1, "supervisor_alive_at_deadline": False, "hrx_alive_at_deadline": False,
            "descendants_at_deadline": {"hrx": 1}, "relay_requests": 3,
            "relay_outcomes": ["403:-", "429:request_budget"],
            "screen_class": "hrx_catalog_stale_or_schema", "screen_keywords": [], "screen_bytes": len(screen)})
        self.assertEqual(diagnostics["screen_keywords"], [])
        self.assertNotIn("secret", json.dumps(diagnostics))
        self.assertNotIn("/tmp/", json.dumps(diagnostics))

    def test_live_launch_without_exit_reports_alive(self):
        diagnostics = ca.launch_diagnostics(None, [], b"", True, {"claude": 1, "hrx": 1})
        self.assertEqual((diagnostics["hrx_exit"], diagnostics["hrx_alive_at_deadline"]), (None, True))

    def test_descendant_names_are_allowlisted_counts(self):
        table = {10: (1, "python3"), 11: (10, "hrx"), 12: (11, "claude"), 13: (12, "secret-tool"),
                 14: (13, "bash"), 99: (1, "claude")}
        self.assertEqual(ca.descendant_names(10, table), {"bash": 1, "claude": 1, "hrx": 1, "other": 1})

    def test_cursor_positioned_words_are_still_classified(self):
        # Job 4079: Ink draws spaces as cursor moves; deleting escapes glued the words.
        screen = (b"\x1b[2;3HDo\x1b[1Cyou\x1b[1Ctrust\x1b[1Cthe\x1b[1Cfiles\x1b[1Cin\x1b[1Cthis\x1b[1Cfolder?"
                  b"\x1b[4;3H1.\x1b[1CYes,\x1b[1Cproceed\x1b[5;3H2.\x1b[1CNo,\x1b[1Cexit")
        self.assertEqual(ca.screen_class(screen), "trust_dialog")
        self.assertEqual(ca.screen_keywords(screen), ["trust", "folder", "yes", "no, exit"])

    def test_glued_words_are_still_classified(self):
        self.assertEqual(ca.screen_class(b"\x1b[1mChoose\x1b[0mthe\x1b[0mtext\x1b[0mstyle"), "theme_onboarding")

    def test_keywords_are_a_fixed_vocabulary_only(self):
        words = ca.screen_keywords(b"Welcome to Claude Code! sk-ant-SECRET /tmp/path Press Enter to continue")
        self.assertEqual(words, ["welcome", "claude code", "press enter", "continue"])
        self.assertTrue(set(words) <= set(ca.SCREEN_VOCABULARY))

    def terminal(self, clock):
        terminal = ca.Terminal.__new__(ca.Terminal)
        terminal.screen, terminal.timeline, terminal.sampled = b"", [], None
        terminal.clock = clock
        terminal.started = clock()
        return terminal

    def test_timeline_records_each_screen_change_in_order(self):
        now = [100.0]
        terminal = self.terminal(lambda: now[0])
        for seconds, text in ((1, b"Choose the text style that looks best"), (3, b"Choose the text style again"),
                              (6, b"\x1b[2J" * 2000 + b"Quick safety check: Is this a project you created or one you trust?"),
                              (9, b"\x1b[2J" * 4000 + b"Bypass Permissions mode, Claude Code will not ask")):
            now[0] = 100 + seconds
            terminal.screen += text
            terminal.sample()
        self.assertEqual([(e["seconds"], e["class"]) for e in terminal.timeline],
                         [(1.0, "theme_onboarding"), (6.0, "trust_dialog"), (9.0, "bypass_permissions_acceptance")])
        self.assertIn("quick safety check", terminal.timeline[1]["keywords"])

    def test_timeline_is_bounded_and_rate_limited(self):
        now = [0.0]
        terminal = self.terminal(lambda: now[0])
        for index in range(100):
            now[0] = index * .5
            terminal.screen = (b"Choose the text style" if index % 2 else b"Do you trust this folder?")
            terminal.sample()
        self.assertLessEqual(len(terminal.timeline), ca.Terminal.TIMELINE_LIMIT)
        gaps = [b["seconds"] - a["seconds"] for a, b in zip(terminal.timeline, terminal.timeline[1:])]
        self.assertTrue(all(gap >= ca.Terminal.SAMPLE_SECONDS for gap in gaps))
        self.assertNotIn("text", json.dumps(terminal.timeline).replace("text style", ""))

    def test_forced_sample_captures_the_final_screen(self):
        now = [0.0]
        terminal = self.terminal(lambda: now[0])
        terminal.screen = b"Choose the text style"
        terminal.sample()
        terminal.screen += b"\x1b[2J" * 4000 + b"Quick safety check"
        now[0] = .1
        terminal.sample()
        terminal.sample(force=True)
        self.assertEqual(terminal.timeline[-1]["class"], "trust_dialog")

    def test_seed_parts_are_explicit_and_default_to_fresh(self):
        self.assertEqual(ca.seed_parts(""), ())
        self.assertEqual(ca.seed_parts("1"), ("theme", "trust", "bypass"))
        self.assertEqual(ca.seed_parts("theme"), ("theme",))
        self.assertEqual(ca.seed_parts(" bypass , theme "), ("theme", "bypass"))
        for bad in ("0", "yes", "theme,login", "all", "true"):
            with self.subTest(value=bad):
                self.assertEqual(ca.seed_parts(bad), (), "unknown selections seed nothing")

    def test_seed_state_contains_only_the_chosen_answers(self):
        project = Path("/tmp/run/project")
        self.assertEqual(ca.seed_state(("theme",), project), {"hasCompletedOnboarding": True, "theme": "dark"})
        self.assertEqual(ca.seed_state(("trust",), project),
                         {"projects": {"/tmp/run/project": {"hasTrustDialogAccepted": True}}})
        full = ca.seed_state(ca.SEED_PARTS, project)
        # The full seed is exactly what run 4147 used.
        self.assertEqual(full, {"hasCompletedOnboarding": True, "theme": "dark", "bypassPermissionsModeAccepted": True,
                                "projects": {"/tmp/run/project": {"hasTrustDialogAccepted": True}}})
        self.assertNotIn("Key", json.dumps(full))

    def test_quick_safety_check_wording_is_classified(self):
        text = b"Quick safety check: Is this a project you created or one you trust? (Like your own code)"
        self.assertEqual(ca.screen_class(text), "trust_dialog")

    def test_current_trust_prompt_wording_is_classified(self):
        for text in (b"Is this a project you created or one you trust?", b"Do you trust this folder?",
                     b"Do you trust the files in this folder?"):
            with self.subTest(text=text):
                self.assertEqual(ca.screen_class(text), "trust_dialog")

    def test_screen_classification(self):
        self.assertEqual(ca.screen_class(b"\x1b[1mDo you trust the files in this folder?"), "trust_dialog")
        self.assertEqual(ca.screen_class(b"WARNING: Bypass Permissions mode"), "bypass_permissions_acceptance")
        self.assertEqual(ca.screen_class(b"something else"), "unclassified")


class FakeStep:
    def __init__(self, log, name, result=True, error=None):
        self.log, self.name, self.result, self.error = log, name, result, error

    def __call__(self, *args, **kwargs):
        self.log.append(self.name)
        if self.error:
            raise self.error
        return self.result


class TeardownTests(unittest.TestCase):
    """Every cleanup step is attempted independently, with bounded evidence."""

    def resources(self, log, failing=()):
        class Closable:
            def __init__(self, name):
                self.close = FakeStep(log, name, error=RuntimeError("secret detail") if name in failing else None)

        class Server:
            pid = 1

            def terminate(self):
                log.append("server")
                if "server" in failing:
                    raise OSError("secret detail")

            def wait(self, timeout=None):
                return 0

        return {"terminal": Closable("terminal"), "gateway": Closable("gateway"),
                "responder": Closable("responder"), "server": Server(),
                "postgres": ("pg_ctl", Path("/tmp/none"), {}), "root": Path("/tmp/run")}

    def teardown(self, resources):
        report = {"cleanup": {}}
        calls = []

        def fake_run(args, **kwargs):
            calls.append(args[-1])
            return subprocess.CompletedProcess(args, 3 if args[-1] == "status" else 0)

        with patch.object(ca, "run", fake_run), patch.object(ca, "leftovers", lambda root: []):
            ca.teardown(resources, report)
        return report, calls

    def test_a_failing_step_never_skips_later_steps(self):
        log = []
        report, calls = self.teardown(self.resources(log, failing={"terminal", "server", "gateway"}))
        self.assertEqual(log, ["terminal", "server", "gateway", "responder"])
        self.assertEqual(calls, ["stop", "status"], "PostgreSQL stop must still be attempted")
        cleanup = report["cleanup"]
        self.assertEqual((cleanup["drain_stopped"], cleanup["pitput_server"], cleanup["gateway_closed"]),
                         ("FAIL", "FAIL", "FAIL"))
        self.assertEqual((cleanup["responder_closed"], cleanup["postgres_stopped"],
                          cleanup["no_leftover_processes"]), ("PASS", "PASS", "PASS"))
        self.assertEqual(report["cleanup_errors"], {"drain_stopped": "RuntimeError", "pitput_server": "OSError",
                                                    "gateway_closed": "RuntimeError"})
        self.assertNotIn("secret detail", json.dumps(report))

    def test_only_acquired_resources_are_touched(self):
        report, calls = self.teardown({"root": Path("/tmp/run")})
        self.assertEqual(report["cleanup"], {"no_leftover_processes": "PASS"})
        self.assertEqual(calls, [])

    def test_a_live_drain_keeps_the_terminal_open(self):
        log = []
        resources = self.resources(log)
        resources["terminal"].close = FakeStep(log, "terminal", result=False)
        resources["master"] = 999999
        report, _ = self.teardown(resources)
        self.assertNotIn("terminal_closed", report["cleanup"])

    def test_postgres_still_running_fails(self):
        report = {"cleanup": {}}
        with patch.object(ca, "run", lambda args, **k: subprocess.CompletedProcess(args, 0)), \
                patch.object(ca, "leftovers", lambda root: []):
            ca.teardown({"postgres": ("pg_ctl", Path("/x"), {})}, report)
        self.assertEqual(report["cleanup"]["postgres_stopped"], "FAIL")

    def test_unobservable_processes_fail(self):
        report = {"cleanup": {}}
        with patch.object(ca, "leftovers", lambda root: None):
            ca.teardown({"root": Path("/x")}, report)
        self.assertEqual(report["cleanup"]["no_leftover_processes"], "FAIL")


class ProveFailureTests(unittest.TestCase):
    """Startup failures are acquired under the guard and always produce a report."""

    def prove(self, fail_on):
        calls = []

        def fake_run(args, **kwargs):
            calls.append(Path(args[0]).name + ":" + args[-1])
            if fail_on in args[0] or fail_on == args[-1]:
                raise AssertionError("fixture failure with secret detail")
            return subprocess.CompletedProcess(args, 3 if args[-1] == "status" else 0)

        with tempfile.TemporaryDirectory() as temp, \
                patch.object(ca.shutil, "which", lambda name: "/usr/lib/postgresql/17/bin/" + name), \
                patch.object(ca, "run", fake_run), patch.object(ca, "leftovers", lambda root: []):
            report = ca.prove("hrx", Path(temp), TIMEOUT)
        return report, calls

    def test_initdb_failure_reports_and_still_stops_postgres(self):
        report, calls = self.prove("initdb")
        self.assertEqual((report["status"], report["reason"]), ("FAIL", "controller_error:AssertionError"))
        self.assertEqual(report["stage"], "postgres")
        self.assertIn("pg_ctl:status", calls)
        self.assertEqual(report["cleanup"]["postgres_stopped"], "PASS")
        self.assertNotIn("secret detail", json.dumps(report))

    def test_postgres_start_failure_reports_and_still_cleans_up(self):
        report, calls = self.prove("start")
        self.assertEqual(report["reason"], "controller_error:AssertionError")
        self.assertEqual(calls[-2:], ["pg_ctl:stop", "pg_ctl:status"])
        self.assertIn("no_leftover_processes", report["cleanup"])

    def test_relay_gets_an_existing_private_directory(self):
        # Pipeline 760 / job 4046: the relay's openssl could not write into a missing directory.
        seen = {}

        class Relay:
            def __init__(self, root, *args):
                seen.update(exists=root.is_dir(), mode=root.stat().st_mode & 0o777)
                raise RuntimeError("stop after the relay check")

        def fake_run(args, **kwargs):
            return subprocess.CompletedProcess(args, 3 if args[-1] == "status" else 0)

        with tempfile.TemporaryDirectory() as temp, \
                patch.object(ca.shutil, "which", lambda name: "/usr/lib/postgresql/17/bin/" + name), \
                patch.object(ca, "run", fake_run), patch.object(ca, "leftovers", lambda root: []), \
                patch.object(ca, "pitput", lambda *a, **k: None), patch.object(ca, "Gateway", Relay), \
                patch.object(ca.subprocess, "Popen") as popen, patch.object(ca, "wait_until", lambda *a, **k: True):
            popen.return_value.wait.return_value = 0
            report = ca.prove("hrx", Path(temp), TIMEOUT)
        self.assertEqual(seen, {"exists": True, "mode": 0o700})
        self.assertEqual((report["stage"], report["reason"]), ("relay", "controller_error:RuntimeError"))

    def test_failed_subprocess_reports_only_its_name_and_exit_code(self):
        class Relay:
            def __init__(self, root, *args):
                raise subprocess.CalledProcessError(1, ["/usr/bin/openssl", "req", "-keyout", "/secret/key.pem"],
                                                    output="secret output", stderr="secret error")

        def fake_run(args, **kwargs):
            return subprocess.CompletedProcess(args, 3 if args[-1] == "status" else 0)

        with tempfile.TemporaryDirectory() as temp, \
                patch.object(ca.shutil, "which", lambda name: "/usr/lib/postgresql/17/bin/" + name), \
                patch.object(ca, "run", fake_run), patch.object(ca, "leftovers", lambda root: []), \
                patch.object(ca, "pitput", lambda *a, **k: None), patch.object(ca, "Gateway", Relay), \
                patch.object(ca.subprocess, "Popen") as popen, patch.object(ca, "wait_until", lambda *a, **k: True):
            popen.return_value.wait.return_value = 0
            report = ca.prove("hrx", Path(temp), TIMEOUT)
        self.assertEqual(report["reason"], "controller_error:CalledProcessError")
        self.assertEqual(report["failed_command"], {"name": "openssl", "exit_code": 1})
        self.assertEqual(report["stage"], "relay")
        self.assertNotIn("secret", json.dumps(report))

    def test_missing_prerequisites_are_blocked_with_cleanup_evidence(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(ca.shutil, "which", lambda name: None), \
                patch.object(ca, "leftovers", lambda root: []):
            report = ca.prove("hrx", Path(temp), TIMEOUT)
        self.assertEqual((report["status"], report["reason"]), ("BLOCKED", "postgresql_or_claude_missing"))
        self.assertEqual(report["cleanup"], {"no_leftover_processes": "PASS"})

    def test_crashing_controller_still_writes_a_report(self):
        with tempfile.TemporaryDirectory() as temp, \
                patch.object(ca, "prove", side_effect=RuntimeError("secret detail")), \
                patch.object(ca.sys, "platform", "linux"), \
                patch.dict(os.environ, {"HRX_CLAUDE_ATTENTION_AUTHORIZED": "1"}), \
                patch.object(ca.sys, "argv", ["claude_attention.py", "--hrx", "hrx", "--output", temp]):
            self.assertEqual(ca.main(), 1)
            report = json.loads((Path(temp) / "report.json").read_text())
            junit = (Path(temp) / "junit.xml").read_text()
        self.assertEqual((report["status"], report["reason"]), ("FAIL", "controller_error:RuntimeError"))
        self.assertIn("failures=\"1\"", junit)
        self.assertNotIn("secret detail", json.dumps(report))


class ManagedConfigModeTests(unittest.TestCase):
    """The product path: the real hrx initializes its own managed Claude config."""

    def test_off_by_default(self):
        self.assertEqual(ca.managed_mode({}, {"CLAUDE_CONFIG_DIR": "/nope"}), ())
        self.assertEqual(ca.managed_args({"CLAUDE_CONFIG_DIR": "/c"}, ()), [])

    def test_parts_become_the_real_hrx_flags(self):
        with tempfile.TemporaryDirectory() as temp:
            env = {"CLAUDE_CONFIG_DIR": str(Path(temp) / "claude")}
            parts = ca.managed_mode({"HRX_CLAUDE_ATTENTION_HRX_INIT": "trust, theme"}, env)
            self.assertEqual(parts, ("theme", "trust"))
            self.assertEqual(ca.managed_args(env, parts),
                             ["--claude-config-dir", env["CLAUDE_CONFIG_DIR"], "--claude-init", "theme,trust"])

    def test_invalid_or_conflicting_requests_are_blocked(self):
        with tempfile.TemporaryDirectory() as temp:
            env = {"CLAUDE_CONFIG_DIR": str(Path(temp) / "claude")}
            for environ, reason in (({"HRX_CLAUDE_ATTENTION_HRX_INIT": "login"}, "hrx_init_parts_invalid"),
                                    ({"HRX_CLAUDE_ATTENTION_HRX_INIT": "theme,theme"}, "hrx_init_parts_invalid"),
                                    ({"HRX_CLAUDE_ATTENTION_HRX_INIT": "theme",
                                      "HRX_CLAUDE_ATTENTION_SEED_ONBOARDING": "1"},
                                     "hrx_init_and_fixture_seed_are_exclusive")):
                with self.subTest(environ=environ):
                    with self.assertRaises(ca.Blocked) as caught:
                        ca.managed_mode(environ, env)
                    self.assertEqual(str(caught.exception), reason)
            Path(env["CLAUDE_CONFIG_DIR"]).mkdir()
            with self.assertRaises(ca.Blocked) as caught:
                ca.managed_mode({"HRX_CLAUDE_ATTENTION_HRX_INIT": "theme"}, env)
            self.assertEqual(str(caught.exception), "hrx_managed_config_dir_not_fresh")

    def test_provenance_is_bounded_part_names_only(self):
        with tempfile.TemporaryDirectory() as temp:
            marker = {"schema": 1, "created_by": "hrx", "created_at": "2026-10-23T12:00:00Z", "uid": 1001,
                      "initialized": [{"part": "theme", "at": "t"},
                                      {"part": "trust", "workspace": "/secret/path", "at": "t"}]}
            (Path(temp) / ".hrx-claude-config.json").write_text(json.dumps(marker))
            evidence = ca.managed_provenance(temp)
            self.assertEqual(evidence, {"created_by": "hrx", "schema": 1, "initialized": ["theme", "trust"]})
            self.assertNotIn("/secret", json.dumps(evidence))
            self.assertIsNone(ca.managed_provenance(Path(temp) / "missing"))


class ManagedRepeatResumeTests(unittest.TestCase):
    """Native repeat/resume/mismatch checks for the hrx --claude-init path (pure fakes)."""

    def world(self, temp, native="native-1", mismatch_exit=1, resume_exit=0, repeat_mutates=None,
              resume_child=None, timed_out=()):
        root = Path(temp)
        home, directory = root / "agent", root / "agent" / "claude"
        directory.mkdir(parents=True)
        marker = {"schema": 1, "created_by": "hrx", "uid": 1, "created_at": "t",
                  "initialized": [{"part": "theme", "at": "t1"}, {"part": "trust", "workspace": "/w", "at": "t2"}]}
        path = directory / ".hrx-claude-config.json"
        path.write_text(json.dumps(marker))
        index = home / "state/hrx/sessions/index.json"
        index.parent.mkdir(parents=True)
        records = [{"harness": "claude", "native_id": native, "claude_config_dir": str(directory)}] if native else []
        index.write_text(json.dumps({"sessions": records}))
        env = {"HOME": str(home), "CLAUDE_CONFIG_DIR": str(directory), "PATH": "/bin"}
        calls = []

        def child(environ, config):
            # What the probe would observe if HRX execed claude with this config dir.
            Path(environ["HRX_PROBE_OUT"]).write_text(
                "match" if config == environ["HRX_PROBE_EXPECTED"] else "mismatch")

        def run(command, environ):
            calls.append((command, dict(environ)))
            name = "mismatch" if "--claude-config-dir" in command and "--resume" in command else \
                "resume" if "--resume" in command else "repeat"
            out = {"cleanup": "clean", "timed_out": name in timed_out}
            if name == "mismatch":
                return dict(out, exit_code=mismatch_exit)
            if name == "resume":
                child(environ, resume_child if resume_child is not None else str(directory))
                return dict(out, exit_code=resume_exit)
            if repeat_mutates:
                repeat_mutates(marker)
                path.write_text(json.dumps(marker))
            child(environ, environ.get("CLAUDE_CONFIG_DIR"))
            return dict(out, exit_code=0)

        return root, env, directory, run, calls

    def repeat(self, temp, **options):
        root, env, directory, run, calls = self.world(temp, **options)
        return ca.repeat_resume("hrx", env, root, ("theme", "trust"), directory, root, run, "/real/claude"), calls

    def test_all_three_checks_pass_and_resume_runs_without_the_env_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            result, calls = self.repeat(temp)
            self.assertTrue(result["passed"], result)
            self.assertEqual((result["repeat"]["child_config"], result["resume"]["child_config"],
                              result["mismatch"]["child_config"]), ("match", "match", None))
            repeat, resume, mismatch = calls
            self.assertEqual(repeat[0][-2:], ["--", "--version"])
            # The resume proves restoration from the session record: no env, no flag.
            self.assertNotIn("CLAUDE_CONFIG_DIR", resume[1])
            self.assertNotIn("--claude-config-dir", resume[0])
            self.assertEqual(resume[0][2:4], ["--resume", "native-1"])
            self.assertNotIn("CLAUDE_CONFIG_DIR", mismatch[1])
            for _, environ in calls:
                self.assertTrue(environ["PATH"].startswith(str(Path(temp) / "claude-probe")))
                self.assertEqual(environ["HRX_PROBE_REAL"], "/real/claude")

    def test_any_marker_byte_change_fails_even_with_identical_part_names(self):
        mutations = (lambda m: m["initialized"][0].update(at="later"),
                     lambda m: m.update(uid=2),
                     lambda m: m["initialized"][1].update(workspace="/other"),
                     lambda m: m["initialized"].append({"part": "theme", "at": "t3"}))
        for mutate in mutations:
            with self.subTest(mutate=mutate), tempfile.TemporaryDirectory() as temp:
                result, _ = self.repeat(temp, repeat_mutates=mutate)
                self.assertFalse(result["marker_unchanged_after_repeat"])
                self.assertFalse(result["passed"])
                self.assertNotIn("sha", json.dumps(result).lower(), "digests are compared, never reported")

    def test_marker_change_diagnostics_name_parts_and_answer_presence(self):
        # Job 4402: the repeat changed the marker. Record which parts and which
        # answers, as names and booleans only, without raw values or paths.
        with tempfile.TemporaryDirectory() as temp:
            root, env, directory, run, _ = self.world(
                temp, repeat_mutates=lambda m: m["initialized"].append({"part": "bypass", "at": "t3"}))
            (directory / ".claude.json").write_text(json.dumps({
                "hasCompletedOnboarding": True, "theme": "dark",
                "projects": {str(root): {"hasTrustDialogAccepted": True}}}))
            (directory / "settings.json").write_text(json.dumps({"skipDangerousModePermissionPrompt": True}))
            result = ca.repeat_resume("hrx", env, root, ("theme", "trust", "bypass"), directory, root, run,
                                      "/real/claude")
            self.assertEqual(result["parts_before"], ["theme", "trust"])
            self.assertEqual(result["parts_after_repeat"], ["theme", "trust", "bypass"])
            state = result["state_before_repeat"]
            self.assertTrue(state["workspace_trusted"] and state["settings_skipDangerousModePermissionPrompt"])
            self.assertFalse(state["bypassPermissionsModeAccepted"])
            self.assertFalse(result["passed"])
            self.assertNotIn(str(root), json.dumps(result))

    def test_marker_change_distinguishes_missing_unparseable_and_changed(self):
        marker = {"schema": 1, "created_by": "hrx", "uid": 1, "created_at": "t",
                  "initialized": [{"part": "theme", "at": "t1"}, {"part": "bypass", "at": "t2"}]}
        before = json.dumps(marker).encode()
        grown = json.loads(before)
        grown["initialized"].append({"part": "bypass", "at": "/secret 2026-10-23"})
        change = ca.marker_change(before, json.dumps(grown).encode())
        self.assertEqual((change["before"], change["after"], change["bytes_equal"]), ("present", "present", False))
        self.assertEqual(change["changed_fields"], ["initialized"])
        self.assertEqual((change["part_counts_before"], change["part_counts_after"]),
                         ({"bypass": 1, "theme": 1}, {"bypass": 2, "theme": 1}))
        self.assertNotIn("secret", json.dumps(change))
        self.assertEqual(ca.marker_change(before, None)["after"], "missing")
        self.assertIsNone(ca.marker_change(before, None)["changed_fields"])
        self.assertEqual(ca.marker_change(before, b"{junk")["after"], "unparseable")
        self.assertTrue(ca.marker_change(before, before)["bytes_equal"])
        self.assertEqual(ca.marker_change(before, before)["changed_fields"], [])
        self.assertFalse(ca.marker_change(None, None)["bytes_equal"])

    def test_repeat_reuses_the_identity_and_checks_the_workspace(self):
        with tempfile.TemporaryDirectory() as temp:
            root, env, directory, run, calls = self.world(temp)
            result = ca.repeat_resume("hrx", env, root, ("theme", "trust"), directory, root, run, "/real/claude")
            repeat = calls[0][0]
            self.assertEqual(repeat[repeat.index("--agent-identity") + 1], ca.IDENTITY)
            # The fake marker's trust record names "/w", not this launch's workspace.
            self.assertEqual(result["trust_workspace"], {"trust_records": 1, "all_match_launch_workspace": False})
            self.assertNotIn("/w", json.dumps(result["trust_workspace"]))

    def test_resume_child_with_the_wrong_config_dir_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            result, _ = self.repeat(temp, resume_child="/somewhere/else")
            self.assertEqual(result["resume"]["child_config"], "mismatch")
            self.assertFalse(result["passed"])

    def test_resume_exit_zero_without_a_probed_child_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            root, env, directory, run, _ = self.world(temp)

            def no_child(command, environ):
                if "--resume" in command and "--claude-config-dir" not in command:
                    return {"exit_code": 0, "cleanup": "clean", "timed_out": False}
                return run(command, environ)

            result = ca.repeat_resume("hrx", env, root, ("theme",), directory, root, no_child, "/real/claude")
            self.assertIsNone(result["resume"]["child_config"])
            self.assertFalse(result["passed"])

    def test_any_phase_timing_out_fails(self):
        for phase in ("repeat", "resume", "mismatch"):
            with self.subTest(phase=phase), tempfile.TemporaryDirectory() as temp:
                result, _ = self.repeat(temp, timed_out=(phase,))
                self.assertFalse(result["passed"])

    def test_accepted_mismatch_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            self.assertFalse(self.repeat(temp, mismatch_exit=0)[0]["passed"])

    def test_failed_resume_or_missing_session_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            self.assertFalse(self.repeat(temp, resume_exit=1)[0]["passed"])
        with tempfile.TemporaryDirectory() as temp:
            result, calls = self.repeat(temp, native=None)
            self.assertEqual((result["session_recorded"], result["passed"], len(calls)), (False, False, 1))

    def test_unclean_ownership_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            root, env, directory, run, _ = self.world(temp)
            dirty = lambda command, environ: dict(run(command, environ), cleanup="failed")
            self.assertFalse(ca.repeat_resume("hrx", env, root, ("theme",), directory, root, dirty,
                                              "/real/claude")["passed"])

    def test_probe_script_records_match_and_execs_the_real_claude(self):
        with tempfile.TemporaryDirectory() as temp:
            real = Path(temp) / "real-claude"
            real.write_text("#!/bin/sh\necho real \"$@\"\n")
            real.chmod(0o700)
            environ, out = ca.probe_environment({"PATH": os.environ["PATH"], "CLAUDE_CONFIG_DIR": "/managed"},
                                                temp, "repeat", "/managed", str(real))
            done = subprocess.run(["claude", "--version"], env=environ, capture_output=True, text=True, timeout=30)
            self.assertEqual((done.stdout.strip(), out.read_text()), ("real --version", "match"))
            environ["CLAUDE_CONFIG_DIR"] = "/other"
            subprocess.run(["claude"], env=environ, capture_output=True, timeout=30)
            self.assertEqual(out.read_text(), "mismatch")

    def test_stop_main_never_closes_a_terminal_under_a_live_reader(self):
        class Stuck:
            def close(self):
                return False

        resources = {"terminal": Stuck(), "master": 99, "summary": "/x"}
        report = {"cleanup": {}}
        ca.stop_main(resources, report)
        self.assertEqual(report["cleanup"]["drain_stopped"], "FAIL")
        self.assertNotIn("master", resources)


class GuardTests(unittest.TestCase):
    def run_main(self, env):
        with tempfile.TemporaryDirectory() as temp:
            result = subprocess.run([sys.executable, str(Path(ca.__file__)), "--hrx", "hrx", "--output", temp],
                                    env=env, capture_output=True, text=True, timeout=60)
            return result.returncode, json.loads((Path(temp) / "report.json").read_text())

    def test_unauthorized_run_is_blocked_and_starts_nothing(self):
        env = {k: v for k, v in os.environ.items() if k != "HRX_CLAUDE_ATTENTION_AUTHORIZED"}
        code, report = self.run_main(env)
        self.assertEqual((code, report["status"], report["reason"]), (1, "BLOCKED", "not_authorized_native_fixture"))

    @unittest.skipIf(sys.platform.startswith("linux"), "covers the non-Linux refusal")
    def test_non_linux_is_blocked(self):
        code, report = self.run_main(dict(os.environ, HRX_CLAUDE_ATTENTION_AUTHORIZED="1"))
        self.assertEqual((code, report["reason"]), (1, "linux_only"))


if __name__ == "__main__":
    unittest.main()
