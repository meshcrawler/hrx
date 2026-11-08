"""Deterministic loopback proof of Claude Code's native Pitput attention path (#23).

HRX starts no bridge for Claude. It appends the Claude priming to the launch, and
the model itself runs ONE tracked Bash background task that waits for one
metadata-only Pitput event. Claude Code must then wake the idle session with a task
notification; the model reads the exact post, ACKs that exact cursor with the same
filters and cursor file, and re-arms. This proof drives the REAL `hrx claude --pitput`
launch and the REAL pinned Claude Code against a scripted Anthropic Messages
responder on loopback, a disposable PostgreSQL/Pitput server and the subreaper
ownership boundary. No paid inference, no shared credentials, Linux only.

    HRX_CLAUDE_ATTENTION_AUTHORIZED=1 python3 tests/compatibility/claude_attention.py \\
        --hrx PATH --output artifacts/claude-attention

The scripted model is a fixture: it proves the transport and lifecycle contract, not
model judgement. See docs/internal/claude-attention.md.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import http.server
import json
import os
from pathlib import Path
import pty
import re
import secrets
import select
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

from gateway import Gateway
from run import Runner
from conversation import SUPERVISOR, read_summary, running, stop_supervised
from owned_capture import capture

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration"))
from pitput_exchange import pitput, private, run  # noqa: E402

ROUTE = "fixture-claude-attention"
IDENTITY = "ci/claude-attention/agent"
CONTROLLER = "ci/claude-attention-controller/agent"
BOARD = "exchange"
PRIMING_MARKERS = ("CLAUDE BACKGROUND DELIVERY", "HRX has not started this monitor")
EVENT_KEYS = {"seq", "event_key", "path", "author", "reason", "cursor", "trust"}
# Strings taken from the Claude Code binary (2.1.309 locally; unverified for the pinned 2.1.306).
SCREEN_CLASSES = (("trust_dialog", re.compile(r"quick safety check|trust (the files|this folder|this project)|one you trust|"
                                               r"trust this (folder|directory|workspace)", re.I)),
                  ("bypass_permissions_acceptance", re.compile(r"bypass permissions", re.I)),
                  ("theme_onboarding", re.compile(r"choose the text style|dark mode|light mode", re.I)),
                  ("login_required", re.compile(r"select login method|/login|not logged in", re.I)),
                  ("api_key_confirmation", re.compile(r"detected a custom api key", re.I)))
ANSI = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[\]P^_].*?(?:\x07|\x1b\\)|\x1b.")


class Blocked(RuntimeError):
    """A prerequisite or measured CLI behavior prevented the proof; never a pass."""


class Stop(RuntimeError):
    """The scripted model recorded a contract violation; the run fails."""


# ---------------------------------------------------------------- scripted model

def monitor_args(state, timeout):
    return ["pitput", "--agent", IDENTITY, "monitor", f"/b/{BOARD}", "--mention", IDENTITY,
            "--exclude-author", IDENTITY, "--cursor-file", f"{state}/attention.json",
            "--timeout", str(timeout), "--once"]


def arm_command(state, timeout):
    """The primed one-event wait loop, plus a per-iteration cycle marker."""
    monitor = " ".join(monitor_args('"$state"', timeout))
    return ("umask 077\n"
            f"state={state}\n"
            'mkdir -p "$state"\nchmod 700 "$state"\n'
            f"monitor=({monitor})\n"
            "while :; do\n"
            '  printf . >> "$state/cycles"\n'
            '  event=$("${monitor[@]}") || exit "$?"\n'
            '  if [ -n "$event" ]; then\n'
            "    printf '%s\\n' \"$event\"\n"
            "    break\n"
            "  fi\n"
            "done")


def ack_command(state, timeout, cursor):
    return " ".join(monitor_args(state, timeout) + ["--ack", cursor])


# Trigger -> action. A wake is a task notification for the currently armed task;
# "result:X" is the tool_result of the previous action X. Round one deliberately
# re-arms before ACKing to prove an unACKed event replays, then ACKs it.
# HRX enrolls only Codex/OpenCode identities (preparePitputRuntime). For Claude the
# priming tells the model to join its explicit board and check status itself (job 4126).
PLAN = [
    ("start", "join"), ("result:join", "status"), ("result:status", "arm"), ("result:arm", "idle"),
    ("wake", "event"), ("result:event", "read"), ("result:read", "arm"), ("result:arm", "idle"),
    ("wake", "event"), ("result:event", "ack"), ("result:ack", "arm"), ("result:arm", "idle"),
    ("wake", "event"), ("result:event", "read"), ("result:read", "ack"), ("result:ack", "arm"),
    ("result:arm", "idle"),
]
# Plan index of the armed idle and of each round's final idle.
ARMED = PLAN.index(("result:arm", "idle"))
ROUND_ONE_DONE, ROUND_TWO_DONE = ARMED + 8, len(PLAN) - 1


def text_of(content):
    if isinstance(content, str):
        return content
    parts = []
    for block in content or []:
        if isinstance(block, dict):
            if block.get("type") == "text":
                parts.append(block.get("text", ""))
            elif block.get("type") == "tool_result":
                parts.append(text_of(block.get("content")))
    return "\n".join(parts)


def system_envelope(item):
    """Recognize pinned text and tool-metadata system forms, never role alone."""
    if not set(item) <= {"role", "content", "output_config", "clear_at"}:
        return False
    if "output_config" in item and type(item["output_config"]) is not dict:
        return False
    if "clear_at" in item and item["clear_at"] != "next_user_message":
        return False
    content = item.get("content")
    if type(content) is str:
        return True
    if type(content) is not list:
        return False
    for block in content:
        if type(block) is not dict:
            return False
        cache = block.get("cache_control")
        if "cache_control" in block and (type(cache) is not dict
                or cache.get("type") != "ephemeral"
                or ("ttl" in cache and cache["ttl"] not in ("5m", "1h"))):
            return False
        if block.get("type") == "text":
            if type(block.get("text")) is not str:
                return False
        elif block.get("type") in ("tool_addition", "tool_removal"):
            tool = block.get("tool")
            if type(tool) is not dict:
                return False
            if tool.get("type") == "tool_reference":
                if type(tool.get("name")) is not str or not tool["name"]:
                    return False
            elif tool.get("type") == "tool_definition" and block["type"] == "tool_addition":
                definition = tool.get("definition")
                if (type(definition) is not dict or type(definition.get("name")) is not str
                        or not definition["name"] or type(definition.get("input_schema")) is not dict):
                    return False
            else:
                return False
        else:
            return False
    return True


def latest_dialogue_message(request):
    # Pinned Claude can append mid-conversation system entries after a user turn.
    # Never skip an assistant or unknown role to recover an older notification.
    for item in reversed(request.get("messages") or []):
        if type(item) is not dict:
            return {}
        if item.get("role") != "system":
            return item
        if not system_envelope(item):
            return {}
    return {}


def enrollment_ready(output):
    """`pitput agent status` for exactly this identity: ready, holding the board."""
    try:
        status = json.loads(output.strip())
    except ValueError:
        return False
    return (isinstance(status, dict) and status.get("identity") == IDENTITY and status.get("status") == "ready"
            and isinstance(status.get("boards"), list)
            and bool({BOARD, f"/b/{BOARD}"} & set(status["boards"])))


def ack_confirmed(output, event):
    """Pitput's ACK result is exactly {"acknowledged": "<cursor>"} for the handled event."""
    cursor = (event or {}).get("cursor")
    try:
        result = json.loads(output.strip())
    except ValueError:
        return False
    return (isinstance(result, dict) and set(result) == {"acknowledged"} and isinstance(cursor, str)
            and bool(cursor) and result["acknowledged"] == cursor)


# Allowlisted failure classes for a failed monitor task; never its output text.
FAILURE_CLASSES = (
    ("pitput_http_401", r"remote_http_401"), ("pitput_http_403", r"remote_http_403"),
    ("pitput_http_404", r"remote_http_404"), ("pitput_http_409", r"remote_http_409"),
    ("pitput_http_other", r"remote_http_(?!40[1349])\d{3}"),
    ("pitput_connection_unavailable", r"remote_connection_unavailable"),
    ("pitput_agent_not_enrolled", r"agent_not_enrolled"), ("pitput_dm_consumer_busy", r"dm_consumer_busy"),
    ("pitput_usage_error", r"usage: pitput|unrecognized arguments|invalid choice|the following arguments are required"),
    ("command_not_found", r"command not found|No such file or directory"),
    ("permission_denied", r"Permission denied|EACCES"),
    ("python_traceback", r"Traceback \(most recent call last\)"),
    ("module_not_found", r"ModuleNotFoundError|No module named"),
    ("bash_syntax", r"syntax error"),
)


def failure_classes(output):
    return [name for name, pattern in FAILURE_CLASSES if re.search(pattern, output)]


def tag(text, name):
    found = re.search(rf"<{name}>\s*(.*?)\s*</{name}>", text, re.S)
    return found.group(1) if found else None


def priming_location(request):
    """Where HRX's Claude priming reached the model: system and/or message indexes."""
    found = []
    system = request.get("system")
    system_text = text_of(system) if isinstance(system, list) else system or ""
    if all(marker in system_text for marker in PRIMING_MARKERS):
        found.append("system")
    for index, message in enumerate(request.get("messages", [])):
        if all(marker in text_of(message.get("content")) for marker in PRIMING_MARKERS):
            found.append(f"messages[{index}]")
    return found


