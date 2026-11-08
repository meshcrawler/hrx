"""Native cross-model resume diagnostic: real HRX and pinned native CLIs, scripted loopback models.

For each harness a session starts on one fixture model and resumes on a different
one, through the real `hrx` launch/`--resume` path (Runner.cell), the CI relay
(gateway.py) and the owned-process boundary. A scripted provider plays both models
deterministically, so the diagnostic proves the transport contract:
- the same native session;
- the source conversation present in the resumed provider request;
- the target model and route selected;
- a native tool round trip consumed;
- clean shutdown in both phases.
It does not prove model judgement and is never paid semantic or release
certification.

    HRX_NATIVE_CROSS_RESUME_AUTHORIZED=1 python3 tests/compatibility/native_cross_resume.py \\
        --hrx PATH --output artifacts/native-cross-resume
"""

import argparse
from datetime import datetime, timezone
import http.server
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import sys
import tempfile
import threading
import time
import xml.etree.ElementTree as ET

from gateway import Gateway
from run import Runner

CLASSIFICATION = "native_diagnostic_not_certification"
PROTOCOLS = {"claude": "messages", "codex": "responses", "opencode": "chat_completions"}
NONCE = re.compile(r"[0-9a-f]{48}")


class Violation(RuntimeError):
    """The request broke the scripted contract (model, protocol, tool or history)."""


# ---------------------------------------------------------------- cells and catalog

def pair_cells(harness):
    def cell(role):
        return {"id": f"native-{harness}-{role}", "harness": harness, "route": f"fixture-{harness}-{role}",
                "canonical_model": f"fixture-{harness}-{role}-model", "protocol": PROTOCOLS[harness],
                "reasoning": "low" if harness == "codex" else None}
    return cell("source"), cell("target")


def codex_template_slug(path):
    """A slug from the Codex models template that fixture routes borrow schemas from."""
    try:
        models = json.loads(Path(path).read_text()).get("models") or []
    except (OSError, ValueError, AttributeError):
        return None
    slugs = [m.get("slug") for m in models if isinstance(m, dict) and isinstance(m.get("slug"), str)]
    return slugs[0] if slugs else None


def fixture_catalog(now, codex_template=None):
    models = []
    for harness, protocol in PROTOCOLS.items():
        for cell in pair_cells(harness):
            capability = {"wire_api": protocol, "reasoning_levels": [], "default_reasoning": ""}
            if harness == "codex":
                capability.update(reasoning_levels=["low"], default_reasoning="low")
                if codex_template:
                    capability["catalog_template"] = codex_template
            models.append({"canonical_id": cell["canonical_model"], "available": True, "sources": [
                {"gateway_route": cell["route"], "available": True, "provider": "fixture", "account": "fixture",
                 "details": {"harnesses": {harness: capability}, "max_input_tokens": 128000,
                             "max_output_tokens": 8192}}]})
    return {"schema_version": 1, "generated_at": now.isoformat(),
            "coverage": {"providers": [{"source": "gateway", "ok": True}]}, "models": models}


# ---------------------------------------------------------------- scripted model

def request_text(value):
    """All text a provider request carries (messages, tool results, inputs)."""
    if isinstance(value, str):
        return value
    if isinstance(value, dict):
        return "\n".join(request_text(v) for v in value.values())
    if isinstance(value, list):
        return "\n".join(request_text(v) for v in value)
    return ""


def offered_tools(protocol, request):
    tools = request.get("tools") or []
    named = {}
    for tool in tools if isinstance(tools, list) else []:
        if not isinstance(tool, dict):
            continue
        if protocol == "chat_completions":
            function = tool.get("function") if tool.get("type") == "function" else None
            if isinstance(function, dict):
                named[function.get("name")] = function.get("parameters") or {}
        elif protocol == "responses":
            if tool.get("type") == "function":
                named[tool.get("name")] = tool.get("parameters") or {}
        else:
            named[tool.get("name")] = tool.get("input_schema") or {}
    return named


def tool_call(protocol, request, command, workdir):
    """(tool name, arguments) for a shell command, chosen from the offered schema."""
    tools = offered_tools(protocol, request)

    def properties(name):
        schema = tools.get(name)
        return schema.get("properties") if isinstance(schema, dict) else None

    if protocol == "messages":
        if isinstance(properties("Bash"), dict) and "command" in properties("Bash"):
            return "Bash", {"command": command, "description": "fixture"}
    elif protocol == "responses":
        for name in ("shell", "shell_command", "exec_command"):
            props = properties(name)
            if not isinstance(props, dict):
                continue
            arguments = ({"command": ["bash", "-lc", command], "workdir": workdir} if name == "shell" else
                         {"command": command, "workdir": workdir} if name == "shell_command" else
                         {"cmd": command, "workdir": workdir, "yield_time_ms": 30000})
            if set(arguments) <= set(props):
                return name, arguments
    else:
        for name in ("shell", "bash"):
            props = properties(name)
            if isinstance(props, dict) and "command" in props:
                arguments = {"command": command}
                for extra, value in (("workdir", workdir), ("description", "fixture")):
                    if extra in props:
                        arguments[extra] = value
                return name, arguments
    raise Violation("native_tool_schema")


