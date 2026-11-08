"""Pure scripted-model checks: no native process, socket, file or database."""

import json
import sys
import types
import unittest
from unittest.mock import patch

import claude_attention as base
from claude_compact_task import make_script, recovery_wrapper


class CompactTaskScriptTests(unittest.TestCase):
    def test_recovery_fault_skips_bootstrap_and_initial_native_session(self):
        for backend, native, extra in (("opencode", "ses_hrx_bootstrap", ["--initialize-only"]),
                                       ("opencode", "ses_hrx_bootstrap", []),
                                       ("claude-task", "original", [])):
            parser = types.SimpleNamespace(loads=lambda _: {"backend": backend, "agents": [{"thread_id": native}]})
            with patch.dict(sys.modules, {"tomllib": parser}), \
                    patch.object(sys, "argv", ["pitput", "bridge", "prepare", "--config", "/fixture/config", *extra]), \
                    patch.object(base.Path, "read_text", return_value="config"), \
                    patch("os.execv") as run, patch("os.open") as create:
                exec(compile(recovery_wrapper("/python", "original", "/fixture/marker"), "fixture", "exec"), {})
                create.assert_not_called()
                run.assert_called_once()

    def test_resume_selects_newest_session_not_uuid_order(self):
        records = [{"harness": "claude", "claude_config_dir": "/fixture/claude",
                    "native_id": "aaa-predecessor", "updated_at": "2026-10-25T03:00:00Z"},
                   {"harness": "claude", "claude_config_dir": "/fixture/claude",
                    "native_id": "zzz-successor", "updated_at": "2026-10-25T03:00:00.1Z"}]
        with patch.object(base.Path, "read_text", return_value=json.dumps({"sessions": records})):
            self.assertEqual(base.managed_session_id("/fixture/home", "/fixture/claude"),
                             "zzz-successor")

    def test_clear_requires_fresh_native_hook_commands(self):
        script = make_script(base, "/fixture/new-state", 20,
                             {"first": "FIRST", "second": "SECOND"}, launch=True, handoff=True)
        request = {"tools": [{"name": "Bash"}], "system": " ".join(base.PRIMING_MARKERS),
                   "messages": [{"role": "user", "content": "HRX compact clear fixture"}]}
        with patch.dict(sys.modules, {"ysg_mail.attention_delivery": self.protocol}):
            script.decide(request)
        self.assertIn("clear_hook_commands_missing_from_native_request", script.violations)
        self.assertEqual(script.step, 0)

    def test_clear_accepts_native_hook_with_exact_successor_binding(self):
        script = make_script(base, "/fixture/new-state", 20,
                             {"first": "FIRST", "second": "SECOND"}, launch=True, handoff=True)
        request = {"tools": [{"name": "Bash"}], "system": " ".join(base.PRIMING_MARKERS),
                   "messages": [{"role": "user", "content":
                                 "HRX has retired the predecessor session tasks. "
                                 + self.protocol.PROTOCOL + " " + script.native_id + " " + str(script.config)}]}
        with patch.dict(sys.modules, {"ysg_mail.attention_delivery": self.protocol}):
            result = script.decide(request)
        self.assertTrue(script.handoff_commands_received)
        self.assertEqual(result["tool"]["name"], "Bash")
        self.assertEqual(script.violations, [])

    def setUp(self):
        self.script = make_script(base, "/fixture/state", 20,
                                  {"first": "FIRST-NONCE", "second": "SECOND-NONCE"})
        self.script.events.append({})
        self.handle = "at-" + "a" * 28
        self.protocol = types.ModuleType("ysg_mail.attention_delivery")
        self.protocol.PROTOCOL = "Trusted fixture protocol."

    def presentation(self, handle=None, primer=True):
        handle = handle or self.handle
        routine = (f"Pitput board from=sender/dev/agent ref={handle} why=new_activity\n"
                   f"Read: pitput --agent {base.IDENTITY} attention read {handle}\n"
                   f"Handle ACK: pitput --agent {base.IDENTITY} attention ack {handle}")
        return (self.protocol.PROTOCOL + "\n\n" if primer else "") + routine + "\n"

    def event(self, output):
        with patch.dict(sys.modules, {"ysg_mail.attention_delivery": self.protocol}):
            return self.script.on_result("event", output)

    def test_real_read_establishes_replay_identity(self):
        text = self.presentation()
        self.assertTrue(self.event(text))
        self.assertIsNone(self.script.events[-1]["event_key"])
        fragment = {"source": "board", "complete_message": True, "has_more": False,
                    "start_byte": 0, "handle": "/b/fixture/topic/7", "event_key": "event-7",
                    "body_utf8": "FIRST-NONCE", "end_byte": 11, "total_bytes": 11,
                    "snapshot_version": "snapshot-one", "next_cursor": None}
        self.assertTrue(self.script.on_result("read", json.dumps(fragment)))
        self.script.events.append({})
        self.assertTrue(self.event(text))
        self.assertEqual(self.script.events[-1]["event_key"], "event-7")
        self.assertEqual(self.script.events[-1]["seq"], 7)

    def test_resume_reads_new_handle_and_checks_pending_identity_before_ack(self):
        for event_key in ("original", "wrong-event"):
            script = make_script(base, "/fixture/state", 20,
                                 {"first": "FIRST-NONCE", "second": "SECOND-NONCE"}, resume=True)
            script.resume_received = True
            script.events = [{"event_key": "original"}, {}]
            script.presentations = {"old-handle": "old presentation"}
            script.step = 9  # Replay presentation received; ACK is the next plan action.
            with patch.dict(sys.modules, {"ysg_mail.attention_delivery": self.protocol}):
                self.assertTrue(script.on_result("event", self.presentation()))
            read = script.advance("ack")["tool"]
            self.assertIn("attention read", read["input"]["command"])
            self.assertEqual(script.step, 9)
            fragment = {"source": "board", "complete_message": True, "has_more": False,
                        "start_byte": 0, "handle": "/b/fixture/topic/7", "event_key": event_key,
                        "body_utf8": "FIRST-NONCE", "end_byte": 11, "total_bytes": 11,
                        "snapshot_version": "new-snapshot", "next_cursor": None}
            response = script.decide({"tools": [{"name": "Bash"}], "messages": [
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": read["id"],
                                               "content": json.dumps(fragment)}]}]})
            if event_key == "original":
                self.assertIn("attention ack", response["tool"]["input"]["command"])
                self.assertEqual(script.violations, [])
            else:
                self.assertNotIn("tool", response)
                self.assertIn("resume_pending_event_changed", script.violations)

    def test_changed_replay_is_rejected(self):
        self.assertTrue(self.event(self.presentation()))
        self.assertFalse(self.event(self.presentation().replace("new_activity", "mention")))
        self.assertIn("compact_replay_changed", self.script.violations)

    def test_native_success_footer_is_not_part_of_presentation(self):
        text = self.presentation().rstrip("\n")
        self.assertTrue(self.event(text + "\n\n[exited with code 0]"))
        self.assertTrue(self.event(text))
        self.assertEqual(self.script.violations, [])

    def test_other_native_footer_is_rejected(self):
        self.assertFalse(self.event(self.presentation().rstrip("\n")
                                    + "\n\n[exited with code 1]"))
        self.assertIn("compact_presentation_invalid", self.script.violations)

    def test_partial_read_cannot_count_as_complete(self):
        self.assertTrue(self.event(self.presentation()))
        self.assertFalse(self.script.on_result("read", json.dumps({"source": "board",
                                                                 "complete_message": False})))
        self.assertIn("compact_read_incomplete", self.script.violations)

    def test_continuation_keeps_plan_on_read_until_whole_message(self):
        self.assertTrue(self.event(self.presentation()))
        fragment = {"source": "board", "complete_message": False, "has_more": True,
                    "start_byte": 0, "end_byte": 4, "total_bytes": 15,
                    "handle": "/b/fixture/topic/7", "event_key": "event-7",
                    "snapshot_version": "snapshot-one", "next_cursor": "cursor-one",
                    "body_utf8": "abcd"}
        self.assertTrue(self.script.on_result("read", json.dumps(fragment)))
        step = self.script.step
        command = self.script.advance("arm")["tool"]["input"]["command"]
        self.assertIn("--cursor cursor-one", command)
        self.assertEqual(self.script.step, step)
        self.assertNotIn("body_read", self.script.events[-1])
        fragment.update(start_byte=4, end_byte=15, has_more=False, next_cursor=None,
                        body_utf8="FIRST-NONCE")
        self.assertTrue(self.script.on_result("read", json.dumps(fragment)))
        self.assertTrue(self.script.events[-1]["body_read"])
        self.assertEqual(self.script.events[-1]["read_fragments"], 2)

    def test_continuation_cannot_switch_snapshot(self):
        self.assertTrue(self.event(self.presentation()))
        self.script.snapshot = ("old-snapshot", "event-7", "/b/fixture/topic/7", 11)
        fragment = {"source": "board", "complete_message": True, "has_more": False,
                    "start_byte": 0, "end_byte": 11, "total_bytes": 11,
                    "handle": "/b/fixture/topic/7", "event_key": "event-7",
                    "snapshot_version": "changed-snapshot", "next_cursor": None,
                    "body_utf8": "FIRST-NONCE"}
        self.assertFalse(self.script.on_result("read", json.dumps(fragment)))

    def test_wrong_event_ack_is_rejected(self):
        self.assertTrue(self.event(self.presentation()))
        self.script.event["event_key"] = "expected"
        self.assertFalse(self.script.on_result("ack", json.dumps({"acknowledged": True,
                         "agent_id": base.IDENTITY, "event_key": "other"})))

    def test_next_event_requires_compact_presentation(self):
        self.assertTrue(self.event(self.presentation()))
        self.assertTrue(self.event(self.presentation("at-" + "b" * 28, primer=False)))
        self.assertEqual(self.script.violations, [])

    def test_commands_use_owned_task_and_handle_helpers(self):
        command = self.script.advance("arm")["tool"]["input"]
        self.assertTrue(command["run_in_background"])
        self.assertIn("bridge claude-task", command["command"])
        self.assertIn(self.script.native_id, command["command"])
        self.script.event = {"handle": self.handle}
        command = self.script.advance("read")["tool"]["input"]["command"]
        self.assertEqual(command, f"pitput --agent {base.IDENTITY} attention read {self.handle}"
                                  " --body-byte-limit 64")

    def dm_event(self):
        self.script = make_script(base, "/fixture/state", 20,
                                  {"first": "FIRST-NONCE", "second": "SECOND-NONCE"}, dm=True)
        self.script.events.append({})
        text = self.presentation().replace("Pitput board", "Pitput dm").replace(
            "why=new_activity", "why=incoming_dm message=dm-abcdefghijkl conversation=dc-abcdefghijkl")
        self.assertTrue(self.event(text))
        return text

    def dm_fragment(self):
        return {"source": "dm", "complete_message": True, "has_more": False,
                "start_byte": 0, "end_byte": 11, "total_bytes": 11,
                "handle": "dm-abcdefghijkl", "event_key": "dm-event-one",
                "snapshot_version": "snapshot-one", "next_cursor": None,
                "body_utf8": "FIRST-NONCE"}

    def test_dm_read_and_replay_use_message_identity_without_board_sequence(self):
        text = self.dm_event()
        self.assertTrue(self.script.on_result("read", json.dumps(self.dm_fragment())))
        self.script.events.append({})
        self.assertTrue(self.event(text))
        self.assertEqual(self.script.events[-1]["message"], "dm-abcdefghijkl")
        self.assertEqual(self.script.events[-1]["event_key"], "dm-event-one")
        self.assertIsNone(self.script.events[-1]["seq"])

    def test_dm_read_cannot_switch_message(self):
        self.dm_event()
        fragment = self.dm_fragment()
        fragment["handle"] = "dm-bbcdefghijkl"
        self.assertFalse(self.script.on_result("read", json.dumps(fragment)))

    def test_dm_read_cannot_switch_source(self):
        self.dm_event()
        fragment = self.dm_fragment()
        fragment["source"] = "board"
        self.assertFalse(self.script.on_result("read", json.dumps(fragment)))

    def test_dm_acceptance_requires_message_and_event_identity(self):
        from test_claude_attention import full_run, WINDOWS, CLEAN

        script, _ = full_run()
        script.source = "dm"
        for event, message in zip(script.events, ["dm-abcdefghijkl"] * 2 + ["dm-bbcdefghijkl"]):
            event.pop("seq")
            event["message"] = message
        self.assertEqual(base.evaluate(script, WINDOWS, CLEAN)[1], "PASS")
        script.events[1]["message"] = "dm-cbcdefghijkl"
        self.assertFalse(base.evaluate(script, WINDOWS, CLEAN)[0]["unacked_event_replayed"])
        script.events[1]["message"] = script.events[0]["message"]
        script.events[2]["event_key"] = script.events[0]["event_key"]
        self.assertFalse(base.evaluate(script, WINDOWS, CLEAN)[0]["acked_event_not_redelivered"])

    def reset_script(self):
        self.script = make_script(base, "/fixture/state", 20,
                                  {"first": "FIRST-NONCE", "second": "SECOND-NONCE"}, reset=True)
        self.script.events.append({})
        self.assertTrue(self.event(self.presentation()))

    def test_reset_retries_same_id_before_pending_replay(self):
        self.reset_script()
        command = self.script.advance("arm")["tool"]["input"]["command"]
        self.assertEqual(command.count("--re-prime " + self.script.reset_id), 2)
        self.script.events.append({})
        self.assertTrue(self.event((self.protocol.PROTOCOL + "\n") * 2 + self.presentation()))
        self.assertEqual(self.script.events[-1]["reset_primers_received"], 2)
        self.assertEqual(self.script.violations, [])

    def test_reset_missing_retry_primer_is_rejected(self):
        self.reset_script()
        self.script.events.append({})
        self.assertFalse(self.event(self.protocol.PROTOCOL + "\n" + self.presentation()))

    def test_reset_does_not_allow_pending_presentation_to_change(self):
        self.reset_script()
        self.script.events.append({})
        self.assertFalse(self.event((self.protocol.PROTOCOL + "\n") * 2
                                    + self.presentation().replace("new_activity", "mention")))
        self.assertIn("compact_replay_changed", self.script.violations)

    def test_launch_mode_does_not_create_or_prepare_fixture_config(self):
        script = make_script(base, "/fixture/state", 20, {}, launch=True)
        with patch.object(base, "private", side_effect=AssertionError("fixture wrote config")):
            script.configure({"HOME": "/fixture/home", "XDG_STATE_HOME": "/fixture/state"})
        self.assertIsNotNone(script.launch_root)
        command = script.advance("status")["tool"]["input"]["command"]
        self.assertIn("HRX_CLAUDE_TASK_CONFIGS", command)
        self.assertNotIn("bridge prepare", command)

    def test_launch_uses_only_config_in_actual_system_instructions(self):
        script = make_script(base, "/fixture/state", 20, {}, launch=True)
        script.configure({"HOME": "/fixture/home"})
        paths = [script.launch_root / name / script.native_id / base.BOARD / "bridge.toml"
                 for name in (".claude-launch-old", ".claude-launch-new")]
        request = {"tools": [{"name": "Bash"}],
                   "system": " ".join(base.PRIMING_MARKERS) + " " + str(paths[1]),
                   "messages": [{"role": "user", "content": str(paths[0])}]}
        with patch.object(base.Path, "glob", return_value=paths):
            script.decide(request)
        self.assertEqual(script.config, paths[1])
        self.assertEqual(script.violations, [])

    def test_native_resume_requires_new_launch_config(self):
        for fresh in (False, True):
            script = make_script(base, "/fixture/state", 20, {}, launch=True, resume=True)
            script.configure({"HOME": "/fixture/home"})
            old = script.launch_root / ".claude-launch-old" / script.native_id / base.BOARD / "bridge.toml"
            new = script.launch_root / ".claude-launch-new" / script.native_id / base.BOARD / "bridge.toml"
            script.config = old
            script.resume_paused = script.resume_released = True
            request = {"tools": [{"name": "Bash"}], "system": str(new if fresh else old),
                       "messages": [{"role": "user", "content": "HRX compact resume fixture"}]}
            with patch.object(base.Path, "glob", return_value=[old, new]):
                script.decide(request)
            self.assertEqual(script.resume_generation_changed, fresh)
            self.assertEqual(script.resume_received, fresh)
            if not fresh:
                self.assertIn("resume_reused_predecessor_task_config", script.violations)

    def test_resume_pauses_before_ack_and_rearms_only_after_release(self):
        script = make_script(base, "/fixture/state", 20, {}, resume=True)
        script.events.append({"event_key": "pending-one"})
        step = script.step
        self.assertIn("text", script.advance("arm"))
        self.assertTrue(script.resume_paused)
        self.assertEqual(script.step, step)
        script.resume_released = True
        request = {"tools": [{"name": "Bash"}], "messages": [
            {"role": "user", "content": "HRX compact resume fixture"}]}
        response = script.decide(request)
        self.assertTrue(response["tool"]["input"]["run_in_background"])
        self.assertTrue(script.resume_received)
        self.assertFalse(script.resume_paused)
        self.assertEqual(script.step, step + 1)
        self.assertNotIn("ack", response["tool"]["input"]["command"])

    def test_resume_cannot_proceed_before_predecessor_release(self):
        script = make_script(base, "/fixture/state", 20, {}, resume=True)
        script.events.append({})
        script.advance("arm")
        script.decide({"tools": [{"name": "Bash"}], "messages": [
            {"role": "user", "content": "HRX compact resume fixture"}]})
        self.assertIn("compact_resume_unexpected_request", script.violations)

    def test_dm_only_launch_readiness_needs_exact_identity_without_board_grants(self):
        script = make_script(base, "/fixture/state", 20, {}, launch=True, dm=True)
        self.assertTrue(script.on_result("status", json.dumps(
            {"identity": base.IDENTITY, "status": "ready", "boards": []})))
        self.assertFalse(script.on_result("status", json.dumps(
            {"identity": "other/dev/agent", "status": "ready", "boards": []})))

    def test_clear_observation_projects_only_validated_identity(self):
        from claude_clear_hook import project

        event = {"hook_event_name": "SessionStart", "source": "clear", "cwd": "/fixture/project",
                 "session_id": "12345678-1234-4123-8123-123456789abc", "prompt": "PRIVATE-BODY"}
        receipt = project(json.dumps(event), "/fixture/project")
        self.assertEqual(set(receipt), {"source", "session_digest"})
        self.assertNotIn("PRIVATE-BODY", json.dumps(receipt))
        with self.assertRaises(ValueError):
            project(json.dumps(event), "/different/project")
        event["source"] = "resume"
        with self.assertRaises(ValueError):
            project(json.dumps(event), "/fixture/project")

    def test_task_lifetime_observation_handles_comm_and_zombies(self):
        from claude_clear_hook import live_start

        fields = ["S"] + ["0"] * 18 + ["12345"]
        self.assertEqual(live_start("42 (bash (owned task)) " + " ".join(fields)), 12345)
        fields[0] = "Z"
        self.assertIsNone(live_start("42 (bash) " + " ".join(fields)))
