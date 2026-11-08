"""Compact-handle variant of the owned native Claude task fixture."""

import json
import hashlib
import re
import shlex
import time
import uuid
from pathlib import Path


def recovery_wrapper(python, native_id, marker):
    """Runner-only fault: fail one successor prepare, never a poll or ACK."""
    return f'''#!{python}
import os, sys, tomllib
from pathlib import Path
marker = Path({str(marker)!r})
if sys.argv[1:3] == ["bridge", "prepare"] and "--initialize-only" not in sys.argv:
    config = Path(sys.argv[sys.argv.index("--config") + 1])
    source = tomllib.loads(config.read_text())
    native = source["agents"][0]["thread_id"]
    if source.get("backend") == "claude-task" and native != {native_id!r}:
        try:
            fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            pass
        else:
            os.write(fd, b"successor_prepare_failed_once\\n")
            os.close(fd)
            sys.exit(77)
os.execv(sys.executable, [sys.executable, "-m", "ysg_mail.pitput", *sys.argv[1:]])
'''


def make_script(base, state, timeout, nonces, *, dm=False, reset=False, launch=False, resume=False,
                handoff=False, clear_pending=False):
    class CompactScript(base.Script):
        def __init__(self):
            super().__init__(state, timeout, nonces)
            self.native_id = str(uuid.uuid4())
            self.config = Path(state) / "compact.toml"
            self.seen = {}
            self.presentations = {}
            self.next_cursor = None
            self.fragments = []
            self.snapshot = None
            self.source = "dm" if dm else "board"
            self.reset_id = "fixture-reset-" + uuid.uuid4().hex
            self.resume_paused = False
            self.resume_released = False
            self.resume_received = False
            self.handoff_commands_received = False
            self.retiring = False
            self.clear_paused = False
            self.launch_root = None
            self.resume_generation_changed = False
            self.resume_replay_read = False

        def _decide(self, request):
            if self.resume_replay_read and any(tool.get("name") == "Bash" for tool in request.get("tools") or []):
                self.records.append({"at": time.monotonic(), "bytes": len(json.dumps(request).encode()),
                                     "class": "main"})
                last = base.latest_dialogue_message(request)
                results = [b for b in last.get("content") or [] if isinstance(b, dict)
                           and b.get("type") == "tool_result"] if last.get("role") == "user" else []
                if (len(results) != 1 or self.pending.pop(results[0].get("tool_use_id"), None) != "read"
                        or results[0].get("is_error")):
                    self.violate("resume_replay_read_correlation")
                    return {"text": "fixture stopped"}
                output = base.text_of(results[0].get("content"))
                self.tools.append({"purpose": "read", "tool_use_id": results[0]["tool_use_id"],
                                   "result_bytes": len(output.encode())})
                if not self.on_result("read", output):
                    return {"text": "fixture stopped"}
                if self.next_cursor is not None:
                    return self.advance("ack")
                if self.event.get("event_key") != self.events[0].get("event_key"):
                    self.violate("resume_pending_event_changed")
                    return {"text": "fixture stopped"}
                self.resume_replay_read = False
                return self.advance("ack")
            if (launch and not handoff and (self.step == 0 or self.resume_paused)
                    and any(tool.get("name") == "Bash" for tool in request.get("tools") or [])):
                system = base.text_of(request.get("system"))
                candidates = ([] if self.launch_root is None else list(self.launch_root.glob(
                    ".claude-launch-*/" + self.native_id + "/" +
                    ("@dm" if dm else base.BOARD) + "/bridge.toml")))
                matches = [path for path in candidates if str(path) in system]
                if len(matches) != 1:
                    self.violate("launch_generation_missing_or_ambiguous_in_native_system")
                    return {"text": "fixture stopped"}
                if self.resume_paused and self.resume_released:
                    self.resume_generation_changed = matches[0] != self.config
                    if not self.resume_generation_changed:
                        self.violate("resume_reused_predecessor_task_config")
                        return {"text": "fixture stopped"}
                self.config = matches[0]
            if self.retiring and any(tool.get("name") == "Bash" for tool in request.get("tools") or []):
                last = base.latest_dialogue_message(request)
                text = base.text_of(last.get("content"))
                if (last.get("role") == "user" and "<task-notification>" in text
                        and self.task and base.tag(text, "task-id") == self.task[0]):
                    self.records.append({"at": time.monotonic(), "bytes": len(json.dumps(request).encode()),
                                         "class": "retired_task_notification"})
                    return {"text": "Predecessor task retired; awaiting fresh session commands."}
            if handoff and self.step == 0 and any(tool.get("name") == "Bash" for tool in request.get("tools") or []):
                from ysg_mail.attention_delivery import PROTOCOL

                messages = base.text_of(request.get("system")) + "\n" + "\n".join(
                    base.text_of(m.get("content")) for m in request.get("messages") or [])
                self.handoff_commands_received = all(part in messages for part in (
                    "HRX has retired the predecessor session tasks.", self.native_id,
                    str(self.config), PROTOCOL))
                if not self.handoff_commands_received:
                    self.violate("clear_hook_commands_missing_from_native_request")
                    return {"text": "fixture stopped"}
            if self.resume_paused and any(tool.get("name") == "Bash" for tool in request.get("tools") or []):
                self.records.append({"at": time.monotonic(), "bytes": len(json.dumps(request).encode()),
                                     "class": "main"})
                last = base.latest_dialogue_message(request)
                if (not self.resume_released or last.get("role") != "user"
                        or base.text_of(last.get("content")).strip() != "HRX compact resume fixture"):
                    self.violate("compact_resume_unexpected_request")
                    return {"text": "fixture stopped"}
                self.resume_paused = False
                self.resume_received = True
                return self.advance("arm")
            return super()._decide(request)

        def read_command(self):
            args = ["pitput", "--agent", base.IDENTITY, "attention", "read",
                    self.event["handle"], "--body-byte-limit", "64"]
            if self.next_cursor is not None:
                args += ["--cursor", self.next_cursor]
            return shlex.join(args)

        def configure(self, env):
            if launch:
                root = Path(env.get("XDG_STATE_HOME", str(Path(env["HOME"]) / ".local/state")))
                self.launch_root = root / "hrx/pitput" / hashlib.sha256(base.IDENTITY.encode()).hexdigest()
                return
            self.config.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            base.private(self.config, '\n'.join([
                'backend = "claude-task"',
                'claude_home = ' + json.dumps(env["CLAUDE_CONFIG_DIR"]),
                'state_directory = ' + json.dumps(str(self.config.parent / "bridge")),
                'monitor_timeout = ' + str(timeout),
                '[[agents]]',
                'agent_id = ' + json.dumps(base.IDENTITY),
                'thread_id = ' + json.dumps(self.native_id),
            ] + (['source = "dm"'] if dm else [
                'board = ' + json.dumps(base.BOARD), 'targeted = true',
            ])) + '\n')

        def command(self):
            return shlex.join(["pitput", "bridge", "claude-task", "--config", str(self.config),
                               "--agent-id", base.IDENTITY, "--session-id", self.native_id])

        def advance(self, action):
            if (resume and self.resume_received and action == "ack" and len(self.events) == 2
                    and self.event.get("event_key") is None):
                self.resume_replay_read = True
            if self.next_cursor is not None or self.resume_replay_read:
                self.calls += 1
                tool_id = f"toolu_fixture{self.calls:04d}"
                self.pending[tool_id] = "read"
                return {"tool": {"id": tool_id, "name": "Bash", "input": {
                    "command": self.read_command(), "description": "fixture read continuation"}}}
            if clear_pending and action == "arm" and len(self.events) == 1:
                self.clear_paused = True
                return {"text": "Fixture awaiting native clear before handling ACK."}
            if resume and action == "arm" and len(self.events) == 1 and not self.resume_received:
                self.resume_paused = True
                return {"text": "Fixture awaiting an HRX resume before handling ACK."}
            # The base owns task IDs, notifications, plan order, and tool IDs.
            # Supply placeholders only while it constructs commands we replace.
            if self.event is not None:
                self.event.setdefault("path", "unused")
                self.event.setdefault("cursor", "unused")
            result = super().advance(action)
            if "tool" not in result:
                return result
            tool = result["tool"]["input"]
            if action == "join" and launch and dm:
                tool["command"] = shlex.join(["pitput", "agent", "status", "--agent-id", base.IDENTITY])
            elif action == "status":
                if launch:
                    if handoff:
                        verify = ("import json,os; b=json.load(open(os.environ['HRX_CLAUDE_TASK_BINDING'])); "
                                  "assert b['Current']==" + repr(self.native_id)
                                  + " and b['Pending']=='' and b['Paths']==" + repr([str(self.config)]))
                    else:
                        verify = ("import json,os; assert json.loads(os.environ['HRX_CLAUDE_TASK_CONFIGS']) == "
                                  + repr([str(self.config)]))
                    tool["command"] = shlex.join(["python3", "-c", verify]) + " && " + tool["command"]
                else:
                    tool["command"] = (shlex.join(["pitput", "bridge", "prepare", "--config",
                                              str(self.config), "--start", "latest"])
                                   + " >/dev/null && " + tool["command"])
            elif action == "arm":
                tool["command"] = (
                    "umask 077\nmkdir -p " + shlex.quote(str(Path(state))) + ' || exit "$?"\n'
                    + 'printf \'%s\\n\' "$BASHPID" > ' + shlex.quote(str(Path(state) / "task.pid")) + '\nwhile :; do\n'
                    + "printf . >> " + shlex.quote(str(Path(state) / "cycles")) + "\n"
                    + "event=$(" + self.command() + ') || exit "$?"\n'
                    + 'if [ -n "$event" ]; then printf \'%s\\n\' "$event"; break; fi\ndone'
                )
                if reset and len(self.events) == 1:
                    # Retry the same reset while the first message is unACKed.
                    # Both primers must reach the model, followed by exact replay.
                    reprime = self.command() + " --re-prime " + shlex.quote(self.reset_id)
                    tool["command"] = (reprime + ' || exit "$?"\n') * 2 + tool["command"]
            elif action in {"read", "ack"}:
                tool["command"] = shlex.join(["pitput", "--agent", base.IDENTITY,
                                              "attention", action, self.event["handle"]])
                if action == "read":
                    tool["command"] = self.read_command()
            return result

        def on_result(self, purpose, output):
            observation = self.events[-1] if self.events else {}
            if purpose == "status" and launch and dm:
                try:
                    status = json.loads(output.strip())
                    self.enrollment = (status.get("identity") == base.IDENTITY
                                       and status.get("status") == "ready"
                                       and status.get("boards") == [])
                except (ValueError, AttributeError):
                    self.enrollment = False
                if not self.enrollment:
                    self.violate("dm_agent_not_ready")
                return self.enrollment
            if purpose in {"event", "read", "ack"}:
                # Native Bash may append its successful exit marker outside the
                # command's stdout. No other suffix or exit status is accepted.
                output = output.removesuffix("\n\n[exited with code 0]")
            if purpose == "event":
                from ysg_mail.attention_delivery import PROTOCOL

                if reset and len(self.events) == 2:
                    prefix = (PROTOCOL + "\n") * 2
                    if not output.startswith(prefix):
                        self.violate("compact_reset_primers_missing")
                        return False
                    output = output[len(prefix):]
                    observation["reset_primers_received"] = 2
                routine = output.strip()
                primer = routine.startswith(PROTOCOL + "\n\n")
                if primer:
                    routine = routine[len(PROTOCOL) + 2:]
                lines = routine.splitlines()
                pattern = (r"Pitput dm from=\S+ ref=(at-[a-z2-7]{28}) why=incoming_dm"
                           r" message=(dm-[a-z2-7]{12}) conversation=(dc-[a-z2-7]{12})"
                           if dm else r"Pitput board from=\S+ ref=(at-[a-z2-7]{28}) why=\S+")
                match = re.fullmatch(pattern,
                                     lines[0] if lines else "")
                handle = match.group(1) if match else ""
                expected = [f"Read: pitput --agent {base.IDENTITY} attention read {handle}",
                            f"Handle ACK: pitput --agent {base.IDENTITY} attention ack {handle}"]
                valid = bool(handle) and len(lines) == 3 and lines[1:] == expected
                absent = not any(n in output for n in nonces.values())
                if not valid or not absent:
                    observation["presentation_diagnostic"] = {
                        "bytes": len(output.encode()), "primer_prefix": primer,
                        "protocol_offset": output.find(PROTOCOL),
                        "line_count": len(lines), "line_lengths": [len(x) for x in lines[:8]],
                        "header_match": bool(match),
                        "read_line_match": len(lines) > 1 and lines[1] == expected[0],
                        "ack_line_match": len(lines) > 2 and lines[2] == expected[1],
                        "body_absent": absent,
                        "stdout_tag": "<stdout>" in output,
                        "stderr_tag": "<stderr>" in output,
                        "reminder_tag": "<system-reminder>" in output,
                    }
                    if (absent and match and len(lines) <= 6 and lines[1:3] == expected
                            and all(len(line) <= 80 for line in lines[3:])):
                        observation["presentation_diagnostic"]["native_suffix_lines"] = lines[3:]
                    self.violate("compact_presentation_invalid")
                    return False
                if handle in self.presentations and self.presentations[handle] != output:
                    self.violate("compact_replay_changed")
                    return False
                if not self.presentations and not primer:
                    self.violate("compact_initial_primer_missing")
                resumed = resume and self.resume_received and len(self.events) == 2
                if resumed and not primer and observation.get("reset_primers_received") != 2:
                    self.violate("resume_generation_primer_missing")
                if handle not in self.presentations and self.presentations and primer and not resumed:
                    self.violate("compact_routine_repeated_primer")
                self.presentations[handle] = output
                self.event = {"handle": handle, **self.seen.get(handle, {})}
                if dm:
                    self.event.update(message=match.group(2), conversation=match.group(3))
                self.fragments, self.snapshot, self.next_cursor = [], None, None
                observation.update(metadata_only=True, body_absent_event=absent,
                                   event_bytes=len(output.encode()), handle=handle,
                                   seq=self.event.get("seq"), event_key=self.event.get("event_key"))
                if dm:
                    observation.update(message=self.event["message"],
                                       conversation=self.event["conversation"])
                return True
            if purpose == "read":
                try:
                    fragment = json.loads(output)
                    assert fragment["source"] == self.source
                    text = fragment["body_utf8"]
                    assert isinstance(text, str)
                    start = sum(len(part.encode()) for part in self.fragments)
                    end, total = fragment["end_byte"], fragment["total_bytes"]
                    assert fragment["start_byte"] == start
                    assert end == start + len(text.encode()) and 0 <= end <= total <= 32768
                    assert end > start or total == 0
                    assert fragment["has_more"] is (end < total)
                    assert fragment["complete_message"] is (start == 0 and end == total)
                    bound = (fragment["snapshot_version"], fragment["event_key"],
                             fragment["handle"], total)
                    assert self.snapshot is None or self.snapshot == bound
                    cursor = fragment["next_cursor"]
                    if end < total:
                        assert isinstance(cursor, str) and 0 < len(cursor) <= 512
                    else:
                        assert cursor is None
                    receipt = {"event_key": fragment["event_key"]}
                    if dm:
                        assert fragment["handle"] == self.event["message"]
                        receipt["message"] = fragment["handle"]
                    else:
                        receipt["seq"] = int(fragment["handle"].rsplit("/", 1)[1])
                except (ValueError, KeyError, TypeError, AssertionError):
                    self.violate("compact_read_incomplete")
                    return False
                self.snapshot = bound
                self.fragments.append(text)
                self.next_cursor = cursor
                observation.update(read_fragments=len(self.fragments), read_assembled_bytes=end,
                                   read_contiguous_complete=end == total)
                if cursor is not None:
                    return True
                self.seen[self.event["handle"]] = receipt
                self.event.update(receipt)
                observation.update(receipt)
                output = "".join(self.fragments)
            elif purpose == "ack":
                try:
                    ack = json.loads(output)
                    valid = (ack["acknowledged"] is True and ack["agent_id"] == base.IDENTITY
                             and ack["event_key"] == self.event["event_key"])
                except (ValueError, KeyError, TypeError):
                    valid = False
                observation.update(owner_ack=valid, ack_bytes=len(output.encode()))
                if not valid:
                    self.violate("compact_owner_ack_failed")
                return valid
            return super().on_result(purpose, output)

    return CompactScript()
