"""Pure unit tests for pitput_targeted_launch.py's ownership, redaction and temp handling.

No processes, sockets or databases: subprocess objects, signals, summaries, clocks,
execve and atexit are all fakes. Run: python3 -m unittest discover -s tests/integration
-p test_pitput_targeted_launch_harness.py
"""

import json
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest
import unittest.mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pitput_targeted_launch as harness  # noqa: E402


class FakeProcess:
    """A supervisor whose wait() times out a scripted number of times."""

    def __init__(self, timeouts=0, exit_code=0, pid=4242):
        self.pid, self.returncode = pid, None
        self.timeouts, self.exit_code, self.waits = timeouts, exit_code, []

    def wait(self, timeout):
        self.waits.append(timeout)
        if self.timeouts:
            self.timeouts -= 1
            raise subprocess.TimeoutExpired("supervisor", timeout)
        self.returncode = self.exit_code
        return self.returncode


def summaries(record, main=None):
    files = {"summary.json": record, "summary.json.main": main}
    return lambda path: files.get(str(path))


class StopOwnedTests(unittest.TestCase):
    def stop(self, process, record, main=None, alive=True):
        kills = []
        result = harness.stop_owned(process, "summary.json", grace=10, kill=lambda pid, sig: kills.append((pid, sig)),
                                    alive=lambda _: alive, read=summaries(record, main))
        return result, kills

    def test_clean_requires_complete_unforced_summary_and_zero_exit(self):
        result, kills = self.stop(FakeProcess(), {"subreaper": True, "complete": True, "forced": False}, {"status": 0})
        self.assertEqual(result["classification"], "clean")
        self.assertEqual(kills, [(4242, signal.SIGTERM)])
        self.assertEqual(result["leader_exit"], 0)
        self.assertTrue(result["supervisor_reaped"])

    def test_forced_descendants(self):
        result, _ = self.stop(FakeProcess(exit_code=1), {"subreaper": True, "complete": True, "forced": True})
        self.assertEqual(result["classification"], "owned_cleanup_forced")

    def test_missing_or_unbounded_summary_is_unknown(self):
        for record in (None, {"subreaper": False, "complete": False, "forced": False},
                       {"subreaper": True, "complete": False, "forced": False}):
            with self.subTest(record=record):
                result, _ = self.stop(FakeProcess(), record)
                self.assertEqual(result["classification"], "owned_cleanup_unknown")

    def test_nonzero_supervisor_exit_without_force_is_not_clean(self):
        result, _ = self.stop(FakeProcess(exit_code=2), {"subreaper": True, "complete": True, "forced": False})
        self.assertEqual(result["classification"], "owned_cleanup_unknown")

    def test_escalates_term_then_kill_with_bounded_waits(self):
        process = FakeProcess(timeouts=1)
        result, kills = self.stop(process, {"subreaper": True, "complete": True, "forced": False})
        self.assertEqual([sig for _, sig in kills], [signal.SIGTERM, signal.SIGKILL])
        self.assertEqual(process.waits, [30, 5])
        self.assertTrue(result["supervisor_reaped"])
        # A killed supervisor cannot prove its descendants were reaped.
        self.assertEqual(result["classification"], "owned_cleanup_unknown")

    def test_unreaped_supervisor_is_unknown(self):
        process = FakeProcess(timeouts=2)
        result, kills = self.stop(process, {"subreaper": True, "complete": True, "forced": False})
        self.assertEqual(result["signals"], ["SIGTERM", "SIGKILL"])
        self.assertFalse(result["supervisor_reaped"])
        self.assertEqual(result["classification"], "owned_cleanup_unknown")
        self.assertEqual({pid for pid, _ in kills}, {4242}, "only the supervisor is ever signalled")

    def test_exited_supervisor_is_not_signalled(self):
        result, kills = self.stop(FakeProcess(), {"subreaper": True, "complete": True, "forced": False}, alive=False)
        self.assertEqual(kills, [])
        self.assertEqual(result["classification"], "clean")


