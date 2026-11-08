"""Paid mixed-harness conversation on a disposable PostgreSQL/Pitput fixture.

Interactive agents are launched by HRX with --pitput. Only the first challenge
is authored by the controller. Every subsequent value must be computed and
posted by a model through its own native tool and enrolled identity.
"""
import hashlib
import json
import os
from pathlib import Path
import pty
import secrets
import select
import shlex
import signal
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration"))
from pitput_exchange import PitputIntegration, pitput, private
import ownership


def process_table():
    table = {}
    output = subprocess.check_output(["ps", "-axo", "pid=,ppid=,lstart="], text=True)
    for line in output.splitlines():
        fields = line.split(None, 2)
        if len(fields) == 3:
            table[int(fields[0])] = (int(fields[1]), fields[2])
    return table


def descendants(pid, table):
    owned = {pid}
    while True:
        found = owned | {child for child, (parent, _) in table.items() if parent in owned}
        if found == owned:
            return {child: table[child] for child in owned if child in table}
        owned = found


SUPERVISOR = Path(__file__).resolve().with_name("subreaper.py")


class BoundaryUnavailable(FileNotFoundError):
    """No Linux child subreaper: descendant cleanup could never be proven, so do not run."""


def running(process):
    """True while the supervisor has not exited. Never reaps: only stop_supervised may,
    so signalling the supervisor's PID stays safe until teardown is finished."""
    if process.returncode is not None:
        return False
    try:
        return os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is None
    except ChildProcessError:
        return False


def read_summary(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError):
        return None


def stop_supervised(process, summary, evidence, *, grace=10, kill=os.kill, alive=running):
    """Stop an agent through its subreaper supervisor (subreaper.py) and classify cleanup.

    The supervisor owns every descendant, including orphaned double-forked setsid
    daemons, and reports complete only after wait() returned ECHILD. Returns "clean"
    only on that proof with no forced kill; "failed" when it had to force descendants;
    "unknown" when the boundary is missing, the summary is unreadable or the
    supervisor itself had to be killed (its descendants may then have escaped)."""
    if alive(process):
        # Unreaped child: its PID cannot have been reused.
        evidence["signals"].append(f"supervisor {process.pid} SIGTERM")
        kill(process.pid, signal.SIGTERM)
    try:
        process.wait(grace + 20)
    except subprocess.TimeoutExpired:
        evidence["signals"].append(f"supervisor {process.pid} SIGKILL")
        kill(process.pid, signal.SIGKILL)
        try:
            process.wait(5)
        except subprocess.TimeoutExpired:
            evidence["supervisor"] = "not reaped"
        evidence["error"] = "supervisor did not finish; descendants may have escaped the boundary"
        return "unknown"
    evidence["supervisor_exit"] = process.returncode
    record = read_summary(summary)
    evidence["summary"] = record
    return ownership.classify_summary(record, process.returncode)


