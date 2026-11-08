"""One aggregate daily cap across lanes; the gateway key budget stays authoritative."""

import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import run
from run import Blocked, policy_from_file, verify_key

BASE = {"daily_budget_usd": 25, "max_requests": 40, "wall_seconds": 3600, "cell_seconds": 600,
        "request_seconds": 120, "max_request_bytes": 4_000_000, "max_output_tokens": 4096,
        "concurrency": 2, "routes": ["route-a"]}


def policy(**changes):
    with tempfile.TemporaryDirectory() as temp:
        path = Path(temp) / "policy.json"
        path.write_text(json.dumps({**BASE, **changes}))
        return policy_from_file(path)


def key_info(**changes):
    info = {"max_budget": 25, "budget_duration": "1d", "spend": 3.5, "blocked": False,
            "metadata": {"purpose": "hrx-compatibility-ci"}, "models": ["route-a"], **changes}

    class Opener:
        def open(self, request, timeout):
            return io.BytesIO(json.dumps({"info": info}).encode())

    return patch.object(run.urllib.request, "build_opener", return_value=Opener())


class BudgetGuardTests(unittest.TestCase):
    def test_per_lane_budgets_are_rejected(self):
        for extra in ({"canary_budget_usd": 5}, {"nightly_daily_budget_usd": 25},
                      {"lanes": {"canary": {"daily_budget_usd": 25}}}):
            with self.subTest(extra=extra), self.assertRaisesRegex(
                    Blocked, "per_lane_budget_forbidden_single_aggregate_cap"):
                policy(**extra)

    def test_start_reserve_must_fit_inside_the_aggregate_cap(self):
        self.assertEqual(policy(start_reserve_usd=4)["start_reserve_usd"], 4)
        for value in (-1, 25, 30, True, float("nan")):
            with self.subTest(value=value), self.assertRaisesRegex(Blocked, "start_reserve"):
                policy(start_reserve_usd=value)

    def test_budget_evidence_is_recorded_and_never_certifies_cost(self):
        with key_info():
            evidence = verify_key("https://gateway.example", "k", policy())
        self.assertEqual(evidence["aggregate_daily_cap_usd"], 25)
        self.assertEqual(evidence["remaining_at_start_usd"], 21.5)
        self.assertEqual(evidence["enforcement"], "gateway_dedicated_key_budget_authoritative")
        self.assertTrue(evidence["cost_bound"].startswith("not_certified"))

    def test_missing_spend_is_unverifiable_not_zero(self):
        info_without_spend = {"max_budget": 25, "budget_duration": "1d", "blocked": False,
                              "metadata": {"purpose": "hrx-compatibility-ci"}, "models": ["route-a"]}

        class Opener:
            def open(self, request, timeout):
                return io.BytesIO(json.dumps({"info": info_without_spend}).encode())

        with patch.object(run.urllib.request, "build_opener", return_value=Opener()), \
                self.assertRaisesRegex(Blocked, "budget_unverifiable"):
            verify_key("https://gateway.example", "k", policy())

    def test_insufficient_remaining_budget_blocks_before_any_run(self):
        with key_info(spend=22), self.assertRaisesRegex(Blocked, "gateway_budget_reserve_insufficient"):
            verify_key("https://gateway.example", "k", policy(start_reserve_usd=4))
        with key_info(spend=21):
            verify_key("https://gateway.example", "k", policy(start_reserve_usd=4))

    def test_key_cap_above_the_aggregate_or_bad_spend_is_blocked(self):
        with key_info(max_budget=50), self.assertRaisesRegex(Blocked, "daily_budget_missing_or_excessive"):
            verify_key("https://gateway.example", "k", policy())
        for spend in ("3", float("nan"), True, -1, None):
            with self.subTest(spend=spend), key_info(spend=spend), \
                    self.assertRaisesRegex(Blocked, "budget_unverifiable"):
                verify_key("https://gateway.example", "k", policy())
        with key_info(spend=25), self.assertRaisesRegex(Blocked, "exhausted_or_key_blocked"):
            verify_key("https://gateway.example", "k", policy())


if __name__ == "__main__":
    unittest.main()
