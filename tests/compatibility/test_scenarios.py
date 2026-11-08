import copy
from datetime import datetime, timedelta, timezone
import unittest

from scenarios import (compare_upgrade, resolve_latest_families,
                       select_exploratory_opencode, validate_release)


NOW = datetime(2026, 10, 22, 12, tzinfo=timezone.utc)


def source(route="payer/model", **overrides):
    return {"gateway_route": route, "available": True, "provider": "fireworks", "account": "contoso",
            "details": {"harnesses": {"opencode": {"wire_api": "chat_completions"},
                                       "claude": {"wire_api": "messages"},
                                       "codex": {"wire_api": "responses"}}}, **overrides}


def model(name, route=None, **overrides):
    return {"canonical_id": name, "sources": [source(route or "payer/" + name, **overrides)]}


def fixture():
    versions = {"claude": "1", "codex": "2", "opencode": "3"}
    expected = {"inventory_sha256": "a" * 64, "resolved_inventory_sha256": "0" * 64, "catalog_sha256": "b" * 64,
                "config_sha256": "c" * 64, "commit": "d" * 40,
                "required_ids": ["tool", "resume", "mixed-pitput-conversation", "broker", "pty"],
                "platforms": {os: {"hrx_sha256": char * 64, "native_versions": versions.copy()}
                              for os, char in (("linux", "e"), ("darwin", "f"))}}
    reports = [{**{key: expected[key] for key in ("inventory_sha256", "resolved_inventory_sha256", "catalog_sha256", "config_sha256", "commit")},
                **target, "os": os, "timestamp": NOW.isoformat(), "schema_version": 1, "lane": "release",
                "results": [{"id": name, "required": True, "status": "PASS"} for name in expected["required_ids"]]}
               for os, target in expected["platforms"].items()]
    return expected, reports


class ReleaseTests(unittest.TestCase):
    def test_complete_cross_platform_evidence(self):
        expected, reports = fixture()
        self.assertTrue(validate_release(reports, expected, now=NOW)["certified"])

    def test_fails_every_nonpass_required_even_if_marked_optional(self):
        for status in ("FAIL", "BLOCKED", "UNSUPPORTED", "NOT_RUN", "SKIP", None):
            with self.subTest(status=status):
                expected, reports = fixture()
                reports[0]["results"][0].update(status=status, required=False)
                self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_unselected_passing_result_is_not_evidence(self):
        expected, reports = fixture()
        reports[0]["results"][0]["selected"] = False
        self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_missing_and_duplicate_results(self):
        for duplicate in (False, True):
            expected, reports = fixture()
            if duplicate:
                reports[0]["results"].append(reports[0]["results"][0].copy())
            else:
                reports[0]["results"].pop()
            self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_additional_required_failure_cannot_hide(self):
        expected, reports = fixture()
        reports[0]["results"].append({"id": "new-scenario", "status": "FAIL"})
        self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_cross_platform_placeholders_resolved_only_with_actual_os(self):
        expected, reports = fixture()
        reports[0]["results"].append({"id": "release-os-darwin", "status": "NOT_RUN",
                                      "reason": "separate_native_runner_evidence_required"})
        self.assertTrue(validate_release(reports, expected, now=NOW)["certified"])
        self.assertFalse(validate_release(reports[:1], expected, now=NOW)["certified"])

    def test_platform_named_failure_is_not_ignored(self):
        expected, reports = fixture()
        reports[0]["results"].append({"id": "release-os-darwin", "status": "FAIL"})
        self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_cannot_remove_darwin_requirement(self):
        expected, reports = fixture()
        del expected["platforms"]["darwin"]
        self.assertFalse(validate_release(reports[:1], expected, now=NOW)["certified"])

    def test_duplicate_os_cannot_hide_failed_attempt(self):
        expected, reports = fixture()
        reports.append(copy.deepcopy(reports[0]))
        reports[-1]["results"][0]["status"] = "FAIL"
        self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_exact_hash_versions_and_release_lane_required(self):
        for key in ("inventory_sha256", "catalog_sha256", "config_sha256", "commit", "hrx_sha256",
                    "native_versions", "lane", "schema_version"):
            with self.subTest(key=key):
                expected, reports = fixture()
                reports[0][key] = "different"
                self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_age_future_and_naive_time_rejected(self):
        for stamp in ((NOW - timedelta(days=2)).isoformat(), (NOW + timedelta(seconds=1)).isoformat(),
                      NOW.replace(tzinfo=None).isoformat(), "bad"):
            expected, reports = fixture()
            reports[0]["timestamp"] = stamp
            self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])

    def test_missing_manifest_cannot_match_missing_report(self):
        expected, reports = fixture()
        del expected["config_sha256"]
        for report in reports:
            del report["config_sha256"]
        self.assertFalse(validate_release(reports, expected, now=NOW)["certified"])


class UpgradeTests(unittest.TestCase):
    def test_only_native_version_may_change(self):
        expected, reports = fixture()
        before = reports[0]
        after = copy.deepcopy(before)
        after["native_versions"]["codex"] = "candidate"
        self.assertTrue(compare_upgrade(before, after, expected["required_ids"])["promotable"])
        after["catalog_sha256"] = "different"
        self.assertFalse(compare_upgrade(before, after, expected["required_ids"])["promotable"])

    def test_candidate_missing_or_failed_reports_regression(self):
        for status in (None, "FAIL", "BLOCKED", "NOT_RUN", "UNSUPPORTED"):
            expected, reports = fixture()
            before = reports[0]
            after = copy.deepcopy(before)
            if status is None:
                after["results"].pop(0)
            else:
                after["results"][0]["status"] = status
            result = compare_upgrade(before, after, expected["required_ids"])
            self.assertFalse(result["promotable"])
            self.assertIn("tool", result["regressions"])

    def test_failed_baseline_inconclusive_not_promotion(self):
        expected, reports = fixture()
        before = reports[0]
        after = copy.deepcopy(before)
        before["results"][0]["status"] = "FAIL"
        self.assertFalse(compare_upgrade(before, after, expected["required_ids"])["promotable"])


