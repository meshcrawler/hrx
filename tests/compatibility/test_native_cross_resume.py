"""Pure tests for the native cross-model resume diagnostic: no native CLI, DB or network."""

import http.client
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import native_cross_resume as ncr


TOOLS = {
    "messages": [{"name": "Bash", "input_schema": {"properties": {"command": {}, "description": {}}}}],
    "responses": [{"type": "function", "name": "shell", "parameters": {"properties": {"command": {}, "workdir": {}}}}],
    "chat_completions": [{"type": "function", "function": {
        "name": "shell", "parameters": {"properties": {"command": {}, "workdir": {}, "description": {}}}}}],
}


class Harness:
    """Plays a native CLI: sends requests in its protocol, runs scripted shell tools
    for real in a temporary project and carries the conversation forward."""

    def __init__(self, protocol, script, project, system="system prompt"):
        self.protocol, self.script, self.project = protocol, script, project
        self.history = [] if protocol == "responses" else (
            [{"role": "system", "content": system}] if protocol == "chat_completions" else [])

    def user(self, text):
        self.history.append({"role": "user", "content": text} if self.protocol != "responses"
                            else {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]})

    def request(self, model):
        key = "input" if self.protocol == "responses" else "messages"
        return {"model": model, "tools": TOOLS[self.protocol], key: self.history, "stream": True}

    def turn(self, model, limit=10):
        """Run one turn to its final text; return the commands executed."""
        commands = []
        for _ in range(limit):
            spec = self.script.decide(self.protocol, self.request(model))
            if "tool" not in spec:
                return commands
            tool = spec["tool"]
            arguments = tool["arguments"]
            command = arguments.get("command") or arguments.get("cmd")
            if isinstance(command, list):
                command = command[-1]
            commands.append(command)
            output = subprocess.run(["bash", "-c", command], cwd=self.project, capture_output=True,
                                    text=True, timeout=10).stdout
            if self.protocol == "messages":
                self.history.append({"role": "assistant", "content": [
                    {"type": "tool_use", "id": tool["id"], "name": tool["name"], "input": arguments}]})
                self.history.append({"role": "user", "content": [
                    {"type": "tool_result", "tool_use_id": tool["id"], "content": output}]})
            elif self.protocol == "responses":
                self.history.append({"type": "function_call", "call_id": tool["id"], "name": tool["name"],
                                     "arguments": json.dumps(arguments)})
                self.history.append({"type": "function_call_output", "call_id": tool["id"], "output": output})
            else:
                self.history.append({"role": "assistant", "tool_calls": [{"id": tool["id"], "type": "function",
                                     "function": {"name": tool["name"], "arguments": json.dumps(arguments)}}]})
                self.history.append({"role": "tool", "tool_call_id": tool["id"], "content": output})
        raise AssertionError("turn did not finish")


def source_phase(protocol, temp):
    """A finished source phase: (script, harness, nonce) with challenge.txt removed."""
    project = Path(temp)
    nonce = os.urandom(24).hex()
    (project / "challenge.txt").write_text(nonce)
    script = ncr.Script("fixture-x-source", "fixture-x-target", str(project))
    harness = Harness(protocol, script, project)
    harness.user("Read challenge.txt, reverse it into result.txt, remember the value.")
    harness.turn("fixture-x-source")
    (project / "challenge.txt").unlink()
    return script, harness, nonce


def flow(protocol, temp, keep_history=True, system="system prompt"):
    project = Path(temp)
    nonce = os.urandom(24).hex()
    (project / "challenge.txt").write_text(nonce)
    script = ncr.Script("fixture-x-source", "fixture-x-target", str(project))
    harness = Harness(protocol, script, project, system)
    harness.user("Read challenge.txt, reverse it into result.txt, remember the value.")
    source_commands = harness.turn("fixture-x-source")
    (project / "challenge.txt").unlink()
    if not keep_history:
        harness.history = [m for m in harness.history if m.get("role") == "system"]
    harness.user("Write the original value to resumed.txt.")
    target_commands = harness.turn("fixture-x-target")
    return script, project, nonce, source_commands, target_commands


