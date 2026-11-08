#!/usr/bin/env python3
"""Aggregate trusted release expectations or compare pinned/candidate evidence.

Scheduled CI examples (download named job artifacts into distinct directories):

  python3 tests/compatibility/evidence.py release \
    --expected "$HRX_CI_RELEASE_MANIFEST" --max-age-seconds 86400 \
    --report artifacts/linux/results.json --report artifacts/darwin/results.json \
    --output artifacts/release-decision

  python3 tests/compatibility/evidence.py upgrade \
    --inventory tests/compatibility/required.yaml --max-age-seconds 86400 \
    --baseline artifacts/pinned/results.json --candidate artifacts/latest/results.json \
    --output artifacts/upgrade-decision

Publish OUTPUT/junit.xml as GitLab reports:junit and OUTPUT/ as artifacts:paths,
with artifacts:when:always. These gates return 1 for incomplete evidence. The
release manifest must come from the trusted build/planning job, independently
of the reports, and bind both platform binaries, versions and frozen catalog /
resolved inventory. Never generate it by copying expectations from test reports.
Upgrade comparison is not a substitute for cross-platform release certification.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import xml.etree.ElementTree as ET

from scenarios import compare_upgrade, validate_release
from report_io import atomic_json
from cross_resume import resume_rows


MAX_INPUT_BYTES = 2 * 1024 * 1024
ERROR_CODES = {
    "invalid_freshness_policy", "linux_and_darwin_required", "invalid_required_inventory",
    "unexpected_or_duplicate_platform", "invalid_expected_artifact", "expected_native_versions_missing",
    "mismatch", "not_release_evidence", "stale_or_invalid_timestamp", "duplicate_or_missing_result_id",
    "required_not_pass", "missing_or_nonpassing", "missing_platform", "required_inventory_empty",
    "comparison_mismatch", "native_versions_missing", "invalid_input", "input_unavailable",
    "trusted_inventory_mismatch", "candidate_regression",
} | {"invalid_expected_" + key for key in ("inventory_sha256", "resolved_inventory_sha256",
                                          "config_sha256", "catalog_sha256", "commit")}


def read_object(path):
    with Path(path).open("rb") as stream:
        raw = stream.read(MAX_INPUT_BYTES + 1)
    if len(raw) > MAX_INPUT_BYTES:
        raise ValueError("oversize")
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("object_required")
    return value


def sanitized_errors(errors):
    """Keep fixed diagnostic codes; never copy untrusted report IDs or messages."""
    clean = []
    for error in errors:
        parts = str(error).split(":", 2)
        context = parts.pop(0) if parts[0] in ("linux", "darwin", "baseline", "candidate") else None
        code = parts.pop(0)
        item = {"code": code if code in ERROR_CODES else "invalid_input"}
        if context:
            item["context"] = context
        if parts:
            item["reference_sha256"] = hashlib.sha256(":".join(parts).encode()).hexdigest()
        clean.append(item)
    return clean


def fresh(report, now, max_age_seconds):
    try:
        stamp = datetime.fromisoformat(report["timestamp"].replace("Z", "+00:00"))
        return stamp.tzinfo is not None and 0 <= (now - stamp).total_seconds() <= max_age_seconds
    except (KeyError, TypeError, ValueError, AttributeError):
        return False


def write_decision(output, mode, decision, errors, now):
    output.mkdir(parents=True, exist_ok=True)
    document = {"schema_version": 1, "mode": mode, "timestamp": now.isoformat(),
                "passed": decision, "errors": sanitized_errors(errors)}
    document["certified" if mode == "release" else "promotable"] = decision
    pending = {**document, "passed": False, "publication_complete": False,
               "errors": [{"code": "report_publication_incomplete"}]}
    pending["certified" if mode == "release" else "promotable"] = False
    atomic_json(output / "decision.json", pending)
    document["publication_complete"] = True
    suite = ET.Element("testsuite", name="hrx-evidence-" + mode, tests="1", failures=str(int(not decision)))
    case = ET.SubElement(suite, "testcase", classname="hrx.evidence", name=mode)
    if not decision:
        ET.SubElement(case, "failure", type="INCOMPLETE", message="Evidence does not satisfy " + mode).text = json.dumps(document["errors"])
    ET.ElementTree(suite).write(output / "junit.xml", encoding="unicode", xml_declaration=True)
    lines = ["# HRX " + mode + " evidence", "", "Result: " + ("PASS" if decision else "INCOMPLETE"),
             "", "Checked: " + now.isoformat(), ""]
    lines.extend("- " + item.get("context", "aggregate") + ": " + item["code"] for item in document["errors"])
    (output / "decision.md").write_text("\n".join(lines) + "\n")
    atomic_json(output / "decision.json", document)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="mode", required=True)
    release = commands.add_parser("release", help="Require independent trusted expectations and both OS reports")
    release.add_argument("--expected", type=Path, required=True)
    release.add_argument("--report", type=Path, action="append", required=True)
    upgrade = commands.add_parser("upgrade", help="Compare the same exact cells/artifact/config before promotion")
    upgrade.add_argument("--inventory", type=Path, required=True)
    upgrade.add_argument("--baseline", type=Path, required=True)
    upgrade.add_argument("--candidate", type=Path, required=True)
    upgrade.add_argument("--required-scenario", action="append", default=[], help="Additional required scenario ID")
    for command in (release, upgrade):
        command.add_argument("--max-age-seconds", type=float, default=86400)
        command.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    now = datetime.now(timezone.utc)
    errors, passed = [], False
    try:
        if not math.isfinite(args.max_age_seconds) or args.max_age_seconds <= 0:
            errors.append("invalid_freshness_policy")
        elif args.mode == "release":
            result = validate_release([read_object(path) for path in args.report], read_object(args.expected),
                                      now=now, max_age_seconds=args.max_age_seconds)
            errors, passed = result["errors"], result["certified"]
        else:
            inventory = read_object(args.inventory)
            required = [cell["id"] for cell in inventory["cells"] if cell.get("required", True)]
            required += inventory.get("required_scenarios", ["mixed-pitput-conversation"])
            # The executable inventory stores auto/broker requirements here.
            # Reports cannot silently omit them from both sides of a comparison.
            required += list(inventory.get("scenarios", {}))
            required += [row["id"] for row in resume_rows(inventory, "release")]
            required += args.required_scenario
            baseline, candidate = read_object(args.baseline), read_object(args.candidate)
            inventory_hash = hashlib.sha256(args.inventory.read_bytes()).hexdigest()
            for name, report in (("baseline", baseline), ("candidate", candidate)):
                if report.get("inventory_sha256") != inventory_hash:
                    errors.append(name + ":trusted_inventory_mismatch")
                if not fresh(report, now, args.max_age_seconds):
                    errors.append(name + ":stale_or_invalid_timestamp")
            result = compare_upgrade(baseline, candidate, required)
            errors += result["errors"]
            errors += ["candidate_regression:" + name for name in result["regressions"]]
            passed = result["promotable"] and not errors
    except OSError:
        errors = ["input_unavailable"]
    except (ValueError, TypeError, KeyError, AttributeError, OverflowError):
        errors = ["invalid_input"]
    try:
        write_decision(args.output, args.mode, passed, errors, now)
    except OSError:
        # Broken storage cannot guarantee a durable failure artifact.
        return 1
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