class DriveTests(unittest.TestCase):
    def run_drive(self, wait=None, act=None, publish=None, wait_leader=None):
        calls, state = [], {}

        def stop(process):
            calls.append(("stop", process))
            return {"classification": "clean"}

        posted = harness.drive(start=lambda: "supervisor", stop=stop, phases=("a", "b"),
                               wait=wait or (lambda p, phase: True),
                               act=act or (lambda phase: {"phase": phase}),
                               publish=publish or (lambda phase, answer: None),
                               wait_leader=wait_leader or (lambda p: None), state=state)
        return posted, calls, state

    def test_cleanup_on_normal_completion(self):
        posted, calls, state = self.run_drive()
        self.assertEqual(list(posted), ["a", "b"])
        self.assertEqual(calls, [("stop", "supervisor")])
        self.assertEqual(state["cleanup"]["classification"], "clean")

    def test_cleanup_on_ready_timeout(self):
        clock = iter([0, 1, 300])

        def wait(process, phase):
            return harness.wait_ready(Path("/nonexistent/ready"), lambda: False, timeout=240,
                                      clock=lambda: next(clock), sleep=lambda _: None)

        calls, state = [], {}
        with self.assertRaises(harness.ReadyTimeout):
            harness.drive(start=lambda: "supervisor", stop=lambda p: calls.append(p) or {"classification": "clean"},
                          phases=("a",), wait=wait, act=lambda phase: {}, publish=lambda *_: None,
                          wait_leader=lambda p: None, state=state)
        self.assertEqual(calls, ["supervisor"])
        self.assertIn("cleanup", state)

    def test_cleanup_on_act_exception(self):
        calls, state = [], {}

        def act(phase):
            raise RuntimeError("pitput post failed")

        with self.assertRaises(RuntimeError):
            harness.drive(start=lambda: "supervisor", stop=lambda p: calls.append(p) or {"classification": "clean"},
                          phases=("a",), wait=lambda p, phase: True, act=act, publish=lambda *_: None,
                          wait_leader=lambda p: None, state=state)
        self.assertEqual(calls, ["supervisor"])

    def test_cleanup_on_leader_timeout(self):
        def wait_leader(process):
            raise AssertionError("native test exceeded its deadline")

        with self.assertRaises(AssertionError):
            self.run_drive(wait_leader=wait_leader)

    def test_leader_exit_before_ready_stops_handshake(self):
        posted, calls, _ = self.run_drive(wait=lambda p, phase: phase != "b")
        self.assertEqual(list(posted), ["a"])
        self.assertEqual(len(calls), 1)

    def test_wait_ready_reports_leader_exit(self):
        self.assertFalse(harness.wait_ready(Path("/nonexistent/ready"), lambda: True,
                                            clock=lambda: 0, sleep=lambda _: None))


class SanitizeTests(unittest.TestCase):
    SECRET = "dm-body-sentinel-0123456789abcdef"  # gitleaks:allow (test fixture)

    def test_redacts_multiline_log(self):
        log = f"=== RUN TestX\nline with {self.SECRET} inside\n{self.SECRET}\n--- PASS: TestX\n"
        clean, leaked = harness.sanitize(log, [self.SECRET])
        self.assertTrue(leaked)
        self.assertNotIn(self.SECRET, clean)
        self.assertEqual(clean.count(harness.REDACTED), 2)
        self.assertIn("--- PASS: TestX", clean)

    def test_redacts_exception_text(self):
        error = AssertionError(f"pitput failed (2): body {self.SECRET} rejected")
        clean, leaked = harness.sanitize(f"{type(error).__name__}: {error}", [self.SECRET])
        self.assertTrue(leaked)
        self.assertNotIn(self.SECRET, clean)

    def test_clean_text_unchanged(self):
        self.assertEqual(harness.sanitize("nothing here", [self.SECRET, ""]), ("nothing here", False))
        self.assertEqual(harness.sanitize(None, [self.SECRET]), ("", False))

    def test_leak_fails_an_otherwise_passing_run(self):
        clean = {"classification": "clean", "leader_exit": 0}
        output = "--- PASS: TestX (1.0s)\n"
        self.assertIsNone(harness.evaluate(clean, output, "TestX", False))
        self.assertEqual(harness.evaluate(clean, output, "TestX", True), ("FAIL", "dm_sentinel_leak"))


class EvaluateTests(unittest.TestCase):
    PASS = "--- PASS: TestX (1.0s)\n"

    def test_cleanup_classification_gates_pass(self):
        for classification in ("owned_cleanup_forced", "owned_cleanup_unknown"):
            with self.subTest(classification=classification):
                self.assertEqual(harness.evaluate({"classification": classification, "leader_exit": 0},
                                                  self.PASS, "TestX", False), ("FAIL", classification))
        self.assertEqual(harness.evaluate(None, self.PASS, "TestX", False), ("FAIL", "owned_cleanup_unknown"))

    def test_blocked_is_never_pass(self):
        output = "BLOCKED pitput_targeted_unsupported: monitor CLI lacks --follow-thread\n"
        self.assertEqual(harness.evaluate({"classification": "clean", "leader_exit": 1}, output, "TestX", False),
                         ("BLOCKED", "pitput_targeted_unsupported"))

    def test_leader_status_and_markers(self):
        clean = {"classification": "clean"}
        for leader, output in ((1, self.PASS), (None, self.PASS), (0, "--- FAIL: TestX"),
                               (0, self.PASS + "--- SKIP: TestX")):
            with self.subTest(leader=leader, output=output):
                self.assertEqual(harness.evaluate(dict(clean, leader_exit=leader), output, "TestX", False),
                                 ("FAIL", "native_test_failed"))


