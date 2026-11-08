"""Failure paths of the two-namespace suite: never leak the proxy or lose evidence."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import hub_two_namespace as suite_module


class FakeProxy:
    instances = []
    close_error = None

    def __init__(self, origin, certificate):
        self.url, self.healed, self.closed, self.cut = "http://127.0.0.1:1", 0, False, False
        FakeProxy.instances.append(self)

    def partition(self):
        self.cut = True

    def heal(self):
        self.healed += 1

    def close(self):
        self.closed = True
        if FakeProxy.close_error:
            raise FakeProxy.close_error


class FakeChild:
    def __init__(self, code=0, wait_error=None, pid=101, name="fake"):
        self.code, self.wait_error, self.pid, self.args = code, wait_error, pid, [name]
        self.waited = 0

    def send_signal(self, signal):
        pass

    def poll(self):
        return self.code

    def wait(self, timeout=None):
        self.waited += 1
        if self.wait_error:
            raise self.wait_error
        return self.code


class FakeHostB:
    def __init__(self, control, error=None):
        self.control, self.error, self.calls = control, error, 0

    def run(self, source, seconds=150):
        self.calls += 1
        if self.error:
            raise self.error
        return json.dumps({"stopped": {}, "postgres_stopped": True, "remaining": [], "zombies": []})


def bare_suite(root):
    suite = suite_module.Suite.__new__(suite_module.Suite)
    suite.root, suite.out, suite.a, suite.shared = root, root / "out", root, root
    suite.out.mkdir()
    suite.children, suite.results = [], []
    suite.hub_url, suite.hrx, suite.helper = "https://hostb:18790", "hrx", "app.test"
    suite.b = FakeHostB(root)
    (root / "config.toml").write_text('[hub]\nurl = "https://hostb:18790"\n')
    return suite


class PartitionProxyTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        FakeProxy.instances.clear()
        patcher = patch.object(suite_module, "PartitionProxy", FakeProxy)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_proxy_closed_when_partition_setup_fails(self):
        suite = bare_suite(self.root)
        with patch.object(suite_module.Suite, "snapshot", side_effect=RuntimeError("hub down")), \
                self.assertRaisesRegex(RuntimeError, "hub down"):
            suite.partition()
        proxy = FakeProxy.instances[0]
        self.assertTrue(proxy.closed)
        self.assertEqual(proxy.healed, 1)

    def test_proxy_healed_and_closed_when_an_assertion_fails_while_partitioned(self):
        suite = bare_suite(self.root)
        suite.beta, suite.replacement, suite.collector = FakeChild(), FakeChild(), FakeChild()
        suite.live = {"fixture/remote/agent": {"run_id": "remote"}}
        running = [{"run_id": "a", "agent": "fixture/alpha/agent", "status": "running"},
                   {"run_id": "b", "agent": "fixture/beta/agent", "status": "running"}]
        # A refused launch that wrongly succeeds (exit 0) must fail the scenario.
        with patch.object(suite_module.Suite, "snapshot", return_value=running), \
                patch.object(suite_module.Suite, "wait_for", return_value=running), \
                patch.object(suite_module.Suite, "spawn", return_value=FakeChild()), \
                patch.object(suite_module.Suite, "agent", return_value=FakeChild(code=0)), \
                self.assertRaises(AssertionError):
            suite.partition()
        proxy = FakeProxy.instances[0]
        self.assertTrue(proxy.cut and proxy.closed)
        self.assertEqual(proxy.healed, 1)


class CoincidingErrorTests(unittest.TestCase):
    def test_scenario_and_proxy_close_errors_are_both_kept(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(suite_module, "PartitionProxy", FakeProxy), \
                patch.object(FakeProxy, "close_error", RuntimeError("close failed")), \
                patch.object(suite_module.Suite, "snapshot", side_effect=ValueError("scenario failed")):
            suite = bare_suite(Path(directory))
            with self.assertRaisesRegex(ValueError, "scenario failed"):
                suite.partition()
        recorded = {result["scenario"]: result["detail"] for result in suite.results}
        self.assertIn("close failed", recorded["partition proxy close"])


class CleanupAndReportTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        # Pure tests: no real signals, sleeps or process listing.
        self.killpg = MagicMock()
        listing = MagicMock(return_value=subprocess.CompletedProcess(["ps"], 0, stdout="", stderr=""))
        for target, value in ((suite_module.os, ("killpg", self.killpg)),
                              (suite_module.time, ("sleep", MagicMock())),
                              (suite_module.subprocess, ("run", listing))):
            patcher = patch.object(target, value[0], value[1])
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_remote_cleanup_runs_even_when_local_cleanup_raises(self):
        suite = bare_suite(self.root)
        suite.children = [FakeChild(code=None, wait_error=subprocess.TimeoutExpired("fake", 10))]
        self.assertFalse(suite.cleanup())
        self.assertEqual(suite.b.calls, 1)
        self.assertTrue((self.root / "stop").exists())
        detail = json.loads(suite.results[-1]["detail"])
        self.assertEqual(suite.results[-1]["status"], "FAIL")
        self.assertIn("host_a", detail["errors"])

    def test_local_cleanup_and_stop_happen_when_remote_cleanup_raises(self):
        suite = bare_suite(self.root)
        suite.b = FakeHostB(self.root, error=AssertionError("host B request timed out"))
        self.assertFalse(suite.cleanup())
        self.assertTrue((self.root / "stop").exists())
        detail = json.loads(suite.results[-1]["detail"])
        self.assertIn("host_b", detail["errors"])
        self.assertIsInstance(detail["host_a_leftovers"], list)

    def test_every_child_is_stopped_even_when_one_fails(self):
        suite = bare_suite(self.root)
        children = [FakeChild(code=None, pid=11), FakeChild(code=None, pid=12, wait_error=OSError("stuck")),
                    FakeChild(code=None, pid=13)]
        suite.children = children
        leftovers, errors = suite.stop_local()
        signalled = {call.args[0] for call in self.killpg.call_args_list}
        self.assertEqual(signalled, {11, 12, 13})
        self.assertTrue(all(child.waited == 1 for child in children))
        self.assertEqual((leftovers, len(errors)), ([], 1))

    def test_any_recorded_failure_fails_the_run(self):
        suite = bare_suite(self.root)
        (self.root / "collector.log").write_text("log")
        with patch.object(suite_module.Suite, "setup"), \
                patch.object(suite_module.Suite, "ownership_exclusion"), \
                patch.object(suite_module.Suite, "collector_recovery"), \
                patch.object(suite_module.Suite, "partition"), \
                patch.object(suite_module.Suite, "cleanup", return_value=True), \
                patch.object(suite_module.shutil, "copy", side_effect=OSError("disk full")):
            self.assertEqual(suite.run(), 1)
        report = json.loads((self.root / "out" / "suite.json").read_text())
        self.assertIn("log collection", [result["scenario"] for result in report["results"]])

    def test_report_keeps_the_original_error_when_cleanup_itself_raises(self):
        suite = bare_suite(self.root)
        with patch.object(suite_module.Suite, "setup", side_effect=ValueError("setup boom")), \
                patch.object(suite_module.Suite, "cleanup", side_effect=RuntimeError("cleanup boom")):
            self.assertEqual(suite.run(), 1)
        report = json.loads((self.root / "out" / "suite.json").read_text())
        details = " ".join(result["detail"] for result in report["results"])
        self.assertIn("setup boom", details)
        self.assertIn("cleanup boom", details)
        self.assertEqual(report["evidence"], suite_module.EVIDENCE)


if __name__ == "__main__":
    unittest.main()