class ScriptTests(unittest.TestCase):
    def test_every_protocol_reads_writes_and_resumes_from_history(self):
        for protocol in ("messages", "responses", "chat_completions"):
            with self.subTest(protocol=protocol), tempfile.TemporaryDirectory() as temp:
                script, project, nonce, source, target = flow(protocol, temp)
                self.assertEqual(source[0], "cat challenge.txt")
                self.assertEqual((project / "result.txt").read_text(), nonce[::-1])
                self.assertEqual((project / "resumed.txt").read_text(), nonce)
                self.assertTrue(script.observations["history_retained"])
                self.assertEqual(script.violations, [])
                self.assertEqual(len(target), 1, "the resume writes once and finishes")

    def test_missing_history_is_a_violation_and_writes_nothing(self):
        with tempfile.TemporaryDirectory() as temp:
            script, project, _, _, target = flow("messages", temp, keep_history=False)
            self.assertIn("resume_history_missing", script.violations)
            self.assertFalse(script.observations["history_retained"])
            self.assertFalse((project / "resumed.txt").exists())
            self.assertEqual(target, [])

    def test_system_prompt_text_never_drives_the_script(self):
        # A system prompt quoting the scripted commands and a hex value must be ignored.
        decoy = "Use printf > result.txt and > resumed.txt; id " + "ab" * 24
        with tempfile.TemporaryDirectory() as temp:
            script, project, nonce, source, _ = flow("chat_completions", temp, system=decoy)
            self.assertEqual(source[0], "cat challenge.txt")
            self.assertEqual((project / "resumed.txt").read_text(), nonce)

    def test_wrong_model_is_a_violation(self):
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        spec = script.decide("messages", {"model": "fixture-x-other", "tools": TOOLS["messages"], "messages": []})
        self.assertEqual(spec, {"text": "fixture stopped"})
        self.assertIn("unexpected_model", script.violations)

    def test_unknown_tool_schema_is_a_violation(self):
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        tools = [{"type": "function", "name": "apply_patch", "parameters": {"properties": {"input": {}}}}]
        script.decide("responses", {"model": "fixture-x-source", "tools": tools, "input": []})
        self.assertIn("native_tool_schema", script.violations)

    def test_auxiliary_requests_never_advance(self):
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        self.assertEqual(script.decide("chat_completions", {"model": "fixture-x-source", "messages": []}),
                         {"text": "ok"})
        self.assertEqual((script.calls, script.observations["phase_routes"]), (0, {}))

    def test_history_counts_only_the_source_cat_result(self):
        nonce = "cd" * 24
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        # The value in user or prompt text, with no correlated source tool result, is missing history.
        spec = script.decide("messages", {"model": "fixture-x-target", "tools": TOOLS["messages"],
                                          "messages": [{"role": "user", "content": "remember " + nonce}]})
        self.assertEqual(spec, {"text": "fixture stopped"})
        self.assertIn("resume_history_missing", script.violations)

    def test_uncorrelated_tool_result_is_a_violation(self):
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        messages = [{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_other",
                                                  "content": "ef" * 24}]}]
        script.decide("messages", {"model": "fixture-x-target", "tools": TOOLS["messages"], "messages": messages})
        self.assertEqual(script.violations, ["tool_result_correlation"])

    def test_source_challenge_needs_exactly_one_value(self):
        self.assertEqual(ncr.source_challenge("exit 0\n" + "ab" * 24 + "\n"), "ab" * 24)
        self.assertIsNone(ncr.source_challenge("ab" * 24 + " " + "cd" * 24))
        self.assertIsNone(ncr.source_challenge(None))

    def target_after(self, mutate, protocol="messages"):
        """Run a source phase, mutate its history, send the first target request."""
        with tempfile.TemporaryDirectory() as temp:
            script, harness, nonce = source_phase(protocol, temp)
            mutate(harness.history, nonce)
            harness.user("Write the original value to resumed.txt.")
            spec = script.decide(protocol, harness.request("fixture-x-target"))
            return script, spec

    @staticmethod
    def first_block(history, kind):
        return next(block for m in history if isinstance(m["content"], list)
                    for block in m["content"] if block["type"] == kind)

    def test_source_round_trip_survives_resume(self):
        script, spec = self.target_after(lambda history, nonce: None)
        self.assertIn("tool", spec)
        self.assertTrue(script.observations["history_retained"])

    def test_history_rejects_anything_but_the_source_round_trip(self):
        def strip_results(history, nonce):
            history[:] = [m for m in history if not (isinstance(m["content"], list)
                                                     and m["content"][0]["type"] == "tool_result")]

        def only_assistant_arguments(history, nonce):
            history[:] = [{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_x", "name": "Bash",
                                                              "input": {"command": "echo " + nonce}}]}]

        def only_user_text(history, nonce):
            history[:] = [{"role": "user", "content": "The value was " + nonce}]

        def unrelated_hex(history, nonce):
            self.first_block(history, "tool_result")["content"] = "ab" * 24

        def failed_result(history, nonce):
            self.first_block(history, "tool_result")["is_error"] = True

        def renamed_call(history, nonce):
            self.first_block(history, "tool_use")["name"] = "Other"

        cases = {"missing source result": (strip_results, "resume_history_missing"),
                 "nonce only in assistant arguments": (only_assistant_arguments, "resume_history_missing"),
                 "nonce only in user text": (only_user_text, "resume_history_missing"),
                 "unrelated hex in the cat result": (unrelated_hex, "resume_history_missing"),
                 "failed cat result": (failed_result, "cat_round_trip_mismatch"),
                 "cat call under another tool name": (renamed_call, "cat_round_trip_mismatch")}
        for name, (mutate, code) in cases.items():
            with self.subTest(name=name):
                script, spec = self.target_after(mutate)
                self.assertEqual(spec, {"text": "fixture stopped"})
                self.assertIn(code, script.violations)
                self.assertFalse(script.observations["history_retained"])

    def test_wrong_call_id_is_uncorrelated(self):
        def rename_ids(history, nonce):
            self.first_block(history, "tool_result")["tool_use_id"] = "toolu_other"
        script, spec = self.target_after(rename_ids)
        self.assertEqual(script.violations, ["tool_result_correlation"])

    def test_codex_tool_shapes_follow_the_offered_schema(self):
        cases = {"shell": {"command": ["bash", "-lc", "ls"], "workdir": "/w"},
                 "shell_command": {"command": "ls", "workdir": "/w"},
                 "exec_command": {"cmd": "ls", "workdir": "/w", "yield_time_ms": 30000}}
        for name, arguments in cases.items():
            with self.subTest(name=name):
                tools = [{"type": "function", "name": name, "parameters": {"properties": dict.fromkeys(arguments)}}]
                self.assertEqual(ncr.tool_call("responses", {"tools": tools}, "ls", "/w"), (name, arguments))


