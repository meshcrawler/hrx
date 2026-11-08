import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

from owned_capture import capture
from run import Runner, retry_safe


class LaunchEvidenceTests(unittest.TestCase):
    def test_resume_cleanup_failure_prevents_cell_retry(self):
        runner = Runner("hrx", Mock(), {})
        def launch(cell, home, project, prompt, resume=None):
            if resume:
                return {"status": "FAIL", "reason": "native_cleanup_unknown", "cleanup": "unknown"}
            nonce = (project / "challenge.txt").read_text()
            (project / "result.txt").write_text(nonce[::-1])
            return {"status": "PASS", "cleanup": "clean", "session_id": "fixture-session",
                    "tool_calls": 1, "tool_results": 1, "exact_route_protocol": True}
        with tempfile.TemporaryDirectory() as temp, patch.object(runner, "launch", launch):
            result = runner.cell({}, Path(temp))
        self.assertEqual(result["cleanup"], "unknown")
        self.assertEqual(result["initial_cleanup"], "clean")
        self.assertFalse(retry_safe(result))

    def test_retry_requires_positive_cleanup_and_keeps_budget_rejections_terminal(self):
        for cleanup in (None, "failed", "unknown"):
            self.assertFalse(retry_safe({"status": "FAIL", "cleanup": cleanup}))
        self.assertTrue(retry_safe({"status": "FAIL", "cleanup": "clean"}))
        self.assertFalse(retry_safe({"status": "BLOCKED", "cleanup": "clean",
                                     "reason": "provider_auth_quota_or_availability"}))

    def result(self, cleanup="clean", exit_code=0, timed_out=False):
        gateway = Mock(requests=[], deadline=time.monotonic() + 30)
        runner = Runner("hrx", gateway, {"cell_seconds": 10})
        with patch("run.sys.platform", "linux"), patch.object(runner, "configure", return_value={}), \
                patch("run.capture", return_value={"stdout": b"", "cleanup": cleanup,
                                                  "exit_code": exit_code, "timed_out": timed_out}):
            return runner.launch({"harness": "codex", "route": "r", "protocol": "responses",
                                  "reasoning": None}, Path("home"), Path("project"), "test")

    def test_forced_or_unknown_cleanup_never_passes(self):
        for status in ("failed", "unknown"):
            with self.subTest(status=status):
                result = self.result(cleanup=status)
                self.assertEqual((result["status"], result["reason"]), ("FAIL", "native_cleanup_" + status))

    def test_native_exit_is_not_replaced_by_supervisor_success(self):
        self.assertEqual(self.result(exit_code=7)["reason"], "native_or_launcher_exit")
        self.assertEqual(self.result(exit_code=None)["reason"], "native_exit_unverified")

    def test_deadline_is_not_a_pass_after_clean_teardown(self):
        self.assertEqual(self.result(timed_out=True)["reason"], "native_deadline")

    def test_unsupported_platform_does_not_launch_or_configure(self):
        runner = Runner("hrx", Mock(), {})
        with patch("run.sys.platform", "darwin"), patch.object(runner, "configure") as configure, \
                patch("run.capture") as execute:
            self.assertEqual(runner.launch({}, None, None, "test")["status"], "BLOCKED")
        configure.assert_not_called()
        execute.assert_not_called()


@unittest.skipUnless(sys.platform.startswith("linux"), "Linux process tests run on GitLab")
class CaptureProcessTests(unittest.TestCase):
    def run_command(self, source, seconds=5):
        with tempfile.TemporaryDirectory() as temp:
            return capture([sys.executable, "-c", source], env=os.environ.copy(), cwd=temp,
                           deadline=time.monotonic() + seconds, abort=lambda: False, grace=.2)

    def test_normal_exit_preserves_output_and_nonzero_status(self):
        for attempt in range(100):
            with self.subTest(attempt=attempt):
                result = self.run_command("import sys; print('fixture-output', flush=True); sys.exit(7)")
                self.assertEqual(result, {"stdout": b"fixture-output\n", "exit_code": 7,
                                          "timed_out": False, "cleanup": "clean", "supervisor_exit": 0,
                                          "supervisor_summary": {"subreaper": True, "complete": True,
                                                                 "forced": False}})

    def test_detached_child_with_open_output_is_reaped(self):
        result = self.run_command("""
import os, time
if os.fork() == 0:
    os.setsid()
    while True: time.sleep(.1)
print('parent-finished', flush=True)
""")
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["cleanup"], "clean")
        self.assertFalse(result["timed_out"])

    def test_detached_child_with_open_stderr_is_reaped(self):
        source = """
import os, sys, time
print('fixture-error', file=sys.stderr, flush=True)
if os.fork() == 0:
    os.setsid()
    time.sleep(60)
else:
    sys.exit(0)
"""
        with tempfile.TemporaryDirectory() as temp:
            result = capture([sys.executable, "-c", source], env=os.environ.copy(), cwd=temp,
                             deadline=time.monotonic() + 5, abort=lambda: False,
                             grace=.2, capture_stderr=True)
        self.assertEqual(result["stderr"], b"fixture-error\n")
        self.assertEqual(result["cleanup"], "clean")
        self.assertFalse(result["timed_out"])

    def test_stubborn_orphan_is_non_green(self):
        result = self.run_command("""
import os, signal, time
r, w = os.pipe()
if os.fork() == 0:
    os.close(r)
    os.setsid()
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    os.write(w, b'1')
    while True: time.sleep(.1)
os.close(w)
os.read(r, 1)
""")
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["cleanup"], "failed")

    def test_term_created_child_cannot_escape_boundary(self):
        result = self.run_command("""
import os, signal, time
def stop(*args):
    if os.fork() == 0:
        os.setsid()
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        while True: time.sleep(.1)
    os._exit(0)
signal.signal(signal.SIGTERM, stop)
while True: time.sleep(.1)
""", seconds=1)
        self.assertTrue(result["timed_out"])
        self.assertEqual(result["cleanup"], "failed")


if __name__ == "__main__":
    unittest.main()