def tool_calls(protocol, request):
    """{call id: tool name} for the tool calls the conversation carries."""
    found = {}
    if protocol == "responses":
        for item in request.get("input") or []:
            if isinstance(item, dict) and item.get("type") == "function_call":
                found[item.get("call_id")] = item.get("name")
        return found
    for message in request.get("messages") or []:
        if not isinstance(message, dict) or message.get("role") != "assistant":
            continue
        if protocol == "chat_completions":
            for call in message.get("tool_calls") or []:
                if isinstance(call, dict) and isinstance(call.get("function"), dict):
                    found[call.get("id")] = call["function"].get("name")
        elif isinstance(message.get("content"), list):
            for block in message["content"]:
                if isinstance(block, dict) and block.get("type") == "tool_use":
                    found[block.get("id")] = block.get("name")
    return found


def tool_results(protocol, request):
    """{call id: (result text, failed)} for the tool results the conversation carries."""
    found = {}
    if protocol == "responses":
        for item in request.get("input") or []:
            if isinstance(item, dict) and item.get("type") == "function_call_output":
                found[item.get("call_id")] = (request_text(item.get("output")), False)
        return found
    for message in request.get("messages") or []:
        if not isinstance(message, dict):
            continue
        if protocol == "chat_completions" and message.get("role") == "tool":
            found[message.get("tool_call_id")] = (request_text(message.get("content")), False)
        elif protocol == "messages" and isinstance(message.get("content"), list):
            for block in message["content"]:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    found[block.get("tool_use_id")] = (request_text(block.get("content")),
                                                       block.get("is_error") is True)
    return found


def source_challenge(result):
    """The challenge from the source `cat` result: exactly one distinct 48-hex value.
    CLIs may wrap stdout in metadata (exit code, timing), so the value is searched
    for rather than matched against the whole result."""
    values = set(NONCE.findall(result or ""))
    return values.pop() if len(values) == 1 else None


class Script:
    """Plays the source and target models. decide() never touches the network.

    Evidence is the exact source tool round trip only: the scripted `cat` call (its
    id and offered tool name) and its successful, correlated result. The value is
    recorded privately from that result in the source phase; the target request must
    carry the same call/result pair with the same value. Prompt or user text,
    assistant arguments, unrelated hex and target output never count."""

    def __init__(self, source_route, target_route, workdir):
        self.source, self.target, self.workdir = source_route, target_route, workdir
        self.lock = threading.Lock()
        self.calls = 0
        self.ids, self.names = {}, {}  # purpose -> call id / offered tool name of each scripted call
        self.source_value = None
        self.violations, self.observations = [], {"history_retained": None, "phase_routes": {},
                                                  "tool_names": []}

    def violate(self, code):
        if code not in self.violations:
            self.violations.append(code)

    def decide(self, protocol, request):
        with self.lock:
            try:
                return self._decide(protocol, request)
            except Violation as error:
                self.violate(str(error))
                return {"text": "fixture stopped"}

    def round_trip(self, protocol, request, purpose):
        """(present, result text) for a scripted call: the call with its recorded name
        and a successful correlated result must both be in the conversation."""
        call_id = self.ids.get(purpose)
        result = tool_results(protocol, request).get(call_id)
        if call_id is None or result is None:
            return False, None
        if tool_calls(protocol, request).get(call_id) != self.names[purpose] or result[1]:
            raise Violation(purpose + "_round_trip_mismatch")
        return True, result[0]

    def _decide(self, protocol, request):
        route = request.get("model")
        if route not in (self.source, self.target):
            raise Violation("unexpected_model")
        if not request.get("tools"):
            return {"text": "ok"}  # Auxiliary request (titles, summaries): never advances.
        phase = "source" if route == self.source else "target"
        self.observations["phase_routes"].setdefault(phase, set()).add(route)
        # Every tool result must answer one of this script's own calls.
        if set(tool_results(protocol, request)) - set(self.ids.values()):
            raise Violation("tool_result_correlation")
        read, output = self.round_trip(protocol, request, "cat")
        if phase == "source":
            if self.round_trip(protocol, request, "reverse")[0]:
                return {"text": "Done."}
            if not read:
                return self.call("cat", protocol, request, "cat challenge.txt")
            value = source_challenge(output)
            if not value:
                raise Violation("source_tool_result_missing_challenge")
            self.source_value = value
            return self.call("reverse", protocol, request, "printf %s " + shlex.quote(value[::-1]) + " > result.txt")
        # Target: the first resumed request decides whether the source round trip survived.
        retained = read and self.source_value is not None and source_challenge(output) == self.source_value
        if self.observations["history_retained"] is None:
            self.observations["history_retained"] = retained
        if not retained:
            raise Violation("resume_history_missing")
        if self.round_trip(protocol, request, "resume")[0]:
            return {"text": "Done."}
        return self.call("resume", protocol, request, "printf %s " + shlex.quote(self.source_value) + " > resumed.txt")

    def call(self, purpose, protocol, request, command):
        if purpose in self.ids:
            raise Violation("duplicate_" + purpose + "_call")
        name, arguments = tool_call(protocol, request, command, self.workdir)
        self.calls += 1
        self.ids[purpose], self.names[purpose] = f"call_fixture{self.calls:04d}", name
        self.observations["tool_names"].append(name)
        return {"tool": {"id": self.ids[purpose], "name": name, "arguments": arguments}}