def sse_data(body):
    return [json.loads(line[6:]) for line in body.decode().splitlines()
            if line.startswith("data: ") and line != "data: [DONE]"]


class WireTests(unittest.TestCase):
    TOOL = {"tool": {"id": "call_fixture0001", "name": "shell", "arguments": {"command": "ls"}}}

    def test_messages_tool_and_text(self):
        events = sse_data(ncr.messages_sse("m", self.TOOL))
        self.assertEqual(json.loads(events[2]["delta"]["partial_json"]), {"command": "ls"})
        self.assertEqual(events[4]["delta"]["stop_reason"], "tool_use")
        self.assertEqual(sse_data(ncr.messages_sse("m", {"text": "hi"}))[4]["delta"]["stop_reason"], "end_turn")

    def test_responses_function_call_and_text(self):
        events = sse_data(ncr.responses_sse(self.TOOL))
        done = [e for e in events if e["type"] == "response.completed"][0]["response"]["output"][0]
        self.assertEqual((done["type"], done["call_id"], json.loads(done["arguments"])),
                         ("function_call", "call_fixture0001", {"command": "ls"}))
        self.assertEqual([e["sequence_number"] for e in events], list(range(len(events))))
        text = sse_data(ncr.responses_sse({"text": "hi"}))
        self.assertEqual(text[-1]["response"]["output"][0]["content"][0]["text"], "hi")

    def test_chat_tool_call_and_text(self):
        chunks = sse_data(ncr.chat_sse("m", self.TOOL))
        call = chunks[0]["choices"][0]["delta"]["tool_calls"][0]
        self.assertEqual((call["id"], json.loads(call["function"]["arguments"])), ("call_fixture0001", {"command": "ls"}))
        self.assertEqual(chunks[1]["choices"][0]["finish_reason"], "tool_calls")
        self.assertTrue(ncr.chat_sse("m", {"text": "hi"}).endswith(b"data: [DONE]\n\n"))


