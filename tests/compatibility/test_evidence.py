import copy
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
from contextlib import ExitStack
import report_io
import xml.etree.ElementTree as ET

from evidence import main


class EvidenceCLITests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "output"
        self.inventory = {"cells": [{"id": "model", "required": True}, {"id": "target", "required": True}],
                          "resume_pairs": [{"id": "cross-resume-fixture", "source": "model", "target": "target"}],
                          "required_scenarios": ["mixed-pitput-conversation", "broker", "pty"]}
        self.inventory_path = self.write("inventory.json", self.inventory)
        self.expected = {"inventory_sha256": hashlib.sha256(self.inventory_path.read_bytes()).hexdigest(),
                         "resolved_inventory_sha256": "1" * 64, "catalog_sha256": "2" * 64,
                         "config_sha256": "3" * 64, "commit": "4" * 40,
                         "required_ids": ["model", "target", "cross-resume-fixture", "mixed-pitput-conversation", "broker", "pty"],
                         "platforms": {os: {"hrx_sha256": char * 64,
                                            "native_versions": {"claude": "1", "codex": "2", "opencode": "3"}}
                                       for os, char in (("linux", "5"), ("darwin", "6"))}}
        self.reports = [{**{k: self.expected[k] for k in ("inventory_sha256", "resolved_inventory_sha256",
                                                        "catalog_sha256", "config_sha256", "commit")},
                         **copy.deepcopy(target), "os": os, "schema_version": 1, "lane": "release",
                         "timestamp": datetime.now(timezone.utc).isoformat(),
                         "results": [{"id": name, "required": True, "status": "PASS"}
                                     for name in self.expected["required_ids"]]}
                        for os, target in self.expected["platforms"].items()]

    def write(self, name, data):
        path = self.root / name
        path.write_text(json.dumps(data))
        return path

    def release_args(self):
        return ["release", "--expected", str(self.write("expected.json", self.expected)),
                "--report", str(self.write("linux.json", self.reports[0])),
                "--report", str(self.write("darwin.json", self.reports[1])), "--output", str(self.output)]

    def upgrade_args(self, candidate=None):
        candidate = candidate or copy.deepcopy(self.reports[0])
        candidate["native_versions"]["codex"] = "new"
        return ["upgrade", "--inventory", str(self.inventory_path),
                "--baseline", str(self.write("baseline.json", self.reports[0])),
                "--candidate", str(self.write("candidate.json", candidate)), "--output", str(self.output)]

    def decision(self):
        return json.loads((self.output / "decision.json").read_text())

    def test_real_cli_exit_and_junit_release_pass(self):
        process = subprocess.run([sys.executable, str(Path(__file__).with_name("evidence.py")), *self.release_args()],
                                 capture_output=True, text=True)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertTrue(self.decision()["certified"])
        self.assertEqual(ET.parse(self.output / "junit.xml").getroot().get("failures"), "0")
        self.assertIn("PASS", (self.output / "decision.md").read_text())

    def test_publication_failures_never_leave_positive_decisions(self):
        for mode in ("release", "upgrade"):
            for fault in ("junit", "markdown", "rename"):
                with self.subTest(mode=mode, fault=fault), ExitStack() as stack:
                    args = self.release_args() if mode == "release" else self.upgrade_args()
                    # Begin with a positive prior decision to exercise invalidation.
                    self.assertEqual(main(args), 0)
                    if fault == "junit":
                        stack.enter_context(patch.object(ET.ElementTree, "write", side_effect=OSError("private-error")))
                    elif fault == "markdown":
                        original = Path.write_text

                        def write(path, *args, **kwargs):
                            if path.name == "decision.md":
                                raise OSError("private-error")
                            return original(path, *args, **kwargs)

                        stack.enter_context(patch.object(Path, "write_text", write))
                    else:
                        original_replace = report_io.os.replace
                        calls = 0

                        def replace(source, target):
                            nonlocal calls
                            calls += 1
                            if calls == 2:
                                raise OSError("private-error")
                            return original_replace(source, target)

                        stack.enter_context(patch.object(report_io.os, "replace", side_effect=replace))
                    self.assertEqual(main(args), 1)
                    result = self.decision()
                    self.assertFalse(result["passed"])
                    self.assertFalse(result["certified" if mode == "release" else "promotable"])
                    self.assertFalse(result["publication_complete"])
                    self.assertNotIn("private-error", json.dumps(result))

    def test_missing_platform_is_nonzero_with_artifacts(self):
        args = self.release_args()
        del args[5:7]
        self.assertEqual(main(args), 1)
        self.assertFalse(self.decision()["certified"])
        self.assertIsNotNone(ET.parse(self.output / "junit.xml").find("testcase/failure"))

    def test_expected_manifest_not_inferred_from_reports(self):
        self.reports[0]["hrx_sha256"] = "7" * 64
        self.assertEqual(main(self.release_args()), 1)
        self.assertFalse(self.decision()["certified"])

    def test_resolution_change_is_not_same_release(self):
        self.reports[1]["resolved_inventory_sha256"] = "8" * 64
        self.assertEqual(main(self.release_args()), 1)

    def test_resolution_hash_required_even_when_both_missing(self):
        del self.expected["resolved_inventory_sha256"]
        for report in self.reports:
            del report["resolved_inventory_sha256"]
        self.assertEqual(main(self.release_args()), 1)

    def test_old_and_future_reports_rejected(self):
        for delta in (-90000, 60):
            self.reports[0]["timestamp"] = (datetime.now(timezone.utc) + timedelta(seconds=delta)).isoformat()
            self.assertEqual(main(self.release_args()), 1)

    def test_required_blocked_and_skipped_fail(self):
        for changes in ({"status": "BLOCKED"}, {"selected": False, "status": "PASS"}):
            self.reports[0]["results"][0].update(changes)
            self.assertEqual(main(self.release_args()), 1)

    def test_arbitrary_ids_errors_and_transcripts_not_published(self):
        secret = "FAKE_SECRET_DO_NOT_PUBLISH_12345"
        self.reports[0]["results"].append({"id": secret, "required": True, "status": "FAIL",
                                           "reason": secret, "transcript": secret})
        self.reports[0]["credentials"] = secret
        self.assertEqual(main(self.release_args()), 1)
        for file in self.output.iterdir():
            self.assertNotIn(secret, file.read_text())
        self.assertIn("required_not_pass", (self.output / "decision.json").read_text())

    def test_malformed_and_missing_input_fail_without_exception_text(self):
        args = self.release_args()
        (self.root / "linux.json").write_text('NOT_JSON_FAKE_SECRET')
        self.assertEqual(main(args), 1)
        self.assertEqual(self.decision()["errors"], [{"code": "invalid_input"}])
        (self.root / "linux.json").unlink()
        self.assertEqual(main(args), 1)
        self.assertEqual(self.decision()["errors"], [{"code": "input_unavailable"}])

    def test_upgrade_latest_version_success(self):
        self.assertEqual(main(self.upgrade_args()), 0)
        self.assertTrue(self.decision()["promotable"])
        self.assertNotIn("certified", self.decision())

    def test_upgrade_same_wrong_inventory_fails(self):
        self.reports[0]["inventory_sha256"] = "9" * 64
        self.assertEqual(main(self.upgrade_args()), 1)
        self.assertIn("trusted_inventory_mismatch", (self.output / "decision.json").read_text())

    def test_cross_model_requirement_cannot_disappear_from_both_reports(self):
        self.reports[0]["results"] = [r for r in self.reports[0]["results"] if r["id"] != "cross-resume-fixture"]
        self.assertEqual(main(self.upgrade_args()), 1)
        self.assertFalse(self.decision()["passed"])

    def test_upgrade_candidate_failure_and_staleness(self):
        candidate = copy.deepcopy(self.reports[0])
        candidate["results"][0]["status"] = "FAIL"
        self.assertEqual(main(self.upgrade_args(candidate)), 1)
        candidate = copy.deepcopy(self.reports[0])
        candidate["timestamp"] = (datetime.now(timezone.utc) - timedelta(days=2)).isoformat()
        self.assertEqual(main(self.upgrade_args(candidate)), 1)

    def test_upgrade_resolved_inventory_mismatch(self):
        candidate = copy.deepcopy(self.reports[0])
        candidate["resolved_inventory_sha256"] = "9" * 64
        self.assertEqual(main(self.upgrade_args(candidate)), 1)

    def test_upgrade_required_scenario_cannot_be_omitted(self):
        candidate = copy.deepcopy(self.reports[0])
        candidate["results"].pop()
        self.assertEqual(main(self.upgrade_args(candidate)), 1)

    def test_inventory_auto_broker_requirements_cannot_disappear_from_both_reports(self):
        self.inventory["scenarios"] = {
            "auto-parser": {"lanes": ["nightly", "release"]},
            "broker-glab-git": {"lanes": ["nightly", "release"]},
        }
        self.write("inventory.json", self.inventory)
        self.reports[0]["inventory_sha256"] = hashlib.sha256(self.inventory_path.read_bytes()).hexdigest()
        # Matching model results and hashes are insufficient when both artifacts
        # omit the required compositions from the trusted inventory.
        self.assertEqual(main(self.upgrade_args()), 1)
        for name in self.inventory["scenarios"]:
            self.reports[0]["results"].append({"id": name, "required": True, "status": "PASS"})
        self.assertEqual(main(self.upgrade_args()), 0)

    def test_invalid_age_cannot_disable_freshness(self):
        for age in ("nan", "inf", "0", "-1"):
            self.assertEqual(main(self.release_args() + ["--max-age-seconds", age]), 1)
            self.assertEqual(self.decision()["errors"], [{"code": "invalid_freshness_policy"}])


if __name__ == "__main__":
    unittest.main()
