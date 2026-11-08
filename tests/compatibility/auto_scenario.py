"""Bounded live --auto parser scenario through the dedicated CI relay.

HRX runs on a private pseudo-terminal in an isolated home. One natural-language
request is translated by the exact parser route, the structured preview is
checked against the request, an invalid answer is refused, and "n" cancels.
Cancellation must leave no launch, Pitput, broker or enrollment side effect.

Evidence is sanitized: no prompts, previews, credentials or terminal output.
"""
import fcntl
import http.server
import os
from pathlib import Path
import secrets
import select
import shlex
import signal
import subprocess
import sys
import termios
import threading
import time

ALLOWED_FLAGS = {"--model", "--reasoning", "--agent-identity", "--pitput-board"}
ALLOWED_SWITCHES = {"--pitput", "--gitlab", "--no-yolo"}
SENTINEL_COMMANDS = ("claude", "codex", "opencode", "pitput", "glab", "hrx-broker")
LAUNCH_PROMPT = b"Launch? [y/n/reprompt] "
QUESTION = b"(enter n to cancel)"
CANCELLED = b"hrx: Cancelled."


def private(path, text, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(text)
    path.chmod(mode)


class Sentinel:
    """Loopback endpoint and PATH stubs that record any attempted side effect."""

    def __init__(self, root):
        self.log = root / "side-effects.log"
        self.log.touch(mode=0o600)
        self.bin = root / "sentinel-bin"
        self.bin.mkdir(mode=0o700)
        for name in SENTINEL_COMMANDS:
            private(self.bin / name, "#!/bin/sh\nprintf '%s\\n' " + shlex.quote(name) +
                    " >> " + shlex.quote(str(self.log)) + "\nexit 97\n", 0o700)
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def record(self):
                with open(owner.log, "a") as log:
                    log.write("http:" + self.command + "\n")
                self.send_response(503)
                self.send_header("Content-Length", "0")
                self.end_headers()

            do_GET = do_POST = do_PUT = do_DELETE = record

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def events(self):
        return [line for line in self.log.read_text().splitlines() if line]

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)


# HRX keeps enrollment, broker tokens, session records, Pitput profiles and
# priming under XDG config/state/data, or in the project. Caches and native
# interpreter bytecode elsewhere in the private home are not launch effects.
WATCHED = (".config", "state", "data", "project")


def snapshot(root):
    found = {}
    for top in WATCHED:
        for path in (root / top).rglob("*") if (root / top).exists() else ():
            if path.is_file() or path.is_symlink():
                found[str(path.relative_to(root))] = (path.lstat().st_size, path.lstat().st_mtime_ns)
    return found


def changed_paths(before, after):
    return sorted(path for path in set(before) | set(after) if before.get(path) != after.get(path))


def expected_request(harness, identity, board, task):
    return f"{harness} as {identity} using pitput board {board} and gitlab. Task: {task}"


def proposal_argv(output):
    """Return the argv of the last proposed launch preview, or None."""
    lines = output.decode(errors="replace").replace("\r", "").splitlines()
    for index in range(len(lines) - 1, 0, -1):
        if lines[index - 1].startswith("hrx: Proposed launch") and lines[index].startswith("  hrx "):
            try:
                return shlex.split(lines[index][len("  hrx "):])
            except ValueError:
                return None
    return None


def check_translation(argv, harness, route, identity, board, task):
    """Every requested launcher setting is present exactly; nothing else is."""
    if not argv or argv[0] != harness:
        return False, "harness"
    separator = ["--", "--prompt"] if harness == "opencode" else ["--", "--"]
    if argv[-3:] != separator + [task]:
        return False, "task"
    flags, switches, index = {}, set(), 0
    settings = argv[1:-3]
    while index < len(settings):
        name = settings[index]
        if name in ALLOWED_FLAGS and index + 1 < len(settings):
            if name in flags:
                return False, "duplicate_" + name
            flags[name] = settings[index + 1]
            index += 2
        elif name in ALLOWED_SWITCHES:
            switches.add(name)
            index += 1
        else:
            return False, "unexpected_argument"
    if flags.get("--model") != route:
        return False, "model"
    if flags.get("--agent-identity") != identity:
        return False, "identity"
    if flags.get("--pitput-board") != board:
        return False, "board"
    if switches != ALLOWED_SWITCHES:
        return False, "integrations_or_permissions"
    return True, None


