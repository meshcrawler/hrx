"""Pure orchestration tests: no native commands, servers or inference."""
from contextlib import ExitStack
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch
import xml.etree.ElementTree as ET

import run
from scenarios import compare_upgrade, validate_release


class FinalizationTests(unittest.TestCase):
    def exercise(self, close_effect, publication_failure=None, active=0, drain=False, listener_alive=False):
        with tempfile.TemporaryDirectory() as temp, ExitStack() as stack:
            root = Path(temp)
            inventory = {"cells": [], "conversation": [], "release_dimensions": []}
            for name, data in [("inventory", json.dumps(inventory)), ("catalog", "{}"),
                               ("key", "private-test-key"), ("hrx", "not executable")]:
                (root / name).write_text(data)
                (root / name).chmod(0o600)
            gateway = Mock(admitted=0, requests=[], active=active, closed=True, lock=threading.Condition())
            gateway.thread.is_alive.return_value = listener_alive

            def wait(timeout):
                if drain:
                    gateway.active = 0
                    gateway.admitted = 1
                    gateway.requests.append({})
                else:
                    raise TimeoutError("secret-cleanup-detail")

            stack.enter_context(patch.object(gateway.lock, "wait", side_effect=wait))
            gateway.close.side_effect = close_effect
            stack.enter_context(patch.dict("os.environ", {
                "HRX_CI_KEY_FILE": str(root / "key"), "HRX_CI_CLAUDE_LEASE_FILE": "",
                "HRX_CI_GATEWAY": "https://gateway.invalid"}))
            stack.enter_context(patch("sys.argv", ["run.py", "--lane", "release",
                "--inventory", str(root / "inventory"), "--catalog", str(root / "catalog"),
                "--policy", str(root / "catalog"),
                "--hrx", str(root / "hrx"), "--output", str(root / "out")]))
            mocks = {
                "resolve_latest_families": inventory, "scenario_rows": [], "resume_rows": [],
                "policy_from_file": {}, "verify_key": {}, "Gateway": gateway,
                "Runner": Mock(), "run_scenarios": None, "run_resumes": None,
                "shutil.which": None, "subprocess.run": Mock(stdout="test-commit"),
            }
            for name, value in mocks.items():
                stack.enter_context(patch("run." + name, return_value=value))
            stack.enter_context(patch("conversation.conversation", return_value={
                "id": "mixed-pitput-conversation", "required": True, "status": "PASS"}))
            snapshots = []
            real_report = run.report

            def report(output, results, meta):
                if publication_failure and results[-1]["status"] == "PASS":
                    if publication_failure == "json":
                        original_replace = run.os.replace
                        replacements = 0

                        def replace(source, destination):
                            nonlocal replacements
                            replacements += 1
                            if replacements == 2:
                                raise OSError("secret-cleanup-detail")
                            return original_replace(source, destination)

                        with patch("run.os.replace", side_effect=replace):
                            return real_report(output, results, meta)
                    if publication_failure == "junit":
                        with patch.object(ET.ElementTree, "write", side_effect=OSError("secret-cleanup-detail")):
                            return real_report(output, results, meta)
                    original_write = Path.write_text

                    def write(path, *args, **kwargs):
                        if path.name == "compatibility.md":
                            raise OSError("secret-cleanup-detail")
                        return original_write(path, *args, **kwargs)

                    with patch.object(Path, "write_text", write):
                        return real_report(output, results, meta)
                outcome = real_report(output, results, meta)
                snapshots.append(json.loads((output / "results.json").read_text()))
                return outcome

            stack.enter_context(patch("run.report", side_effect=report))
            code = run.main()
            document = json.loads((root / "out/results.json").read_text())
            failures = ET.parse(root / "out/junit.xml").findall(".//failure")
            self.assertTrue(all(not item["certified"] for item in snapshots[:-1]))
            self.assertNotIn("private-test-key", json.dumps(document))
            self.assertNotIn("secret-cleanup-detail", json.dumps(document))
            return code, document, len(failures), gateway.close.call_count

    def test_success_requires_completed_finalization(self):
        code, result, failures, calls = self.exercise(None)
        self.assertEqual((code, failures, calls), (0, 0, 1))
        self.assertTrue(result["certified"])
        self.assertEqual(result["results"][-1]["status"], "PASS")

    def test_cleanup_failure_stays_failed_after_successful_retry(self):
        code, result, failures, calls = self.exercise([OSError("secret-cleanup-detail"), None])
        self.assertEqual((code, failures, calls), (1, 1, 2))
        self.assertFalse(result["certified"])
        self.assertFalse(result["lane_passed"])
        self.assertEqual(result["results"][0]["status"], "PASS")
        self.assertEqual(result["results"][-1]["reason"], "infrastructure_OSError")

    def test_repeated_cleanup_failure_still_writes_final_artifacts(self):
        code, result, failures, calls = self.exercise(OSError("secret-cleanup-detail"))
        self.assertEqual((code, failures, calls), (1, 1, 2))
        self.assertFalse(result["certified"])
        self.assertEqual(result["results"][-1]["reason"], "gateway_cleanup_OSError")

    def test_active_or_unknown_requests_and_live_listener_cannot_finalize(self):
        for active, listener in ((1, False), (None, False), (0, True)):
            with self.subTest(active=active, listener=listener):
                code, result, failures, _ = self.exercise(None, active=active, listener_alive=listener)
                self.assertEqual((code, failures), (1, 1))
                self.assertFalse(result["certified"])
                self.assertEqual(result["results"][-1]["status"], "FAIL")

    def test_drain_precedes_final_accounting(self):
        code, result, failures, calls = self.exercise(None, active=1, drain=True)
        self.assertEqual((code, failures, calls), (0, 0, 1))
        self.assertTrue(result["gateway_drained"])
        self.assertEqual(result["inference_request_count"], 1)
        self.assertEqual(result["admission_attempt_count"], 1)

    def test_drain_deadline_expires_when_activity_never_reaches_zero(self):
        gateway = Mock(active=1, closed=True, lock=threading.Condition())
        gateway.thread.is_alive.return_value = False
        meta = {}
        with patch("run.time.monotonic", side_effect=[0, 0, 6]), \
                patch.object(gateway.lock, "wait") as wait:
            with self.assertRaises(TimeoutError):
                run.finalize_gateway(gateway, meta, timeout=5)
        wait.assert_called_once_with(5)
        self.assertFalse(meta["gateway_drained"])
        self.assertNotIn("inference_request_count", meta)

    def test_late_zero_activity_cannot_bypass_original_deadline(self):
        for late_step in ("close", "wait"):
            with self.subTest(late_step=late_step):
                clock = [0]
                gateway = Mock(active=0 if late_step == "close" else 1,
                               closed=True, lock=threading.Condition())
                gateway.thread.is_alive.return_value = False

                def finish_late(*args):
                    clock[0] = 6
                    gateway.active = 0

                gateway.close.side_effect = finish_late if late_step == "close" else None
                meta = {}
                with patch("run.time.monotonic", side_effect=lambda: clock[0]), \
                        patch.object(gateway.lock, "wait", side_effect=finish_late):
                    with self.assertRaises(TimeoutError):
                        run.finalize_gateway(gateway, meta, timeout=5)
                self.assertFalse(meta["gateway_drained"])
                self.assertNotIn("inference_request_count", meta)

    def test_final_report_faults_cannot_supply_certifying_json(self):
        for artifact in ("junit", "markdown", "json"):
            with self.subTest(artifact=artifact):
                code, result, _, calls = self.exercise(None, artifact)
                self.assertEqual((code, calls), (1, 1))
                self.assertFalse(result["certified"])
                self.assertEqual(result["results"][-1]["id"], "report-publication")
                self.assertEqual(result["results"][-1]["status"], "NOT_RUN")
                # Supply otherwise valid independent expectations for both OSes.
                shared = {key: result[key] for key in ("inventory_sha256", "resolved_inventory_sha256",
                          "config_sha256", "catalog_sha256", "commit")}
                shared["commit"] = "a" * 40
                versions = {name: "test" for name in ("claude", "codex", "opencode")}
                reports = [{**result, **shared, "os": os_name, "hrx_sha256": "b" * 64,
                            "native_versions": versions} for os_name in ("linux", "darwin")]
                expected = {**shared, "required_ids": ["mixed-pitput-conversation"],
                            "platforms": {name: {"hrx_sha256": "b" * 64, "native_versions": versions}
                                          for name in ("linux", "darwin")}}
                verdict = validate_release(reports, expected)
                self.assertFalse(verdict["certified"])
                self.assertTrue(all("required_not_pass:report-publication" in error
                                    for error in verdict["errors"]), verdict)
                upgrade = compare_upgrade(reports[0], reports[0], expected["required_ids"])
                self.assertFalse(upgrade["promotable"])
                self.assertTrue(all("required_not_pass:report-publication" in error
                                    for error in upgrade["errors"]), upgrade)


if __name__ == "__main__":
    unittest.main()