class Replaced(BaseException):
    """Stands in for a successful execve: control never comes back to the caller."""


class OverrideTempTests(unittest.TestCase):
    def setUp(self):
        self.base = Path(tempfile.mkdtemp(prefix="hrx-harness-test-"))
        self.addCleanup(shutil.rmtree, self.base, True)

    def test_early_build_failure_removes_work_directory(self):
        created = []

        def mkdtemp(prefix, dir):
            path = tempfile.mkdtemp(prefix=prefix, dir=dir)
            created.append(Path(path))
            return path

        def failing(args, **_):
            raise SystemExit("pitput source override failed at `git fetch`")

        with unittest.mock.patch.dict("os.environ", {"HRX_PITPUT_SOURCE_URL": "/nonexistent"}):
            with self.assertRaises(SystemExit):
                harness.build_override("a" * 40, run=failing, mkdtemp=mkdtemp, tmp=str(self.base))
        self.assertEqual(len(created), 1)
        self.assertTrue(created[0].name.startswith("hrx-pitput-src-"))
        self.assertFalse(created[0].exists())

    def test_successful_exec_retains_work_directory(self):
        work = self.base / "hrx-pitput-src-ok"
        work.mkdir()
        removed, executed = [], []

        def execve(path, argv, env):
            executed.append((path, env["HRX_PITPUT_OVERRIDE_WORKDIR"], work.exists()))
            raise Replaced()

        with self.assertRaises(Replaced):
            harness.reexec_override("a" * 40, build=lambda sha: (work / "venv/bin/python", work / "provenance.json", work),
                                    execve=execve, rmtree=lambda path, **_: removed.append(path))
        self.assertEqual(executed, [(str(work / "venv/bin/python"), str(work), True)])
        self.assertEqual(removed, [])
        self.assertTrue(work.exists())

    def test_failed_exec_removes_work_directory(self):
        work = self.base / "hrx-pitput-src-fail"
        work.mkdir()
        removed = []

        def execve(*_):
            raise OSError("exec format error")

        with self.assertRaises(OSError):
            harness.reexec_override("a" * 40, build=lambda sha: (work / "python", work / "provenance.json", work),
                                    execve=execve, rmtree=lambda path, **_: removed.append(path))
        self.assertEqual(removed, [work])

    def test_reexecuted_process_adopts_and_removes_on_exit(self):
        registered, handlers = [], {}
        adopted = harness.adopt_override_workdir(
            {"HRX_PITPUT_OVERRIDE_WORKDIR": str(self.base / "hrx-pitput-src-x")},
            register=lambda *args: registered.append(args),
            on_signal=lambda sig, handler: handlers.__setitem__(sig, handler))
        self.assertEqual(adopted, self.base / "hrx-pitput-src-x")
        self.assertEqual(len(registered), 1)
        self.assertEqual(registered[0][1:], (str(self.base / "hrx-pitput-src-x"), True))
        with self.assertRaises(SystemExit):
            handlers[signal.SIGTERM](signal.SIGTERM, None)

    def test_adoption_refuses_a_missing_handover(self):
        for env in ({}, {"HRX_PITPUT_OVERRIDE_WORKDIR": "/tmp/unrelated"}):
            with self.subTest(env=env), self.assertRaises(SystemExit):
                harness.adopt_override_workdir(env, register=lambda *_: None, on_signal=lambda *_: None)


def quiet_receipt(unrelated=4, own=5, **overrides):
    quiet = {"unrelated_seq": unrelated, "self_authored_followed_seq": own, "max_quiet_seq": max(unrelated, own),
             "observed_window_ms": 20500, "post_injection_samples": 12, "injection_after_baseline_ms": 300,
             "requests_at_baseline": 3, "requests_at_window_end": 3,
             "board_pending_per_sample": [False] * 12, "dm_pending_per_sample": [False] * 12}
    quiet.update(overrides)
    return quiet


QUIET_POSTS = {"fresh-B": [{"kind": "unrelated", "seq": 4, "path": "/b/exchange/unrelated/4"},
                           {"kind": "self_followed", "seq": 5, "path": "/b/exchange/followed/5"}]}