class Terminal:
    """HRX child with the pseudo-terminal as its controlling terminal."""

    def __init__(self, args, cwd, env):
        self.master, slave = os.openpty()

        def controlling():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        self.process = subprocess.Popen(args, cwd=cwd, env=env, stdin=slave, stdout=slave, stderr=slave,
                                        preexec_fn=controlling, close_fds=True)
        os.close(slave)
        self.output = b""

    def until(self, markers, deadline):
        """Read until any marker appears after the current position, exit or deadline."""
        start = len(self.output)
        while time.monotonic() < deadline:
            if any(marker in self.output[start:] for marker in markers):
                return True
            ready, _, _ = select.select([self.master], [], [], max(0, min(.2, deadline - time.monotonic())))
            if ready:
                try:
                    chunk = os.read(self.master, 65536)
                except OSError:
                    chunk = b""
                if not chunk:
                    break
                self.output += chunk
                if len(self.output) > 1024 * 1024:
                    break
            elif self.process.poll() is not None:
                break
        return any(marker in self.output[start:] for marker in markers)

    def send(self, text):
        try:
            os.write(self.master, text.encode())
        except OSError:
            pass  # the child already exited; its exit is judged separately

    def close(self, deadline):
        try:
            self.process.wait(timeout=max(.1, deadline - time.monotonic()))
            timed_out = False
        except subprocess.TimeoutExpired:
            timed_out = True
        try:
            os.killpg(self.process.pid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        if timed_out:
            self.process.wait(timeout=5)
        os.close(self.master)
        return timed_out


def configure(runner, home, parser_route, launch_harness, launch_route, sentinel):
    env = runner.isolated_env(home)
    provider = f'base_url = "{runner.gateway.url}/v1"\nenv_key = "HRX_CI_KEY"\n'
    private(home / ".config/hrx/config.toml",
            '[permissions]\nyolo = true\n'
            f'[auto]\nprovider_harness = "opencode"\nmodel = "{parser_route}"\n'
            f'[claude]\nmodel = "{launch_route}"\n[claude.provider]\n' + provider +
            f'[opencode]\nmodel = "{launch_route}"\n[opencode.provider]\n' + provider +
            f'[plugins.gitlab-service-account-broker]\nurl = "{sentinel.url}"\n'
            'enrollment_token_file = "enrollment.token"\n')
    private(home / ".config/hrx/enrollment.token", "sentinel-enrollment-" + secrets.token_hex(16))
    private(home / ".config/pitput/enrollment.toml", f'url = "{sentinel.url}"\ntoken_file = "enrollment.token"\n')
    private(home / ".config/pitput/enrollment.token", "sentinel-pitput-" + secrets.token_hex(16))
    private(home / "codex/config.toml", 'model_provider = "ci"\nmodel = "' + launch_route + '"\n'
            '[model_providers.ci]\nname = "HRX isolated CI"\nwire_api = "responses"\n' + provider)
    template = os.environ.get("HRX_CI_CODEX_CATALOG")
    if template:
        private(home / "codex/models_cache.json", Path(template).read_text())
    env["PATH"] = str(sentinel.bin) + os.pathsep + env["PATH"]
    env["HRX_GITLAB_BROKER_URL"] = sentinel.url
    return env


def auto_scenario(runner, root, parser_route, launch_harness, launch_route, timeout=90):
    """Run one bounded live --auto translation, then cancel it.

    runner is run.Runner (its gateway is the dedicated CI relay). parser_route
    is the exact Inkling route; launch_route must be authorized for
    launch_harness. Returns sanitized evidence with status PASS, FAIL or BLOCKED.
    """
    result = {"id": "auto-parser-" + launch_harness, "required": True, "parser_route": parser_route,
              "launch_harness": launch_harness, "launch_route": launch_route}
    if launch_harness not in ("claude", "codex", "opencode"):
        return {**result, "status": "FAIL", "reason": "unsupported_launch_harness"}
    if not Path(runner.hrx).is_file():
        return {**result, "status": "BLOCKED", "reason": "hrx_artifact_missing"}
    if not {parser_route, launch_route} <= set(runner.policy["routes"]):
        return {**result, "status": "BLOCKED", "reason": "route_not_authorized_by_budget_policy"}
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    home, project = root / "home", root / "home/project"
    project.mkdir(parents=True, exist_ok=True, mode=0o700)
    nonce = secrets.token_hex(4)
    identity, board = f"ci/auto-{nonce}/agent", f"auto-{nonce}"
    task = f"Read NOTE-{nonce}.md and summarize it"
    gateway = runner.gateway
    previous_routes = gateway.allowed_routes
    gateway.allowed_routes = {parser_route}
    sentinel = Sentinel(root)
    start = len(gateway.requests)
    deadline = time.monotonic() + min(timeout, max(1, gateway.deadline - time.monotonic()))
    terminal = None
    try:
        env = configure(runner, home, parser_route, launch_harness, launch_route, sentinel)
        before = snapshot(home)
        terminal = Terminal([runner.hrx, "--auto", expected_request(launch_harness, identity, board, task),
                             "--model", launch_route], project, env)
        prompted = terminal.until([LAUNCH_PROMPT, QUESTION], deadline)
        asked = QUESTION in terminal.output
        argv = proposal_argv(terminal.output) if prompted and not asked else None
        reprompted = cancelled = False
        if prompted:
            if asked:
                terminal.send("n\n")
            else:
                # An unrecognized answer must not be approval; HRX asks again.
                terminal.send("maybe\n")
                reprompted = terminal.until([LAUNCH_PROMPT], deadline)
                terminal.send("n\n")
            cancelled = terminal.until([CANCELLED], deadline)
        timed_out = terminal.close(deadline)
        exit_code = terminal.process.returncode
        output = terminal.output
        terminal = None
        after = snapshot(home)
    finally:
        if terminal:
            terminal.close(time.monotonic())
        gateway.allowed_routes = previous_routes
        sentinel.close()
    events = gateway.requests[start:]
    translated, mismatch = check_translation(argv, launch_harness, launch_route, identity, board, task)
    side_effects = sentinel.events()
    filesystem = changed_paths(before, after)
    result.update(inference_requests=len(events), observed_routes=sorted({e["route"] for e in events}),
                  http_statuses=[e["http_status"] for e in events],
                  exact_route_protocol=bool(events) and all(
                      e["route"] == parser_route and e["protocol"] == "/v1/chat/completions" for e in events),
                  confirmation_prompted=prompted and not asked, clarification_requested=asked,
                  invalid_answer_reprompted=reprompted, translation_assertion=translated,
                  cancelled=cancelled, exit_code=exit_code, timed_out=timed_out,
                  launch_side_effects=sorted(set(side_effects)), filesystem_side_effects=filesystem[:20],
                  credential_echoed=gateway.secret.encode() in output)
    if any(e["http_status"] in (401, 403, 429, 500, 502, 503, 504) for e in events):
        return {**result, "status": "BLOCKED", "reason": "provider_auth_quota_or_availability"}
    if any(e.get("transport_error") for e in events):
        return {**result, "status": "BLOCKED", "reason": "inference_transport_or_deadline"}
    if side_effects or filesystem:
        return {**result, "status": "FAIL", "reason": "side_effect_before_approval"}
    if result["credential_echoed"]:
        return {**result, "status": "FAIL", "reason": "credential_in_terminal_output"}
    if not events:
        return {**result, "status": "FAIL", "reason": "no_parser_inference"}
    if len(events) != 1 or not result["exact_route_protocol"]:
        return {**result, "status": "FAIL", "reason": "parser_route_or_request_count"}
    if timed_out or not prompted:
        return {**result, "status": "FAIL", "reason": "no_confirmation_prompt"}
    if asked:
        return {**result, "status": "FAIL", "reason": "unexpected_clarification"}
    if not translated:
        return {**result, "status": "FAIL", "reason": "translation_" + mismatch}
    if not reprompted:
        return {**result, "status": "FAIL", "reason": "invalid_answer_not_refused"}
    if not cancelled or exit_code != 0:
        return {**result, "status": "FAIL", "reason": "cancellation"}
    return {**result, "status": "PASS"}


if __name__ == "__main__":
    sys.exit("auto_scenario is called by the compatibility runner; see docs/internal/live-scenarios.md")