class Script:
    """The deterministic model. decide() turns one request into one response spec
    and records every observation; it never touches the network."""

    def __init__(self, state, timeout, nonces):
        self.state, self.timeout, self.nonces = state, timeout, nonces
        self.step, self.calls, self.pending = 0, 0, {}
        self.task = None  # (task_id, output_file) of the armed background task
        self.event = None
        self.events, self.records, self.violations, self.tools = [], [], [], []
        self.priming = None
        self.diagnostic = None  # None, "pending", "collected" or "timeout"
        self.enrollment = None
        self.lock = threading.Lock()

    @property
    def done(self):
        return self.step >= len(PLAN)

    def phase(self):
        return "done" if self.done else PLAN[self.step][1]

    def violate(self, code):
        if code not in self.violations:
            self.violations.append(code)

    def decide(self, request):
        with self.lock:
            return self._decide(request)

    def _decide(self, request):
        record = {"at": time.monotonic(), "bytes": len(json.dumps(request).encode()), "class": "auxiliary"}
        self.records.append(record)
        if not any(tool.get("name") == "Bash" for tool in request.get("tools") or []):
            return {"text": "ok"}
        record["class"] = "main"
        last = latest_dialogue_message(request)
        content = last.get("content") if last.get("role") == "user" else None
        results = [block for block in content or [] if isinstance(block, dict)
                   and block.get("type") == "tool_result"]
        diagnostic = [block for block in results if self.pending.get(block.get("tool_use_id")) == "diagnose"]
        if diagnostic:
            # The one diagnostic read of a failed task's output: classify it, then stop.
            self.pending.pop(diagnostic[0]["tool_use_id"])
            output = text_of(diagnostic[0].get("content"))
            self.events[-1].update(failure_output_bytes=len(output.encode()),
                                   failure_classes=failure_classes(output))
            if self.diagnostic == "pending":
                self.diagnostic = "collected"
                self.events[-1]["failure_diagnostic"] = "collected"
            return {"text": "fixture stopped"}
        if self.done:
            self.violate("request_after_plan_complete")
            return {"text": "fixture complete"}
        trigger, action = PLAN[self.step]
        if trigger == "start":
            self.priming = priming_location(request)
            if not self.priming:
                self.violate("priming_absent")
            return self.advance(action)
        if trigger == "wake":
            text = text_of(content)
            if results or "<task-notification>" not in text:
                self.violate("unexpected_request_while_idle")
                return {"text": "fixture waiting"}
            return self.wake(text, record, action)
        purpose = trigger.split(":", 1)[1]
        matched = [block for block in results if block.get("tool_use_id") in self.pending]
        if len(results) != 1 or len(matched) != 1:
            self.violate("tool_result_correlation")
            return {"text": "fixture stopped"}
        block = matched[0]
        if self.pending.pop(block["tool_use_id"]) != purpose:
            self.violate("tool_result_purpose")
        if block.get("is_error"):
            self.violate(f"tool_error_{purpose}")
        output = text_of(block.get("content"))
        self.tools.append({"purpose": purpose, "tool_use_id": block["tool_use_id"],
                           "result_bytes": len(output.encode())})
        if not self.on_result(purpose, output):
            return {"text": "fixture stopped"}
        return self.advance(action)

    def wake(self, text, record, action):
        record["notification"] = True
        task_id, output_file = tag(text, "task-id"), tag(text, "output-file")
        code = re.search(r"exit code (-?\d+)", text)
        observation = {"notification_bytes": len(text.encode()), "status": tag(text, "status"),
                       "exit_code": int(code.group(1)) if code else None,
                       "task_matches": bool(self.task) and task_id == self.task[0],
                       "output_file_matches": bool(self.task) and output_file == self.task[1],
                       "body_absent": not any(nonce in text for nonce in self.nonces.values())}
        self.events.append(observation)
        # Enforced before acting: only the owned, completed task's own output file is read.
        failures = [code for code, ok in (
            ("notification_output_file_missing", bool(output_file)),
            ("notification_not_for_armed_task", observation["task_matches"]),
            ("notification_output_file_mismatch", observation["output_file_matches"]),
            ("notification_not_completed", observation["status"] == "completed"),
            ("notification_contains_post_body", observation["body_absent"])) if not ok]
        for failure in failures:
            self.violate(failure)
        if failures == ["notification_not_completed"]:
            # The owned task itself failed: read its output once, only to classify why.
            self.calls += 1
            tool_id = f"toolu_fixture{self.calls:04d}"
            self.pending[tool_id] = "diagnose"
            self.diagnostic = "pending"
            return {"tool": {"id": tool_id, "name": "Bash",
                             "input": {"command": "cat " + shlex.quote(output_file),
                                       "description": "fixture diagnose"}}}
        if failures:
            return {"text": "fixture stopped"}
        self.notified_file = output_file
        return self.advance(action)

    def on_result(self, purpose, output):
        observation = self.events[-1] if self.events else {}
        if purpose == "status":
            self.enrollment = enrollment_ready(output)
            if not self.enrollment:
                self.violate("agent_not_ready_after_join")
                return False
        elif purpose == "arm":
            found = re.search(r"\bID:\s*([A-Za-z0-9_-]+).*?written to:\s*(\S+?)\.?\s*(?:$|You )", output, re.S)
            if not found:
                self.violate("background_result_unrecognized")
                return False
            self.task = (found.group(1), found.group(2))
        elif purpose == "event":
            lines = [line for line in output.splitlines() if line.strip().startswith("{")]
            try:
                event = json.loads(lines[0]) if len(lines) == 1 else None
            except ValueError:
                event = None
            if not isinstance(event, dict):
                self.violate("event_output_unrecognized")
                return False
            observation.update(event_bytes=len(output.encode()), event_keys=sorted(event),
                               metadata_only=set(event) <= EVENT_KEYS,
                               body_absent_event=not any(n in output for n in self.nonces.values()),
                               seq=event.get("seq"), event_key=event.get("event_key"))
            if not observation["metadata_only"] or not observation["body_absent_event"]:
                self.violate("event_not_metadata_only")
            self.event = event
        elif purpose == "read":
            nonce = self.nonces["second" if len(self.events) > 2 else "first"]
            observation.update(read_bytes=len(output.encode()), body_read=nonce in output)
            if not observation["body_read"]:
                self.violate("read_missing_post_body")
        elif purpose == "ack":
            observation.update(ack_bytes=len(output.encode()), owner_ack=ack_confirmed(output, self.event))
            if not observation["owner_ack"]:
                self.violate("ack_not_confirmed")
                return False
        return True

    def advance(self, action):
        self.step += 1
        if action == "idle":
            return {"text": "Waiting for the next Pitput event."}
        if action == "join":
            command, background = f"pitput join /b/{BOARD} --identity {IDENTITY}", False
        elif action == "status":
            command, background = f"pitput agent status --agent-id={IDENTITY}", False
        elif action == "arm":
            command, background = arm_command(self.state, self.timeout), True
        elif action == "event":
            command, background = "cat " + shlex.quote(self.notified_file), False
        elif action == "read":
            command, background = f"pitput --agent {IDENTITY} read {self.event['path']}", False
        else:
            command, background = ack_command(self.state, self.timeout, self.event["cursor"]), False
        self.calls += 1
        tool_id = f"toolu_fixture{self.calls:04d}"
        self.pending[tool_id] = action
        tool_input = {"command": command, "description": f"fixture {action}"}
        if background:
            tool_input["run_in_background"] = True
        return {"tool": {"id": tool_id, "name": "Bash", "input": tool_input}}


def synthetic_usage(spec):
    """Bound fixture counters; these values are never measured or billed usage."""
    usage = spec.get("usage", {"input_tokens": 1, "output_tokens": 1})
    required = {"input_tokens", "output_tokens"}
    allowed = required | {"cache_creation_input_tokens", "cache_read_input_tokens"}
    if (not isinstance(usage, dict) or not required <= usage.keys()
            or not usage.keys() <= allowed
            or any(type(value) is not int or not 0 <= value <= 1_000_000
                   for value in usage.values())
            or sum(usage.values()) > 1_000_000):
        raise ValueError("invalid_synthetic_usage")
    return dict(usage)


def response_id(spec):
    value = spec.get("response_id", "msg_" + secrets.token_hex(8))
    if type(value) is not str or re.fullmatch(r"msg_[a-zA-Z0-9]{1,64}", value) is None:
        raise ValueError("invalid_response_id")
    return value