class Agent:
    def __init__(self, runner, cell, home, project, identity, enrollment, url):
        self.runner, self.cell, self.home, self.project, self.identity = runner, cell, home, project, identity
        self.env = runner.configure(home, project, cell)
        config = Path(self.env["XDG_CONFIG_HOME"]) / "pitput"
        private(config / "enrollment.token", enrollment)
        private(config / "enrollment.toml", f'url = "{url}"\ntoken_file = "enrollment.token"\n')
        binary = home / "bin/pitput"
        private(binary, f'#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput "$@"\n')
        binary.chmod(0o700)
        self.env["PATH"] = str(binary.parent) + os.pathsep + self.env["PATH"]
        self.process = None
        self.session_id = None
        self.thread = None
        self.clean_stops = []
        self.stop_evidence = []
        self.draining = threading.Event()
        self.summary = None

    def command(self, prompt, resume=False):
        args = [self.runner.hrx, self.cell["harness"], "--model", self.cell["route"],
                "--agent-identity", self.identity, "--pitput", "--pitput-board", "exchange"]
        if self.cell["reasoning"]:
            args += ["--reasoning", self.cell["reasoning"]]
        if resume:
            args += ["--resume", self.session_id, "--no-yolo"]
        elif self.cell["harness"] == "claude":
            # HRX initializes the disposable home; resume restores its saved config.
            args += ["--claude-config-dir", self.env["CLAUDE_CONFIG_DIR"],
                     "--claude-init", "theme,trust,bypass"]
        if self.cell["harness"] == "opencode":
            args += ["--", "--prompt", prompt]
        else:
            args += ["--", prompt]
        return args

    def start(self, prompt, resume=False):
        if ownership.available(sys.platform).mechanism != ownership.LINUX_SUBREAPER:
            raise BoundaryUnavailable("conversation cleanup needs a Linux child subreaper")
        if not all(self.clean_stops):
            raise RuntimeError("previous conversation run cleanup was not proven clean")
        if self.process is not None or (self.thread and self.thread.is_alive()):
            raise RuntimeError("previous HRX run or drain thread was not stopped")
        # A fresh event per run: a resumed agent must not inherit the last stop request.
        self.draining = threading.Event()
        index = self.home / "state/hrx/sessions/index.json"
        self.previous_updates = {r["native_id"]: r.get("updated_at") for r in
                                 json.loads(index.read_text()).get("sessions", [])} if index.exists() else {}
        self.started_ns = time.time_ns()
        args = self.command(prompt, resume)
        self.summary = self.home / f"supervisor-{self.started_ns}.json"
        args = [sys.executable, "-I", str(SUPERVISOR), "--summary", str(self.summary), "--", *args]
        self.master, slave = pty.openpty()
        self.process = subprocess.Popen(args, cwd=self.project, env=self.env,
                                        stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        os.close(slave)
        self.thread = threading.Thread(target=self.drain, daemon=True)
        self.thread.start()

    def drain(self):
        # Consume TUI output without persisting credentials/transcripts. Respond
        # to terminal position queries, needed by native full-screen clients.
        while not self.draining.is_set() and running(self.process):
            try:
                if select.select([self.master], [], [], .2)[0]:
                    data = os.read(self.master, 65536)
                    if b"\x1b[6n" in data:
                        os.write(self.master, b"\x1b[1;1R")
            except OSError:
                return

    def session(self):
        if not self.hrx_running():
            raise RuntimeError("HRX exited before fresh session registration")
        path = self.home / "state/hrx/sessions/index.json"
        if not path.exists():
            return None
        for record in json.loads(path.read_text()).get("sessions", []):
            if record.get("agent_identity") == self.identity:
                if self.previous_updates.get(record["native_id"]) == record.get("updated_at"):
                    continue
                self.session_id = record["native_id"]
                return record
        return None

    def bridges(self):
        if not self.hrx_running():
            raise RuntimeError("HRX exited before bridge readiness")
        return sorted(p for p in (self.home / "state/hrx/pitput").glob("*/*/*/bridge.toml")
                      if p.stat().st_mtime_ns >= self.started_ns)

    def pending(self):
        return [pitput(self.env, "bridge", "status", "--config", str(config))["agents"][0]
                for config in self.bridges()]

    def hrx_running(self):
        """HRX itself is alive: the supervisor runs and has not recorded HRX's exit."""
        return (self.process is not None and running(self.process)
                and not Path(str(self.summary) + ".main").exists())

    def stop(self):
        if self.process is None:
            return
        evidence = {"supervisor": self.process.pid, "signals": []}
        # Stop the drain thread before any signal so nothing else observes the supervisor.
        self.draining.set()
        drained = True
        if self.thread:
            self.thread.join(5)
            drained = not self.thread.is_alive()
        evidence["drain_stopped"] = drained
        try:
            evidence["status"] = stop_supervised(self.process, self.summary, evidence)
        finally:
            evidence.setdefault("status", "unknown")
            # A drain thread that never stopped leaves cleanup unproven.
            clean = evidence["status"] == "clean" and drained
            self.stop_evidence.append(evidence)
            self.clean_stops.append(clean)
            if drained:
                os.close(self.master)  # Never close an FD a live thread may still read.
            self.process = None

    def exited_records(self):
        records = [json.loads(p.read_text()) for p in (self.home / "state/hrx/monitor").glob("*.json")]
        return bool(records) and all(r.get("status") == "exited" and r.get("ended_at") for r in records)

    def prompt(self, text):
        os.write(self.master, text.encode() + b"\r")


def wait(predicate, runner, seconds=120):
    deadline = min(time.monotonic() + seconds, runner.gateway.deadline)
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(.25)
    raise TimeoutError("conversation assertion deadline")


def cleanup_fixture(fixture, result):
    fixture.doClassCleanups()
    errors = [error[0].__name__ for error in fixture.tearDown_exceptions]
    result["fixture_cleanup"] = {"status": "FAIL" if errors else "PASS", "errors": errors}
    if errors and result["status"] == "PASS":
        result.update(status="FAIL", reason="fixture_cleanup_failed")


def conversation(runner, inventory, cells, catalog, root):
    from run import capabilities
    result = {"id": "mixed-pitput-conversation", "required": True, "status": "NOT_RUN",
              "scenario": "real-native-tools-and-forum", "assertions": {}}
    selected = [next(c for c in cells if c["id"] == ident) for ident in inventory["conversation"]]
    if any(not capabilities(catalog, c) or c["route"] not in runner.policy["routes"] for c in selected):
        return {**result, "status": "BLOCKED", "reason": "conversation_required_route_unavailable"}
    agents = []
    fixture = type("LivePitput", (PitputIntegration,), {})
    try:
        fixture.setUpClass()
        nonce = secrets.token_hex(24)
        identities = ["test/live-" + c["id"] + "/agent" for c in selected]
        for index, cell in enumerate(selected):
            directory = root / "conversation" / cell["id"]
            project = directory / "project"
            project.mkdir(parents=True, mode=0o700)
            agent = Agent(runner, cell, directory / "home", project, identities[index],
                          fixture.enrollment_token, fixture.url)
            agents.append(agent)
            pitput(agent.env, "join", "exchange", "--identity", agent.identity)
            peer = identities[index + 1] if index < 2 else "test/controller/agent"
            prompt = (
                f'This is an isolated integration test. Your identity is {agent.identity}. '
                'Wait for a message on /b/exchange/live-chain mentioning your identity. '
                'Do not introduce yourself or post anything before that message. '
                'Read it with pitput using your exact --agent identity. Its body ends with VALUE=<hex>. '
                'Use a native shell tool to compute SHA256 of the exact UTF-8 hex value (no newline). '
                f'Write the hash to completed.txt and post exactly "@{peer} VALUE=<computed hash>" '
                'to /b/exchange/live-chain with your identity as --author and '
                f'--request-id live-stage-{index}. Never post twice. '
                'After completing work, explicitly handling-ACK the exact notification using its helper command. '
                'Ignore messages addressed to other identities. Do not alter credentials or other files. '
            )
            if cell["harness"] == "claude":
                prompt += ('Use pitput monitor with --mention your identity, --exclude-author your identity, '
                           '--cursor-file monitor.json --timeout 120 --once using your supported background tool. '
                           'When its output arrives, do the work then ACK that exact cursor with identical filters.')
            agent.start(prompt)
            wait(agent.session, runner, 60)
            if cell["harness"] != "claude":
                wait(agent.bridges, runner, 30)
        result["assertions"]["distinct_registered_sessions"] = len({a.session_id for a in agents}) == 3
        if not result["assertions"]["distinct_registered_sessions"]:
            raise AssertionError("native sessions are not distinct")
        pitput(fixture.controller, *fixture.prefix, "create", "/b/exchange/live-chain",
               "--author", "test/controller/agent", "--title", "Bounded live CI chain",
               "--body", "@" + identities[0] + " VALUE=" + nonce, "--request-id", "live-seed")
        expected = nonce
        for index, agent in enumerate(agents):
            expected = hashlib.sha256(expected.encode()).hexdigest()
            def completed():
                messages = pitput(fixture.controller, *fixture.prefix, "read", "/b/exchange/live-chain")
                authored = [m for m in messages if m["author"] == agent.identity]
                if len(authored) > 1:
                    raise AssertionError("duplicate model-authored reply")
                if not authored:
                    return False
                peer = identities[index + 1] if index < 2 else "test/controller/agent"
                return authored[0]["body"] == "@" + peer + " VALUE=" + expected
            wait(completed, runner)
            result["assertions"][agent.cell["id"] + "_authored_nonce_handoff"] = True
            path = agent.project / "completed.txt"
            if not path.exists() or path.read_text().strip() != expected:
                raise AssertionError("native filesystem effect missing")
            if agent.cell["harness"] != "claude":
                wait(lambda: agent.pending() and all(s["pending"] is None for s in agent.pending()), runner)
                result["assertions"][agent.cell["id"] + "_handling_ack"] = True
            else:
                cursor = agent.project / "monitor.json"
                wait(lambda: cursor.is_file() and json.loads(cursor.read_text()).get("pending") is None and
                     json.loads(cursor.read_text()).get("after", 0) > 0, runner)
                result["assertions"][agent.cell["id"] + "_monitor_handling_ack"] = True
        result["assertions"]["model_driven_three_hop_chain"] = True
        target = next(a for a in agents if a.cell["harness"] == "opencode")
        # Keep a native tool running while another event is delivered. The
        # gate is only an execution barrier; it never computes or posts replies.
        private(target.project / "gate.py", 'import pathlib,time\n'
                'pathlib.Path("busy.started").touch()\n'
                'while not pathlib.Path("busy.release").exists(): time.sleep(.2)\n')
        target.prompt('For the integration test run python3 gate.py with your shell tool now. '
                      'It will wait for the controller to release it. Do not cancel it.')
        wait(lambda: (target.project / "busy.started").exists(), runner, 60)
        value = secrets.token_hex(24)
        recovery_expected = hashlib.sha256(value.encode()).hexdigest()
        body = ('@' + target.identity + ' Recovery test supersedes the earlier one-reply rule. '
                'Compute SHA256 of the UTF-8 value after VALUE= using a native tool, write recovery.txt, '
                'post exactly "@test/controller/agent RECOVERY=<hash>" to this topic with request-id '
                'live-recovery, then handling-ACK. VALUE=' + value)
        pitput(fixture.controller, *fixture.prefix, "post", "/b/exchange/live-chain",
               "--author", "test/controller/agent", "--body", body, "--request-id", "recovery-seed")
        pending = wait(lambda: next((s for s in target.pending() if s.get("pending")), None), runner, 30)
        result["assertions"]["notification_queued_while_native_tool_busy"] = True
        native_id = target.session_id
        target.stop()
        private(target.project / "busy.release", "release")
        target.start('Continue the pending authorized recovery test. Handle and ACK the pending '
                     'Pitput recovery notification, preserving its request-id.', resume=True)
        wait(lambda: target.session() and target.session_id == native_id, runner, 60)

        def recovered():
            messages = pitput(fixture.controller, *fixture.prefix, "read", "/b/exchange/live-chain")
            replies = [m for m in messages if m["author"] == target.identity and
                       m["body"] == "@test/controller/agent RECOVERY=" + recovery_expected]
            if len(replies) > 1:
                raise AssertionError("duplicate recovery side effect")
            return len(replies) == 1

        wait(recovered, runner)
        wait(lambda: target.pending() and all(s["pending"] is None for s in target.pending()), runner)
        if (target.project / "recovery.txt").read_text().strip() != recovery_expected:
            raise AssertionError("recovery native tool output mismatch")
        result["assertions"]["pending_restart_same_session_reply_and_ack"] = True
        # Replay the already handled write using its original id and payload.
        # Forum idempotency and a second native restart must not add a reply.
        pitput(fixture.controller, *fixture.prefix, "post", "/b/exchange/live-chain",
               "--author", "test/controller/agent", "--body", body, "--request-id", "recovery-seed")
        target.stop()
        target.start('The recovery test is complete; remain idle, do not post again.', resume=True)
        wait(lambda: target.bridges() and target.pending() and
             all(s["pending"] is None for s in target.pending()), runner, 60)
        time.sleep(min(3, max(0, runner.gateway.deadline - time.monotonic())))
        if not recovered():
            raise AssertionError("recovery reply lost after restart")
        result["assertions"]["handled_restart_deduplication"] = True
        isolated = secrets.token_hex(24)
        pitput(fixture.controller, *fixture.prefix, "create", "/b/restricted/private-marker",
               "--author", "test/controller/agent", "--title", "Isolation marker",
               "--body", '@' + target.identity + ' ' + isolated, "--request-id", "isolation-marker")
        denied = pitput(target.env, "--agent", target.identity, "read", "/b/restricted/private-marker", check=False)
        if denied.returncode == 0:
            raise AssertionError("ungranted board readable")
        result["assertions"]["ungranted_board_denied"] = True
        result.update(status="PASS", session_ids=[a.session_id for a in agents])
    except Exception as exc:
        result.update(status="BLOCKED" if isinstance(exc, (ImportError, FileNotFoundError)) else "FAIL",
                      reason="conversation_" + type(exc).__name__)
    finally:
        cleanup_failed = False
        for agent in reversed(agents):
            try:
                agent.stop()
            except Exception:
                cleanup_failed = True
        result["cleanup"] = [evidence for agent in agents for evidence in agent.stop_evidence]
        if result["status"] == "PASS":
            if not cleanup_failed and all(a.exited_records() and a.clean_stops and all(a.clean_stops) for a in agents):
                result["assertions"]["all_hrx_runs_deregistered"] = True
                result["assertions"]["owned_descendants_exited_without_forced_cleanup"] = True
            else:
                result.update(status="FAIL", reason="exit_deregistration_missing")
        cleanup_fixture(fixture, result)
    return result
