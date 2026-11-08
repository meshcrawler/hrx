"""Opt-in native Claude attention compaction composition; runner execution only."""

import json
import fcntl
import os
from pathlib import Path
import shlex
import secrets
import stat
import sys
import time

import claude_compaction_hook as hooks
import claude_compaction_receipts as receipts


PREFIX = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n"
SUFFIX = ". Tool calls will be rejected and you will fail the task."
SUMMARY = ("<summary>The user requested monitoring of the authorized Pitput board. "
           "The existing tracked background task owns the monitor. Handle its notification, "
           "read the exact event, replay unacknowledged work, and ACK only handled work. "
           "Do not deploy or make paid requests.</summary>")


def summary_prompt(message):
    if message.get("role") != "user":
        return False
    content = message.get("content")
    if type(content) is str:
        texts = [content]
    elif type(content) is list:
        # Cache-sharing compaction can retain preceding tool results in this turn.
        if any(type(block) is not dict or block.get("type") not in ("text", "tool_result")
               for block in content):
            return False
        texts = [block.get("text") for block in content if block.get("type") == "text"]
    else:
        return False
    return (len(texts) == 1 and type(texts[0]) is str
            and texts[0].startswith(PREFIX) and texts[0].endswith(SUFFIX))


class Control:
    def __init__(self, script, api):
        self.script, self.api = script, api
        self.journal = self.project = self.directory = self.home = None
        self.injected = self.summaries = 0
        self.resumed_at_ns = None
        self.requests = []
        self.initial_task = None
        self.completions = []
        self.evidence_stage = "configured"
        self.observed = []
        self.markers = []
        self.completed = []
        self.request_shapes = []
        self.canonical_protocol_retained = False

    def __getattr__(self, key):
        return getattr(self.script, key)

    def configure(self, root, project, env):
        self.project, self.directory, self.home = project, Path(env["CLAUDE_CONFIG_DIR"]), env["HOME"]
        folder = root / "compaction-hooks"
        folder.mkdir(mode=0o700)
        self.journal = folder / "events.jsonl"
        self.journal.touch(mode=0o600)
        command = shlex.join([sys.executable, "-I", str(Path(hooks.__file__).resolve()),
                              "--journal", str(self.journal), "--cwd", str(project)])
        settings = {"autoCompactEnabled": True, "idleCompaction": False,
                    "precomputeCompactionEnabled": False,
                    "hooks": {event: [{"matcher": "auto|manual", "hooks": [
                        {"type": "command", "command": command, "timeout": 5}]}]
                        for event in ("PreCompact", "PostCompact")}}
        self.api.private(project / ".claude/settings.local.json", json.dumps(settings))
        env.update(CLAUDE_CODE_AUTO_COMPACT_WINDOW="100000", CI="true",
                   HRX_CLAUDE_ATTENTION_AUTHORIZED="1")
        for key in ("DISABLE_COMPACT", "DISABLE_AUTO_COMPACT", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"):
            env.pop(key, None)

    def decide(self, request):
        with self.script.lock:
            try:
                return self._decide(request)
            except (ValueError, OSError):
                self.script.violate("compaction_control_refused")
                return {"text": "Synthetic fixture stopped."}

    def _decide(self, request):
        messages = request.get("messages") or []
        last = messages[-1] if messages else {}
        text = self.api.text_of(last.get("content")) if last.get("role") == "user" else ""
        summary = summary_prompt(self.api.latest_dialogue_message(request))
        serial = len(self.requests) + 1
        receipts.require(serial <= 80, "request_budget")
        if len(self.request_shapes) < 80:
            content = last.get("content")
            blocks = content if type(content) is list else [{"type": "text", "text": content}]
            texts = [block.get("text") for block in blocks if type(block) is dict
                     and block.get("type") == "text" and type(block.get("text")) is str]
            self.request_shapes.append({
                "serial": serial, "user_last": last.get("role") == "user",
                "block_count": len(blocks), "text_block_count": len(texts),
                "aggregate_prefix": text.startswith(PREFIX), "aggregate_suffix": text.endswith(SUFFIX),
                "contains_prefix": PREFIX in text, "contains_suffix": SUFFIX in text,
                "task_notification": "<task-notification>" in text,
                "text_blocks": [{"prefix": value.startswith(PREFIX), "suffix": value.endswith(SUFFIX),
                                 "contains_prefix": PREFIX in value, "contains_suffix": SUFFIX in value}
                                for value in texts[:16]],
                "tail": [{"role": item.get("role") if item.get("role") in ("user", "assistant", "system") else "other",
                          "task_notification": "<task-notification>" in self.api.text_of(item.get("content")),
                          "idle_text": self.api.text_of(item.get("content")) == "Waiting for the next Pitput event.",
                          "summary_text": "The existing tracked background task owns the monitor." in self.api.text_of(item.get("content"))}
                         for item in messages[-6:] if type(item) is dict],
            })
        if summary:
            receipts.require(self.injected == 1 and self.summaries == 0, "unexpected_summary")
            fd = hooks.open_journal(self.journal)
            try:
                fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
                rows = hooks.read_rows(fd)
            finally:
                os.close(fd)
            receipts.require(len(rows) == 1 and rows[0].get("event") == "PreCompact"
                             and rows[0].get("trigger") == "auto", "summary_without_pre_hook")
            session = self.api.managed_session_id(self.home, self.directory)
            receipts.session(session, session)
            receipts.require(rows[0].get("session_digest") == receipts.digest(session)
                             and rows[0].get("cwd_digest") == receipts.digest(str(self.project)),
                             "summary_hook_binding")
            self.summaries += 1
            self.requests.append({"serial": serial, "role": "summary", "input_tokens": 200})
            self.script.records.append({"at": time.monotonic(), "bytes": len(json.dumps(request).encode()),
                                        "class": "auxiliary"})
            return {"text": SUMMARY, "usage": {"input_tokens": 200, "output_tokens": 1}}
        main = any(tool.get("name") == "Bash" for tool in request.get("tools") or [])
        if main and self.summaries:
            session = self.api.managed_session_id(self.home, self.directory)
            observed = hooks.observed_hooks(self.journal, expected_session=session,
                                            expected_cwd=str(self.project))
            receipts.require(self.api.priming_location(request), "post_compaction_priming_missing")
            if getattr(self.script, "source", None) in {"board", "dm"}:
                from ysg_mail.attention_delivery import PROTOCOL

                system = request.get("system")
                system_text = self.api.text_of(system) if isinstance(system, list) else system or ""
                receipts.require(PROTOCOL in system_text, "canonical_protocol_lost_after_compaction")
                self.canonical_protocol_retained = True
            now = time.monotonic_ns()
            receipts.require(now > observed[-1]["at_ns"], "post_hook_order")
            if self.resumed_at_ns is None:
                receipts.require(self.script.task == self.initial_task, "monitor_task_changed")
                self.resumed_at_ns = now
        before = self.script.step
        spec = self.script._decide(request)
        if main and self.summaries and before in (self.api.ROUND_ONE_DONE, self.api.ROUND_TWO_DONE):
            receipts.require(spec.get("text") == "Waiting for the next Pitput event.",
                             "round_not_idle")
            spec["response_id"] = "msg_" + secrets.token_hex(16)
            self.completions.append(spec["response_id"])
        usage = 1
        if main and before == self.api.ARMED:
            receipts.require(self.injected == 0 and self.summaries == 0, "duplicate_trigger")
            self.initial_task = self.script.task
            receipts.require(self.initial_task is not None, "monitor_not_owned")
            self.injected = 1
            usage = 90000
            spec["usage"] = {"input_tokens": usage, "output_tokens": 1}
        self.requests.append({"serial": serial, "role": "main" if main else "auxiliary",
                              "input_tokens": usage,
                              **({"response_digest": receipts.digest(spec["response_id"]),
                                  "round": len(self.completions)} if "response_id" in spec else {})})
        return spec

    def failure_receipt(self):
        return {"status": "FAIL", "scope": "claude_owned_auto_attention_continuity",
                "stage": self.evidence_stage, "trigger_count": self.injected,
                "summary_count": self.summaries, "requests": list(self.requests),
                "expected_completion_digests": [receipts.digest(value) for value in self.completions],
                "hooks": self.observed, "boundaries": self.markers,
                "completions": self.completed, "request_shapes": self.request_shapes,
                "hook_progress": self.hook_progress()}

    def hook_progress(self):
        if self.journal is None:
            return {"status": "not_configured"}
        try:
            fd = hooks.open_journal(self.journal)
            try:
                fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
                rows = hooks.read_rows(fd)
            finally:
                os.close(fd)
            session = self.api.managed_session_id(self.home, self.directory)
            receipts.session(session, session)
            return {"status": "read", "count": len(rows), "events": [
                {"event": row.get("event") if row.get("event") in ("PreCompact", "PostCompact") else "invalid",
                 "automatic": row.get("trigger") == "auto",
                 "session_matches": row.get("session_digest") == receipts.digest(session),
                 "project_matches": row.get("cwd_digest") == receipts.digest(str(self.project))}
                for row in rows]}
        except (OSError, ValueError):
            return {"status": "unavailable"}

    def finish(self):
        self.evidence_stage = "hook_binding"
        session = self.api.managed_session_id(self.home, self.directory)
        observed = hooks.observed_hooks(self.journal, expected_session=session,
                                        expected_cwd=str(self.project))
        self.observed = observed
        self.evidence_stage = "round_response_count"
        receipts.require(len(self.completions) == 2, "round_completion_count")
        self.evidence_stage = "transcript_correlation"
        markers, completed = transcript(self.directory, session, self.completions)
        self.markers, self.completed = markers, completed
        self.evidence_stage = "completion_gates"
        receipts.require(self.injected == self.summaries == len(markers) == 1,
                         "compaction_counts")
        receipts.require(self.resumed_at_ns is not None and len(completed) == 2,
                         "continuation_not_completed")
        receipts.require(markers[0]["trigger"] == "auto", "manual_boundary")
        receipts.require(not self.script.violations and self.script.done, "attention_not_complete")
        self.evidence_stage = "complete"
        return {"status": "PASS", "scope": "claude_owned_auto_attention_continuity",
                "session_digest": receipts.digest(session), "hooks": observed,
                "boundary": markers[0], "requests": self.requests,
                "completions": completed,
                "manual_compact_calls": 0, "post_boundary_instructions": True,
                "original_monitor_preserved_at_resume": True, "post_boundary_completed": True,
                "canonical_protocol_retained": self.canonical_protocol_retained}


def transcript(directory, session, expected_responses):
    receipts.session(session, session)
    paths = list((directory / "projects").glob("*/" + session + ".jsonl"))
    receipts.require(len(paths) == 1, "transcript_ownership")
    path = paths[0]
    receipts.require(path.resolve().is_relative_to(directory.resolve()), "transcript_escape")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        receipts.require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                         and info.st_nlink == 1 and info.st_size <= 16 * 1024 * 1024,
                         "transcript_budget")
        with os.fdopen(fd, "rb", closefd=False) as stream:
            markers, completed, chain = [], [], set()
            for _ in range(4096):
                raw = stream.readline(receipts.MAX_RECORD_BYTES + 1)
                if not raw:
                    return markers, completed
                receipts.require(raw.endswith(b"\n"), "transcript_partial")
                row = receipts.record(raw)
                if row.get("type") == "system" and row.get("subtype") == "compact_boundary":
                    markers.append(receipts.boundary(raw, expected_session=session))
                    chain = {row["uuid"]}
                elif markers and row.get("sessionId") == session:
                    identifier, parent = row.get("uuid"), row.get("parentUuid")
                    local = (row.get("version") == receipts.VERSION
                             and row.get("isSidechain", False) is False
                             and row.get("remoteSourced", False) is False)
                    linked = (local and type(identifier) is str
                              and receipts.UUID.fullmatch(identifier) is not None
                              and type(parent) is str and parent in chain)
                    if linked:
                        chain.add(identifier)
                    if row.get("type") != "assistant":
                        continue
                    receipts.require(row.get("version") == receipts.VERSION,
                                     "wrong_completion_version")
                    receipts.require(row.get("isSidechain", False) is False
                                     and row.get("remoteSourced", False) is False,
                                     "nonlocal_completion")
                    message = row.get("message", {})
                    receipts.require(type(message) is dict
                                     and type(message.get("content")) is list,
                                     "invalid_completion_message")
                    if message.get("id") not in expected_responses:
                        continue
                    receipts.require(linked, "completion_off_chain")
                    receipts.require(len(completed) < len(expected_responses)
                                     and message["id"] == expected_responses[len(completed)],
                                     "completion_order")
                    receipts.require(message.get("stop_reason") == "end_turn" and any(
                        part.get("type") == "text" and part.get("text") == "Waiting for the next Pitput event."
                        for part in message.get("content", []) if isinstance(part, dict)
                    ), "completion_incomplete")
                    completed.append({"response_digest": receipts.digest(message["id"]),
                                      "uuid_digest": receipts.digest(identifier),
                                      "parent_digest": receipts.digest(parent)})
            raise ValueError("transcript_record_budget")
    finally:
        os.close(fd)