def sse(model, spec):
    """One Anthropic Messages streaming response for a response spec."""
    usage = synthetic_usage(spec)
    events = [("message_start", {"type": "message_start", "message": {
        "id": response_id(spec), "type": "message", "role": "assistant", "model": model,
        "content": [], "stop_reason": None, "stop_sequence": None,
        "usage": usage}})]
    if "tool" in spec:
        tool = spec["tool"]
        events += [("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {
                        "type": "tool_use", "id": tool["id"], "name": tool["name"], "input": {}}}),
                   ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {
                        "type": "input_json_delta", "partial_json": json.dumps(tool["input"])}})]
        stop = "tool_use"
    else:
        events += [("content_block_start", {"type": "content_block_start", "index": 0,
                                            "content_block": {"type": "text", "text": ""}}),
                   ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                            "delta": {"type": "text_delta", "text": spec["text"]}})]
        stop = "end_turn"
    events += [("content_block_stop", {"type": "content_block_stop", "index": 0}),
               ("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                                  "usage": {"output_tokens": usage["output_tokens"]}}),
               ("message_stop", {"type": "message_stop"})]
    return "".join(f"event: {name}\ndata: {json.dumps(data)}\n\n" for name, data in events).encode()


def message(model, spec):
    """The same response as a non-streaming Messages body."""
    usage = synthetic_usage(spec)
    if "tool" in spec:
        content, stop = [{"type": "tool_use", **spec["tool"]}], "tool_use"
    else:
        content, stop = [{"type": "text", "text": spec["text"]}], "end_turn"
    return {"id": response_id(spec), "type": "message", "role": "assistant", "model": model,
            "content": content, "stop_reason": stop, "stop_sequence": None,
            "usage": usage}


class Responder:
    """Loopback upstream behind the CI relay; every request is recorded."""

    def __init__(self, script):
        owner = self
        self.script, self.paths = script, []

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def log_message(self, *args):
                pass

            def do_POST(self):
                owner.paths.append(self.path.split("?")[0])
                size = int(self.headers.get("Content-Length", "0"))
                try:
                    request = json.loads(self.rfile.read(size))
                    spec = owner.script.decide(request)
                    model = request.get("model", ROUTE)
                    if request.get("stream"):
                        body, kind = sse(model, spec), "text/event-stream"
                    else:
                        body, kind = json.dumps(message(model, spec)).encode(), "application/json"
                except (ValueError, KeyError, TypeError):
                    owner.script.violate("responder_invalid_request")
                    self.send_response(400)
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", kind)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


# ---------------------------------------------------------------- evaluation

def quiet(records, start, end):
    """Every provider request in [start, end), main and auxiliary, labelled."""
    inside = [record for record in records if start <= record["at"] < end]
    return {"seconds": round(end - start, 3), "requests": len(inside),
            "main": sum(r["class"] == "main" for r in inside),
            "auxiliary": sum(r["class"] == "auxiliary" for r in inside)}


def evaluate(script, windows, cleanup):
    """PASS only with every lifecycle observation, quiet idle windows and clean exit."""
    events = script.events
    first, replay, second = (events + [{}, {}, {}])[:3]
    identity_field = "message" if getattr(script, "source", "board") == "dm" else "seq"
    checks = {
        "priming_reached_model": bool(script.priming),
        "plan_complete": script.done,
        "notifications_for_owned_task": len(events) == 3 and all(e.get("task_matches") for e in events),
        "notifications_metadata_only": len(events) == 3 and all(
            e.get("body_absent") and e.get("metadata_only") and e.get("body_absent_event") for e in events),
        "real_reads": bool(first.get("body_read")) and bool(second.get("body_read")),
        "unacked_event_replayed": bool(first.get("event_key")) and replay.get("event_key") == first.get("event_key")
                                  and bool(first.get(identity_field))
                                  and replay.get(identity_field) == first.get(identity_field),
        "owner_acks": bool(replay.get("owner_ack")) and bool(second.get("owner_ack")),
        "acked_event_not_redelivered": bool(second.get(identity_field))
                                      and second.get(identity_field) != first.get(identity_field)
                                      and bool(second.get("event_key"))
                                      and second.get("event_key") != first.get("event_key"),
        "idle_windows_quiet": len(windows) >= 2 and all(
            w["requests"] == 0 and w["cycles"] >= 2 for w in windows),
        "no_violations": not script.violations,
        "cleanup": all(value == "PASS" for value in cleanup.values()) and bool(cleanup),
    }
    return checks, ("PASS" if all(checks.values()) else "FAIL")


# ---------------------------------------------------------------- controller

# HRX launcher errors (internal/app, internal/catalog), checked before onboarding screens.
LAUNCHER_CLASSES = (
    ("hrx_catalog_stale_or_schema", re.compile(r"gateway catalog is stale or has an unsupported schema")),
    ("hrx_catalog_cache", re.compile(r"run hrx refresh")),
    ("hrx_gateway_discovery", re.compile(r"gateway discovery did not succeed")),
    ("hrx_pitput_enrollment", re.compile(r"Pitput enrollment failed|Pitput identity is not ready|"
                                         r"Pitput board is invalid or not enrolled")),
    ("hrx_pitput_cli_missing", re.compile(r"--pitput requires the Pitput CLI")),
    ("hrx_provider_config", re.compile(r"base_url without credentials|cannot read hrx configuration|"
                                       r"invalid hrx TOML configuration|configure exactly one credential")),
    ("hrx_claude_settings", re.compile(r"Claude settings|Claude apiKeyHelper")),
    ("hrx_route_unavailable", re.compile(r"not declared for this model|model is not available|unknown model", re.I)),
)
PROCESS_NAMES = ("hrx", "claude", "node", "pitput", "python3", "bash", "sh")


def descendant_names(root_pid, table):
    """Allowlisted command names of root_pid's live descendants, as counts only."""
    owned, counts = {root_pid}, {}
    while True:
        found = owned | {pid for pid, (ppid, _) in table.items() if ppid in owned}
        if found == owned:
            break
        owned = found
    for pid in owned - {root_pid}:
        name = table[pid][1] if table[pid][1] in PROCESS_NAMES else "other"
        counts[name] = counts.get(name, 0) + 1
    return dict(sorted(counts.items()))


def process_names():
    output = subprocess.check_output(["ps", "-eo", "pid=,ppid=,comm="], text=True, timeout=10)
    table = {}
    for line in output.splitlines():
        fields = line.split(None, 2)
        if len(fields) == 3:
            table[int(fields[0])] = (int(fields[1]), Path(fields[2].strip()).name[:15])
    return table


def launch_diagnostics(main_exit, relay_events, screen, supervisor_alive, descendants, timeline=()):
    """Bounded evidence for a launch that never armed: no terminal text, keys or paths."""
    return {"screen_timeline": list(timeline),"hrx_exit": main_exit.get("status") if main_exit else None,
            "supervisor_alive_at_deadline": supervisor_alive,
            "hrx_alive_at_deadline": supervisor_alive and not main_exit,
            "descendants_at_deadline": descendants,
            "relay_requests": len(relay_events),
            "relay_outcomes": sorted({f'{e.get("http_status")}:{e.get("local_limit") or "-"}'
                                      for e in relay_events})[:10],
            "screen_class": screen_class(screen), "screen_keywords": screen_keywords(screen),
            "screen_bytes": len(screen)}


SEED_PARTS = ("theme", "trust", "bypass")


def seed_parts(value):
    """Which synthetic first-run answers an operator selected: "1" means all (as run
    4147), otherwise a comma list of theme, trust and bypass. Anything else seeds
    nothing rather than guessing, so the default stays a fresh configuration."""
    value = value.strip()
    if value == "1":
        return SEED_PARTS
    chosen = {part.strip() for part in value.split(",") if part.strip()}
    if not chosen or not chosen <= set(SEED_PARTS):
        return ()
    return tuple(part for part in SEED_PARTS if part in chosen)


def seed_state(parts, project):
    """.claude.json for the chosen parts only (keys as seeded in run 4147)."""
    state = {}
    if "theme" in parts:
        state.update(hasCompletedOnboarding=True, theme="dark")
    if "trust" in parts:
        state["projects"] = {str(project): {"hasTrustDialogAccepted": True}}
    if "bypass" in parts:
        state["bypassPermissionsModeAccepted"] = True
    return state


HRX_INIT_PARTS = ("theme", "trust", "bypass")


def hrx_init_parts(value):
    """Parts for the real `hrx --claude-init` (product path): a comma list of theme,
    trust and bypass. Anything else is no parts, which the caller reports BLOCKED."""
    chosen = [part.strip() for part in value.split(",") if part.strip()]
    if not chosen or len(set(chosen)) != len(chosen) or not set(chosen) <= set(HRX_INIT_PARTS):
        return ()
    return tuple(part for part in HRX_INIT_PARTS if part in chosen)


def managed_mode(environ, env):
    """Validate the product-path request before launch. It never combines with the
    fixture seed, and HRX must create the managed directory itself."""
    requested = environ.get("HRX_CLAUDE_ATTENTION_HRX_INIT", "")
    if not requested:
        return ()
    parts = hrx_init_parts(requested)
    if not parts:
        raise Blocked("hrx_init_parts_invalid")
    if environ.get("HRX_CLAUDE_ATTENTION_SEED_ONBOARDING"):
        raise Blocked("hrx_init_and_fixture_seed_are_exclusive")
    if Path(env["CLAUDE_CONFIG_DIR"]).exists():
        raise Blocked("hrx_managed_config_dir_not_fresh")
    return parts


def managed_args(env, parts):
    """hrx flags for an HRX-managed Claude config at the run's isolated CLAUDE_CONFIG_DIR
    (the same path, so HRX sees no conflict); empty when the product path is off."""
    if not parts:
        return []
    return ["--claude-config-dir", env["CLAUDE_CONFIG_DIR"], "--claude-init", ",".join(parts)]


def managed_provenance(directory):
    """Bounded evidence from HRX's own marker: who created it and which parts it
    initialized, with no paths or times."""
    try:
        marker = json.loads((Path(directory) / ".hrx-claude-config.json").read_text())
    except (OSError, ValueError):
        return None
    if not isinstance(marker, dict):
        return None
    initialized = marker.get("initialized") if isinstance(marker.get("initialized"), list) else []
    return {"created_by": marker.get("created_by"), "schema": marker.get("schema"),
            "initialized": [entry.get("part") for entry in initialized if isinstance(entry, dict)][:10]}


def stop_main(resources, report):
    """Stop the main Claude session (drain first, then its boundary) before the
    repeat/resume launches, so they never share the live session. Teardown then
    skips these already-recorded steps."""
    cleanup = report["cleanup"]
    terminal = resources.pop("terminal", None)
    if terminal:
        cleanup["drain_stopped"] = "PASS" if terminal.close() else "FAIL"
        if cleanup["drain_stopped"] != "PASS":
            resources.pop("master", None)  # A live reader may still hold it: never close it.
    supervisor = resources.pop("supervisor", None)
    if supervisor:
        status = stop_supervised(supervisor, resources["summary"], {"signals": []})
        report["supervisor"] = read_summary(resources["summary"])
        cleanup["process_boundary"] = "PASS" if status == "clean" else "FAIL"


def managed_session_id(home, directory):
    """The native id HRX recorded for the session launched with this managed config."""
    try:
        index = json.loads((Path(home) / "state/hrx/sessions/index.json").read_text())
    except (OSError, ValueError):
        return None
    matches = []
    for record in index.get("sessions", []) if isinstance(index, dict) else []:
        if (isinstance(record, dict) and record.get("harness") == "claude"
                and record.get("claude_config_dir") == str(directory) and record.get("native_id")):
            matches.append(record)
    if not matches:
        return None
    # The index is sorted by UUID, not recency. After /clear the predecessor
    # remains indexed; a repeat/resume check must select the current session.
    def updated(record):
        stamp = record.get("updated_at", "0001-01-01T00:00:00+00:00")
        # Preserve Go's nanoseconds separately; Python 3.9 cannot parse all of
        # its variable-width fractions and datetime has only microseconds.
        fraction = re.search(r"\.(\d{1,9})(?=Z|[+-]\d{2}:\d{2}$)", stamp)
        nanos = int(fraction.group(1).ljust(9, "0")) if fraction else 0
        if fraction:
            stamp = stamp[:fraction.start()] + stamp[fraction.end():]
        return datetime.fromisoformat(stamp.replace("Z", "+00:00")), nanos

    try:
        return max(matches, key=updated)["native_id"]
    except (ValueError, TypeError):
        return None


def first_run_state(directory, workspace):
    """Which first-run answers are present now, as booleans only: in .claude.json
    and in settings.json, where Claude may migrate the bypass acceptance."""
    def load(name):
        try:
            value = json.loads((Path(directory) / name).read_text())
            return value if isinstance(value, dict) else None
        except (OSError, ValueError):
            return None

    state, settings = load(".claude.json"), load("settings.json")
    project = ((state or {}).get("projects") or {}).get(str(workspace)) or {}
    return {"state_readable": state is not None,
            "hasCompletedOnboarding": (state or {}).get("hasCompletedOnboarding") is True,
            "theme_set": "theme" in (state or {}),
            "workspace_trusted": isinstance(project, dict) and project.get("hasTrustDialogAccepted") is True,
            "bypassPermissionsModeAccepted": (state or {}).get("bypassPermissionsModeAccepted") is True,
            "settings_readable": settings is not None,
            "settings_skipDangerousModePermissionPrompt":
                (settings or {}).get("skipDangerousModePermissionPrompt") is True}


def trust_workspace_match(directory, project):
    """Compared privately: does every trust record name this launch's canonical
    workspace? A repeat in another workspace would legitimately add a record, which
    is a fixture mismatch rather than a product mutation."""
    try:
        marker = json.loads((Path(directory) / ".hrx-claude-config.json").read_text())
    except (OSError, ValueError):
        return None
    workspaces = [entry.get("workspace") for entry in (marker.get("initialized") or [])
                  if isinstance(entry, dict) and entry.get("part") == "trust"]
    canonical = os.path.realpath(project)
    return {"trust_records": len(workspaces), "all_match_launch_workspace": all(w == canonical for w in workspaces)}


def marker_bytes(directory):
    try:
        return (Path(directory) / ".hrx-claude-config.json").read_bytes()
    except OSError:
        return None


def marker_change(before, after):
    """How the marker changed, without contents: missing, unparseable or present on
    each side; the names of changed top-level fields; per-part record counts."""
    def shape(data):
        if data is None:
            return "missing", None
        try:
            value = json.loads(data)
        except ValueError:
            return "unparseable", None
        return ("present", value) if isinstance(value, dict) else ("unparseable", None)

    def counts(value):
        found = {}
        for entry in (value or {}).get("initialized") or []:
            if isinstance(entry, dict) and entry.get("part") in HRX_INIT_PARTS:
                found[entry["part"]] = found.get(entry["part"], 0) + 1
        return dict(sorted(found.items()))

    (state_before, old), (state_after, new) = shape(before), shape(after)
    fields = sorted(name for name in set(old or {}) | set(new or {})
                    if json.dumps((old or {}).get(name), sort_keys=True)
                    != json.dumps((new or {}).get(name), sort_keys=True))[:10]
    return {"before": state_before, "after": state_after, "bytes_equal": before == after and before is not None,
            "changed_fields": fields if old is not None and new is not None else None,
            "part_counts_before": counts(old), "part_counts_after": counts(new)}


def marker_digest(directory):
    """SHA-256 of the whole provenance marker: any byte change (time, uid, workspace,
    records) is visible. Only the digest is compared; it is never reported."""
    try:
        return hashlib.sha256((Path(directory) / ".hrx-claude-config.json").read_bytes()).hexdigest()
    except OSError:
        return None


PROBE = """#!/bin/sh
# Instrumentation for the managed-config resume check: record whether this Claude
# child received the expected configuration directory, then exec the real Claude.
if [ "${CLAUDE_CONFIG_DIR-}" = "$HRX_PROBE_EXPECTED" ]; then printf match > "$HRX_PROBE_OUT"
else printf mismatch > "$HRX_PROBE_OUT"; fi
exec "$HRX_PROBE_REAL" "$@"
"""


def probe_environment(environ, root, phase, expected, real_claude):
    """A copy of environ whose PATH resolves `claude` to the labelled probe."""
    probe_dir = Path(root) / "claude-probe"
    probe_dir.mkdir(mode=0o700, exist_ok=True)
    script = probe_dir / "claude"
    if not script.exists():
        script.write_text(PROBE)
        script.chmod(0o700)
    out = probe_dir / (phase + ".observed")
    return dict(environ, PATH=str(probe_dir) + os.pathsep + environ["PATH"], HRX_PROBE_OUT=str(out),
                HRX_PROBE_EXPECTED=str(expected), HRX_PROBE_REAL=real_claude), out


def observed(path):
    try:
        return Path(path).read_text()
    except OSError:
        return None


def repeat_resume(hrx, env, project, parts, directory, root, run, real_claude):
    """Real-hrx checks after the main session stopped, each ending at `claude --version`
    through a labelled probe that records the child's effective CLAUDE_CONFIG_DIR:
    a repeat launch must leave the marker byte-identical and give the child the managed
    directory; a resume WITHOUT CLAUDE_CONFIG_DIR must give the child the restored
    directory from the session record; a resume naming another directory must be
    refused before any Claude child runs and must create nothing."""
    # The same model and identity as the main launch, so the repeat is genuine.
    base = [hrx, "claude", "--model", ROUTE, "--agent-identity", IDENTITY]
    without = {name: value for name, value in env.items() if name != "CLAUDE_CONFIG_DIR"}
    before = marker_digest(directory)
    raw_before = marker_bytes(directory)
    # Bounded evidence for any marker change: part names and answer presence only.
    result_diagnostics = {"parts_before": (managed_provenance(directory) or {}).get("initialized"),
                          "state_before_repeat": first_run_state(directory, project)}

    def phase(name, command, environ):
        probed, out = probe_environment(environ, root, name, directory, real_claude)
        captured = run(command, probed)
        return {"exit_code": captured.get("exit_code"), "cleanup": captured.get("cleanup"),
                "timed_out": captured.get("timed_out"), "child_config": observed(out)}

    result = {"instrumentation": "claude_config_dir_probe",
              "repeat": phase("repeat", base + managed_args(env, parts) + ["--", "--version"], env),
              **result_diagnostics}
    result["parts_after_repeat"] = (managed_provenance(directory) or {}).get("initialized")
    result["trust_workspace"] = trust_workspace_match(directory, project)
    result["state_after_repeat"] = first_run_state(directory, project)
    result["marker_change_repeat"] = marker_change(raw_before, marker_bytes(directory))
    result["marker_unchanged_after_repeat"] = before is not None and marker_digest(directory) == before
    native = managed_session_id(env["HOME"], directory)
    result["session_recorded"] = bool(native)
    if native:
        result["resume"] = phase("resume", [hrx, "claude", "--resume", native, "--", "--version"], without)
        other = Path(root) / "other-claude-config"
        result["mismatch"] = phase("mismatch", [hrx, "claude", "--resume", native, "--claude-config-dir",
                                                str(other), "--", "--version"], without)
        result["mismatch_created_nothing"] = not other.exists()
    result["marker_unchanged_overall"] = before is not None and marker_digest(directory) == before
    result["marker_change_overall"] = marker_change(raw_before, marker_bytes(directory))

    def ok(name, exit_ok):
        outcome = result.get(name) or {}
        return (outcome.get("cleanup") == "clean" and outcome.get("timed_out") is False
                and exit_ok(outcome.get("exit_code")))

    result["passed"] = bool(
        ok("repeat", lambda code: code == 0) and result["repeat"]["child_config"] == "match"
        and result["marker_unchanged_after_repeat"] and native
        and ok("resume", lambda code: code == 0) and result["resume"]["child_config"] == "match"
        and ok("mismatch", lambda code: code not in (0, None)) and result["mismatch"]["child_config"] is None
        and result["mismatch_created_nothing"] and result["marker_unchanged_overall"])
    return result


def fixture_catalog(now):
    """One Claude Messages route. catalog.Join rejects a catalog without a fresh
    generated_at, which held job 4062 before Claude was ever launched."""
    return {"schema_version": 1, "generated_at": now.isoformat(),
            "coverage": {"providers": [{"source": "gateway", "ok": True}]},
            "models": [{"canonical_id": ROUTE, "available": True, "sources": [
                {"gateway_route": ROUTE, "available": True, "provider": "fixture", "account": "fixture",
                 "details": {"harnesses": {"claude": {"wire_api": "messages"}}}}]}]}


# Fixed vocabulary reported for an unarmed launch: which known words appeared, never text.
SCREEN_VOCABULARY = ("welcome", "claude code", "theme", "text style", "dark mode", "light mode", "trust",
                     "quick safety check", "security notes",
                     "folder", "workspace", "security", "bypass", "permissions", "accept", "login", "log in",
                     "api key", "subscription", "console", "press enter", "continue", "terminal setup",
                     "onboarding", "yes", "no, exit", "error", "update", "telemetry")


def screen_text(screen):
    """Readable terminal text. Ink TUIs draw spaces as cursor moves, so every escape
    sequence becomes a space instead of being deleted (which would glue words)."""
    text = ANSI.sub(" ", screen.decode("utf-8", "replace"))
    return " ".join(text.split()).lower()


def screen_class(screen):
    text = screen_text(screen)
    squeezed = text.replace(" ", "")
    for name, pattern in LAUNCHER_CLASSES + SCREEN_CLASSES:
        flags = pattern.flags | re.I
        # Also match with all spaces removed, for renderers that position each word.
        if re.search(pattern.pattern, text, flags) or re.search(pattern.pattern.replace(" ", ""), squeezed, flags):
            return name
    return "unclassified"


def screen_keywords(screen):
    text = screen_text(screen)
    squeezed = text.replace(" ", "")
    return [word for word in SCREEN_VOCABULARY if word in text or word.replace(" ", "") in squeezed]


class Terminal:
    """PTY drain that answers cursor-position queries and keeps the last 64 KiB.

    It also keeps a bounded timeline of screen classifications (class and allowlisted
    keywords of the recent screen, recorded only on change), so a launch held by a
    sequence of first-run screens shows which ones appeared and in what order."""

    TIMELINE_LIMIT, SAMPLE_SECONDS, RECENT_BYTES = 20, 2.0, 8192

    def __init__(self, master, process, clock=time.monotonic):
        self.master, self.process, self.screen = master, process, b""
        self.clock, self.started, self.sampled = clock, clock(), None
        self.timeline = []
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.drain, daemon=True)
        self.thread.start()

    def drain(self):
        while not self.stop.is_set() and running(self.process):
            try:
                if select.select([self.master], [], [], .2)[0]:
                    data = os.read(self.master, 65536)
                    if b"\x1b[6n" in data:
                        os.write(self.master, b"\x1b[1;1R")
                    self.screen = (self.screen + data)[-65536:]
                self.sample()
            except OSError:
                return

    def sample(self, force=False):
        now = self.clock()
        if not force and self.sampled is not None and now - self.sampled < self.SAMPLE_SECONDS:
            return
        self.sampled = now
        recent = self.screen[-self.RECENT_BYTES:]
        if not recent:
            return
        entry = {"class": screen_class(recent), "keywords": screen_keywords(recent)}
        last = self.timeline[-1] if self.timeline else None
        if last is None or (last["class"], last["keywords"]) != (entry["class"], entry["keywords"]):
            if len(self.timeline) < self.TIMELINE_LIMIT:
                self.timeline.append({"seconds": round(now - self.started, 1), **entry})

    def close(self):
        self.stop.set()
        self.thread.join(5)
        return not self.thread.is_alive()