# ---------------------------------------------------------------- wire formats

def messages_sse(model, spec):
    blocks = ([("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {
                  "type": "tool_use", "id": spec["tool"]["id"], "name": spec["tool"]["name"], "input": {}}}),
               ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {
                  "type": "input_json_delta", "partial_json": json.dumps(spec["tool"]["arguments"])}})]
              if "tool" in spec else
              [("content_block_start", {"type": "content_block_start", "index": 0,
                                        "content_block": {"type": "text", "text": ""}}),
               ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                        "delta": {"type": "text_delta", "text": spec["text"]}})])
    events = [("message_start", {"type": "message_start", "message": {
                  "id": "msg_" + secrets.token_hex(8), "type": "message", "role": "assistant", "model": model,
                  "content": [], "stop_reason": None, "stop_sequence": None,
                  "usage": {"input_tokens": 1, "output_tokens": 1}}}),
              *blocks,
              ("content_block_stop", {"type": "content_block_stop", "index": 0}),
              ("message_delta", {"type": "message_delta", "delta": {
                  "stop_reason": "tool_use" if "tool" in spec else "end_turn", "stop_sequence": None},
                  "usage": {"output_tokens": 1}}),
              ("message_stop", {"type": "message_stop"})]
    return "".join(f"event: {name}\ndata: {json.dumps(data)}\n\n" for name, data in events).encode()


