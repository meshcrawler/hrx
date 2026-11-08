import copy
import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import Mock

from cross_resume import resume_rows, run_resumes
from run import Runner, report


class CrossResumeTests(unittest.TestCase):
    def setUp(self):
        self.cells = [{"id": name, "harness": "codex", "canonical_model": name, "route": name,
                       "protocol": "responses", "reasoning": "low"} for name in ("source", "target")]
        self.inventory = {"cells": self.cells, "resume_pairs": [
            {"id": "cross-resume-codex", "source": "source", "target": "target"}]}
        self.gateway = Mock(allowed_routes={"prior"}, deadline=time.monotonic() + 60)
        self.runner = Runner("hrx", self.gateway, {"routes": ["source", "target"]})

    def execute(self, capability=lambda catalog, cell: True, versions=None):
        rows = resume_rows(self.inventory, "release")
        with tempfile.TemporaryDirectory() as temp:
            run_resumes(rows, self.runner, self.cells, {}, Path(temp), capability,
                        {"codex": "fixture"} if versions is None else versions)
        return rows[0]

    def fake_launch(self, *, wrong_session=False, wrong_route=False, stale_tool=False):
        calls, nonce = [], []
        def launch(cell, home, project, prompt, resume=None):
            calls.append((cell["id"], resume, set(self.gateway.allowed_routes)))
            if resume:
                self.assertFalse((project / "challenge.txt").exists())
                self.assertFalse((project / "result.txt").exists())
                self.assertNotIn(nonce[0], prompt)
                (project / "resumed.txt").write_text(nonce[0])
            else:
                nonce.append((project / "challenge.txt").read_text())
                (project / "result.txt").write_text(nonce[0][::-1])
            return {"status": "PASS", "cleanup": "clean", "session_id": "other" if resume and wrong_session else "session",
                    "tool_calls": 1, "tool_results": 1, "exact_route_protocol": not (resume and wrong_route),
                    "fresh_tool_result_consumed": not stale_tool}
        self.runner.launch = launch
        return calls

    def test_resume_changes_route_keeps_session_and_restores_admission(self):
        calls = self.fake_launch()
        result = self.execute()
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(calls, [("source", None, {"source"}), ("target", "session", {"target"})])
        self.assertEqual(self.gateway.allowed_routes, {"prior"})

    def test_wrong_session_source_route_or_stale_tool_result_cannot_pass(self):
        for option in ("wrong_session", "wrong_route", "stale_tool"):
            with self.subTest(option=option):
                self.fake_launch(**{option: True})
                self.assertEqual(self.execute()["status"], "FAIL")
                self.assertEqual(self.gateway.allowed_routes, {"prior"})

    def test_missing_support_policy_binary_or_generation_blocks_launch(self):
        self.runner.cell = Mock(side_effect=AssertionError("must not launch"))
        self.assertEqual(self.execute(capability=lambda c, cell: cell["id"] == "source")["status"], "UNSUPPORTED")
        self.assertEqual(self.execute(versions={})["reason"], "native_binary_missing")
        self.runner.policy["routes"] = ["source"]
        self.assertEqual(self.execute()["reason"], "route_not_authorized_by_budget_policy")
        self.cells[1].update(resolution_status="BLOCKED", resolution_reason="latest_unavailable")
        self.assertEqual(self.execute()["reason"], "latest_unavailable")

    def test_same_model_or_cross_harness_is_not_model_change_evidence(self):
        self.runner.cell = Mock(side_effect=AssertionError("must not launch"))
        self.cells[1]["canonical_model"] = "source"
        self.assertEqual(self.execute()["status"], "BLOCKED")
        self.cells[1].update(canonical_model="target", harness="claude")
        self.assertEqual(self.execute()["status"], "BLOCKED")

    def test_omissions_remain_required_not_run_and_cannot_certify(self):
        for lane, diagnostic in (("canary", None), ("release", ["source"])):
            rows = resume_rows(self.inventory, lane, diagnostic)
            self.assertEqual(rows[0]["status"], "NOT_RUN")
            self.assertFalse(rows[0]["selected"])
            with tempfile.TemporaryDirectory() as temp:
                report(Path(temp), [{"id": "other", "status": "PASS"}] + rows,
                       {"lane": "release", "timestamp": "fixture"})
                self.assertFalse(json.loads((Path(temp) / "results.json").read_text())["certified"])

    def test_missing_pairs_and_duplicate_or_unsafe_ids_fail_closed(self):
        self.assertEqual(resume_rows({"cells": self.cells}, "release")[0]["status"], "BLOCKED")
        bad = copy.deepcopy(self.inventory)
        bad["resume_pairs"] *= 2
        with self.assertRaises(ValueError):
            resume_rows(bad, "release")
        bad["resume_pairs"] = [{"id": "cross-resume-../../escape", "source": "source", "target": "target"}]
        with self.assertRaises(ValueError):
            resume_rows(bad, "release")

    def test_deadline_preserves_not_run(self):
        self.gateway.deadline = 0
        self.runner.cell = Mock(side_effect=AssertionError("must not launch"))
        self.assertEqual(self.execute()["reason"], "aggregate_deadline")

    def test_lost_cleanup_proof_stops_further_pairs_and_restores_admission(self):
        self.runner.cell = Mock(return_value={"status": "FAIL", "cleanup": "unknown"})
        with self.assertRaisesRegex(RuntimeError, "native_cleanup_unverified"):
            self.execute()
        self.assertEqual(self.gateway.allowed_routes, {"prior"})


if __name__ == "__main__":
    unittest.main()