def controller_pitput(env, *args):
    """Every controller Pitput call goes through here; the controller never ACKs."""
    if "--ack" in args:
        raise AssertionError("the controller must never ACK the agent's events")
    return pitput(env, "--agent", CONTROLLER, *args)


def wait_until(predicate, seconds, step=.2):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(step)
    return predicate()


def cycles(state):
    try:
        return len((Path(state) / "cycles").read_text())
    except OSError:
        return 0


def settle(script, quiet_seconds, bound):
    """Wait until no provider request for quiet_seconds (bounded); return its start."""
    deadline = time.monotonic() + bound
    while time.monotonic() < deadline:
        last = max((r["at"] for r in script.records), default=0)
        if time.monotonic() - last >= quiet_seconds:
            return time.monotonic()
        time.sleep(.5)
    raise Blocked("provider requests did not settle")


def drain_diagnostic(script, seconds=60):
    """Before a failure path tears down Claude, wait (bounded) for the one pending
    diagnostic result, so its classes are collected. A result that never arrives is
    recorded as a timeout. The run stays FAIL either way."""
    def settled():
        # Under the decide lock: a wake that has published its violation but not yet
        # set "pending" is still running, so this waits for the whole transition.
        with script.lock:
            return script.diagnostic != "pending"

    wait_until(settled, seconds, .2)
    with script.lock:
        if script.diagnostic == "pending":
            script.diagnostic = "timeout"
            script.events[-1]["failure_diagnostic"] = "timeout"