def responses_sse(spec):
    """The Responses SSE schema proven by Pitput's native Codex runs."""
    response_id = "resp_" + secrets.token_hex(8)
    if "tool" in spec:
        tool = spec["tool"]
        item = {"type": "function_call", "id": "fc_" + tool["id"][5:], "call_id": tool["id"], "name": tool["name"],
                "arguments": json.dumps(tool["arguments"]), "status": "completed"}
        middle = [{"type": "response.output_item.added", "output_index": 0,
                   "item": {**item, "status": "in_progress", "arguments": ""}},
                  {"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": 0,
                   "delta": item["arguments"]},
                  {"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": 0,
                   "arguments": item["arguments"]}]
    else:
        item = {"id": "msg_" + response_id, "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": spec["text"], "annotations": []}]}
        middle = [{"type": "response.output_item.added", "output_index": 0,
                   "item": {**item, "status": "in_progress", "content": []}},
                  {"type": "response.content_part.added", "item_id": item["id"], "output_index": 0,
                   "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}},
                  {"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0,
                   "content_index": 0, "delta": spec["text"]},
                  {"type": "response.output_text.done", "item_id": item["id"], "output_index": 0,
                   "content_index": 0, "text": spec["text"]},
                  {"type": "response.content_part.done", "item_id": item["id"], "output_index": 0,
                   "content_index": 0, "part": item["content"][0]}]
    events = [{"type": "response.created", "response": {"id": response_id, "object": "response",
                                                         "status": "in_progress", "output": []}},
              *middle, {"type": "response.output_item.done", "output_index": 0, "item": item},
              {"type": "response.completed", "response": {"id": response_id, "object": "response",
                                                           "status": "completed", "output": [item]}}]
    return "".join(f"event: {e['type']}\ndata: {json.dumps({**e, 'sequence_number': i})}\n\n"
                   for i, e in enumerate(events)).encode()


def chat_sse(model, spec):
    """The Chat Completions SSE schema proven by Pitput's native OpenCode runs."""
    if "tool" in spec:
        tool = spec["tool"]
        delta = {"role": "assistant", "content": None, "tool_calls": [{
            "index": 0, "id": tool["id"], "type": "function",
            "function": {"name": tool["name"], "arguments": json.dumps(tool["arguments"])}}]}
        finish = "tool_calls"
    else:
        delta, finish = {"role": "assistant", "content": spec["text"]}, "stop"
    chunks = [{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": model,
               "choices": [{"index": 0, "delta": d, "finish_reason": r}]} for d, r in ((delta, None), ({}, finish))]
    return ("".join("data: " + json.dumps(chunk) + "\n\n" for chunk in chunks) + "data: [DONE]\n\n").encode()


REQUEST_SECONDS, SHUTDOWN_SECONDS, MAX_REQUEST_BYTES = 60, 15, 8_000_000


def bounded(function, seconds):
    """Run function in a daemon thread; True only if it returned without raising in time."""
    outcome = []

    def target():
        try:
            function()
            outcome.append(True)
        except Exception:
            outcome.append(False)
    worker = threading.Thread(target=target, daemon=True)
    worker.start()
    worker.join(seconds)
    return outcome == [True]


class Provider:
    """Loopback upstream behind the CI relay; dispatches each protocol to its script."""

    PATHS = {"/v1/messages": "messages", "/v1/responses": "responses", "/v1/chat/completions": "chat_completions"}

    def __init__(self, scripts):
        owner = self
        self.scripts, self.protocols_seen = scripts, []

        self.active, self.idle = 0, threading.Condition()

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"
            timeout = REQUEST_SECONDS  # Bounds every socket read, including the body.

            def log_message(self, *args):
                pass

            def handle(self):
                with owner.idle:
                    owner.active += 1
                try:
                    super().handle()
                finally:
                    with owner.idle:
                        owner.active -= 1
                        owner.idle.notify_all()

            def do_POST(self):
                protocol = owner.PATHS.get(self.path.split("?")[0])
                try:
                    length = int(self.headers.get("Content-Length", ""))
                except ValueError:
                    length = -1
                if not 0 <= length <= MAX_REQUEST_BYTES:
                    self.send_response(413)
                    self.end_headers()
                    return
                try:
                    request = json.loads(self.rfile.read(length))
                except (ValueError, OSError):
                    request = None
                script = owner.scripts.get(str((request or {}).get("model", "")).split("-")[1]
                                           if isinstance(request, dict) and "-" in str(request.get("model")) else "")
                if protocol is None or not isinstance(request, dict) or script is None:
                    self.send_response(400)
                    self.end_headers()
                    return
                owner.protocols_seen.append(protocol)
                if self.headers.get("Authorization") != "Bearer " + FIXTURE_KEY:
                    script.violate("non_fixture_credential")
                if PROTOCOLS[script.harness] != protocol:
                    script.violate("unexpected_protocol")
                spec = script.decide(protocol, request)
                model = request.get("model")
                body = (messages_sse(model, spec) if protocol == "messages" else
                        responses_sse(spec) if protocol == "responses" else chat_sse(model, spec))
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self, seconds=SHUTDOWN_SECONDS):
        """Bounded shutdown; True only when the server loop stopped and no request
        handler is still active."""
        deadline = time.monotonic() + seconds
        stopped = bounded(self.server.shutdown, seconds)
        self.server.server_close()
        self.thread.join(max(0, deadline - time.monotonic()))
        with self.idle:
            self.idle.wait_for(lambda: self.active == 0, max(0, deadline - time.monotonic()))
            idle = self.active == 0
        return stopped and not self.thread.is_alive() and idle


# ---------------------------------------------------------------- evaluation

def phase_cleanups(result):
    """(source, resume) cleanup. When a resume ran, Runner.cell moves the source's to
    initial_cleanup and sets cleanup to the resume's; otherwise cleanup is the source's
    and no resume started (Runner.cell never resumes after a non-PASS source)."""
    if "resume" in result:
        return result.get("initial_cleanup"), result.get("cleanup")
    return result.get("cleanup"), None


FIXTURE_KEY = "loopback-fixture"
# Runner.isolated_env is an allowlist; its only credential is the relay's per-run secret.
CHILD_ENV = {"PATH", "HOME", "LANG", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
             "XDG_STATE_HOME", "XDG_CACHE_HOME", "TMPDIR", "HRX_CI_KEY", "HRX_CA_FILE", "SSL_CERT_FILE",
             "NODE_EXTRA_CA_CERTS", "NO_COLOR", "TERM"}