def passing_result(target_route="fixture-x-target"):
    resume = {"status": "PASS", "session_id": "s1", "observed_routes": [target_route], "exact_route_protocol": True,
              "fresh_tool_result_consumed": True}
    return {"status": "PASS", "session_id": "s1", "initial_cleanup": "clean", "cleanup": "clean",
            "filesystem_assertion": True, "tool_results": 2, "resume_assertion": True,
            "exact_route_protocol": True, "resume": resume}


class EvaluationTests(unittest.TestCase):
    def script(self):
        script = ncr.Script("fixture-x-source", "fixture-x-target", "/tmp")
        script.observations.update(history_retained=True, phase_routes={"target": {"fixture-x-target"}})
        return script

    def evaluate(self, result, script=None):
        return ncr.evaluate(result, script or self.script(), {"route": "fixture-x-source"},
                            {"route": "fixture-x-target"})

    def test_all_facts_pass(self):
        checks, status = self.evaluate(passing_result())
        self.assertEqual(status, "PASS", checks)

    def test_each_missing_fact_fails(self):
        mutations = {
            "unclean source": lambda r: r.update(initial_cleanup="failed"),
            "unknown resume cleanup": lambda r: r.update(cleanup="unknown"),
            "different session": lambda r: r["resume"].update(session_id="s2"),
            "no recall": lambda r: r.update(resume_assertion=False),
            "source route in resume": lambda r: r["resume"].update(observed_routes=["fixture-x-source",
                                                                                    "fixture-x-target"]),
            "wrong protocol": lambda r: r["resume"].update(exact_route_protocol=False),
            "no fresh tool result": lambda r: r["resume"].update(fresh_tool_result_consumed=False),
            "no source tool round trip": lambda r: r.update(filesystem_assertion=False),
            "resume failed": lambda r: r["resume"].update(status="FAIL"),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                result = passing_result()
                mutate(result)
                self.assertEqual(self.evaluate(result)[1], "FAIL")

    def test_missing_history_or_violation_fails(self):
        script = self.script()
        script.observations["history_retained"] = False
        self.assertEqual(self.evaluate(passing_result(), script)[1], "FAIL")
        script = self.script()
        script.violate("unexpected_protocol")
        self.assertEqual(self.evaluate(passing_result(), script)[1], "FAIL")

    def test_source_cleanup_failure_survives_a_clean_resume(self):
        result = passing_result()
        result.update(initial_cleanup="forced")
        self.assertEqual(ncr.phase_cleanups(result), ("forced", "clean"))
        self.assertEqual(self.evaluate(result)[1], "FAIL")
        self.assertEqual(ncr.phase_cleanups({"status": "FAIL", "cleanup": "unknown"}), ("unknown", None))

    def test_blocked_stays_blocked(self):
        self.assertEqual(self.evaluate({"status": "BLOCKED", "reason": "x"})[1], "BLOCKED")

    def test_report_is_never_certification(self):
        with tempfile.TemporaryDirectory() as temp:
            clean = {"timestamp": "t", "gateway_stopped": True, "provider_stopped": True}
            passed = ncr.write_report(Path(temp), [{"id": "a", "status": "PASS"}], clean)
            document = json.loads((Path(temp) / "report.json").read_text())
            self.assertTrue(passed)
            self.assertEqual((document["classification"], document["certified"]),
                             ("native_diagnostic_not_certification", False))
            self.assertFalse(ncr.write_report(Path(temp), [{"id": "a", "status": "BLOCKED"}], clean))

    def test_server_shutdown_gates_the_overall_result(self):
        for meta in ({"gateway_stopped": True, "provider_stopped": False},
                     {"gateway_stopped": False, "provider_stopped": True},
                     {"provider_stopped": True}, {}):
            with self.subTest(meta=meta), tempfile.TemporaryDirectory() as temp:
                self.assertFalse(ncr.write_report(Path(temp), [{"id": "a", "status": "PASS"}], meta))
                report = json.loads((Path(temp) / "report.json").read_text())
                junit = (Path(temp) / "junit.xml").read_text()
                self.assertEqual(report["overall"], "FAIL")
                self.assertIn('name="fixture-server-shutdown"><failure', junit)


class SetupTests(unittest.TestCase):
    def test_pairs_are_distinct_same_harness_models(self):
        for harness in ncr.PROTOCOLS:
            source, target = ncr.pair_cells(harness)
            self.assertEqual((source["harness"], target["harness"]), (harness, harness))
            self.assertNotEqual(source["route"], target["route"])
            self.assertNotEqual(source["canonical_model"], target["canonical_model"])

    def test_catalog_declares_each_harness_and_codex_template(self):
        from datetime import datetime, timezone
        catalog = ncr.fixture_catalog(datetime.now(timezone.utc), "gpt-template")
        capabilities = {s["gateway_route"]: s["details"]["harnesses"] for m in catalog["models"] for s in m["sources"]}
        self.assertEqual(capabilities["fixture-codex-target"]["codex"],
                         {"wire_api": "responses", "reasoning_levels": ["low"], "default_reasoning": "low",
                          "catalog_template": "gpt-template"})
        self.assertEqual(capabilities["fixture-claude-source"]["claude"]["wire_api"], "messages")
        self.assertEqual(capabilities["fixture-opencode-source"]["opencode"]["wire_api"], "chat_completions")

    def test_codex_template_slug(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "models.json"
            path.write_text(json.dumps({"models": [{"slug": "gpt-a"}, {"slug": "gpt-b"}]}))
            self.assertEqual(ncr.codex_template_slug(path), "gpt-a")
            self.assertIsNone(ncr.codex_template_slug(Path(temp) / "missing.json"))

    def test_managed_claude_flags_on_launch_only(self):
        runner = ncr.ManagedRunner.__new__(ncr.ManagedRunner)
        runner.hrx, runner.claude_dirs = "hrx", {"native-claude": "/home/claude"}
        source, _ = ncr.pair_cells("claude")
        launch = runner.command(source, "prompt")
        at = launch.index("--")
        self.assertEqual(launch[at - 4:at], ["--claude-config-dir", "/home/claude", "--claude-init",
                                             "theme,trust,bypass"])
        self.assertNotIn("--claude-init", runner.command(source, "prompt", resume="s1"))
        codex, _ = ncr.pair_cells("codex")
        self.assertNotIn("--claude-init", runner.command(codex, "prompt"))


class FakeGateway:
    def __init__(self, root, upstream, key, catalog, policy):
        self.url, self.deadline, self.allowed_routes = "https://127.0.0.1:1", time.monotonic() + 60, set()
        self.thread = threading.Thread()
        self.lock, self.active = threading.Condition(), 0

    def close(self):
        pass


class FakeProvider:
    url = "http://127.0.0.1:2"

    def __init__(self, scripts):
        pass

    def close(self):
        return True


class ControllerTests(unittest.TestCase):
    def run_with(self, pair, gateway=FakeGateway, provider=FakeProvider):
        rows, meta = [], {}
        with tempfile.TemporaryDirectory() as temp, \
                patch.dict(os.environ, {"HRX_CI_CODEX_CATALOG": ""}), \
                patch.object(ncr, "Gateway", gateway), patch.object(ncr, "Provider", provider), \
                patch.object(ncr, "ManagedRunner", lambda *args: None), \
                patch.object(ncr, "run_pair", side_effect=pair), patch.object(ncr, "SHUTDOWN_SECONDS", .3), \
                patch.object(ncr, "observe_relay", lambda gateway: []):
            try:
                ncr.run("hrx", Path(temp), ["claude", "opencode"], rows, meta)
            except Exception as error:
                meta["raised"] = type(error).__name__
        return {row["harness"]: (row["status"], row["reason"]) for row in rows}, meta

    @staticmethod
    def passing(runner, gateway, script, harness, root, row, policy):
        row.update(status="PASS", reason=None, cleanup={"source": "clean", "resume": "clean"})
        return True

    def test_later_pair_exception_keeps_earlier_evidence(self):
        def pair(*args):
            if args[3] == "opencode":
                raise OSError("injected")
            return self.passing(*args)
        rows, meta = self.run_with(pair)
        self.assertEqual(rows, {"claude": ("PASS", None), "opencode": ("FAIL", "controller_error:OSError")})
        self.assertEqual((meta["gateway_stopped"], meta["provider_stopped"]), (True, True))

    def test_unknown_cleanup_leaves_unreached_pairs_not_run(self):
        def unknown(runner, gateway, script, harness, root, row, policy):
            row.update(status="FAIL", reason="native_cleanup_unknown")
            return False
        rows, _ = self.run_with(unknown)
        self.assertEqual(rows, {"claude": ("FAIL", "native_cleanup_unknown"), "opencode": ("NOT_RUN", "not_reached")})

    def test_hanging_gateway_close_is_bounded_and_recorded(self):
        class Hanging(FakeGateway):
            def close(self):
                time.sleep(5)
        started = time.monotonic()
        rows, meta = self.run_with(self.passing, gateway=Hanging)
        self.assertLess(time.monotonic() - started, 3)
        self.assertEqual((meta["gateway_stopped"], meta["provider_stopped"]), (False, True))

    def test_active_gateway_request_is_not_stopped(self):
        class Busy(FakeGateway):
            def close(self):
                self.active = 1  # The listener stops but an upstream request is in flight.
        started = time.monotonic()
        rows, meta = self.run_with(self.passing, gateway=Busy)
        self.assertLess(time.monotonic() - started, 3)
        self.assertEqual((meta["gateway_stopped"], meta["provider_stopped"]), (False, True))

    def test_gateway_request_finishing_within_the_bound_is_stopped(self):
        class Draining(FakeGateway):
            def close(self):
                self.active = 1

                def finish():
                    time.sleep(.05)
                    with self.lock:
                        self.active = 0
                        self.lock.notify_all()
                threading.Thread(target=finish, daemon=True).start()
        rows, meta = self.run_with(self.passing, gateway=Draining)
        self.assertTrue(meta["gateway_stopped"])

    def test_setup_failure_still_closes_the_provider(self):
        class Broken(FakeGateway):
            def __init__(self, *args):
                raise OSError("injected")
        rows, meta = self.run_with(self.passing, gateway=Broken)
        self.assertEqual(meta["raised"], "OSError")
        self.assertEqual(rows, {"claude": ("NOT_RUN", "not_reached"), "opencode": ("NOT_RUN", "not_reached")})
        self.assertNotIn("gateway_stopped", meta)
        self.assertTrue(meta["provider_stopped"])


class DiagnosticTests(unittest.TestCase):
    SECRET = "sk-" + "Zq9" * 12

    def test_tls_reason_allowlist_preserves_alerts_without_echoing_values(self):
        from types import SimpleNamespace
        import ssl

        for reason in ("TLSV1_ALERT_UNKNOWN_CA", "SSLV3_ALERT_BAD_CERTIFICATE", self.SECRET, None):
            with self.subTest(reason=reason):
                error = ssl.SSLError("private error detail")
                error.reason = reason

                def reject():
                    raise error

                server = SimpleNamespace(RequestHandlerClass=http.server.BaseHTTPRequestHandler,
                                         get_request=reject)
                log = ncr.observe_relay(SimpleNamespace(server=server))
                with self.assertRaises(ssl.SSLError):
                    server.get_request()
                self.assertEqual(log[0]["reason"], reason if reason in ncr.TLS_REASONS else "<other>")
                self.assertNotIn(self.SECRET, json.dumps(log))
                self.assertNotIn("private error detail", json.dumps(log))

    def test_paths_only_retain_known_endpoints(self):
        for path in ("/" + "ab" * 24, "/v1/" + self.SECRET, "/private/thread-id", "/v1/responses/secret"):
            with self.subTest(path=path):
                self.assertEqual(ncr.diagnostic_path(path), "<other>")
        self.assertEqual(ncr.diagnostic_path("/v1/responses?token=" + self.SECRET), "/v1/responses")

    def test_unknown_event_and_item_types_cannot_carry_values(self):
        sentinel = "private.message.content"
        shape = ncr.output_shape((json.dumps({"type": sentinel}) + "\n" +
                                  json.dumps({"type": "item.completed", "item": {"type": "ab" * 24}})).encode())
        self.assertEqual(shape["event_types"], ["<other>", "item.completed:<other>"])
        self.assertNotIn(sentinel, json.dumps(shape))
        self.assertNotIn("ab" * 24, json.dumps(shape))

    def test_redaction_keeps_only_allowlisted_words(self):
        text = (f"Error: TLS handshake failed: certificate verify failed for https://127.0.0.1:43123/v1 "
                f"key {self.SECRET} nonce {'ab' * 24} /tmp/hrx-native-cross-resume-x/home")
        redacted = ncr.redact(text)
        # Short numbers (status codes, loopback octets) survive by design; values never do.
        for leaked in (self.SECRET, "ab" * 24, "43123", "native-cross-resume-x"):
            self.assertNotIn(leaked, redacted)
        for kept in ("Error", "TLS", "handshake", "certificate", "verify", "failed"):
            self.assertIn(kept, redacted)
        self.assertLessEqual(len(ncr.redact("error " * 2000)), 2048)

    def test_output_shape_reports_types_never_content(self):
        stdout = "\n".join([json.dumps({"type": "thread.started", "thread_id": "secret-thread"}),
                            json.dumps({"type": "error", "message": "stream disconnected: " + self.SECRET}),
                            json.dumps({"type": "item.completed", "item": {"type": "agent_message",
                                                                            "text": "the value " + "cd" * 24}}),
                            "plain text " + self.SECRET]).encode()
        shape = ncr.output_shape(stdout)
        self.assertEqual(shape["event_types"], ["thread.started", "error", "item.completed:agent_message"])
        self.assertEqual((shape["lines"], shape["json_lines"]), (4, 3))
        dumped = json.dumps(shape)
        for leaked in (self.SECRET, "secret-thread", "cd" * 24, "the value"):
            self.assertNotIn(leaked, dumped)
        self.assertIn("stream disconnected", shape["error_messages"][0])

    def test_relay_observer_records_rejections_and_tls_failures(self):
        import socket
        import ssl

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(403)
                self.end_headers()

        with tempfile.TemporaryDirectory() as temp:
            cert, key = Path(temp) / "cert.pem", Path(temp) / "key.pem"
            subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                            "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
                            "-keyout", str(key), "-out", str(cert)], check=True, capture_output=True)
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(cert, key)
            server.socket = context.wrap_socket(server.socket, server_side=True)
            gateway = type("G", (), {"server": server})()
            log = ncr.observe_relay(gateway)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                port = server.server_address[1]
                client = ssl.create_default_context(cafile=str(cert))
                connection = http.client.HTTPSConnection("127.0.0.1", port, context=client, timeout=5)
                connection.request("GET", "/v1/responses?token=" + self.SECRET, headers={"Upgrade": "websocket"})
                self.assertEqual(connection.getresponse().status, 403)
                connection.close()
                untrusted = ssl.create_default_context()  # Does not trust the fixture certificate.
                with socket.create_connection(("127.0.0.1", port), timeout=5) as raw, \
                        self.assertRaises(ssl.SSLError):
                    untrusted.wrap_socket(raw, server_hostname="127.0.0.1")
                deadline = time.monotonic() + 5
                while len(log) < 2 and time.monotonic() < deadline:
                    time.sleep(.05)
            finally:
                server.shutdown()
                server.server_close()
        self.assertEqual(log[0], {"method": "GET", "status": 403, "path": "/v1/responses", "upgrade": True})
        self.assertEqual(log[1]["accept_error"], "SSLError")
        self.assertNotIn(self.SECRET, json.dumps(log))

    def test_launch_stage_from_hrx_markers_only(self):
        exec_line = b"hrx: test/agent -> fixture-codex-source; reasoning=low; provider=ci\n"
        cases = {
            exec_line + b"codex noise " + self.SECRET.encode() + b"\n": ("launch_marker_observed",
                                                                         ["hrx_launch_marker"]),
            b"hrx: launch validation failed: " + self.SECRET.encode() + b"\n": ("launch_marker_not_observed",
                                                                               ["hrx_unclassified"]),
            b"hrx: codex catalog skipped fixture-codex-source: gateway context limit unknown\n" + exec_line:
                ("launch_marker_observed", ["hrx_codex_catalog_skipped", "hrx_launch_marker"]),
            b"hrx: session=abc; private metadata will be indexed at process start\n":
                ("launch_marker_not_observed", ["hrx_session_capture"]),
            b"": ("launch_marker_not_observed", []),
            b"x" * ncr.CAPTURE_TAIL: ("unknown_stderr_truncated", []),
            b"hrx: informational notice\n": ("launch_marker_not_observed", ["hrx_unclassified"]),
            b"x" * ncr.CAPTURE_TAIL + b"\nhrx: unknown status\n":
                ("unknown_stderr_truncated", ["hrx_unclassified"]),
        }
        for stderr, (stage, markers) in cases.items():
            with self.subTest(stage=stage, markers=markers):
                found = ncr.launch_stage(stderr)
                self.assertEqual((found["stage"], found["markers"]), (stage, markers))
                self.assertNotIn(self.SECRET, json.dumps(found))

    def test_runner_default_adds_no_diagnostics(self):
        self.assertFalse(ncr.Runner.capture_stderr)
        self.assertIsNone(ncr.Runner.diagnose(None, {}, {"stdout": b"x"}))
        self.assertTrue(ncr.ManagedRunner.capture_stderr)


