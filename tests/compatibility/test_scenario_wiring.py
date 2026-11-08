"""Deterministic wiring of the required live scenarios into run.py lanes (no inference)."""

import json
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from run import parser_route, report, run_scenarios, scenario_rows

INVENTORY = json.loads((Path(__file__).parent / "required.yaml").read_text())
CELLS = {cell["id"]: cell for cell in INVENTORY["cells"]}


class FakeGateway:
    def __init__(self, deadline=None):
        self.deadline = deadline if deadline is not None else time.monotonic() + 60
        self.allowed_routes = {"previous-route"}


class FakeRunner:
    def __init__(self, gateway, hrx="/nonexistent/hrx", routes=("inkling-route", "r")):
        self.gateway, self.hrx = gateway, hrx
        self.policy = {"routes": list(routes)}


def catalog(*sources):
    return {"models": [{"canonical_id": "inkling", "sources": list(sources)}]}


def chat_source(route, available=True):
    return {"gateway_route": route, "available": available,
            "details": {"harnesses": {"opencode": {"wire_api": "chat_completions"}}}}


class ScenarioWiringTests(unittest.TestCase):
    def test_inventory_declares_both_required_scenarios_against_real_cells(self):
        scenarios = INVENTORY["scenarios"]
        self.assertEqual(set(scenarios), {"auto-parser", "broker-glab-git"})
        self.assertIn(scenarios["auto-parser"]["launch_cell"], CELLS)
        self.assertIn(scenarios["broker-glab-git"]["cell"], CELLS)

    def test_rows_are_always_present_and_selected_only_by_lane(self):
        for lane, selected in (("canary", False), ("nightly", True), ("release", True)):
            rows = scenario_rows(INVENTORY, lane, None)
            self.assertEqual([row["id"] for row in rows], ["auto-parser", "broker-glab-git"])
            self.assertTrue(all(row["required"] and row["status"] == "NOT_RUN" for row in rows))
            self.assertEqual({row["selected"] for row in rows}, {selected}, lane)
        # Diagnostic --cell runs never claim scenario evidence.
        self.assertFalse(any(row["selected"] for row in scenario_rows(INVENTORY, "release", ["codex-glm"])))

    def test_parser_route_is_scoped_to_authorized_routes_without_fallback(self):
        two_payers = catalog(chat_source("payer-a"), chat_source("payer-b"))
        # Two public candidates, exactly one authorized: that one, never "the first".
        self.assertEqual(parser_route(two_payers, "inkling", ["payer-b"]), "payer-b")
        self.assertEqual(parser_route(two_payers, "inkling", ["payer-a", "other"]), "payer-a")
        # Zero or two authorized candidates fail closed.
        self.assertIsNone(parser_route(two_payers, "inkling", ["other"]))
        self.assertIsNone(parser_route(two_payers, "inkling", ["payer-a", "payer-b"]))
        self.assertIsNone(parser_route(catalog(), "inkling", ["payer-a"]))
        self.assertIsNone(parser_route(catalog(chat_source("a", available=False)), "inkling", ["a"]))
        responses = {"gateway_route": "r", "available": True,
                     "details": {"harnesses": {"opencode": {"wire_api": "responses"}}}}
        self.assertIsNone(parser_route(catalog(responses), "inkling", ["r"]))

    def test_missing_prerequisites_are_blocked_not_passed(self):
        rows = scenario_rows(INVENTORY, "nightly", None)
        gateway = FakeGateway()
        with tempfile.TemporaryDirectory() as temp, patch.dict("os.environ", {}, clear=False):
            import os
            os.environ.pop("HRX_BROKER_SCENARIO_FIXTURE", None)
            run_scenarios(rows, FakeRunner(gateway), INVENTORY, INVENTORY["cells"],
                          catalog(chat_source("inkling-route")), Path(temp))
        by_id = {row["id"]: row for row in rows}
        self.assertEqual((by_id["auto-parser"]["status"], by_id["auto-parser"]["reason"]),
                         ("BLOCKED", "hrx_artifact_missing"))
        self.assertEqual((by_id["broker-glab-git"]["status"], by_id["broker-glab-git"]["reason"]),
                         ("BLOCKED", "broker_fixture_config_missing"))
        self.assertEqual(gateway.allowed_routes, {"previous-route"}, "relay scope restored")
        with tempfile.TemporaryDirectory() as temp:
            self.assertFalse(report(Path(temp), [{"id": "cell", "status": "PASS"}, *rows],
                                    {"timestamp": "now", "lane": "nightly"}))

    def test_unresolvable_parser_route_and_expired_deadline(self):
        rows = scenario_rows(INVENTORY, "release", None)
        with tempfile.TemporaryDirectory() as temp:
            run_scenarios(rows[:1], FakeRunner(FakeGateway()), INVENTORY, INVENTORY["cells"],
                          catalog(), Path(temp))
            self.assertEqual(rows[0]["reason"], "auto_parser_route_unauthorized_unavailable_or_ambiguous")
            late = scenario_rows(INVENTORY, "release", None)
            run_scenarios(late, FakeRunner(FakeGateway(deadline=time.monotonic() - 1)), INVENTORY,
                          INVENTORY["cells"], catalog(chat_source("r")), Path(temp))
            self.assertEqual({(row["status"], row["reason"]) for row in late},
                             {("NOT_RUN", "aggregate_deadline")})

    def test_scenario_evidence_keeps_stable_row_ids(self):
        rows = scenario_rows(INVENTORY, "nightly", None)
        gateway = FakeGateway()
        seen = {}

        def broker(runner, cell, root):
            seen["broker_routes"] = set(runner.gateway.allowed_routes)
            return {"id": "broker-glab-git-claude", "status": "PASS", "cleanup_verified": True}

        with tempfile.TemporaryDirectory() as temp, \
                patch("auto_scenario.auto_scenario", return_value={"id": "auto-x", "status": "PASS"}), \
                patch("broker_scenario.broker_scenario", side_effect=broker):
            run_scenarios(rows, FakeRunner(gateway), INVENTORY, INVENTORY["cells"],
                          catalog(chat_source("inkling-route")), Path(temp))
        self.assertEqual([(row["id"], row["status"]) for row in rows],
                         [("auto-parser", "PASS"), ("broker-glab-git", "PASS")])
        self.assertEqual(rows[1]["scenario_evidence_id"], "broker-glab-git-claude")
        self.assertEqual(seen["broker_routes"], {CELLS["claude-sonnet"]["route"]})
        self.assertEqual(gateway.allowed_routes, {"previous-route"})


if __name__ == "__main__":
    unittest.main()