def require_loopback(url):
    if not re.fullmatch(r"https?://127\.0\.0\.1:\d+/?(v1)?", url.rstrip("/")):
        raise Violation("non_loopback_endpoint")


def evaluate(result, script, source, target):
    """PASS requires every transport fact in both phases; anything else is FAIL."""
    resume = result.get("resume") or {}
    routes = script.observations["phase_routes"]
    source_cleanup, resume_cleanup = phase_cleanups(result)
    checks = {
        "both_phases_pass": result.get("status") == "PASS" and resume.get("status") == "PASS",
        "both_phases_clean": source_cleanup == "clean" and resume_cleanup == "clean",
        "same_session": bool(result.get("session_id")) and resume.get("session_id") == result.get("session_id"),
        "source_tool_round_trip": bool(result.get("filesystem_assertion")) and bool(result.get("tool_results")),
        "resume_recalled_history": bool(result.get("resume_assertion")),
        "history_retained_in_target_request": script.observations["history_retained"] is True,
        "target_route_only_in_resume": routes.get("target") == {target["route"]}
                                       and resume.get("observed_routes") == [target["route"]],
        "exact_route_protocol_both_phases": bool(result.get("exact_route_protocol"))
                                            and bool(resume.get("exact_route_protocol")),
        "fresh_tool_result_consumed": bool(resume.get("fresh_tool_result_consumed")),
        "no_violations": not script.violations,
    }
    if result.get("status") == "BLOCKED":
        return checks, "BLOCKED"
    return checks, "PASS" if all(checks.values()) else "FAIL"


# ---------------------------------------------------------------- bounded, redacted diagnostics

# Only these words survive redaction; every other token (values, paths, ids, keys,
# transcript text) becomes a placeholder. Numbers survive only as 1-3 digits.
SAFE_WORDS = frozenset("""
error errors failed failure unable cannot could not find no such file directory home config configuration
codex claude opencode hrx HRX Codex Claude OpenCode CODEX_HOME CLAUDE_CONFIG_DIR model models catalog
model_provider model_providers provider providers wire_api responses response request requests env_key
request_max_retries stream_max_retries requires_openai_auth supports_websockets websocket websockets WebSocket
features unified_exec web_search disabled enabled true false read reading load loading parse parsing invalid
value type missing field unknown expected found unexpected argument usage help permission denied address
connection connect connecting refused reset closed timeout timed out deadline retry retrying reconnecting
stream disconnected tls TLS ssl SSL certificate certificates cert issuer verify verification handshake
untrusted trust trusted self signed http https HTTP HTTPS status bad forbidden unauthorized not_found
gateway relay base_url login auth authentication key api token tokens exec session thread turn started
completed interrupted cancelled canceled shutdown exit exited code signal killed waiting wait for to the a an
of or and in on at is be was does with from by
""".split())
SAFE_LOWER = frozenset(word.lower() for word in SAFE_WORDS)
TLS_REASONS = frozenset({"TLSV1_ALERT_UNKNOWN_CA", "SSLV3_ALERT_BAD_CERTIFICATE",
                         "SSLV3_ALERT_CERTIFICATE_UNKNOWN", "SSLV3_ALERT_UNSUPPORTED_CERTIFICATE",
                         "TLSV1_ALERT_ACCESS_DENIED", "TLSV1_ALERT_PROTOCOL_VERSION",
                         "SSLV3_ALERT_HANDSHAKE_FAILURE", "CERTIFICATE_VERIFY_FAILED",
                         "UNEXPECTED_EOF_WHILE_READING", "WRONG_VERSION_NUMBER",
                         "NO_SHARED_CIPHER"})
DIAGNOSTIC_PATHS = frozenset({"/dashboard/models.json", "/v1/messages", "/v1/responses",
                              "/v1/chat/completions"})
DIAGNOSTIC_EVENTS = frozenset({"thread.started", "turn.started", "turn.completed", "turn.failed",
                               "item.started", "item.updated", "item.completed", "error", "system",
                               "assistant", "user", "result", "stream_event", "tool_progress",
                               "auth_status", "rate_limit_event", "step_start", "step_finish", "text", "tool_use"})
DIAGNOSTIC_ITEMS = frozenset({"agent_message", "reasoning", "command_execution", "file_change",
                              "mcp_tool_call", "web_search", "todo_list", "error", "context_compaction",
                              "collab_tool_call"})


def diagnostic_path(path):
    path = str(path or "").split("?")[0]
    return path if path in DIAGNOSTIC_PATHS else "<other>"