class GuardTests(unittest.TestCase):
    def test_provider_rejects_oversized_or_unsized_bodies(self):
        provider = ncr.Provider({})
        try:
            port = int(provider.url.rsplit(":", 1)[1])
            for headers in ({"Content-Length": str(ncr.MAX_REQUEST_BYTES + 1)}, {}):
                with self.subTest(headers=headers):
                    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
                    connection.putrequest("POST", "/v1/messages")
                    for name, value in headers.items():
                        connection.putheader(name, value)
                    connection.endheaders()
                    self.assertEqual(connection.getresponse().status, 413)
                    connection.close()
        finally:
            self.assertTrue(provider.close(seconds=5))

    def test_endpoints_must_be_loopback(self):
        for url in ("http://127.0.0.1:8080", "https://127.0.0.1:443/v1"):
            ncr.require_loopback(url)
        for url in ("https://api.anthropic.com", "http://10.0.0.1:80", "http://127.0.0.1.evil:80"):
            with self.subTest(url=url), self.assertRaises(ncr.Violation):
                ncr.require_loopback(url)

    def test_child_environment_is_fixture_only(self):
        runner = ncr.ManagedRunner.__new__(ncr.ManagedRunner)
        with tempfile.TemporaryDirectory() as temp:
            runner.gateway = type("G", (), {"url": "https://127.0.0.1:1", "secret": "relay", "cert": "/c"})()
            source, _ = ncr.pair_cells("claude")
            env = runner.configure(Path(temp) / "home", Path(temp) / "project", source)
            self.assertLessEqual(set(env), ncr.CHILD_ENV)
            with patch.object(ncr.Runner, "isolated_env", lambda self, home: {**env, "ANTHROPIC_API_KEY": "k"}):
                with self.assertRaises(ncr.Violation):
                    runner.configure(Path(temp) / "home2", Path(temp) / "project2", source)

    def test_provider_shutdown_is_bounded(self):
        provider = ncr.Provider({})
        ncr.require_loopback(provider.url)
        self.assertTrue(provider.close(seconds=5))

    def test_unauthorized_run_is_blocked(self):
        with tempfile.TemporaryDirectory() as temp:
            env = {k: v for k, v in os.environ.items() if k != "HRX_NATIVE_CROSS_RESUME_AUTHORIZED"}
            done = subprocess.run([sys.executable, str(Path(ncr.__file__)), "--hrx", "hrx", "--output", temp],
                                  env=env, capture_output=True, text=True, timeout=60)
            report = json.loads((Path(temp) / "report.json").read_text())
            self.assertEqual((done.returncode, report["rows"][0]["reason"]), (1, "not_authorized_native_fixture"))


if __name__ == "__main__":
    unittest.main()