class PartialReceiptTests(unittest.TestCase):
    """Job 5607: fresh-C failed and the fresh-B evidence was lost. Each phase is now
    persisted; a partial receipt is cross-checked but can never become a PASS."""

    def test_partial_receipt_is_checked_but_not_complete(self):
        receipt = {"passed": False, "phases_completed": ["fresh-B"], "fresh": {"quiet": quiet_receipt()},
                   "diagnostics": {"fresh-C": {"requests_since_phase_start": 1}}}
        self.assertEqual(harness.partial_check(receipt, QUIET_POSTS), [])
        self.assertFalse(harness.receipt_complete(receipt))
        # The run still fails on the leader's status, whatever the partial evidence says.
        self.assertEqual(harness.evaluate({"classification": "clean", "leader_exit": 1}, "--- FAIL: TestX", "TestX",
                                          False), ("FAIL", "native_test_failed"))

    def test_partial_receipt_inconsistencies_are_reported(self):
        receipt = {"phases_completed": ["fresh-B"],
                   "fresh": {"quiet": quiet_receipt(unrelated=9, observed_window_ms=1000,
                                                    board_pending_per_sample=[False, True])}}
        self.assertEqual(harness.partial_check(receipt, QUIET_POSTS),
                         ["fresh-B:unrelated_seq_mismatch", "fresh-B:window_too_short", "fresh-B:pending_observed"])

    def test_delivery_must_be_handle_bound_and_past_the_barrier(self):
        posts = dict(QUIET_POSTS, **{"fresh-C": [{"kind": "followed_no_mention", "seq": 6, "path": "/b/exchange/followed/6"}]})
        receipt = {"phases_completed": ["fresh-B", "fresh-C"],
                   "fresh": {"quiet": quiet_receipt(), "followed": {"seq": 6, "delivered": True,
                                                                   "handle_bound_to_event": False}}}
        self.assertEqual(harness.partial_check(receipt, posts), ["fresh-C:delivery_not_bound"])
        receipt["fresh"]["followed"]["handle_bound_to_event"] = True
        self.assertEqual(harness.partial_check(receipt, posts), [])
        receipt["fresh"]["quiet"]["max_quiet_seq"] = 6
        self.assertEqual(harness.partial_check(receipt, posts), ["fresh-C:seq_barrier_not_passed"])

    def test_missing_receipt_and_completeness(self):
        self.assertEqual(harness.partial_check(None, {}), ["no_receipt"])
        self.assertTrue(harness.receipt_complete({"phases_completed": list(harness.RECEIPT_PHASES)}))
        self.assertFalse(harness.receipt_complete({"phases_completed": list(harness.RECEIPT_PHASES)[:-1]}))

    def test_drive_exposes_posted_phases_when_a_later_phase_fails(self):
        state = {}

        def act(phase):
            if phase == "b":
                raise AssertionError("fresh-C was not delivered")
            return {"posts": [phase]}

        with self.assertRaises(AssertionError):
            harness.drive(start=lambda: "supervisor", stop=lambda p: {"classification": "clean"}, phases=("a", "b"),
                          wait=lambda p, phase: True, act=act, publish=lambda *_: None,
                          wait_leader=lambda p: None, state=state)
        self.assertEqual(list(state["posted"]), ["a"])
        self.assertIn("cleanup", state)


class HandshakeRefTests(unittest.TestCase):
    SENT = {"message": "dm-abcdefghijk2", "conversation": "dc-abcdefghijk3", "conv_seq": 1,
            "recipient": "test/codextgt/agent", "event_key": "ek-0123abcd", "request_id": "r", "stored": True}

    def test_dm_ref_carries_the_send_identifiers(self):
        self.assertEqual(harness.dm_ref(self.SENT), {"kind": "dm", "path": "dm-abcdefghijk2", "seq": 0,
                                                     "event_key": "ek-0123abcd", "conversation": "dc-abcdefghijk3"})

    def test_dm_ref_rejects_incomplete_sends(self):
        for change in ({"stored": False}, {"message": None}, {"conversation": "dc-x"}, {"event_key": ""}):
            with self.subTest(change=change), self.assertRaises(AssertionError):
                harness.dm_ref(dict(self.SENT, **change))

    def test_post_ref_keeps_a_valid_event_key(self):
        self.assertEqual(harness.post_ref({"path": "/b/exchange/followed/6", "event_key": "k1"}),
                         {"path": "/b/exchange/followed/6", "seq": 6, "event_key": "k1"})
        self.assertNotIn("event_key", harness.post_ref({"path": "/b/exchange/followed/6", "event_key": "bad key"}))


class ProvenanceFileTests(unittest.TestCase):
    def test_read_json_tolerates_missing_and_invalid(self):
        with tempfile.TemporaryDirectory() as directory:
            bad = Path(directory) / "bad.json"
            bad.write_text("{")
            good = Path(directory) / "good.json"
            good.write_text(json.dumps({"status": 0}))
            self.assertIsNone(harness.read_json(Path(directory) / "missing.json"))
            self.assertIsNone(harness.read_json(bad))
            self.assertEqual(harness.read_json(good), {"status": 0})


if __name__ == "__main__":
    unittest.main()