def redact(text, limit=2048):
    """Keep only allowlisted words and short numbers; collapse everything else."""
    kept, hidden = [], False
    for token in re.findall(r"[A-Za-z_][A-Za-z0-9_]*|\d+|[^\sA-Za-z0-9_]", text or ""):
        if token.lower() in SAFE_LOWER or (token.isdigit() and len(token) <= 3) or token in ":()[]=,.-/":
            kept.append(token)
            hidden = False
        elif not hidden:
            kept.append("<redacted>")
            hidden = True
    return " ".join(kept)[-limit:]


def output_shape(stdout):
    """Line counts and JSON event types of native stdout: never values or transcript."""
    lines = stdout.decode(errors="replace").splitlines()
    types, errors, json_lines = [], [], 0
    for line in lines:
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        json_lines += 1
        raw_kind = event.get("type")
        kind = raw_kind if isinstance(raw_kind, str) and raw_kind in DIAGNOSTIC_EVENTS else "<other>"
        item = event.get("item") if isinstance(event.get("item"), dict) else {}
        if isinstance(item.get("type"), str):
            kind += ":" + (item["type"] if item["type"] in DIAGNOSTIC_ITEMS else "<other>")
        types.append(kind)
        if "error" in kind or "failed" in kind:
            message = event.get("message") or item.get("message") or (event.get("error") or {})
            errors.append(redact(message if isinstance(message, str) else json.dumps(message), 512))
    return {"lines": len(lines), "json_lines": json_lines,
            "event_types": types if len(types) <= 40 else types[:30] + ["..."] + types[-10:],
            "error_messages": errors[-5:]}


# HRX's own stderr lines (internal/app/run.go, session_launch.go, cmd/hrx/main.go).
# Only marker labels are kept, never the line text.
HRX_MARKERS = (
    ("hrx_session_capture", re.compile(r"hrx: session=\S+; private metadata will be indexed")),
    ("hrx_hook_trust_notice", re.compile(r"hrx: session capture requires trusting this hrx SessionStart hook")),
    ("hrx_codex_catalog_skipped", re.compile(r"hrx: codex catalog skipped ")),
    ("hrx_profile", re.compile(r"hrx: profile=\S+ route=\S+$")),
    ("hrx_launch_marker", re.compile(r"hrx: \S+ -> \S+; reasoning=.*; provider=")),
)
CAPTURE_TAIL = 65536  # owned_capture keeps the last 64 KiB of stderr.


def launch_stage(stderr):
    """Report observed launch markers, not proof that native execution succeeded.

    Unknown HRX lines can be informational. A truncated tail cannot establish
    that an absent launch marker was never emitted.
    """
    markers = []
    lines = stderr.decode(errors="replace").splitlines()
    for line in lines:
        label = next((name for name, pattern in HRX_MARKERS if pattern.match(line)), None)
        if label is None and line.startswith("hrx: "):
            label = "hrx_unclassified"
        if label and label not in markers:
            markers.append(label)
    if "hrx_launch_marker" in markers:
        stage = "launch_marker_observed"
    elif len(stderr) >= CAPTURE_TAIL:
        stage = "unknown_stderr_truncated"
    else:
        stage = "launch_marker_not_observed"
    return {"stage": stage, "markers": markers, "stderr_lines": len(lines)}


