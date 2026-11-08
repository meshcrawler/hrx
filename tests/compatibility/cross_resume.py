"""Explicit representative model changes, independent of discovered capabilities."""
import time
import re


def resume_rows(inventory, lane, diagnostic_cells=None):
    selected = lane in ("nightly", "release") and not diagnostic_cells
    pairs = inventory.get("resume_pairs")
    if not isinstance(pairs, list) or not pairs:
        return [{"id": "cross-model-resume-inventory", "required": True, "selected": selected,
                 "status": "BLOCKED" if selected else "NOT_RUN", "reason": "resume_pairs_missing"}]
    known = {cell["id"] for cell in inventory["cells"]}
    ids = set(known) | set(inventory.get("scenarios", {})) | {"mixed-pitput-conversation"}
    rows = []
    for pair in pairs:
        if (not isinstance(pair, dict) or set(pair) != {"id", "source", "target"} or
                not isinstance(pair["id"], str) or not re.fullmatch(r"cross-resume-[a-z0-9-]+", pair["id"]) or
                pair["id"] in ids or pair["source"] not in known or pair["target"] not in known or
                pair["source"] == pair["target"]):
            raise ValueError("invalid_resume_pair")
        ids.add(pair["id"])
        rows.append({**pair, "required": True, "selected": selected, "status": "NOT_RUN",
                     "reason": "not_started" if selected else "not_selected_by_lane"})
    return rows


def run_resumes(rows, runner, cells, catalog, root, capability, native_versions):
    by_id = {cell["id"]: cell for cell in cells}
    for row in rows:
        if not row["selected"] or row["status"] != "NOT_RUN":
            continue
        source, target = by_id[row["source"]], by_id[row["target"]]
        row.update(source_route=source["route"], target_route=target["route"], harness=source["harness"],
                   source_model=source["canonical_model"], target_model=target["canonical_model"])
        if (source["harness"] != target["harness"] or source["protocol"] != target["protocol"] or
                source["canonical_model"] == target["canonical_model"] or source["route"] == target["route"]):
            row.update(status="BLOCKED", reason="resume_pair_not_distinct_same_harness_models")
            continue
        unavailable = next((cell for cell in (source, target)
                            if cell.get("resolution_status") in ("BLOCKED", "UNSUPPORTED")), None)
        if unavailable:
            row.update(status=unavailable["resolution_status"], reason=unavailable["resolution_reason"])
        elif not all(capability(catalog, cell) for cell in (source, target)):
            row.update(status="UNSUPPORTED", reason="required_route_capability_absent")
        elif not {source["route"], target["route"]} <= set(runner.policy["routes"]):
            row.update(status="BLOCKED", reason="route_not_authorized_by_budget_policy")
        elif source["harness"] not in native_versions:
            row.update(status="BLOCKED", reason="native_binary_missing")
        elif time.monotonic() >= runner.gateway.deadline:
            row.update(status="NOT_RUN", reason="aggregate_deadline")
        else:
            previous = runner.gateway.allowed_routes
            try:
                runner.gateway.allowed_routes = {source["route"]}
                result = runner.cell(source, root / row["id"], resume_cell=target)
                row.pop("reason", None)
                row.update(result, attempts=1)
            finally:
                runner.gateway.allowed_routes = previous
            if row.get("cleanup") == "unknown":
                # Do not start any further native command after loss of ownership proof.
                raise RuntimeError("native_cleanup_unverified")
    return rows
