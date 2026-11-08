"""Fail-closed selection and evidence checks; these helpers never run inference."""
from datetime import datetime, timezone
import copy
import hashlib
import math
import re


def _generation(canonical, family):
    if family.startswith("claude-"):
        match = re.fullmatch(re.escape(family) + r"-(\d+(?:[-.]\d{1,3})*)(?:-\d{8})?", canonical)
    elif family.startswith("gpt-"):
        match = re.fullmatch(r"gpt-(\d+(?:\.\d{1,3})*)-" + re.escape(family[4:]) + r"(?:-\d{8})?", canonical)
    else:
        return None
    if not match:
        return None
    return tuple(int(n) for n in re.split(r"[-.]", match[1]))


def resolve_latest_families(inventory, catalog):
    """Freeze latest stable numeric family generations without capability fallback.

    Latest cells specify family, selection='latest', and route_policy containing
    provider/account and/or route_prefixes. All specified constraints must match.
    Newest available generation is chosen before checking those constraints and
    capability: an unavailable payer or protocol cannot silently select an older
    generation. Ambiguous routes require a narrower policy. Unresolved cells stay
    in the required inventory with route/canonical_model=None and BLOCKED status.
    """
    resolved = copy.deepcopy(inventory)
    for cell in resolved.get("cells", []):
        if cell.get("selection") != "latest":
            continue
        cell.update(route=None, canonical_model=None, resolution_status="BLOCKED")
        family = cell.get("family", "")
        policy = cell.get("route_policy", {})
        if (not isinstance(policy, dict) or not policy or
                set(policy) - {"provider", "account", "route_prefixes"} or
                any(not isinstance(policy[k], str) or not policy[k] for k in ("provider", "account") if k in policy) or
                ("route_prefixes" in policy and (not isinstance(policy["route_prefixes"], list) or
                 not policy["route_prefixes"] or not all(isinstance(p, str) and p for p in policy["route_prefixes"])))):
            cell["resolution_reason"] = "explicit_route_policy_required"
            continue
        generations = []
        for model in catalog.get("models", []):
            generation = _generation(model.get("canonical_id", ""), family)
            if generation is not None and any(s.get("available") is True and s.get("gateway_route")
                                              for s in model.get("sources", [])):
                generations.append((generation, model))
        if not generations:
            cell["resolution_reason"] = "latest_family_unavailable"
            continue
        newest = max(g for g, _ in generations)
        models = [m for g, m in generations if g == newest]
        routes = []
        for model in models:
            for source in model.get("sources", []):
                route = source.get("gateway_route")
                if source.get("available") is not True or not route:
                    continue
                if any(source.get(k) != policy[k] for k in ("provider", "account") if k in policy):
                    continue
                if "route_prefixes" in policy and not any(route.startswith(p) for p in policy["route_prefixes"]):
                    continue
                routes.append((model, source))
        if len(models) == 1:
            cell["canonical_model"] = models[0]["canonical_id"]
        if len(routes) != 1:
            cell["resolution_reason"] = "latest_route_ambiguous" if routes else "latest_route_policy_unavailable"
            continue
        model, source = routes[0]
        cell.update(canonical_model=model["canonical_id"], route=source["gateway_route"],
                    provider=source.get("provider"), account=source.get("account"))
        capability = source.get("details", {}).get("harnesses", {}).get(cell["harness"], {})
        if capability.get("wire_api") != cell["protocol"]:
            cell.update(resolution_status="UNSUPPORTED", resolution_reason="latest_route_capability_absent")
        else:
            cell.update(resolution_status="RESOLVED", resolution_reason="latest_stable_generation")
    return resolved


def select_exploratory_opencode(catalog, required_cells, authorized_routes):
    """Return additional exact authorized routes and explicit omission reasons.

    Required cells remain the caller's inventory; discovery never replaces them.
    Ambiguous routes are omitted rather than choosing a provider arbitrarily.
    """
    required = {c["route"] for c in required_cells if c["harness"] == "opencode"}
    authorized = set(authorized_routes)
    candidates = {}
    for model in catalog.get("models", []):
        for source in model.get("sources", []):
            route = source.get("gateway_route")
            if route:
                candidates.setdefault(route, []).append((model, source))
    cells, omitted = [], []
    for route, sources in sorted(candidates.items()):
        reason = None
        if route in required:
            reason = "already_required"
        elif route not in authorized:
            reason = "not_authorized"
        elif len(sources) != 1:
            reason = "ambiguous_route"
        else:
            model, source = sources[0]
            cap = source.get("details", {}).get("harnesses", {}).get("opencode", {})
            if source.get("available") is not True:
                reason = "unavailable"
            elif not model.get("canonical_id"):
                reason = "canonical_identity_missing"
            elif (cap.get("wire_api") != "chat_completions" or
                  cap.get("evidence", "") not in ("", "live-tested", "provider-declared", "gateway-declared")):
                reason = "opencode_capability_absent"
            elif cap.get("default_reasoning", "") and cap["default_reasoning"] not in cap.get("reasoning_levels", []):
                reason = "invalid_reasoning_declaration"
            else:
                cells.append({"id": "exploratory-opencode-" + hashlib.sha256(route.encode()).hexdigest()[:24],
                              "harness": "opencode", "canonical_model": model["canonical_id"],
                              "route": route, "protocol": "chat_completions",
                              "reasoning": cap.get("default_reasoning", ""),
                              "provider": source.get("provider"), "account": source.get("account"),
                              "auth_profile": "ci-gateway", "required": False, "canary": False})
        if reason:
            omitted.append({"route": route, "reason": reason})
    for route in sorted(authorized - candidates.keys() - required):
        omitted.append({"route": route, "reason": "absent_from_catalog"})
    return {"cells": cells, "omitted": omitted}


