"""Pure compaction composition tests; all native/provider work is simulated."""

import json
from pathlib import Path
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import claude_attention as attention
import claude_compaction_control as control
import claude_compaction_hook as hook_capture
import test_claude_attention as attention_tests
from test_claude_compaction_receipts import SESSION, boundary, hook


class ControlTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.project = self.root / "project"
        self.project.mkdir(mode=0o700)
        self.directory = self.root / "claude"
        self.script = SimpleNamespace(
            lock=threading.Lock(), step=attention.ARMED, task=("task1", "/synthetic/output"),
            _decide=Mock(return_value={"text": "Waiting for the next Pitput event."}),
            violate=Mock(), records=[], violations=[], done=True,
        )
        self.api = SimpleNamespace(ARMED=attention.ARMED, text_of=attention.text_of,
                                   ROUND_ONE_DONE=attention.ROUND_ONE_DONE,
                                   ROUND_TWO_DONE=attention.ROUND_TWO_DONE,
                                   latest_dialogue_message=attention.latest_dialogue_message,
                                   priming_location=attention.priming_location,
                                   private=attention.private,
                                   managed_session_id=Mock(return_value=SESSION))
        self.driver = control.Control(self.script, self.api)
        self.env = {"HOME": str(self.root), "CLAUDE_CONFIG_DIR": str(self.directory),
                    "DISABLE_COMPACT": "1", "DISABLE_AUTO_COMPACT": "1"}
        self.driver.configure(self.root, self.project, self.env)

    def main_request(self, primed=True):
        return {"system": " ".join(attention.PRIMING_MARKERS) if primed else "missing",
                "tools": [{"name": "Bash"}], "messages": [{"role": "user", "content": "wake"}]}

    def summary_request(self):
        return {"tools": [{"name": "Bash"}], "messages": [{"role": "user", "content":
                control.PREFIX + "Synthetic summary instructions" + control.SUFFIX}]}

    def capture(self, event, **changes):
        hook_capture.capture(hook(event, cwd=str(self.project), **changes),
                             self.driver.journal, expected_cwd=str(self.project))

    def trigger(self):
        response = self.driver.decide(self.main_request())
        self.assertEqual(response["usage"], {"input_tokens": 90000, "output_tokens": 1})
        self.script.step += 1
        self.script._decide.return_value = {"text": "Waiting for the next Pitput event."}

    def test_explicit_fixture_settings_and_no_disable_flags(self):
        settings = json.loads((self.project / ".claude/settings.local.json").read_text())
        self.assertEqual(set(settings["hooks"]), {"PreCompact", "PostCompact"})
        self.assertFalse(settings["idleCompaction"])
        self.assertFalse(settings["precomputeCompactionEnabled"])
        self.assertEqual(self.env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "100000")
        self.assertNotIn("DISABLE_COMPACT", self.env)
        self.assertNotIn("DISABLE_AUTO_COMPACT", self.env)
        self.assertFalse(self.directory.exists())

    def test_summary_with_tools_requires_trigger_and_pre_hook(self):
        self.driver.decide(self.summary_request())
        self.script.violate.assert_called_with("compaction_control_refused")
        self.assertEqual(self.driver.summaries, 0)
        self.trigger()
        self.driver.decide(self.summary_request())
        self.assertEqual(self.driver.summaries, 0)
        self.capture("PreCompact")
        result = self.driver.decide(self.summary_request())
        self.assertEqual(result["text"], control.SUMMARY)
        self.assertEqual(self.driver.summaries, 1)
        self.driver.decide(self.summary_request())
        self.assertEqual(self.driver.summaries, 1)

    def test_summary_text_block_with_cached_tool_result(self):
        self.trigger()
        self.capture("PreCompact")
        request = self.summary_request()
        prompt = request["messages"][-1]["content"]
        request["messages"][-1]["content"] = [
            {"type": "tool_result", "tool_use_id": "cached", "content": "old result"},
            {"type": "text", "text": prompt}]
        before = self.script._decide.call_count
        result = self.driver.decide(request)
        self.assertEqual(result["text"], control.SUMMARY)
        self.assertEqual(self.driver.summaries, 1)
        self.assertEqual(self.script._decide.call_count, before)
        for content in [
                [{"type": "tool_result", "content": prompt}],
                [{"type": "text", "text": prompt}, {"type": "text", "text": "extra"}],
                [{"type": "text", "text": prompt}] * 2,
                [{"type": "text", "text": "prefix " + prompt}],
                [{"type": "text", "text": prompt + " suffix"}],
                [{"type": "image"}, {"type": "text", "text": prompt}]]:
            with self.subTest(content=content):
                self.assertFalse(control.summary_prompt({"role": "user", "content": content}))
        self.assertFalse(control.summary_prompt({"role": "assistant", "content": prompt}))

    def test_post_summary_requires_post_hook_and_actual_priming(self):
        self.trigger()
        self.capture("PreCompact")
        self.driver.decide(self.summary_request())
        self.driver.decide(self.main_request())
        self.assertIsNone(self.driver.resumed_at_ns)
        self.capture("PostCompact")
        self.driver.decide(self.main_request(primed=False))
        self.assertIsNone(self.driver.resumed_at_ns)
        self.driver.decide(self.main_request())
        self.assertIsNotNone(self.driver.resumed_at_ns)

    def test_foreign_pre_hook_cannot_start_summary(self):
        self.trigger()
        self.capture("PreCompact", session_id="aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
        self.driver.decide(self.summary_request())
        self.assertEqual(self.driver.summaries, 0)
        self.script.violate.assert_called_with("compaction_control_refused")

    def test_original_monitor_must_survive_until_resumed_request(self):
        self.trigger()
        self.capture("PreCompact")
        self.driver.decide(self.summary_request())
        self.capture("PostCompact")
        self.script.task = ("new-task", "/synthetic/other")
        self.driver.decide(self.main_request())
        self.script.violate.assert_called_with("compaction_control_refused")
        self.assertIsNone(self.driver.resumed_at_ns)
        calls = self.script._decide.call_count
        self.driver.decide(self.main_request())
        self.assertIsNone(self.driver.resumed_at_ns)
        self.assertEqual(self.script._decide.call_count, calls)

    def completion(self, message_id, number=1, parent=None):
        return {"type": "assistant", "sessionId": SESSION,
                "version": control.receipts.VERSION,
                "uuid": "bbbbbbbb-bbbb-bbbb-bbbb-%012d" % number,
                "parentUuid": parent or json.loads(boundary())["uuid"],
                "message": {"id": message_id, "stop_reason": "end_turn", "content": [
                    {"type": "text", "text": "Waiting for the next Pitput event."}]}}

    def write_transcript(self, *rows):
        folder = self.directory / "projects/owned"
        folder.mkdir(parents=True, mode=0o700, exist_ok=True)
        path = folder / (SESSION + ".jsonl")
        path.write_bytes(boundary() + b"\n" + b"".join(
            json.dumps(row).encode() + b"\n" for row in rows))

    def test_native_boundary_and_exact_round_completions_required(self):
        self.trigger()
        self.capture("PreCompact")
        self.driver.decide(self.summary_request())
        self.capture("PostCompact")
        self.script.step = attention.ROUND_ONE_DONE
        first = self.driver.decide(self.main_request())["response_id"]
        self.script.step = attention.ROUND_TWO_DONE
        # Mock returns a fresh spec, as the actual Script does.
        self.script._decide.return_value = {"text": "Waiting for the next Pitput event."}
        final = self.driver.decide(self.main_request())["response_id"]
        row1 = self.completion(first)
        row2 = self.completion(final, 2, row1["uuid"])
        self.write_transcript(row1)
        with self.assertRaisesRegex(ValueError, "continuation_not_completed"):
            self.driver.finish()
        failed = self.driver.failure_receipt()
        self.assertEqual(failed["status"], "FAIL")
        self.assertEqual(failed["stage"], "completion_gates")
        self.assertEqual(len(failed["hooks"]), 2)
        self.assertEqual(len(failed["completions"]), 1)
        self.assertNotIn(final, json.dumps(failed))
        self.write_transcript(row1, row2)
        result = self.driver.finish()
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(len(result["completions"]), 2)
        self.assertNotIn(final, json.dumps(result))

    def test_completion_refuses_missing_wrong_and_off_chain_identity(self):
        ids = ["msg_first", "msg_final"]
        first = self.completion(ids[0])
        final = self.completion(ids[1], 2, first["uuid"])
        for change in [{"id": None}, {"id": "msg_other"}]:
            with self.subTest(change=change):
                altered = dict(final, message=dict(final["message"], **change))
                self.write_transcript(first, altered)
                self.assertEqual(len(control.transcript(self.directory, SESSION, ids)[1]), 1)
        for change, reason in [({"uuid": None}, "completion_off_chain"),
                               ({"parentUuid": "unrelated"}, "completion_off_chain"),
                               ({"version": "other"}, "wrong_completion_version"),
                               ({"isSidechain": True}, "nonlocal_completion"),
                               ({"remoteSourced": True}, "nonlocal_completion"),
                               ({"message": []}, "invalid_completion_message"),
                               ({"message": {"content": None}}, "invalid_completion_message")]:
            with self.subTest(change=change):
                self.write_transcript(first, dict(final, **change))
                with self.assertRaisesRegex(ValueError, reason):
                    control.transcript(self.directory, SESSION, ids)
        for stop in [None, "tool_use", "error"]:
            self.write_transcript(first, dict(final, message=dict(final["message"], stop_reason=stop)))
            with self.assertRaisesRegex(ValueError, "completion_incomplete"):
                control.transcript(self.directory, SESSION, ids)
        self.write_transcript(final, first)
        with self.assertRaises(ValueError):
            control.transcript(self.directory, SESSION, ids)
        self.write_transcript(first, final, final)
        with self.assertRaisesRegex(ValueError, "completion_order"):
            control.transcript(self.directory, SESSION, ids)

    def test_renderers_keep_exact_response_identity(self):
        spec = {"text": "done", "response_id": "msg_bound123"}
        self.assertEqual(attention.message("fixture", spec)["id"], spec["response_id"])
        wire = attention.sse("fixture", spec).decode()
        start = json.loads(wire.split("data: ", 1)[1].split("\n", 1)[0])
        self.assertEqual(start["message"]["id"], spec["response_id"])
        for value in [None, "", "wrong", "msg_" + "x" * 65]:
            for renderer in [attention.message, attention.sse]:
                with self.assertRaises(ValueError):
                    renderer("fixture", dict(spec, response_id=value))

    def test_failure_shape_diagnostics_never_include_text_or_session(self):
        self.trigger()
        self.capture("PreCompact")
        request = self.summary_request()
        request["messages"][-1]["content"] = [
            {"type": "text", "text": request["messages"][-1]["content"]},
            {"type": "text", "text": "private synthetic suffix"}]
        self.driver.decide(request)
        receipt = self.driver.failure_receipt()
        shape = receipt["request_shapes"][-1]
        self.assertTrue(shape["contains_suffix"])
        self.assertFalse(shape["aggregate_suffix"])
        self.assertTrue(shape["text_blocks"][0]["suffix"])
        self.assertEqual(receipt["hook_progress"]["events"], [
            {"event": "PreCompact", "automatic": True, "session_matches": True,
             "project_matches": True}])
        encoded = json.dumps(receipt)
        for private in ["private synthetic suffix", SESSION, str(self.project), control.PREFIX]:
            self.assertNotIn(private, encoded)

    def test_tail_diagnostics_locate_notification_without_accepting_it(self):
        request = self.main_request()
        request["messages"] = [
            {"role": "user", "content": "<task-notification>private task</task-notification>"},
            {"role": "assistant", "content": "Waiting for the next Pitput event."}]
        self.driver.decide(request)
        tail = self.driver.failure_receipt()["request_shapes"][-1]["tail"]
        self.assertEqual([row["role"] for row in tail], ["user", "assistant"])
        self.assertTrue(tail[0]["task_notification"])
        self.assertTrue(tail[1]["idle_text"])
        self.assertNotIn("private task", json.dumps(tail))
        self.assertEqual(self.driver.summaries, 0)

    def test_real_attention_plan_accepts_trailing_system_entries(self):
        original = attention.Script.decide
        def with_system(script, request):
            return original(script, dict(request, messages=[*request.get("messages", []),
                {"role": "system", "content": "synthetic system context"}]))
        with patch.object(attention.Script, "decide", with_system):
            script, _ = attention_tests.full_run()
        self.assertTrue(script.done)
        self.assertEqual(script.violations, [])
        self.assertEqual(len(script.events), 3)

    def test_dialogue_selection_never_skips_assistant_or_unknown_role(self):
        user = {"role": "user", "content": "<task-notification>old</task-notification>"}
        system = {"role": "system", "content": "<task-notification>not a user event</task-notification>"}
        self.assertEqual(attention.latest_dialogue_message({"messages": [user, system]}), user)
        self.assertEqual(attention.latest_dialogue_message({"messages": [system]}), {})
        for last in [{"role": "assistant", "content": "done"}, {"role": "unknown"}, {}]:
            self.assertEqual(attention.latest_dialogue_message({"messages": [user, last, system]}), last)
        self.assertEqual(attention.latest_dialogue_message({"messages": [user, None, system]}), {})

    def test_system_envelope_validation_blocks_malformed_suffix(self):
        user = {"role": "user", "content": "<task-notification>old owned-looking event</task-notification>"}
        invalid = [{}, {"content": None}, {"content": 4}, {"content": {}},
                   {"content": [None]}, {"content": [{"type": "text"}]},
                   {"content": [{"type": "text", "text": 4}]},
                   {"content": [{"type": "tool_addition", "tool": {}}]},
                   {"content": [{"type": "tool_removal", "tool": {"type": "tool_reference", "name": None}}]},
                   {"content": [{"type": "text", "text": "x", "cache_control": None}]},
                   {"content": "x", "output_config": []},
                   {"content": "x", "clear_at": 1}]
        for suffix in invalid:
            with self.subTest(suffix=suffix):
                self.assertEqual(attention.latest_dialogue_message(
                    {"messages": [user, dict(suffix, role="system")]}), {})
        for content in ["text", [], [{"type": "text", "text": "text",
                                      "cache_control": {"type": "ephemeral", "ttl": "1h"}}],
                        [{"type": "tool_removal", "tool": {"type": "tool_reference", "name": "Bash"}}],
                        [{"type": "tool_addition", "tool": {"type": "tool_definition", "definition":
                          {"name": "Bash", "input_schema": {"type": "object"}}}}]]:
            with self.subTest(content=content):
                self.assertEqual(attention.latest_dialogue_message({"messages": [user,
                    {"role": "system", "content": content, "output_config": {},
                     "clear_at": "next_user_message"}]}), user)


if __name__ == "__main__":
    unittest.main()
