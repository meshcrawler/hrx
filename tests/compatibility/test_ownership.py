"""Pure tests for the ownership interface: no process is launched, signalled or scanned."""

import signal
import unittest

import ownership
from ownership import Entry, ScanError, UidStop, classify_darwin_final, classify_summary

U, ME = 601, Entry(pid=50, start=1000, ruid=601, euid=601)


class AvailabilityTests(unittest.TestCase):
    def test_only_linux_has_a_verified_boundary(self):
        self.assertEqual(ownership.available("linux"), (ownership.LINUX_SUBREAPER, None))
        self.assertEqual(ownership.available("darwin"), (None, "darwin_containment_unproven"))
        for platform in ("win32", "freebsd14", "cygwin", ""):
            with self.subTest(platform=platform):
                self.assertEqual(ownership.available(platform), (None, "ownership_boundary_unavailable"))

    def test_darwin_refusal_reaches_every_core_guard(self):
        import tempfile
        from pathlib import Path
        from unittest.mock import patch
        import owned_capture
        import run
        with patch("owned_capture.sys.platform", "darwin"), self.assertRaises(RuntimeError) as raised:
            owned_capture.capture(["true"], env={}, cwd="/", deadline=0, abort=lambda: False)
        self.assertEqual(str(raised.exception), "linux_subreaper_required")
        runner = run.Runner.__new__(run.Runner)
        with patch("run.sys.platform", "darwin"), tempfile.TemporaryDirectory() as temp:
            result = runner.launch({"harness": "claude"}, Path(temp), Path(temp), "prompt")
        self.assertEqual(result, {"status": "BLOCKED", "reason": "native_cleanup_boundary_unavailable"})


class SummaryTests(unittest.TestCase):
    def test_linux_summaries_classify_exactly_as_before(self):
        cases = [({"subreaper": True, "complete": True, "forced": False}, 0, "clean"),
                 ({"subreaper": True, "complete": True, "forced": True}, 1, "failed"),
                 ({"subreaper": True, "complete": True, "forced": False}, 1, "failed"),
                 ({"subreaper": False, "complete": False, "forced": False}, 2, "unknown"),
                 ({"subreaper": True, "complete": False, "forced": False}, 0, "unknown"),
                 (None, 0, "unknown"), ({}, 0, "unknown"), ([], 0, "unknown")]
        for record, code, expected in cases:
            with self.subTest(record=record, code=code):
                self.assertEqual(classify_summary(record, code), expected)

    def test_darwin_helper_summary_alone_is_never_clean(self):
        helper = {"boundary": "darwin-uid", "helper_empty": True, "forced": False, "complete": True,
                  "subreaper": True}
        self.assertEqual(classify_summary(helper, 0), "unknown")


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def tick(self):
        self.now += 1.0


def stop(scans, grace=3.0, hard_limit=6.0):
    """Run UidStop over scripted scans; the last scan repeats."""
    clock, sent, queue = Clock(), [], list(scans)

    def scan():
        result = queue.pop(0) if len(queue) > 1 else queue[0]
        if isinstance(result, Exception):
            raise result
        return result
    summary = UidStop(U, ME, scan, sent.append, clock, clock.tick, grace=grace, hard_limit=hard_limit).run()
    return summary, sent