class DiscoveryTests(unittest.TestCase):
    def test_only_exact_authorized_declared_routes(self):
        catalog = {"models": [model("one"), model("two"), model("three", details={})]}
        result = select_exploratory_opencode(catalog, [], ["payer/one", "payer/three", "missing"])
        self.assertEqual([c["route"] for c in result["cells"]], ["payer/one"])
        self.assertFalse(result["cells"][0]["required"])
        self.assertEqual({r["reason"] for r in result["omitted"]},
                         {"not_authorized", "opencode_capability_absent", "absent_from_catalog"})

    def test_required_route_not_replaced_by_discovery(self):
        required = [{"harness": "opencode", "route": "payer/one", "required": True}]
        result = select_exploratory_opencode({"models": [model("one")]}, required, ["payer/one"])
        self.assertEqual(result["cells"], [])
        self.assertTrue(required[0]["required"])

    def test_ambiguous_and_unavailable_never_selected(self):
        result = select_exploratory_opencode({"models": [model("one"), model("one"), model("two", available=False)]},
                                            [], ["payer/one", "payer/two"])
        self.assertEqual(result["cells"], [])
        self.assertEqual({r["reason"] for r in result["omitted"]}, {"ambiguous_route", "unavailable"})


class LatestTests(unittest.TestCase):
    def resolve(self, family, models, **overrides):
        cell = {"id": family, "family": family, "selection": "latest", "required": True,
                "harness": "claude" if family.startswith("claude") else "codex",
                "protocol": "messages" if family.startswith("claude") else "responses",
                "route_policy": {"account": "contoso", "route_prefixes": ["payer/"]}, **overrides}
        return resolve_latest_families({"cells": [cell]}, {"models": models})["cells"][0]

    def test_all_seven_families_resolve_newest_numeric(self):
        for family in ("claude-haiku", "claude-sonnet", "claude-opus", "gpt-sol", "gpt-luna", "gpt-terra", "gpt-astra"):
            with self.subTest(family=family):
                names = ([family + "-5-9", family + "-5-10"] if family.startswith("claude") else
                         ["gpt-6.9-" + family[4:], "gpt-6.10-" + family[4:]])
                result = self.resolve(family, [model(n) for n in names])
                self.assertEqual(result["canonical_model"], names[-1])
                self.assertEqual(result["resolution_status"], "RESOLVED")

    def test_preview_and_date_not_newer_generations(self):
        result = self.resolve("claude-haiku", [model(n) for n in
                              ("claude-haiku-4-5-20251001", "claude-haiku-5-5", "claude-haiku-9-preview")])
        self.assertEqual(result["canonical_model"], "claude-haiku-5-5")

    def test_newest_unsupported_does_not_fallback(self):
        result = self.resolve("gpt-sol", [model("gpt-6.1-sol"), model("gpt-6.2-sol", details={})])
        self.assertEqual(result["canonical_model"], "gpt-6.2-sol")
        self.assertEqual(result["resolution_status"], "UNSUPPORTED")

    def test_newest_wrong_payer_does_not_fallback(self):
        result = self.resolve("gpt-sol", [model("gpt-6.1-sol"), model("gpt-6.2-sol", account="personal")])
        self.assertEqual(result["resolution_status"], "BLOCKED")
        self.assertEqual(result["canonical_model"], "gpt-6.2-sol")
        self.assertIsNone(result["route"])

    def test_unavailable_newest_uses_latest_available(self):
        result = self.resolve("gpt-sol", [model("gpt-6.1-sol"), model("gpt-6.2-sol", available=False)])
        self.assertEqual(result["canonical_model"], "gpt-6.1-sol")

    def test_upstream_discovery_without_gateway_route_is_not_deployed(self):
        for route in (None, ""):
            with self.subTest(route=route):
                upstream_only = model("claude-opus-5-5-2")
                upstream_only["sources"][0]["gateway_route"] = route
                result = self.resolve("claude-opus", [model("claude-opus-5-5"), upstream_only])
                self.assertEqual(result["canonical_model"], "claude-opus-5-5")
                self.assertEqual(result["resolution_status"], "RESOLVED")

    def test_missing_family_stays_required(self):
        result = self.resolve("gpt-luna", [model("gpt-6-astra")])
        self.assertTrue(result["required"])
        self.assertEqual(result["resolution_status"], "BLOCKED")

    def test_policy_must_be_explicit_and_ambiguity_blocked(self):
        for policy in ({}, {"route_prefixes": []}, {"route_prefixes": [""]}, {"unknown": "x"}):
            self.assertEqual(self.resolve("gpt-sol", [model("gpt-6.1-sol")], route_policy=policy)["resolution_status"], "BLOCKED")
        result = self.resolve("gpt-sol", [model("gpt-6.1-sol"), model("gpt-6.1-sol", route="payer/other")])
        self.assertEqual(result["resolution_reason"], "latest_route_ambiguous")

    def test_does_not_mutate_frozen_inventory(self):
        inventory = {"cells": [{"id": "old", "selection": "latest", "family": "gpt-sol",
                                "route": "old", "canonical_model": "old", "route_policy": {}}]}
        resolve_latest_families(inventory, {"models": []})
        self.assertEqual(inventory["cells"][0]["route"], "old")


if __name__ == "__main__":
    unittest.main()