def observe_relay(gateway, limit=500):
    """Record every relay response (method, safe path, status, upgrade) and every
    failed TLS accept (class and OpenSSL reason), which the relay otherwise drops
    before logging a request. Diagnostic only: behaviour is unchanged."""
    log, server = [], gateway.server
    base = server.RequestHandlerClass

    class Observed(base):
        def send_response(self, code, message=None):
            path = diagnostic_path(getattr(self, "path", ""))
            headers = getattr(self, "headers", None)
            if len(log) < limit:
                method = getattr(self, "command", None)
                log.append({"method": method if method in {"GET", "POST", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS"}
                            else "<other>", "status": code, "path": path,
                            "upgrade": bool(headers and headers.get("Upgrade"))})
            super().send_response(code, message)

    accept = server.get_request

    def get_request():
        try:
            return accept()
        except OSError as error:
            if len(log) < limit:
                reason = getattr(error, "reason", None)
                log.append({"accept_error": type(error).__name__,
                            "reason": reason if isinstance(reason, str) and reason in TLS_REASONS
                            else "<other>"})
            raise
    server.RequestHandlerClass, server.get_request = Observed, get_request
    return log


class ManagedRunner(Runner):
    """Runner whose Claude launches use HRX-managed first-run initialization and
    whose launches carry bounded, redacted diagnostics; the paid lanes keep the plain
    Runner."""

    capture_stderr = True

    def diagnose(self, cell, captured):
        return {"launch": launch_stage(captured.get("stderr") or b""),
                "stdout": output_shape(captured.get("stdout") or b""),
                "stderr": redact((captured.get("stderr") or b"").decode(errors="replace")),
                "supervisor_exit": captured.get("supervisor_exit")}

    def command(self, cell, prompt, resume=None):
        args = super().command(cell, prompt, resume)
        if cell["harness"] == "claude" and not resume:
            # The resume restores the managed directory from the session record.
            at = args.index("--")
            args[at:at] = ["--claude-config-dir", self.claude_dirs[cell["id"].rsplit("-", 1)[0]],
                           "--claude-init", "theme,trust,bypass"]
        return args

    def configure(self, home, project, cell):
        env = super().configure(home, project, cell)
        # Fixture-only credentials: the child gets exactly the isolated allowlist and
        # the relay's own secret, so no paid key or endpoint can reach a native child.
        if set(env) - CHILD_ENV or env.get("HRX_CI_KEY") != self.gateway.secret:
            raise Violation("child_environment_not_fixture_only")
        self.claude_dirs = getattr(self, "claude_dirs", {})
        self.claude_dirs[cell["id"].rsplit("-", 1)[0]] = env["CLAUDE_CONFIG_DIR"]
        return env


def close_servers(gateway, provider, meta):
    """Independently attempt both bounded shutdowns and record each outcome."""
    if gateway is not None:
        deadline = time.monotonic() + SHUTDOWN_SECONDS
        closed = bounded(gateway.close, SHUTDOWN_SECONDS) and not gateway.thread.is_alive()
        # Gateway.close stops only the listener; its handler threads are daemons, so an
        # upstream request may still be in flight. Stopped means idle as well.
        with gateway.lock:
            gateway.lock.wait_for(lambda: gateway.active == 0, max(0, deadline - time.monotonic()))
            idle = gateway.active == 0
        meta["gateway_stopped"] = closed and idle
    if provider is not None:
        try:
            meta["provider_stopped"] = provider.close()
        except Exception:
            meta["provider_stopped"] = False


def run_pair(runner, gateway, script, harness, root, row, policy):
    source, target = pair_cells(harness)
    gateway.allowed_routes = {source["route"]}
    try:
        result = runner.cell(source, root / harness, resume_cell=target)
    finally:
        gateway.allowed_routes = set(policy["routes"])
    checks, status = evaluate(result, script, source, target)
    source_cleanup, resume_cleanup = phase_cleanups(result)
    row.update(status=status, checks=checks, violations=script.violations,
               reason=result.get("reason") if status != "PASS" else None,
               tool_names=script.observations["tool_names"],
               phases={"source": {k: result.get(k) for k in PHASE_FIELDS},
                       "resume": {k: (result.get("resume") or {}).get(k) for k in PHASE_FIELDS}},
               cleanup={"source": source_cleanup, "resume": resume_cleanup},
               resume_started="resume" in result)
    return "unknown" not in (source_cleanup, resume_cleanup)


PHASE_FIELDS = ("exit_code", "timed_out", "inference_requests", "tool_results", "observed_routes",
                "http_statuses", "duration_seconds", "diagnostics")


def run(hrx, root, harnesses, rows, meta):
    """Fill one predeclared row per selected pair. Rows start NOT_RUN, so an unknown
    cleanup or an exception leaves every unreached pair explicitly unrun, and every
    finished pair keeps its evidence."""
    for harness in harnesses:
        source, target = pair_cells(harness)
        rows.append({"id": f"native-cross-resume-{harness}", "harness": harness, "source_route": source["route"],
                     "target_route": target["route"], "classification": CLASSIFICATION,
                     "status": "NOT_RUN", "reason": "not_reached"})
    codex_template = codex_template_slug(os.environ.get("HRX_CI_CODEX_CATALOG", ""))
    scripts = {}
    for harness in harnesses:
        source, target = pair_cells(harness)
        scripts[harness] = Script(source["route"], target["route"], str(root / harness / "project"))
        scripts[harness].harness = harness
    gateway = provider = None
    try:
        provider = Provider(scripts)
        require_loopback(provider.url)
        policy = {"max_requests": 400, "wall_seconds": 2400, "cell_seconds": 300, "request_seconds": REQUEST_SECONDS,
                  "max_request_bytes": MAX_REQUEST_BYTES, "max_output_tokens": 4096, "concurrency": 2,
                  "routes": [cell["route"] for h in harnesses for cell in pair_cells(h)]}
        meta["synthetic_routes"] = policy["routes"]
        (root / "gateway").mkdir(mode=0o700)
        gateway = Gateway(root / "gateway", provider.url, FIXTURE_KEY,
                          fixture_catalog(datetime.now(timezone.utc), codex_template), policy)
        require_loopback(gateway.url)
        relay = observe_relay(gateway)
        runner = ManagedRunner(hrx, gateway, policy)
        for harness, row in zip(harnesses, rows[-len(harnesses):]):
            if time.monotonic() >= gateway.deadline:
                # One aggregate monotonic deadline bounds the whole diagnostic.
                row["reason"] = "aggregate_deadline"
                continue
            if harness == "codex" and not codex_template:
                row.update(status="BLOCKED", reason="codex_catalog_template_missing")
                continue
            seen = len(relay)
            try:
                owned = run_pair(runner, gateway, scripts[harness], harness, root, row, policy)
            except Exception as error:  # Ownership of this pair is now unproven.
                row.update(status="FAIL", reason="controller_error:" + type(error).__name__,
                           cleanup={"source": "unknown", "resume": "unknown"})
                owned = False
            row["relay_responses"] = relay[seen:seen + 100]
            if not owned:
                break  # No further native launch after loss of ownership proof.
    finally:
        close_servers(gateway, provider, meta)
    return rows


def native_versions():
    """Version strings of the pinned CLIs, bounded; None when unavailable."""
    import subprocess
    found = {}
    for name in PROTOCOLS:
        try:
            done = subprocess.run([name, "--version"], capture_output=True, text=True, timeout=30)
            found[name] = done.stdout.strip().splitlines()[0][:80] if done.returncode == 0 and done.stdout else None
        except (OSError, subprocess.SubprocessError, IndexError):
            found[name] = None
    return found


def servers_clean(meta):
    return meta.get("gateway_stopped") is True and meta.get("provider_stopped") is True


def write_report(output, rows, meta):
    """Overall PASS needs every row PASS and positive shutdown evidence for both
    fixture servers; missing or failed shutdown evidence is a failure."""
    output.mkdir(parents=True, exist_ok=True)
    passed = bool(rows) and all(row.get("status") == "PASS" for row in rows) and servers_clean(meta)
    document = {"schema_version": 1, "classification": CLASSIFICATION, "certified": False,
                "overall": "PASS" if passed else "FAIL", **meta, "rows": rows}
    (output / "report.json").write_text(json.dumps(document, indent=2, default=sorted) + "\n")
    suite = ET.Element("testsuite", name="native-cross-resume")
    failures = 0
    cases = [(row["id"], row.get("status"), row.get("reason")) for row in rows]
    cases.append(("fixture-server-shutdown", "PASS" if servers_clean(meta) else "FAIL",
                  "gateway_stopped=%s provider_stopped=%s" % (meta.get("gateway_stopped"), meta.get("provider_stopped"))))
    for name, status, reason in cases:
        case = ET.SubElement(suite, "testcase", classname="hrx.native", name=name)
        if status != "PASS":
            failures += 1
            ET.SubElement(case, "failure", type=str(status), message=str(reason or "checks"))
    suite.set("tests", str(len(cases)))
    suite.set("failures", str(failures))
    ET.ElementTree(suite).write(output / "junit.xml", encoding="unicode", xml_declaration=True)
    return passed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hrx", required=True)
    parser.add_argument("--output", type=Path, default=Path("artifacts/native-cross-resume"))
    parser.add_argument("--harness", action="append", choices=sorted(PROTOCOLS))
    args = parser.parse_args()
    meta = {"timestamp": datetime.now(timezone.utc).isoformat(),
            "source_sha": (os.environ.get("CI_COMMIT_SHA") or "")[:40] or None}
    rows = []
    if os.environ.get("HRX_NATIVE_CROSS_RESUME_AUTHORIZED") != "1":
        rows = [{"id": "native-cross-resume", "status": "BLOCKED", "reason": "not_authorized_native_fixture"}]
    elif not sys.platform.startswith("linux"):
        rows = [{"id": "native-cross-resume", "status": "BLOCKED", "reason": "linux_only"}]
    else:
        meta["native_versions"] = native_versions()
        try:
            with tempfile.TemporaryDirectory(prefix="hrx-native-cross-resume-", dir="/tmp") as temp:
                run(str(Path(args.hrx).resolve()), Path(temp).resolve(), args.harness or sorted(PROTOCOLS),
                    rows, meta)
        except Exception as error:  # Durable evidence: predeclared rows kept, error by class only.
            rows.append({"id": "native-cross-resume", "status": "FAIL",
                         "reason": "controller_error:" + type(error).__name__})
    passed = write_report(args.output, rows, meta)
    print(json.dumps([{k: row.get(k) for k in ("id", "status", "reason")} for row in rows]))
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