class UidStopTests(unittest.TestCase):
    child = Entry(pid=51, start=1001, ruid=U, euid=U)

    def test_empty_except_self_ends_the_loop_but_is_not_completion(self):
        summary, sent = stop([[ME, self.child], [ME]])
        self.assertTrue(summary["helper_empty"])
        self.assertNotIn("complete", summary)
        self.assertEqual(sent, [signal.SIGTERM, signal.SIGCONT])
        self.assertEqual(classify_summary(summary, 0), "unknown")

    def test_ignoring_sigterm_escalates_and_records_forced(self):
        summary, sent = stop([[ME, self.child]] * 4 + [[ME]])
        self.assertTrue(summary["forced"])
        self.assertIn(signal.SIGKILL, sent)
        self.assertTrue(summary["helper_empty"])

    def test_residual_at_hard_limit_is_not_empty(self):
        summary, _ = stop([[ME, self.child]])
        self.assertFalse(summary["helper_empty"])
        self.assertEqual(summary["residual"], 1)

    def test_self_identity_must_appear_exactly_once_and_match(self):
        cases = {"absent (empty scan)": ([], "self_identity_absent"),
                 "absent among others": ([self.child], "self_identity_absent"),
                 "duplicate": ([ME, ME], "self_identity_duplicate"),
                 "reused pid": ([Entry(ME.pid, ME.start + 1, U, U)], "self_identity_mismatch"),
                 "changed uid": ([Entry(ME.pid, ME.start, U, 0)], "self_identity_mismatch")}
        for name, (scan, error) in cases.items():
            with self.subTest(name=name):
                summary, _ = stop([scan])
                self.assertEqual(summary["error"], error)
                self.assertFalse(summary["helper_empty"])

    def test_late_self_loss_ends_with_an_error(self):
        summary, _ = stop([[ME, self.child], [self.child]])
        self.assertEqual(summary["error"], "self_identity_absent")

    def test_uid_transition_is_unknown(self):
        for entry in (Entry(52, 2, U, 0), Entry(53, 3, 0, U)):
            with self.subTest(entry=entry):
                summary, _ = stop([[ME, entry]])
                self.assertTrue(summary["transition"])
                self.assertEqual(summary["error"], "uid_transition_observed")

    def test_scan_failure_is_unknown(self):
        summary, _ = stop([ScanError("proc_listpids")])
        self.assertEqual(summary["error"], "scan_failed")


HELPER = {"boundary": "darwin-uid", "helper_empty": True, "forced": False, "transition": False,
          "error": None, "residual": 0}


def final(helper=None, exit_at=10.0, scans=((11.0, []), (25.0, [])), settle=10.0, lock=True,
          baseline=("a",), after=("a",), cron=(), proof=None):
    helper = dict(HELPER) if helper is None else helper
    return classify_darwin_final(helper, exit_at, list(scans), settle, lock, baseline, after, cron, proof)


class FinalProofTests(unittest.TestCase):
    def test_never_clean_while_containment_is_unproven(self):
        self.assertEqual(final(), ("unknown", ["containment_unproven"]))
        self.assertEqual(final(proof={"backend": "anything"}), ("unknown", ["containment_proof_unreviewed"]))

    def test_incomplete_or_untimed_evidence_is_unknown(self):
        missing = {k: v for k, v in HELPER.items()}
        cases = {}
        for field in ("forced", "transition", "helper_empty", "boundary", "error"):
            partial = dict(missing)
            del partial[field]
            cases["missing " + field] = dict(helper=partial)
        cases.update({
            "non-bool forced": dict(helper={**HELPER, "forced": None}),
            "helper error": dict(helper={**HELPER, "error": "scan_failed"}),
            "transition": dict(helper={**HELPER, "transition": True}),
            "no helper exit time": dict(exit_at=None),
            "scan before helper exit": dict(scans=((9.0, []), (25.0, []))),
            "scans not settled": dict(scans=((11.0, []), (12.0, []))),
            "scans out of order": dict(scans=((25.0, []), (11.0, []))),
            "one scan only": dict(scans=((11.0, []),)),
            "untimed scans": dict(scans=([], [])),
            "scan failed": dict(scans=((11.0, []), (25.0, None))),
            "lock released early": dict(lock=False),
            "launchd unenumerable": dict(after=None),
            "cron unenumerable": dict(cron=None)})
        for name, kwargs in cases.items():
            with self.subTest(name=name):
                result, reasons = final(**kwargs)
                self.assertEqual(result, "unknown")
                self.assertNotIn("containment_unproven", reasons)

    def test_proven_leftovers_fail(self):
        residual = Entry(60, 5, U, U)
        cases = {"residual process": dict(scans=((11.0, []), (25.0, [residual]))),
                 "delayed launchd definition remains": dict(after=("a", "run.created.interval")),
                 "cron entry remains": dict(cron=("* * * * * x",)),
                 "forced": dict(helper={**HELPER, "forced": True}),
                 "helper never empty": dict(helper={**HELPER, "helper_empty": False})}
        for name, kwargs in cases.items():
            with self.subTest(name=name):
                self.assertEqual(final(**kwargs)[0], "failed")


if __name__ == "__main__":
    unittest.main()
