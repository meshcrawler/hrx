"""Conversation teardown is clean only with a persistent ownership boundary (#26).

Mocked tests cover classification and the Agent lifecycle; process tests run the real
subreaper supervisor on Linux with double-forked setsid daemons (no models or DB)."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import conversation
from conversation import SUPERVISOR, stop_supervised


class Process:
    """A Popen stand-in: returncode stays None until wait() reaps it."""

    def __init__(self, pid=100, code=0, hangs=False, terminal=None):
        self.pid, self.returncode, self.code, self.hangs = pid, None, code, hangs
        # Like a real child, hold the PTY slave open: on Linux a master without any
        # slave reads EIO, which would end the drain thread.
        self.terminal = os.dup(terminal) if terminal is not None else None

    def release(self):
        if self.terminal is not None:
            os.close(self.terminal)
            self.terminal = None

    def poll(self):
        raise AssertionError("teardown must never poll(): that would reap the supervisor")

    def wait(self, timeout=None):
        if self.hangs:
            raise subprocess.TimeoutExpired("supervisor", timeout)
        self.release()
        self.returncode = self.code
        return self.code


class StopSupervisedTests(unittest.TestCase):
    def stop(self, process, record, alive=True):
        signals = []
        with tempfile.TemporaryDirectory() as temp:
            summary = Path(temp) / "summary.json"
            if record is not None:
                summary.write_text(json.dumps(record))
            evidence = {"signals": []}
            status = stop_supervised(process, summary, evidence, grace=0,
                                     kill=lambda pid, sig: signals.append((pid, sig)), alive=lambda p: alive)
        return status, signals, evidence

    def test_complete_unforced_boundary_is_clean(self):
        status, signals, _ = self.stop(Process(), {"subreaper": True, "complete": True, "forced": False})
        self.assertEqual(status, "clean")
        self.assertEqual(signals, [(100, signal.SIGTERM)])

    def test_forced_descendants_fail(self):
        status, _, _ = self.stop(Process(code=1), {"subreaper": True, "complete": True, "forced": True})
        self.assertEqual(status, "failed")

    def test_nonzero_supervisor_exit_is_never_clean(self):
        status, _, _ = self.stop(Process(code=1), {"subreaper": True, "complete": True, "forced": False})
        self.assertEqual(status, "failed")

    def test_missing_or_unreadable_summary_is_unknown(self):
        self.assertEqual(self.stop(Process(), None)[0], "unknown")

    def test_missing_subreaper_is_unknown(self):
        status, _, _ = self.stop(Process(code=2), {"subreaper": False, "complete": False, "forced": False})
        self.assertEqual(status, "unknown")

    def test_incomplete_boundary_is_unknown(self):
        status, _, _ = self.stop(Process(), {"subreaper": True, "complete": False, "forced": False})
        self.assertEqual(status, "unknown")

    def test_supervisor_timeout_is_killed_and_unknown(self):
        status, signals, evidence = self.stop(Process(hangs=True), None)
        self.assertEqual(status, "unknown")
        self.assertEqual(signals, [(100, signal.SIGTERM), (100, signal.SIGKILL)])
        self.assertIn("escaped", evidence["error"])

    def test_exited_supervisor_is_not_signalled(self):
        status, signals, _ = self.stop(Process(), {"subreaper": True, "complete": True, "forced": False},
                                       alive=False)
        self.assertEqual(signals, [])
        self.assertEqual(status, "clean")


class AgentLifecycleTests(unittest.TestCase):
    """Drain ordering, resume and join-timeout handling (#26 review 811/816)."""

    def agent(self, temp):
        agent = conversation.Agent.__new__(conversation.Agent)
        agent.home, agent.project, agent.env = Path(temp), Path(temp), {}
        agent.identity, agent.session_id = "ci/test/agent", None
        agent.runner = type("Runner", (), {"hrx": "hrx"})()
        agent.cell = {"harness": "codex", "route": "r", "reasoning": None}
        agent.process, agent.thread, agent.summary = None, None, None
        agent.stop_evidence, agent.clean_stops = [], []
        agent.draining = threading.Event()
        # The drain thread outlives start(): the fake liveness check must too.
        alive = patch.object(conversation, "running", lambda p: p.returncode is None)
        alive.start()
        self.addCleanup(alive.stop)
        return agent

    def launch(self, agent):
        """start() with a fake Popen whose supervisor runs until signalled."""
        launched = []

        def popen(args, **options):
            launched.append(Process(terminal=options["stdin"]))
            self.addCleanup(launched[-1].release)
            return launched[-1]

        with patch.object(conversation.sys, "platform", "linux"), \
                patch.object(conversation.subprocess, "Popen", popen):
            agent.start("prompt")
        return launched[0]

    def stop(self, agent, status="clean"):
        observed = {}

        def fake(process, summary, evidence):
            observed.update(draining=agent.draining.is_set(), thread_alive=agent.thread.is_alive())
            process.wait()
            return status

        with patch.object(conversation, "stop_supervised", fake):
            agent.stop()
        return observed

    def test_drain_stops_before_supervisor_teardown(self):
        with tempfile.TemporaryDirectory() as temp:
            agent = self.agent(temp)
            self.launch(agent)
            self.assertTrue(agent.thread.is_alive())
            self.assertEqual(self.stop(agent), {"draining": True, "thread_alive": False})
            self.assertEqual(agent.clean_stops, [True])

    def test_resumed_agent_gets_a_fresh_drain_thread(self):
        with tempfile.TemporaryDirectory() as temp:
            agent = self.agent(temp)
            self.launch(agent)
            self.stop(agent)
            self.launch(agent)
            self.assertFalse(agent.draining.is_set())
            time.sleep(0.5)
            self.assertTrue(agent.thread.is_alive(), "resumed drain exited immediately")
            self.stop(agent)
            self.assertEqual(agent.clean_stops, [True, True])

    def test_start_refuses_while_a_previous_run_is_unstopped(self):
        with tempfile.TemporaryDirectory() as temp:
            agent = self.agent(temp)
            self.launch(agent)
            with self.assertRaises(RuntimeError):
                self.launch(agent)
            self.stop(agent)

    def test_drain_join_timeout_is_never_clean_and_keeps_the_fd_open(self):
        with tempfile.TemporaryDirectory() as temp:
            agent = self.agent(temp)
            read, write = os.pipe()
            self.addCleanup(os.close, write)
            agent.master, agent.process = read, Process()
            release = threading.Event()
            agent.thread = threading.Thread(target=release.wait)
            agent.thread.start()
            self.addCleanup(release.set)
            with patch.object(agent.thread, "join"):
                self.stop(agent)
            self.assertEqual(agent.clean_stops, [False])
            self.assertFalse(agent.stop_evidence[0]["drain_stopped"])
            os.fstat(read)  # Still open: a live thread may read it.
            os.close(read)

    def test_non_linux_refuses_to_launch(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(conversation.sys, "platform", "darwin"):
            with self.assertRaises(conversation.BoundaryUnavailable):
                self.agent(temp).start("prompt")

    def test_no_code_path_reaps_with_poll(self):
        import inspect
        self.assertNotIn(".poll()", inspect.getsource(conversation))


DAEMON = r'''
import os, signal, sys, time
from pathlib import Path
# Double fork into a new session: the daemon is orphaned and re-parented.
if os.fork() == 0:
    os.setsid()
    if os.fork() == 0:
        if sys.argv[2] == "stubborn":
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
        Path(sys.argv[1]).write_text(str(os.getpid()))
        while True:
            time.sleep(1)
    os._exit(0)
time.sleep(int(sys.argv[3]))
'''

LATE_CHILD = r'''
import os, signal, sys, time
from pathlib import Path
ready, late = Path(sys.argv[1]), Path(sys.argv[2])

def on_term(*_):
    # Shutdown creates a new descendant in its own session, then the leader exits.
    if os.fork() == 0:
        os.setsid()
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        late.write_text(str(os.getpid()))
        while True:
            time.sleep(1)
    os._exit(0)

signal.signal(signal.SIGTERM, on_term)
ready.write_text(str(os.getpid()))
while True:
    time.sleep(1)
'''


@unittest.skipUnless(sys.platform.startswith("linux"), "process boundary needs a Linux child subreaper")
class SubreaperProcessTests(unittest.TestCase):
    def test_child_created_in_term_handler_is_owned_forced_and_reported(self):
        with tempfile.TemporaryDirectory() as temp:
            summary, ready, late = (Path(temp) / name for name in ("summary.json", "ready", "late.pid"))
            script = Path(temp) / "late.py"
            script.write_text(LATE_CHILD)
            process = subprocess.Popen([sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary),
                                        "--grace", "1", "--", sys.executable, "-I", str(script), str(ready),
                                        str(late)], start_new_session=True)
            deadline = time.monotonic() + 10
            while not ready.exists() and time.monotonic() < deadline:
                time.sleep(0.05)
            self.assertTrue(ready.exists(), "leader never installed its TERM handler")
            # Handshake: the late child does not exist until shutdown starts.
            self.assertFalse(late.exists())
            process.send_signal(signal.SIGTERM)
            code = process.wait(30)
            self.assertTrue(late.exists(), "the TERM handler never created the late child")
            pid = int(late.read_text())
            self.assertNotEqual(pid, int(ready.read_text()))
            self.assertEqual((code, json.loads(summary.read_text())),
                             (1, {"subreaper": True, "complete": True, "forced": True}))
            self.assertGone(pid)
            status = stop_supervised(process, summary, {"signals": []}, grace=0, alive=lambda p: False)
            self.assertEqual(status, "failed")

    def supervise(self, temp, mode, lifetime, stop_after=None, grace=1):
        summary, daemon = Path(temp) / "summary.json", Path(temp) / "daemon.pid"
        script = Path(temp) / "fixture.py"
        script.write_text(DAEMON)
        process = subprocess.Popen([sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary),
                                    "--grace", str(grace), "--", sys.executable, "-I", str(script),
                                    str(daemon), mode, str(lifetime)], start_new_session=True)
        deadline = time.monotonic() + 10
        while not daemon.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        pid = int(daemon.read_text())
        if stop_after is not None:
            time.sleep(stop_after)
            process.send_signal(signal.SIGTERM)
        code = process.wait(30)
        return code, json.loads(summary.read_text()), pid, summary

    def assertGone(self, pid):
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_cooperative_orphaned_setsid_daemon_stops_clean(self):
        with tempfile.TemporaryDirectory() as temp:
            code, record, pid, summary = self.supervise(temp, "cooperative", 60, stop_after=0.5)
            self.assertEqual((code, record), (0, {"subreaper": True, "complete": True, "forced": False}))
            self.assertGone(pid)

    def test_stubborn_daemon_after_leader_exit_is_forced_and_reported(self):
        with tempfile.TemporaryDirectory() as temp:
            # The leader exits at once; only the re-parented daemon keeps the boundary open.
            code, record, pid, summary = self.supervise(temp, "stubborn", 0, stop_after=1)
            self.assertEqual((code, record["complete"], record["forced"]), (1, True, True))
            self.assertEqual(json.loads(Path(str(summary) + ".main").read_text()), {"status": 0})
            self.assertGone(pid)

    def test_leader_without_descendants_exits_clean_on_its_own(self):
        with tempfile.TemporaryDirectory() as temp:
            summary = Path(temp) / "summary.json"
            code = subprocess.call([sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary), "--",
                                    sys.executable, "-c", "pass"], timeout=30)
            self.assertEqual(code, 0)
            self.assertEqual(json.loads(summary.read_text())["forced"], False)


@unittest.skipIf(sys.platform.startswith("linux"), "covers platforms without a child subreaper")
class NoSubreaperTests(unittest.TestCase):
    def test_supervisor_refuses_and_never_runs_the_command(self):
        with tempfile.TemporaryDirectory() as temp:
            summary, marker = Path(temp) / "summary.json", Path(temp) / "ran"
            code = subprocess.call([sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary), "--",
                                    sys.executable, "-c", f"open({str(marker)!r}, 'w')"], timeout=30)
            self.assertEqual(code, 2)
            self.assertFalse(json.loads(summary.read_text())["subreaper"])
            self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()
