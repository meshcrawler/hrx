"""Production HRX command dispatcher + real Pitput/Postgres, fixture harness.

Run via make test-pitput PITPUT_SOURCE=/path/to/pitput. All credentials,
profiles, databases, and services are disposable and bound to loopback.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import http.server
import json
import math
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.request


def private(path, text):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(text)
    path.chmod(0o600)


def run(args, *, env=None, check=True, timeout=20, input=None):
    result = subprocess.run(args, env=env, input=input, capture_output=True,
                            text=True, timeout=timeout)
    if check and result.returncode:
        # Only isolated fixture processes are invoked; never run user profiles.
        raise AssertionError(f"{Path(args[0]).name} failed ({result.returncode}): "
                             f"{result.stderr[-2000:]}")
    return result


# Sequential CLI process cost dominates the chained exchange under runner load;
# workers report it so a slow run shows where its time went.
CLI_COST = {"calls": 0, "seconds": 0.0}


def pitput(env, *args, check=True, timeout=20):
    started = time.monotonic()
    try:
        result = run([sys.executable, "-m", "ysg_mail.pitput", *args], env=env,
                     check=check, timeout=timeout)
    finally:
        CLI_COST["calls"] += 1
        CLI_COST["seconds"] += time.monotonic() - started
    if not check:
        return result
    return json.loads(result.stdout) if result.stdout.strip() else None


class MonitorDeliveryError(AssertionError):
    """Bounded metadata only: never render CLI output, cursor, path or credentials."""

    def __init__(self, reason, polls, elapsed, *, exit_code=None, error_class=None):
        self.diagnostics = {
            "reason": reason, "polls": polls, "elapsed_seconds": round(elapsed, 3),
        }
        if exit_code is not None:
            self.diagnostics["exit_code"] = exit_code
        if error_class is not None:
            self.diagnostics["error_class"] = error_class
        super().__init__("monitor_delivery_wait:" + json.dumps(self.diagnostics, sort_keys=True))


def wait_for_monitor_event(env, monitor, *, wait_seconds=10.0, execute=None,
                           clock=time.monotonic, pause=time.sleep):
    """Await positive delivery, not a failed-test retry or an implicit ACK.

    A finite monitor poll may legitimately return no event. Keep the exact
    cursor/filter/monitor arguments throughout one bounded monotonic wait.
    Command/schema errors are failures, never empty-poll retries.
    """
    if (type(wait_seconds) not in (int, float) or not math.isfinite(wait_seconds)
            or not 0 < wait_seconds <= 60):
        raise MonitorDeliveryError("invalid_wait_bounds", 0, 0)
    arguments = tuple(monitor)
    if (not all(isinstance(value, str) for value in arguments)
            or arguments.count("monitor") != 1 or "--once" not in arguments
            or "--cursor-file" not in arguments
            or any(value == "--ack" or value.startswith("--ack=") for value in arguments)):
        raise MonitorDeliveryError("invalid_positive_monitor_spec", 0, 0)
    execute = pitput if execute is None else execute
    started = clock()
    if type(started) not in (int, float) or not math.isfinite(started):
        raise MonitorDeliveryError("invalid_clock", 0, 0)
    last = started
    deadline = started + wait_seconds
    polls = 0

    def fail(reason, **details):
        raise MonitorDeliveryError(reason, polls, last - started, **details) from None

    def observe_time():
        nonlocal last
        value = clock()
        if (type(value) not in (int, float) or not math.isfinite(value) or value < last):
            fail("invalid_clock")
        last = value
        return value

    while True:
        remaining = deadline - observe_time()
        if remaining <= 0:
            fail("delivery_deadline")
        polls += 1
        try:
            result = execute(env, *arguments, check=False, timeout=min(20, remaining))
        except subprocess.TimeoutExpired:
            observe_time()
            fail("command_timeout")
        except (OSError, UnicodeError) as error:
            observe_time()
            fail("command_exception", error_class=type(error).__name__)
        # Even a valid event returned after the overall deadline is not a pass.
        observe_time()
        if last >= deadline:
            fail("delivery_deadline")
        if type(result.returncode) is not int:
            fail("invalid_command_status")
        if result.returncode != 0:
            fail("command_failed", exit_code=result.returncode)
        if not isinstance(result.stdout, str):
            fail("invalid_command_output")
        try:
            output_bytes = len(result.stdout.encode("utf-8"))
        except UnicodeError:
            fail("invalid_command_output")
        if output_bytes > 65536:
            fail("command_output_bounds")
        try:
            event = json.loads(result.stdout) if result.stdout.strip() else None
        except ValueError:
            fail("invalid_event_json")
        if event is not None:
            fields = {"seq", "event_key", "path", "author", "reason", "cursor", "trust"}
            if (not isinstance(event, dict) or set(event) != fields
                    or type(event["seq"]) is not int or event["seq"] <= 0
                    or any(not isinstance(event[key], str) or not event[key]
                           for key in ("event_key", "path", "author", "cursor"))
                    or event["trust"] != "untrusted_peer_content"
                    or not isinstance(event["reason"], list) or not event["reason"]
                    or any(not isinstance(reason, str) or not reason
                           for reason in event["reason"])):
                fail("invalid_event_schema")
            return event
        remaining = deadline - observe_time()
        if remaining <= 0:
            fail("delivery_deadline")
        pause(min(0.05, remaining))


def worker():
    """Fixture replaces Claude only; HRX, hooks, enrollment and messages are real."""
    env = os.environ.copy()
    args = sys.argv[2:]
    prompt = args[args.index("--append-system-prompt") + 1]
    assert "Pitput communications" in prompt
    assert "Claude can run a Pitput monitor" in prompt
    assert "HRX has not started this monitor" in prompt
    # Installed priming is optional and bounded; concurrent cold starts can
    # legitimately use HRX's built-in platform guidance instead.
    assert ("PLATFORM: claude" in prompt or
            "Pitput priming was unavailable; use this built-in guidance" in prompt)
    assert "--system-prompt-snapshot" in args
    identity, peer = env["TEST_IDENTITY"], env["TEST_PEER"]
    run_key = env.get("TEST_RUN_KEY", "")
    topic = env.get("TEST_TOPIC", "/b/exchange/conversation")
    hook = json.loads(args[args.index("--settings") + 1])["hooks"]["SessionStart"][0]["hooks"][0]["command"]
    import shlex
    event = {"hook_event_name": "SessionStart", "source": "startup",
             "session_id": "fixture-" + identity.split("/")[1] + run_key,
             "cwd": str(Path.cwd()), "model": "fixture/route"}
    run(shlex.split(hook), env=env, input=json.dumps(event))
    pitput(env, "join", "/b/exchange", "--identity", identity)
    status = pitput(env, "agent", "status", "--agent-id=" + identity)
    assert status["identity"] == identity and status["boards"] == ["exchange"]
    prefix = ["--agent", identity]
    monitor = [*prefix, "monitor", topic if run_key else "/b/exchange", "--mention", identity,
               "--exclude-author", identity, "--cursor-file", str(Path(env["HOME"]) / ("cursor" + run_key + ".json")),
               "--timeout", "2", "--once"]

    started = time.time()
    timeline = {}

    def phase(message):
        # Timestamped phases show where each peer stops when the chain stalls, and the
        # final report carries them so passing runs also show how much slack remained.
        timeline[message] = round(time.time() - started, 1)
        print(f"fixture: +{timeline[message]}s {message} (cli calls {CLI_COST['calls']}, "
              f"{CLI_COST['seconds']:.1f}s)", file=sys.stderr, flush=True)

    def receive():
        # Later stages legitimately wait for every predecessor's setup and reply, so
        # each receive waits until the run's shared deadline, not a fixed per-process
        # window that starts at launch. The parent still bounds the whole exchange.
        shared = env.get("TEST_DEADLINE")
        deadline = float(shared) if shared else time.time() + 45
        polls = 0
        while time.time() < deadline:
            polls += 1
            attention = pitput(env, *monitor)
            if attention:
                assert pitput(env, *monitor) == attention, "unacked event must replay"
                messages = pitput(env, *prefix, "read", attention["path"])
                payload = json.loads(messages[0]["body"].split(" ", 1)[1])
                pitput(env, *monitor, "--ack", attention["cursor"])
                return payload
        raise AssertionError(f"peer message deadline exceeded after {polls} monitor polls")

    phase("waiting for predecessor")
    payload = receive()
    assert len(payload["chain"]) == int(env["TEST_STAGE"])
    payload["chain"].append(identity)
    body = "@" + peer + " " + json.dumps(payload, sort_keys=True)
    write = [*prefix, "post", topic, "--author", identity,
             "--body", body, "--request-id", "reply-" + identity.split("/")[1] + run_key]
    first = pitput(env, *write)
    assert pitput(env, *write) == first, "write retry must not duplicate"
    phase("reply stored and retry verified")
    if env["TEST_STAGE"] == "0":
        phase("waiting for completed chain")
        final = receive()
        assert len(final["chain"]) == 3 and final["nonce"] == payload["nonce"]
    phase("done")
    timeline["cli_calls"] = CLI_COST["calls"]
    timeline["cli_seconds"] = round(CLI_COST["seconds"], 1)
    print(json.dumps({"identity": identity, "received_chain_length": int(env["TEST_STAGE"]),
                      "reply_path": first["path"], "session_id": event["session_id"],
                      "timeline": timeline}))


class Gateway(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/v1/models" and self.headers.get("Authorization") == "Bearer fixture-key":
            data = {"data": [{"id": "fixture/route"}]}
        elif self.path == "/dashboard/models.json" and not self.headers.get("Authorization"):
            data = {"schema_version": 1, "generated_at": datetime.now(timezone.utc).isoformat(),
                    "coverage": {"providers": [{"source": "gateway", "ok": True}]},
                    "models": [{"canonical_id": "fixture", "available": True, "sources": [{
                        "gateway_route": "fixture/route", "available": True,
                        "details": {"harnesses": {"claude": {"wire_api": "messages"}}}}]}]}
        else:
            self.send_error(403)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())

    def do_POST(self):
        self.server.inference_attempts += 1
        self.send_error(403, "inference forbidden in offline integration")


class PitputIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.root = Path(tempfile.mkdtemp(prefix="hrx-pitput-", dir="/tmp")).resolve()
        cls.postgres_cleanup_verified = True
        cls.addClassCleanup(cls.remove_root)
        cls.processes = []
        cls.env = {"PATH": os.environ["PATH"], "HOME": str(cls.root),
                   "LANG": "en_US.UTF-8", "NO_COLOR": "1"}
        pg_bin = os.environ.get("PG_BIN")
        if not pg_bin:
            executable = shutil.which("initdb")
            if executable:
                pg_bin = str(Path(executable).parent)
            else:
                choices = sorted(Path("/opt/homebrew/opt").glob("postgresql*/bin/initdb"))
                if choices:
                    pg_bin = str(choices[-1].parent)
        if not pg_bin:
            raise RuntimeError("PostgreSQL binaries required: set PG_BIN (no existing database is used)")
        cls.pg_ctl = str(Path(pg_bin) / "pg_ctl")
        cls.pg_data = cls.root / "postgres"
        cls.pg_socket = cls.root / "socket"
        cls.pg_socket.mkdir(mode=0o700)
        run([str(Path(pg_bin) / "initdb"), "-D", str(cls.pg_data), "-U", "fixture",
             "--auth=trust", "--no-locale", "--encoding=UTF8"], env=cls.env, timeout=30)
        cls.postgres_cleanup_verified = False
        cls.addClassCleanup(cls.stop_postgres)
        run([cls.pg_ctl, "-D", str(cls.pg_data), "-l", str(cls.root / "postgres.log"),
             "-o", f"-k {cls.pg_socket} -c listen_addresses=''", "-w", "start"], env=cls.env)
        cls.addClassCleanup(cls.stop_processes)
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            cls.port = sock.getsockname()[1]
        cls.url = f"http://127.0.0.1:{cls.port}"
        cls.enrollment_token = secrets.token_hex(32)
        cls.server_config = cls.root / "server.toml"
        private(cls.server_config, f'''[server]
port = {cls.port}
poll_seconds = 0.05
[server.database]
host = "{cls.pg_socket}"
dbname = "postgres"
user = "fixture"
sslmode = "disable"
[server.agents."test/bootstrap/agent"]
token_sha256 = "{hashlib.sha256(secrets.token_bytes(32)).hexdigest()}"
boards = ["exchange"]
[server.enrollment.fixture]
token_sha256 = "{hashlib.sha256(cls.enrollment_token.encode()).hexdigest()}"
boards = ["exchange", "restricted"]
''')
        cls.start_server()
        cls.controller = cls.agent_env("controller")
        pitput(cls.controller, "join", "exchange", "restricted", "--identity", "test/controller/agent")
        cls.prefix = ["--agent", "test/controller/agent"]
        cls.gateway = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        cls.gateway.inference_attempts = 0
        cls.addClassCleanup(cls.gateway.server_close)
        cert, key = cls.root / "cert.pem", cls.root / "key.pem"
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
             "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
             "-keyout", str(key), "-out", str(cert)], env=cls.env)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        cls.gateway.socket = context.wrap_socket(cls.gateway.socket, server_side=True)
        cls.gateway_thread = threading.Thread(target=cls.gateway.serve_forever, daemon=True)
        cls.gateway_thread.start()
        cls.addClassCleanup(cls.gateway.shutdown)
        cls.bin_dir = cls.root / "bin"
        cls.bin_dir.mkdir(mode=0o700)
        import shlex
        wrapper = f"#!/bin/sh\nexec {shlex.quote(sys.executable)} {shlex.quote(str(Path(__file__).resolve()))} --worker \"$@\"\n"
        private(cls.bin_dir / "claude", wrapper)
        (cls.bin_dir / "claude").chmod(0o700)

    @classmethod
    def remove_root(cls):
        if not cls.postgres_cleanup_verified:
            raise RuntimeError("fixture directory retained: PostgreSQL shutdown unverified")
        shutil.rmtree(cls.root)

    @classmethod
    def stop_postgres(cls):
        command = [cls.pg_ctl, "-D", str(cls.pg_data)]
        status = run([*command, "status"], env=cls.env, check=False)
        if status.returncode == 3:
            cls.postgres_cleanup_verified = True
            return
        if status.returncode != 0:
            raise RuntimeError("fixture PostgreSQL status unknown")
        stopped = run([*command, "-m", "immediate", "-w", "stop"], env=cls.env, check=False)
        status = run([*command, "status"], env=cls.env, check=False)
        cls.postgres_cleanup_verified = stopped.returncode == 0 and status.returncode == 3
        if not cls.postgres_cleanup_verified:
            raise RuntimeError("fixture PostgreSQL shutdown unverified")

    @classmethod
    def stop_processes(cls):
        for process in reversed(cls.processes):
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)

    @classmethod
    def start_server(cls):
        log = open(cls.root / "server.log", "a")
        cls.addClassCleanup(log.close)
        cls.server = subprocess.Popen([sys.executable, "-m", "ysg_mail.pitput", "--config",
                                       str(cls.server_config), "serve"], env=cls.env,
                                      stdout=log, stderr=log, start_new_session=True)
        cls.processes.append(cls.server)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if cls.server.poll() is not None:
                raise AssertionError("standalone Pitput exited: " + (cls.root / "server.log").read_text()[-2000:])
            try:
                with urllib.request.urlopen(cls.url + "/healthz", timeout=1) as response:
                    if json.load(response)["status"] == "ok":
                        return
            except (OSError, ValueError):
                time.sleep(0.05)
        raise AssertionError("standalone Pitput startup deadline exceeded")

    @classmethod
    def agent_env(cls, name):
        home = cls.root / name
        home.mkdir(mode=0o700, exist_ok=True)
        config = home / ".config" / "pitput"
        private(config / "enrollment.token", cls.enrollment_token)
        private(config / "enrollment.toml", f'url = "{cls.url}"\ntoken_file = "enrollment.token"\n')
        return {**cls.env, "HOME": str(home), "XDG_CONFIG_HOME": str(home / ".config"),
                "XDG_STATE_HOME": str(home / "state"), "XDG_CACHE_HOME": str(home / "cache")}

    def prepare_launch(self, env, config):
        return []

    def test_three_hrx_processes_exchange_and_capture_sessions(self):
        nonce = secrets.token_hex(12)
        pitput(self.controller, *self.prefix, "create", "/b/exchange/conversation",
               "--author", "test/controller/agent",
               "--title", "Isolated HRX exchange", "--body",
               '@test/alpha/agent ' + json.dumps({"nonce": nonce, "chain": []}), "--request-id", "seed")
        children = []
        shared_deadline = str(time.time() + 170)
        for stage, (name, peer) in enumerate([("alpha", "beta"), ("beta", "gamma"), ("gamma", "alpha")]):
            env = self.agent_env(name)
            home = Path(env["HOME"])
            config = home / "hrx.toml"
            private(config, f'''[claude]
model = "fixture"
[claude.provider]
base_url = "https://127.0.0.1:{self.gateway.server_port}/v1"
env_key = "HRX_FIXTURE_KEY"
''')
            env.update(PATH=str(self.bin_dir) + os.pathsep + str(Path(sys.executable).parent) + os.pathsep + self.env["PATH"],
                       HRX_CONFIG=str(config), HRX_FIXTURE_KEY="fixture-key", HRX_INTEGRATION_CA=str(self.root / "cert.pem"),
                       TEST_IDENTITY=f"test/{name}/agent", TEST_PEER=f"test/{peer}/agent", TEST_STAGE=str(stage),
                       TEST_DEADLINE=shared_deadline)
            output = open(home / "launch.log", "w+")
            self.addCleanup(output.close)
            extra = self.prepare_launch(env, config)
            child = subprocess.Popen([HRX, "claude", *extra, "--pitput", "--", "-p", "exchange fixture"],
                                     env=env, cwd=home, stdout=output, stderr=output, start_new_session=True)
            self.processes.append(child)
            children.append((child, output, home, name))
        self.addCleanup(self.stop_processes)
        # One shared deadline covers all children. Workers receive it (wall clock,
        # less a margin for their final report) so a later stage can wait for its
        # predecessors instead of expiring on a private 45-second window.
        deadline = time.monotonic() + 180

        def diagnostics():
            reports = []
            for peer, log, _, role in children:
                log.seek(0)
                reports.append(f"{role} (exit {peer.poll()}): " + log.read()[-3000:])
            return "\n".join(reports)

        for child, output, home, name in children:
            try:
                child.wait(timeout=max(0.1, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                self.fail("exchange deadline exceeded\n" + diagnostics())
            if child.returncode != 0:
                # One peer's failure usually starves the others: report every peer.
                for peer, _, _, _ in children:
                    try:
                        peer.wait(timeout=max(0.1, deadline - time.monotonic()))
                    except subprocess.TimeoutExpired:
                        pass
                self.fail(f"{name} exited {child.returncode}\n" + diagnostics())
            output.seek(0)
            text = output.read()
            report = json.loads(text.splitlines()[-1])
            self.assertEqual(report["identity"], f"test/{name}/agent")
            # Seconds from this worker's launch: shows each stage's slack against the
            # old private 45-second window, in passing runs too.
            print(f"exchange timeline {name}: {json.dumps(report.get('timeline', {}))}", flush=True)
            records = json.loads((home / "state/hrx/sessions/index.json").read_text())["sessions"]
            self.assertEqual(records[0]["native_id"], report["session_id"])
            self.assertEqual(records[0]["model_route"], "fixture/route")
        messages = pitput(self.controller, *self.prefix, "read", "/b/exchange/conversation")
        self.assertEqual(len(messages), 4, "idempotent retries duplicated messages")
        self.assertEqual([m["author"] for m in messages[1:]],
                         [f"test/{name}/agent" for name in ("alpha", "beta", "gamma")])
        self.assertEqual(json.loads(messages[-1]["body"].split(" ", 1)[1])["nonce"], nonce)
        self.assertEqual(self.gateway.inference_attempts, 0)

    def test_enrollment_replay_scope_and_identity_isolation(self):
        env = self.agent_env("scope")
        identity = "test/scope/agent"
        first = pitput(env, "join", "exchange", "--identity", identity)
        self.assertEqual(first, pitput(env, "join", "/b/exchange", "--identity", identity))
        self.assertNotEqual(pitput(env, "join", "restricted", "--identity", identity, check=False).returncode, 0)
        self.assertNotEqual(pitput(env, "--agent", identity, "create", "/b/restricted/denied",
                                  "--author", identity,
                                  "--title", "Denied", "--body", "Denied", "--request-id", "denied", check=False).returncode, 0)
        self.assertNotEqual(pitput(env, "--agent", identity, "create", "/b/exchange/impersonation",
                                  "--author", "test/controller/agent", "--title", "Denied", "--body", "Denied",
                                  "--request-id", "impersonation", check=False).returncode, 0)
        token = Path(env["HOME"]) / ".config/pitput/agent-base/test/scope/agent/token"
        self.assertEqual(token.stat().st_mode & 0o777, 0o600)

    def test_request_id_conflict(self):
        args = [*self.prefix, "create", "/b/exchange/idempotency", "--title", "Retry",
                "--author", "test/controller/agent",
                "--request-id", "same-request"]
        first = pitput(self.controller, *args, "--body", "original")
        self.assertEqual(first, pitput(self.controller, *args, "--body", "original"))
        self.assertNotEqual(pitput(self.controller, *args, "--body", "changed", check=False).returncode, 0)
        self.assertEqual(len(pitput(self.controller, *self.prefix, "read", "/b/exchange/idempotency")), 1)

    def test_monitor_pending_survives_restart_and_requires_exact_ack(self):
        topic = "/b/exchange/durable-monitor"
        pitput(self.controller, *self.prefix, "create", topic, "--title", "Durable monitor",
               "--author", "test/controller/agent", "--body", "@test/controller/agent attention",
               "--request-id", "durable-monitor")
        monitor = [*self.prefix, "monitor", topic, "--mention", "test/controller/agent",
                   "--cursor-file", str(self.root / "durable-cursor.json"), "--timeout", "0.2", "--once"]
        event = wait_for_monitor_event(self.controller, monitor)
        self.assertTrue(event)
        self.assertNotEqual(pitput(self.controller, *monitor, "--ack", "incorrect-cursor", check=False).returncode, 0)
        self.server.terminate()
        self.server.wait(timeout=15)
        self.start_server()
        self.assertEqual(event, wait_for_monitor_event(self.controller, monitor))
        pitput(self.controller, *monitor, "--ack", event["cursor"])
        self.assertIsNone(pitput(self.controller, *monitor), "acked event replayed")

    def test_invalid_credential_is_rejected(self):
        env = self.agent_env("badcredential")
        identity = "test/badcredential/agent"
        pitput(env, "join", "exchange", "--identity", identity)
        token = Path(env["HOME"]) / ".config/pitput/agent-base/test/badcredential/agent/token"
        private(token, secrets.token_hex(32))
        result = pitput(env, "--agent", identity, "boards", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid_agent_profile", result.stderr)
        # A manual remote profile has no local enrollment fingerprint; ensure
        # the real server also rejects an otherwise well-formed unknown bearer.
        profile = Path(env["HOME"]) / "invalid-remote.toml"
        private(profile, f'identity = "{identity}"\n[remote]\nurl = "{self.url}"\ntoken_file = "{token}"\n')
        result = pitput(env, "--config", str(profile), "boards", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("401", result.stderr)

    def test_server_restart_preserves_history(self):
        args = [*self.prefix, "create", "/b/exchange/restart", "--title", "Restart",
                "--author", "test/controller/agent",
                "--body", "persisted", "--request-id", "restart"]
        first = pitput(self.controller, *args)
        self.server.terminate()
        self.server.wait(timeout=15)
        self.start_server()
        self.assertEqual(first, pitput(self.controller, *args))
        self.assertEqual(pitput(self.controller, *self.prefix, "read", "/b/exchange/restart")[0]["body"], "persisted")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--worker":
        worker()
    else:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("--hrx", required=True)
        options = parser.parse_args()
        HRX = str(Path(options.hrx).resolve())
        unittest.main(argv=[sys.argv[0]], verbosity=2)