def stop_on_violation(script):
    if script.violations:
        drain_diagnostic(script)
        raise Stop("violation:" + script.violations[0])


def idle_window(script, state, timeout):
    """At least two monitor timeout cycles with zero provider requests of any kind."""
    start, before = settle(script, 10, 120), cycles(state)
    wait_until(lambda: cycles(state) - before >= 2 or script.violations, 3 * timeout + 30, 1)
    # A contract violation (for example a failed monitor task) is FAIL, never BLOCKED.
    stop_on_violation(script)
    if cycles(state) - before < 2:
        raise Blocked("monitor cycles were not observed")
    end = time.monotonic()
    return {**quiet(script.records, start, end), "cycles": cycles(state) - before}


def leftovers(marker):
    try:
        output = subprocess.check_output(["ps", "-eo", "pid=,args="], text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return None
    return [line.split(None, 1)[0] for line in output.splitlines() if marker in line]


def teardown(resources, report):
    """Attempt every cleanup step independently; one failure never skips the rest.

    Only acquired resources are touched. Each step records PASS or FAIL, and an
    exception is recorded by class only (never its text) under cleanup_errors."""
    cleanup, errors = report["cleanup"], report.setdefault("cleanup_errors", {})

    def step(name, action):
        try:
            cleanup[name] = "PASS" if action() else "FAIL"
        except Exception as error:  # Bounded evidence: class name only.
            cleanup[name], errors[name] = "FAIL", type(error).__name__

    terminal, supervisor = resources.get("terminal"), resources.get("supervisor")
    if terminal:
        step("drain_stopped", terminal.close)
    if supervisor:
        def boundary():
            status = stop_supervised(supervisor, resources["summary"], {"signals": []})
            report["supervisor"] = read_summary(resources["summary"])
            return status == "clean"
        step("process_boundary", boundary)
    if "master" in resources and (not terminal or cleanup.get("drain_stopped") == "PASS"):
        step("terminal_closed", lambda: os.close(resources["master"]) or True)  # Never under a live reader.
    server = resources.get("server")
    if server:
        def stop_server():
            server.terminate()
            try:
                server.wait(10)
                return True
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(5)
                return False
        step("pitput_server", stop_server)
    for name in ("gateway", "responder"):
        if resources.get(name):
            step(name + "_closed", lambda item=resources[name]: item.close() or True)
    if "postgres" in resources:
        pg_ctl, pg_data, env = resources["postgres"]

        def stop_postgres():
            run([pg_ctl, "-D", str(pg_data), "-m", "fast", "-w", "stop"], env=env, check=False, timeout=60)
            # 3: not running; 4: no cluster was ever created. Anything else may still run.
            return run([pg_ctl, "-D", str(pg_data), "status"], env=env, check=False).returncode in (3, 4)
        step("postgres_stopped", stop_postgres)
    if "root" in resources:
        # Last, after every service stopped: nothing may still name this run's directory.
        def none_left():
            found = leftovers(str(resources["root"]))
            report["leftover_count"] = None if found is None else len(found)
            return found == []
        step("no_leftover_processes", none_left)


def prove(hrx, root, timeout):
    nonces = {"first": secrets.token_hex(16), "second": secrets.token_hex(16)}
    report = {"status": "FAIL", "scope": "linux_deterministic_loopback_only", "route": ROUTE,
              "monitor_timeout_seconds": timeout, "windows": [], "cleanup": {}, "violations": []}
    script = Script(str(root / "agent-state"), timeout, nonces)
    compact_task = os.environ.get("HRX_CLAUDE_COMPACT_TASK") == "1"
    compact_dm = os.environ.get("HRX_CLAUDE_COMPACT_DM") == "1"
    compact_reset = os.environ.get("HRX_CLAUDE_COMPACT_RESET") == "1"
    compact_launch = os.environ.get("HRX_CLAUDE_COMPACT_LAUNCH") == "1"
    compact_resume = os.environ.get("HRX_CLAUDE_COMPACT_RESUME") == "1"
    targeted_probe = os.environ.get("HRX_CLAUDE_TARGETED_PROBE") == "1"
    clear_probe = os.environ.get("HRX_CLAUDE_CLEAR_PROBE") == "1"
    clear_delivery = os.environ.get("HRX_CLAUDE_CLEAR_DELIVERY") == "1"
    clear_recovery = os.environ.get("HRX_CLAUDE_CLEAR_RECOVERY") == "1"
    clear_pending = os.environ.get("HRX_CLAUDE_CLEAR_PENDING") == "1"
    retired_resume = os.environ.get("HRX_CLAUDE_RETIRED_RESUME") == "1"
    post_clear, post_clear_windows = None, []
    if compact_task:
        from claude_compact_task import make_script
        script = make_script(sys.modules[__name__], script.state, timeout, nonces,
                             dm=compact_dm, reset=compact_reset, launch=compact_launch, resume=compact_resume,
                             clear_pending=clear_pending)
        report["adapter"] = "canonical_compact_task"
        report["source"] = script.source
        report["task_configuration"] = "hrx_launch" if compact_launch else "fixture"
    compaction = None
    if os.environ.get("HRX_CLAUDE_ATTENTION_AUTO_COMPACT") == "1":
        from claude_compaction_control import Control
        compaction = Control(script, sys.modules[__name__])
        script = compaction
        report["automatic_compaction"] = {"status": "NOT_RUN"}
    resources = {"root": root}
    try:
        report["stage"] = "prerequisites"
        if targeted_probe and (not compact_launch or not compact_task or compact_dm or clear_probe):
            raise Blocked("targeted_probe_requires_board_compact_launch_without_clear")
        if retired_resume and (not clear_pending or clear_recovery):
            raise Blocked("retired_resume_requires_pending_clear_without_failure_injection")
        if clear_recovery and not clear_delivery:
            raise Blocked("clear_recovery_requires_clear_delivery")
        if clear_pending and (not clear_delivery or compact_reset):
            raise Blocked("pending_clear_requires_separate_clear_delivery")
        if clear_delivery and not clear_probe:
            raise Blocked("clear_delivery_requires_native_clear_probe")
        if clear_probe and (not compact_launch or compact_resume or compaction):
            raise Blocked("clear_probe_requires_separate_compact_launch")
        if compact_resume and not compact_launch:
            raise Blocked("compact_resume_requires_launch_binding")
        if (compact_dm or compact_reset or compact_launch) and not compact_task:
            raise Blocked("compact_source_and_reset_require_compact_task")
        if compact_task and not os.environ.get("HRX_CLAUDE_ATTENTION_HRX_INIT"):
            raise Blocked("compact_task_requires_managed_config_and_separate_reset_proof")
        if compaction and compact_task and (not compact_launch or compact_resume):
            raise Blocked("compact_compaction_requires_launch_without_resume_scenario")
        if compaction and not os.environ.get("HRX_CLAUDE_ATTENTION_HRX_INIT"):
            raise Blocked("automatic_compaction_requires_managed_config")
        if not shutil.which("initdb") or not shutil.which("pg_ctl") or not shutil.which("claude"):
            raise Blocked("postgresql_or_claude_missing")
        report["stage"] = "postgres"
        pg_bin = Path(shutil.which("initdb")).parent
        base = {"PATH": os.environ["PATH"], "HOME": str(root), "LANG": "C.UTF-8", "NO_COLOR": "1"}
        pg_data, pg_socket = root / "postgres", root / "socket"
        pg_socket.mkdir(mode=0o700)
        resources["postgres"] = (str(pg_bin / "pg_ctl"), pg_data, base)  # Before start: always stopped.
        run([str(pg_bin / "initdb"), "-D", str(pg_data), "-U", "fixture", "--auth=trust", "--no-locale",
             "--encoding=UTF8"], env=base, timeout=60)
        run([str(pg_bin / "pg_ctl"), "-D", str(pg_data), "-l", str(root / "postgres.log"),
             "-o", f"-k {pg_socket} -c listen_addresses=''", "-w", "start"], env=base, timeout=60)
        report["stage"] = "pitput_server"
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        enrollment = secrets.token_hex(32)
        private(root / "server.toml", f'''[server]
port = {port}
poll_seconds = 0.05
[server.database]
host = "{pg_socket}"
dbname = "postgres"
user = "fixture"
sslmode = "disable"
[server.agents."ci/bootstrap/agent"]
token_sha256 = "{hashlib.sha256(secrets.token_bytes(32)).hexdigest()}"
boards = ["{BOARD}"]
[server.enrollment.fixture]
token_sha256 = "{hashlib.sha256(enrollment.encode()).hexdigest()}"
boards = ["{BOARD}"]
allow_dm_only = {str(compact_launch and compact_dm).lower()}
''')
        log = open(root / "server.log", "a")
        server = resources["server"] = subprocess.Popen(
            [sys.executable, "-m", "ysg_mail.pitput", "--config", str(root / "server.toml"), "serve"],
            env=base, stdout=log, stderr=log, start_new_session=True)
        log.close()  # The child keeps its own descriptor.
        url = f"http://127.0.0.1:{port}"

        def healthy():
            try:
                with urllib.request.urlopen(url + "/healthz", timeout=1) as response:
                    return json.load(response).get("status") == "ok"
            except (OSError, ValueError):
                return False

        if not wait_until(healthy, 30):
            raise Blocked("pitput_server_unavailable")

        def pitput_home(name):
            home = root / name
            config = home / ".config/pitput"
            private(config / "enrollment.token", enrollment)
            private(config / "enrollment.toml", f'url = "{url}"\ntoken_file = "enrollment.token"\n')
            return home

        report["stage"] = "controller_enrollment"
        controller = {**base, "HOME": str(pitput_home("controller")),
                      "XDG_CONFIG_HOME": str(root / "controller/.config")}
        pitput(controller, "join", f"/b/{BOARD}", "--identity", CONTROLLER)
        controller_pitput(controller, "create", f"/b/{BOARD}/attention", "--author", CONTROLLER,
                          "--title", "Claude attention proof", "--body", "Topic for the attention proof.",
                          "--request-id", "claude-attention-topic")

        if targeted_probe:
            controller_pitput(controller, "create", f"/b/{BOARD}/followed", "--author", CONTROLLER,
                              "--title", "Followed topic", "--body", "Followed delivery proof.",
                              "--request-id", "claude-followed-topic")

        report["stage"] = "relay"
        responder = resources["responder"] = Responder(script)
        catalog = fixture_catalog(datetime.now(timezone.utc))
        policy = {"max_requests": 200, "wall_seconds": 1800, "cell_seconds": 900, "request_seconds": 30,
                  "max_request_bytes": 4_000_000, "max_output_tokens": 4096, "concurrency": 4,
                  "routes": [ROUTE]}
        (root / "gateway").mkdir(mode=0o700)  # The relay writes its throwaway certificate here.
        gateway = resources["gateway"] = Gateway(root / "gateway", responder.url, "loopback-fixture", catalog,
                                                 policy)
        runner = Runner(hrx, gateway, policy)
        report["stage"] = "agent_environment"
        home, project = pitput_home("agent"), root / "project"
        project.mkdir(mode=0o700)
        env = runner.configure(home, project, {"harness": "claude", "route": ROUTE, "reasoning": None})
        binary = home / "bin/pitput"
        if clear_recovery:
            from claude_compact_task import recovery_wrapper
            recovery_marker = root / "clear-prepare-fault"
            private(binary, recovery_wrapper(sys.executable, script.native_id, recovery_marker))
        else:
            private(binary, f'#!/bin/sh\nexec {sys.executable} -m ysg_mail.pitput "$@"\n')
        binary.chmod(0o700)
        env.update(PATH=str(binary.parent) + os.pathsep + env["PATH"], SHELL="/bin/bash",
                   DISABLE_AUTOUPDATER="1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
        report["fixture_env"] = ["DISABLE_AUTOUPDATER", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "SHELL"]
        managed = managed_mode(os.environ, env)
        if compact_task:
            (compaction.script if compaction else script).configure(env)
        if compaction:
            compaction.configure(root, project, env)
        if clear_probe:
            import shlex

            env["HRX_CLAUDE_ATTENTION_AUTHORIZED"] = "1"
            clear_receipt = root / "clear-observation.json"
            command = shlex.join([sys.executable, "-I", str(Path(__file__).with_name("claude_clear_hook.py")),
                                  "--output", str(clear_receipt), "--cwd", str(project)])
            private(project / ".claude/settings.local.json", json.dumps({"hooks": {"SessionStart": [
                {"matcher": "clear", "hooks": [{"type": "command", "command": command, "timeout": 5}]}]}}))
        parts = () if managed else seed_parts(os.environ.get("HRX_CLAUDE_ATTENTION_SEED_ONBOARDING", ""))
        if managed:
            report["onboarding"] = "hrx_managed:" + ",".join(managed)
        elif parts:
            # Operator-selected synthetic first-run state: no credential, no user config.
            private(home / "claude/.claude.json", json.dumps(seed_state(parts, project)))
            # "synthetic_seeded" stays the label for the full seed used by run 4147.
            report["onboarding"] = ("synthetic_seeded" if parts == SEED_PARTS
                                    else "synthetic_seeded:" + ",".join(parts))
        else:
            report["onboarding"] = "fresh_config"

        report["stage"] = "launch"
        summary = resources["summary"] = root / "supervisor.json"
        args = [sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary), "--",
                hrx, "claude", "--model", ROUTE, "--agent-identity", IDENTITY, "--pitput",
                *([] if compact_launch and compact_dm else ["--pitput-board", BOARD]),
                *(["--pitput-dm-only"] if compact_launch and compact_dm else
                  ["--pitput-no-dm"] if compact_launch else []),
                *(["--pitput-follow-thread", f"/b/{BOARD}/followed"] if targeted_probe else []),
                *managed_args(env, managed), "--",
                *(["--session-id", script.native_id] if compact_task else []),
                f"Monitor /b/{BOARD} for mentions of {IDENTITY} and handle each one."]
        master, slave = pty.openpty()
        resources["master"] = master
        try:
            supervisor = resources["supervisor"] = subprocess.Popen(
                args, cwd=project, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        finally:
            os.close(slave)
        terminal = resources["terminal"] = Terminal(master, supervisor)

        def reached(index, failure):
            settled = wait_until(lambda: script.step > index or script.violations
                                 or supervisor.poll() is not None, 300 if index > 1 else 180)
            if not settled or (script.step <= index and not script.violations):
                raise Blocked(failure())
            stop_on_violation(script)

        def not_armed():
            main_exit = read_summary(str(summary) + ".main")
            try:
                descendants = descendant_names(supervisor.pid, process_names())
            except (OSError, subprocess.SubprocessError, ValueError):
                descendants = None
            terminal.sample(force=True)
            report["launch_diagnostics"] = launch_diagnostics(
                main_exit, gateway.requests, terminal.screen, running(supervisor), descendants, terminal.timeline)
            if main_exit:
                return f"hrx_exited_before_arm:{main_exit.get('status')}"
            if script.records:
                return "requests_without_arm"
            return "claude_not_armed:" + report["launch_diagnostics"]["screen_class"]

        # No request at all means HRX exited or a CLI screen held the launch; never a pass.
        reached(ARMED, not_armed)
        if compact_task:
            actual = managed_session_id(env["HOME"], Path(env["CLAUDE_CONFIG_DIR"]))
            report["task_session_matches_native"] = actual == script.native_id
            if not report["task_session_matches_native"]:
                raise Stop("compact_task_native_session_mismatch")
        report["stage"] = "idle_after_arm"
        report["windows"].append(idle_window(script, script.state, timeout))
        if targeted_probe:
            started = time.monotonic()
            for index, body in enumerate(("Unrelated board activity.", "@test/other/agent unrelated mention.")):
                controller_pitput(controller, "post", f"/b/{BOARD}/attention", "--author", CONTROLLER,
                                  "--body", body, "--request-id", f"claude-irrelevant-{index}")
            window = idle_window(script, script.state, timeout)
            report["targeted_selection"] = {"irrelevant_posts": 2, "window": window,
                "requests_after_irrelevant_posts": quiet(script.records, started, time.monotonic())["requests"]}
            if report["targeted_selection"]["requests_after_irrelevant_posts"] != 0:
                raise Stop("irrelevant_board_activity_woke_model")
        for name, nonce, until in (("first", nonces["first"], ROUND_ONE_DONE),
                                   ("second", nonces["second"], ROUND_TWO_DONE)):
            report["stage"] = "round_" + name
            followed_delivery = targeted_probe and name == "second"
            topic = "followed" if followed_delivery else "attention"
            destination = (["dm", "send", IDENTITY] if compact_dm else
                           ["post", f"/b/{BOARD}/{topic}", "--author", CONTROLLER])
            controller_pitput(controller, *destination,
                              "--body", (("Followed topic update. " if followed_delivery else f"@{IDENTITY} handle this. ")
                                         + ("continuation fixture. " * 8 if compact_task else "")
                                         + f"VALUE={nonce}"),
                              "--request-id", f"claude-attention-{name}")
            if clear_pending:
                if not wait_until(lambda: script.clear_paused or script.violations, 180):
                    raise Stop("pending_clear_pause_not_reached")
                stop_on_violation(script)
                settle(script, 3, 30)
                import tomllib
                binding = tomllib.loads(script.config.read_text())
                state_path = (Path(binding["state_directory"])
                              / hashlib.sha256(IDENTITY.encode()).hexdigest() / "state.json")
                pending_state = json.loads(state_path.read_text())
                pending = pending_state.get("pending") or {}
                event_key = script.events[0].get("event_key")
                valid = (bool(event_key) and pending.get("event", {}).get("event_key") == event_key
                         and pending.get("ack_requested") is False
                         and pending_state.get("handled_through") == 0)
                report["pending_at_clear"] = {"verified_unacknowledged": valid, "event_key": event_key}
                if not valid:
                    raise Stop("pending_clear_not_authoritatively_unhandled")
                break
            if compact_resume and name == "first":
                if not wait_until(lambda: script.resume_paused or script.violations, 180):
                    raise Stop("compact_resume_pause_not_reached")
                stop_on_violation(script)
                settle(script, 3, 30)
                stop_main(resources, report)
                prior_cleanup = dict(report["cleanup"])
                if not all(value == "PASS" for value in prior_cleanup.values()):
                    raise Stop("compact_resume_predecessor_not_clean")
                os.close(resources.pop("master"))
                report["compact_resume"] = {"predecessor_cleanup": prior_cleanup,
                                            "requested_session": script.native_id,
                                            "pending_event": script.events[0].get("event_key")}
                report["cleanup"] = {}
                summary = resources["summary"] = root / "resumed-supervisor.json"
                resume_args = [sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary), "--",
                               hrx, "claude", "--resume", script.native_id, "--", "HRX compact resume fixture"]
                script.resume_released = True
                master, slave = pty.openpty()
                resources["master"] = master
                try:
                    supervisor = resources["supervisor"] = subprocess.Popen(
                        resume_args, cwd=project, env=env, stdin=slave, stdout=slave,
                        stderr=slave, start_new_session=True)
                finally:
                    os.close(slave)
                terminal = resources["terminal"] = Terminal(master, supervisor)
            reached(until, lambda: f"round_{name}_incomplete_at_{script.phase()}")
            if compact_resume and name == "first":
                report["compact_resume"]["same_session"] = (
                    managed_session_id(env["HOME"], Path(env["CLAUDE_CONFIG_DIR"])) == script.native_id)
        report["stage"] = "idle_after_rounds"
        if not clear_pending:
            report["windows"].append(idle_window(script, script.state, timeout))
        if compaction:
            report["stage"] = "automatic_compaction_evidence"
            report["automatic_compaction"] = compaction.finish()
        if clear_probe:
            from claude_clear_hook import process_start

            report["stage"] = "native_clear_observation"
            task_pid = int((Path(script.state) / "task.pid").read_text())
            task_started = process_start(task_pid)
            if task_started is None and not clear_pending:
                raise Stop("clear_probe_task_not_live_before_clear")
            script.retiring = True
            os.write(master, b"/clear\r")
            if not wait_until(lambda: clear_receipt.exists() or supervisor.poll() is not None, 30):
                raise Stop("native_clear_hook_not_observed")
            observed = json.loads(clear_receipt.read_text())
            task_stopped = (task_started is None if clear_pending else
                            wait_until(lambda: process_start(task_pid) != task_started, 5))
            report["clear_observation"] = {
                "scope": "session_and_task_lifetime_only_not_delivery_certification",
                "native_hook_observed": observed.get("source") == "clear",
                "old_owned_task_stopped": task_stopped,
                "old_task_completed_before_clear": clear_pending and task_started is None,
                "same_session": observed.get("session_digest") == hashlib.sha256(script.native_id.encode()).hexdigest()}
            if clear_delivery:
                if not task_stopped or report["clear_observation"]["same_session"]:
                    raise Stop("clear_predecessor_not_retired")
                binding_root = script.config.parents[2]

                def current_binding():
                    try:
                        matches = [json.loads(p.read_text()) for p in binding_root.glob(".claude-binding-*.json")]
                        matches = [b for b in matches if b.get("Origin") == script.native_id
                                   and b.get("Owner") == IDENTITY]
                        if len(matches) != 1:
                            return None
                        b = matches[0]
                        if clear_recovery and b.get("Pending"):
                            pending = b["Pending"]
                            if (b.get("Current") != script.native_id or not recovery_marker.exists()
                                    or hashlib.sha256(pending.encode()).hexdigest() != observed.get("session_digest")):
                                return None
                            report["clear_recovery"] = {"injected_prepare_failure": True,
                                                        "pending_native_session_observed": True}
                            # No product state is written here. The next native
                            # UserPromptSubmit must finish this recorded intent.
                            return {**b, "Current": pending, "Paths": [str(binding_root / pending
                                    / ("@dm" if compact_dm else BOARD) / "bridge.toml")]}
                        expected = str(binding_root / b["Current"] / ("@dm" if compact_dm else BOARD) / "bridge.toml")
                        if (b.get("Pending") != "" or script.native_id not in b.get("Retired", [])
                                or b.get("Paths") != [expected]
                                or hashlib.sha256(b["Current"].encode()).hexdigest() != observed.get("session_digest")):
                            return None
                        return b
                    except (OSError, ValueError, KeyError, TypeError):
                        return None

                if not wait_until(lambda: current_binding() is not None, 15):
                    raise Stop("clear_successor_binding_not_committed")
                binding = current_binding()
                after_nonces = {"first": secrets.token_hex(16), "second": secrets.token_hex(16)}
                if clear_pending:
                    after_nonces["first"] = nonces["first"]
                post_clear = make_script(sys.modules[__name__], str(root / "post-clear-state"), timeout,
                                         after_nonces, dm=compact_dm, launch=True, handoff=not retired_resume)
                post_clear.native_id, post_clear.config = binding["Current"], Path(binding["Paths"][0])
                responder.script = post_clear
                if retired_resume:
                    stop_main(resources, report)
                    prior_cleanup = dict(report["cleanup"])
                    if not all(value == "PASS" for value in prior_cleanup.values()):
                        raise Stop("retired_resume_predecessor_not_clean")
                    os.close(resources.pop("master"))
                    report["retired_resume"] = {"predecessor_cleanup": prior_cleanup}
                    report["cleanup"] = {}
                    post_clear.native_id = script.native_id
                    # Resuming restores earlier tool calls. Do not reuse their
                    # IDs when this new scripted plan starts from its first step.
                    post_clear.calls = script.calls
                    post_clear.configure(env)
                    summary = resources["summary"] = root / "retired-resume-supervisor.json"
                    args = [sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary), "--",
                            hrx, "claude", "--resume", script.native_id, "--", "HRX compact clear fixture"]
                    master, slave = pty.openpty()
                    resources["master"] = master
                    try:
                        supervisor = resources["supervisor"] = subprocess.Popen(
                            args, cwd=project, env=env, stdin=slave, stdout=slave,
                            stderr=slave, start_new_session=True)
                    finally:
                        os.close(slave)
                    terminal = resources["terminal"] = Terminal(master, supervisor)
                else:
                    os.write(master, b"HRX compact clear fixture\r")

                def after_reached(index):
                    if not wait_until(lambda: post_clear.step > index or post_clear.violations
                                      or supervisor.poll() is not None, 180):
                        raise Stop("clear_delivery_timeout")
                    stop_on_violation(post_clear)
                    if post_clear.step <= index:
                        raise Stop("clear_session_exited_before_delivery")

                report["stage"] = "post_clear_arm"
                after_reached(ARMED)
                if retired_resume:
                    report["retired_resume"].update(
                        same_session=managed_session_id(env["HOME"], Path(env["CLAUDE_CONFIG_DIR"])) == script.native_id,
                        fresh_config=post_clear.config != script.config)
                if not clear_pending:
                    post_clear_windows.append(idle_window(post_clear, post_clear.state, timeout))
                for name, until in (("first", ROUND_ONE_DONE), ("second", ROUND_TWO_DONE)):
                    report["stage"] = "post_clear_round_" + name
                    destination = (["dm", "send", IDENTITY] if compact_dm else
                                   ["post", f"/b/{BOARD}/attention", "--author", CONTROLLER])
                    if not (clear_pending and name == "first"):
                        controller_pitput(controller, *destination,
                                          "--body", f"@{IDENTITY} handle this. " + "continuation fixture. " * 8
                                          + f"VALUE={after_nonces[name]}",
                                          "--request-id", f"claude-attention-clear-{name}")
                    after_reached(until)
                    if clear_pending and name == "first":
                        report["pending_at_clear"]["received_same_event_after_clear"] = (
                            post_clear.events[0].get("event_key") == report["pending_at_clear"]["event_key"])
                        post_clear_windows.append(idle_window(post_clear, post_clear.state, timeout))
                post_clear_windows.append(idle_window(post_clear, post_clear.state, timeout))
                if retired_resume:
                    # Release the live consumer lock first, so lock contention
                    # cannot masquerade as a durable stale-generation fence.
                    stop_main(resources, report)
                    stale = run(["pitput", "bridge", "claude-task", "--config", str(script.config),
                                 "--agent-id", IDENTITY, "--session-id", script.native_id],
                                env=env, check=False, timeout=10)
                    report["retired_resume"]["stale_task_rejected"] = (
                        stale.returncode != 0 and "claude_task_retired" in stale.stderr)
        if managed:
            report["stage"] = "managed_repeat_resume"
            stop_main(resources, report)
            report["repeat_resume"] = repeat_resume(
                hrx, env, project, managed, Path(env["CLAUDE_CONFIG_DIR"]), root,
                lambda command, environ: capture(command, env=environ, cwd=project,
                                                 deadline=time.monotonic() + 120, abort=lambda: False),
                shutil.which("claude", path=env["PATH"]))
        report["stage"] = "complete"
    except Blocked as error:
        report.update(status="BLOCKED", reason=str(error)[:200])
    except Stop as error:
        report["reason"] = str(error)[:200]
    except Exception as error:  # Unexpected: fail with bounded evidence, never lose the report.
        report["reason"] = "controller_error:" + type(error).__name__
        if isinstance(error, subprocess.CalledProcessError):
            # Executable name and exit code only: never arguments, output or paths.
            command = error.cmd if isinstance(error.cmd, (list, tuple)) else [error.cmd]
            report["failed_command"] = {"name": Path(str(command[0])).name[:40], "exit_code": error.returncode}
    finally:
        if compaction and report.get("automatic_compaction", {}).get("status") != "PASS":
            report["automatic_compaction"] = compaction.failure_receipt()
        teardown(resources, report)
    if os.environ.get("HRX_CLAUDE_ATTENTION_HRX_INIT"):
        report["hrx_managed_provenance"] = managed_provenance(root / "agent" / "claude")
    report["violations"] = script.violations
    report["priming_location"] = script.priming
    report["model_enrolled"] = script.enrollment
    report["notifications"] = script.events
    report["tools"] = script.tools
    report["requests"] = {"main": sum(r["class"] == "main" for r in script.records),
                          "auxiliary": sum(r["class"] == "auxiliary" for r in script.records),
                          "upstream_paths": sorted(set(resources["responder"].paths))
                          if resources.get("responder") else []}
    if report["status"] != "BLOCKED":
        report["checks"], status = evaluate(script, report["windows"], report["cleanup"])
        if targeted_probe:
            selected = report.get("targeted_selection", {})
            report["checks"]["irrelevant_activity_does_not_wake_model"] = (
                selected.get("irrelevant_posts") == 2 and selected.get("requests_after_irrelevant_posts") == 0)
            report["checks"]["mentions_and_followed_topics_delivered"] = (
                len(script.events) == 3 and all(e.get("body_read") for e in script.events[::2]))
            status = "PASS" if all(report["checks"].values()) else "FAIL"
        if clear_pending:
            first = script.events[0] if len(script.events) == 1 else {}
            report["checks"] = {
                "priming_reached_model": bool(script.priming),
                "native_predecessor_read_pending_event": bool(first.get("body_read")) and bool(first.get("task_matches")),
                "predecessor_did_not_ack": not first.get("owner_ack") and report.get("pending_at_clear", {}).get("verified_unacknowledged") is True,
                "no_violations": not script.violations,
                "cleanup": bool(report["cleanup"]) and all(v == "PASS" for v in report["cleanup"].values()),
            }
        if compact_task:
            report["checks"]["compact_task_native_session"] = report.get("task_session_matches_native") is True
            reads = script.events[::2]
            report["checks"]["compact_continuations_complete"] = len(reads) == (1 if clear_pending else 2) and all(
                e.get("read_fragments", 0) > 1 and e.get("read_contiguous_complete") for e in reads)
            if compact_reset:
                report["checks"]["compact_reset_and_retry_received"] = (
                    len(script.events) == 3 and script.events[1].get("reset_primers_received") == 2)
            if compact_resume:
                report["checks"]["compact_pending_resume"] = (
                    script.resume_received and report.get("compact_resume", {}).get("same_session") is True
                    and script.resume_generation_changed
                    and report["checks"]["unacked_event_replayed"])
            status = "PASS" if all(report["checks"].values()) else "FAIL"
        if compaction:
            report["checks"]["automatic_compaction_continuity"] = (
                report.get("automatic_compaction", {}).get("status") == "PASS")
            status = "PASS" if all(report["checks"].values()) else "FAIL"
        if clear_delivery:
            after_checks = {}
            if post_clear is not None:
                after_checks, _ = evaluate(post_clear, post_clear_windows, report["cleanup"])
                if retired_resume:
                    evidence = report.get("retired_resume", {})
                    after_checks["retired_session_resumed_with_fresh_generation"] = all(
                        evidence.get(key) is True for key in ("same_session", "fresh_config", "stale_task_rejected"))
                else:
                    after_checks["fresh_hook_commands_received"] = post_clear.handoff_commands_received
                after_checks["compact_continuations_complete"] = len(post_clear.events) == 3 and all(
                    e.get("read_contiguous_complete") and e.get("read_fragments", 0) > 1
                    for e in post_clear.events[::2])
            report["post_clear_delivery"] = {"scope": "native_scripted_delivery_not_model_judgement",
                                              "checks": after_checks,
                                              "tools": post_clear.tools if post_clear else [],
                                              "notifications": post_clear.events if post_clear else [],
                                              "violations": post_clear.violations if post_clear else []}
            report["checks"]["native_post_clear_delivery"] = bool(after_checks) and all(after_checks.values())
            if clear_pending:
                report["checks"]["native_pending_event_survived_clear"] = (
                    report.get("pending_at_clear", {}).get("received_same_event_after_clear") is True)
            if clear_recovery:
                recovery = report.get("clear_recovery", {})
                report["checks"]["failed_clear_recovered_on_native_prompt"] = bool(
                    recovery.get("injected_prepare_failure") and recovery.get("pending_native_session_observed")
                    and report["checks"]["native_post_clear_delivery"])
            status = "PASS" if all(report["checks"].values()) else "FAIL"
        if os.environ.get("HRX_CLAUDE_ATTENTION_HRX_INIT"):
            # The product path must also survive a repeat launch and a resume.
            report["checks"]["managed_repeat_resume"] = bool((report.get("repeat_resume") or {}).get("passed"))
            status = "PASS" if all(report["checks"].values()) else "FAIL"
        # A stopped or crashed controller is FAIL even if every recorded check held.
        report["status"] = "FAIL" if "reason" in report else status
    return report


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hrx", required=True)
    parser.add_argument("--output", type=Path, default=Path("artifacts/claude-attention"))
    parser.add_argument("--monitor-timeout", type=int, default=20)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    if os.environ.get("HRX_CLAUDE_ATTENTION_AUTHORIZED") != "1":
        report = {"status": "BLOCKED", "reason": "not_authorized_native_fixture"}
    elif not sys.platform.startswith("linux"):
        report = {"status": "BLOCKED", "reason": "linux_only"}
    else:
        report = None
        provenance = None
        try:
            if os.environ.get("HRX_CLAUDE_COMPACT_TASK") == "1":
                sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration"))
                from pitput_targeted_launch import pitput_provenance
                if not os.environ.get("HRX_PITPUT_SOURCE_SHA"):
                    raise Blocked("compact_task_requires_exact_pitput_source")
                provenance = pitput_provenance(entrypoint=Path(__file__).resolve())
            with tempfile.TemporaryDirectory(prefix="hrx-claude-attention-", dir="/tmp") as temp:
                report = prove(str(Path(args.hrx).resolve()), Path(temp).resolve(), args.monitor_timeout)
                if provenance is not None:
                    report["pitput_provenance"] = provenance
        except Exception as error:  # Including temporary-directory removal: still write the report.
            failure = "controller_error:" + type(error).__name__
            if report is None:
                report = {"status": "FAIL", "reason": failure}
            else:  # prove() returned; only removing the run directory failed.
                report.update(status="FAIL", reason=report.get("reason") or failure, run_directory_removed=False)
    try:
        report["claude_version"] = subprocess.run(["claude", "--version"], capture_output=True, text=True,
                                                  timeout=30).stdout.strip()[:80]
    except (OSError, subprocess.SubprocessError):
        report["claude_version"] = None
    (args.output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    failed = "" if report["status"] == "PASS" else f'<failure message="{report.get("reason", "checks")}"/>'
    (args.output / "junit.xml").write_text(
        '<testsuite name="claude-attention" tests="1" failures="%d"><testcase name="claude_native_attention">'
        "%s</testcase></testsuite>\n" % (report["status"] != "PASS", failed))
    print(json.dumps({key: report.get(key) for key in ("status", "reason", "checks", "violations")}))
    return 0 if report["status"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