def _results(report, required_ids, prefix, platform_placeholders=()):
    errors, indexed = [], {}
    for result in report.get("results", []):
        name = result.get("id")
        if not name or name in indexed:
            errors.append(prefix + ":duplicate_or_missing_result_id")
        indexed[name] = result
        if result.get("required", True) and (result.get("status") != "PASS" or result.get("selected", True) is not True):
            # Cross-platform placeholders are satisfied only by the aggregate.
            placeholder = (name in platform_placeholders and result.get("status") == "NOT_RUN" and
                           result.get("reason") == "separate_native_runner_evidence_required")
            if not placeholder:
                errors.append(prefix + ":required_not_pass:" + str(name))
    for name in required_ids:
        row = indexed.get(name, {})
        if row.get("status") != "PASS" or row.get("selected", True) is not True:
            errors.append(prefix + ":missing_or_nonpassing:" + name)
    return indexed, errors


def validate_release(reports, expected, *, now=None, max_age_seconds=86400):
    """Validate reports against a trusted release manifest, never inferred from reports.

    expected: inventory_sha256, resolved_inventory_sha256, config_sha256, catalog_sha256, commit,
    required_ids (includes composition/PTY/broker evidence), and platforms mapping
    linux/darwin to {hrx_sha256, native_versions: {claude, codex, opencode}}.
    Platform binaries naturally have different hashes. Reports use run.py's flat
    metadata plus results. Freshness and expected versions apply to every report.
    """
    now = now or datetime.now(timezone.utc)
    errors = []
    if now.tzinfo is None or not isinstance(max_age_seconds, (int, float)) or not math.isfinite(max_age_seconds) or max_age_seconds <= 0:
        return {"certified": False, "errors": ["invalid_freshness_policy"]}
    platforms = expected.get("platforms", {})
    if not {"linux", "darwin"} <= platforms.keys():
        errors.append("linux_and_darwin_required")
    required = expected.get("required_ids", [])
    if not required or len(set(required)) != len(required):
        errors.append("invalid_required_inventory")
    shared = ("inventory_sha256", "resolved_inventory_sha256", "config_sha256", "catalog_sha256", "commit")
    for key in shared:
        pattern = r"[a-f0-9]{40}|[a-f0-9]{64}" if key == "commit" else r"[a-f0-9]{64}"
        if not re.fullmatch(pattern, str(expected.get(key, ""))):
            errors.append("invalid_expected_" + key)
    by_os = {}
    for report in reports:
        os_name = report.get("os")
        if os_name not in platforms or os_name in by_os:
            errors.append("unexpected_or_duplicate_platform:" + str(os_name))
            continue
        by_os[os_name] = report
        target = platforms[os_name]
        if not re.fullmatch(r"[a-f0-9]{64}", str(target.get("hrx_sha256", ""))):
            errors.append(os_name + ":invalid_expected_artifact")
        versions = target.get("native_versions", {})
        if not all(isinstance(versions.get(h), str) and versions[h].strip() for h in ("claude", "codex", "opencode")):
            errors.append(os_name + ":expected_native_versions_missing")
        for key in shared:
            if report.get(key) != expected.get(key):
                errors.append(os_name + ":mismatch:" + key)
        for key in ("hrx_sha256", "native_versions"):
            if report.get(key) != target.get(key):
                errors.append(os_name + ":mismatch:" + key)
        if report.get("lane") != "release" or report.get("schema_version") != 1:
            errors.append(os_name + ":not_release_evidence")
        try:
            stamp = datetime.fromisoformat(report["timestamp"].replace("Z", "+00:00"))
            if stamp.tzinfo is None or not 0 <= (now - stamp).total_seconds() <= max_age_seconds:
                raise ValueError("stale")
        except (KeyError, TypeError, ValueError):
            errors.append(os_name + ":stale_or_invalid_timestamp")
        placeholders = {"release-os-" + p for p in platforms if p != os_name}
        _, row_errors = _results(report, required, os_name, placeholders)
        errors.extend(row_errors)
    errors.extend("missing_platform:" + name for name in sorted(platforms.keys() - by_os.keys()))
    return {"certified": not errors, "errors": errors}


def compare_upgrade(baseline, candidate, required_ids):
    """Compare exact same inventory/config/artifact; native versions may differ.

    A failed baseline is inconclusive, never permission to promote a candidate.
    Freshness/release trust is separately enforced by validate_release.
    """
    errors = []
    if not required_ids:
        errors.append("required_inventory_empty")
    for key in ("inventory_sha256", "resolved_inventory_sha256", "catalog_sha256", "config_sha256", "hrx_sha256", "commit", "os"):
        if not baseline.get(key) or baseline.get(key) != candidate.get(key):
            errors.append("comparison_mismatch:" + key)
    for name, report in (("baseline", baseline), ("candidate", candidate)):
        versions = report.get("native_versions", {})
        if not all(versions.get(h) for h in ("claude", "codex", "opencode")):
            errors.append(name + ":native_versions_missing")
    before, before_errors = _results(baseline, required_ids, "baseline")
    after, after_errors = _results(candidate, required_ids, "candidate")
    errors.extend(before_errors + after_errors)
    regressions = sorted(name for name in before if before[name].get("status") == "PASS" and
                         (after.get(name, {}).get("status") != "PASS" or
                          after.get(name, {}).get("selected", True) is not True))
    return {"promotable": not errors and not regressions, "regressions": regressions, "errors": errors}
