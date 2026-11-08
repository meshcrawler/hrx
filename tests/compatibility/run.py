#!/usr/bin/env python3
"""Real HRX compatibility certification; missing evidence is never a pass."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

from gateway import Gateway, NoRedirect, load_claude_lease
from scenarios import resolve_latest_families, select_exploratory_opencode
from owned_capture import capture
import ownership
from report_io import atomic_json
from cross_resume import resume_rows, run_resumes

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent


class Blocked(Exception):
    pass


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def private(path, text):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(text)
    path.chmod(0o600)


def retry_safe(result):
    return (result.get("cleanup") == "clean" and result["status"] in ("FAIL", "BLOCKED") and
            result.get("reason") not in ("local_admission_or_budget_limit", "provider_auth_quota_or_availability"))


def capabilities(catalog, cell):
    for model in catalog.get("models", []):
        if model.get("canonical_id") != cell["canonical_model"]:
            continue
        for source in model.get("sources", []):
            if source.get("gateway_route") != cell["route"] or not source.get("available"):
                continue
            policy = cell.get("route_policy", {})
            if any(source.get(key) != policy[key] for key in ("provider", "account") if key in policy):
                continue
            capability = source.get("details", {}).get("harnesses", {}).get(cell["harness"], {})
            if capability.get("wire_api") == cell["protocol"]:
                return {"provider": source.get("provider"), "account": source.get("account"),
                        "capability": capability}
    return None


def policy_from_file(path):
    if not path:
        raise Blocked("policy_missing")
    policy = json.loads(Path(path).read_text())
    for name in ("daily_budget_usd", "max_requests", "wall_seconds", "cell_seconds",
                 "request_seconds", "max_request_bytes", "max_output_tokens", "concurrency"):
        value = policy.get(name)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
            raise Blocked("policy_invalid_" + name)
    for name in ("max_requests", "max_request_bytes", "max_output_tokens", "concurrency"):
        if not isinstance(policy[name], int):
            raise Blocked("policy_integer_required_" + name)
    if not policy.get("routes") or not all(isinstance(x, str) for x in policy["routes"]):
        raise Blocked("policy_route_allowlist_missing")
    if policy["concurrency"] > 2:
        raise Blocked("initial_concurrency_exceeds_two")
    # One aggregate daily cap covers every lane on the shared dedicated key. A per-lane
    # budget would let canary, nightly and release each spend a full cap.
    if any("budget" in name and name != "daily_budget_usd" for name in policy) or "lanes" in policy:
        raise Blocked("per_lane_budget_forbidden_single_aggregate_cap")
    reserve = policy.get("start_reserve_usd", 0)
    if (isinstance(reserve, bool) or not isinstance(reserve, (int, float)) or not math.isfinite(reserve)
            or not 0 <= reserve < policy["daily_budget_usd"]):
        raise Blocked("policy_invalid_start_reserve_usd")
    return policy


def verify_key(upstream, key, policy):
    # The key introspection response is intentionally never persisted or logged.
    request = urllib.request.Request(upstream + "/key/info", headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=15) as response:
            info = json.load(response).get("info", {})
    except Exception:
        raise Blocked("dedicated_key_budget_unverifiable") from None
    maximum = info.get("max_budget")
    if not isinstance(maximum, (int, float)) or not 0 < maximum <= policy["daily_budget_usd"]:
        raise Blocked("gateway_daily_budget_missing_or_excessive")
    if info.get("budget_duration") != "1d":
        raise Blocked("gateway_daily_reset_unverified")
    if info.get("metadata", {}).get("purpose") != "hrx-compatibility-ci":
        raise Blocked("not_dedicated_ci_key")
    if not info.get("models") or not set(policy["routes"]) <= set(info["models"]):
        raise Blocked("gateway_key_model_allowlist_mismatch")
    spend = info.get("spend")
    # Missing or negative spend would overstate what remains under the shared cap.
    if (not isinstance(spend, (int, float)) or isinstance(spend, bool) or not math.isfinite(spend)
            or spend < 0):
        raise Blocked("dedicated_key_budget_unverifiable")
    if spend >= maximum or info.get("blocked"):
        raise Blocked("gateway_budget_exhausted_or_key_blocked")
    remaining = maximum - spend
    # A starting threshold only: below it, don't begin a run. Meeting it does not prove
    # the run finishes before the gateway exhausts the shared budget.
    if remaining < policy.get("start_reserve_usd", 0):
        raise Blocked("gateway_budget_reserve_insufficient")
    # Request and output-token caps do not bound USD (input, cached-input and
    # long-context pricing, growing tool histories), so cost is never certified here:
    # the gateway's key budget is the authoritative limit.
    return {"aggregate_daily_cap_usd": maximum, "spend_at_start_usd": round(spend, 6),
            "remaining_at_start_usd": round(remaining, 6),
            "enforcement": "gateway_dedicated_key_budget_authoritative",
            "cost_bound": "not_certified_request_and_token_caps_do_not_bound_usd"}


def native_evidence(text, harness):
    session = None
    tools = 0
    for line in text.splitlines():
        try:
            event = json.loads(line)
        except (ValueError, TypeError):
            continue
        if not isinstance(event, dict):
            continue
        session = event.get("session_id", event.get("sessionID", event.get("thread_id", session)))
        item = event.get("item", {})
        if harness == "codex" and event.get("type") == "item.completed":
            tools += int(item.get("type") in ("command_execution", "file_change") and
                         item.get("status") == "completed")
        if harness == "claude":
            tools += sum(1 for block in event.get("message", {}).get("content", [])
                         if isinstance(block, dict) and block.get("type") == "tool_use")
        if harness == "opencode":
            part = event.get("part", {})
            tools += int(event.get("type") == "tool_use" and part.get("state", {}).get("status") == "completed")
    if session and not re.fullmatch(r"[a-zA-Z0-9_-]{8,100}", session):
        session = None
    return {"session_id": session, "tool_calls": tools}


class Runner:
    # A diagnostic subclass may capture stderr and attach bounded, redacted launch
    # diagnostics; the default keeps raw native output private and adds nothing.
    capture_stderr = False

    def __init__(self, hrx, gateway, policy):
        self.hrx, self.gateway, self.policy = str(Path(hrx).resolve()), gateway, policy

    def diagnose(self, cell, captured):
        return None

    def isolated_env(self, home):
        home.mkdir(parents=True, exist_ok=True, mode=0o700)
        (home / "tmp").mkdir(exist_ok=True, mode=0o700)
        return {"PATH": os.environ["PATH"], "HOME": str(home), "LANG": "C.UTF-8",
                "CODEX_HOME": str(home / "codex"), "CLAUDE_CONFIG_DIR": str(home / "claude"),
                "XDG_CONFIG_HOME": str(home / ".config"), "XDG_DATA_HOME": str(home / "data"),
                "XDG_STATE_HOME": str(home / "state"), "XDG_CACHE_HOME": str(home / "cache"),
                "TMPDIR": str(home / "tmp"), "HRX_CI_KEY": self.gateway.secret,
                "HRX_CA_FILE": str(self.gateway.cert),
                "SSL_CERT_FILE": str(self.gateway.cert), "NODE_EXTRA_CA_CERTS": str(self.gateway.cert),
                "NO_COLOR": "1", "TERM": "xterm-256color"}

    def configure(self, home, project, cell):
        env = self.isolated_env(home)
        common = f'base_url = "{self.gateway.url}/v1"\nenv_key = "HRX_CI_KEY"\n'
        private(home / ".config/hrx/config.toml",
                '[permissions]\nyolo = true\n[claude.provider]\n' + common +
                '[opencode.provider]\n' + common)
        private(home / "codex/config.toml", 'model_provider = "ci"\nmodel = ' + json.dumps(cell["route"]) +
                '\n[model_providers.ci]\nname = "HRX isolated CI"\nwire_api = "responses"\n' + common +
                'request_max_retries = 0\nstream_max_retries = 0\n' +
                '[projects.' + json.dumps(str(project)) + ']\ntrust_level = "trusted"\n')
        # HRX's generated Codex catalog requires the native model template,
        # containing public metadata only, never the operator's auth profile.
        template = os.environ.get("HRX_CI_CODEX_CATALOG")
        if template:
            raw_template = json.loads(Path(template).read_text())
            public_template = {name: raw_template[name] for name in
                               ("models", "client_version", "fetched_at", "etag") if name in raw_template}
            private(home / "codex/models_cache.json", json.dumps(public_template))
        return env

    def command(self, cell, prompt, resume=None):
        args = [self.hrx, cell["harness"], "--model", cell["route"]]
        if cell["reasoning"]:
            args += ["--reasoning", cell["reasoning"]]
        if resume:
            args += ["--resume", resume]
        harness = cell["harness"]
        if harness == "claude":
            args += ["--", "-p", "--verbose", "--output-format", "stream-json", "--max-turns", "8", prompt]
        elif harness == "codex":
            args += ["--", "exec", "--json", "--skip-git-repo-check", prompt]
        else:
            args += ["--", "run", "--format", "json", prompt]
        return args

    def launch(self, cell, home, project, prompt, resume=None):
        if ownership.available(sys.platform).mechanism != ownership.LINUX_SUBREAPER:
            return {"status": "BLOCKED", "reason": "native_cleanup_boundary_unavailable"}
        started = time.monotonic()
        attempted_at = datetime.now(timezone.utc).isoformat()
        env = self.configure(home, project, cell)
        start = len(self.gateway.requests)
        captured = capture(self.command(cell, prompt, resume), cwd=project, env=env,
                           deadline=min(self.gateway.deadline, started + self.policy["cell_seconds"]),
                           abort=lambda: any(e["http_status"] in (401, 429)
                                             for e in self.gateway.requests[start:]),
                           capture_stderr=self.capture_stderr)
        timed_out, exit_code = captured["timed_out"], captured["exit_code"]
        evidence = native_evidence(captured["stdout"].decode(errors="replace"), cell["harness"])
        events = self.gateway.requests[start:]
        initial_results = set(events[0].get("tool_result_hashes", [])) if events else set()
        fresh_results = any(set(e.get("tool_result_hashes", [])) - initial_results for e in events[1:]
                            if e["http_status"] == 200 and not e.get("local_limit"))
        evidence.update(attempted_at=attempted_at, completed_at=datetime.now(timezone.utc).isoformat(),
                        duration_seconds=round(time.monotonic() - started, 3),
                        fresh_tool_result_consumed=fresh_results, exit_code=exit_code, timed_out=timed_out,
                        cleanup=captured["cleanup"],
                        supervisor_exit=captured.get("supervisor_exit"),
                        supervisor_summary=captured.get("supervisor_summary"),
                        tool_results=sum(e["tool_results"] for e in events),
                        inference_requests=len(events), observed_routes=sorted({e["route"] for e in events}),
                        http_statuses=[e["http_status"] for e in events])
        expected_protocol = {"responses": "/v1/responses", "messages": "/v1/messages",
                             "chat_completions": "/v1/chat/completions"}[cell["protocol"]]
        evidence["exact_route_protocol"] = bool(events) and all(
            e["route"] == cell["route"] and e["protocol"] == expected_protocol for e in events)
        diagnostics = self.diagnose(cell, captured)
        if diagnostics is not None:
            evidence["diagnostics"] = diagnostics
        # No raw output escapes the private process; errors may contain tokens.
        evidence["status"] = "PASS" if exit_code == 0 and not timed_out else "FAIL"
        if captured["cleanup"] != "clean":
            evidence.update(status="FAIL", reason="native_cleanup_" + captured["cleanup"])
        elif exit_code is None:
            evidence.update(status="FAIL", reason="native_exit_unverified")
        elif any(e.get("local_limit") for e in events):
            evidence.update(status="BLOCKED", reason="local_admission_or_budget_limit",
                            local_limits=sorted({e["local_limit"] for e in events if e.get("local_limit")}))
        elif any(e["http_status"] in (401, 403, 429, 500, 502, 503, 504) for e in events):
            evidence.update(status="BLOCKED", reason="provider_auth_quota_or_availability")
        elif timed_out:
            evidence.update(reason="native_deadline")
        elif exit_code:
            evidence.update(reason="native_or_launcher_exit")
        elif any(e.get("transport_error") for e in events):
            evidence.update(status="BLOCKED", reason="inference_transport_or_deadline")
        return evidence

    def cell(self, cell, root, resume_cell=None):
        home, project = root / "home", root / "project"
        project.mkdir(parents=True, mode=0o700)
        nonce = secrets.token_hex(24)
        private(project / "challenge.txt", nonce)
        prompt = ('Use your native file/shell tools to read challenge.txt. Reverse its exact characters and '
                  'write them to result.txt with no trailing newline. Remember the original value for the '
                  'next turn. Do not print the value. Finish after creating the file.')
        result = self.launch(cell, home, project, prompt)
        target = project / "result.txt"
        valid = target.is_file() and target.read_text() == nonce[::-1]
        result["filesystem_assertion"] = valid
        if result["status"] != "PASS":
            return result
        if not (valid and result["session_id"] and result["tool_calls"] and result["tool_results"] and
                result["exact_route_protocol"]):
            return {**result, "status": "FAIL", "reason": "missing_real_tool_or_session_evidence"}
        (project / "challenge.txt").unlink()
        target.unlink()
        if resume_cell is not None:
            self.gateway.allowed_routes = {resume_cell["route"]}
        resumed = self.launch(resume_cell if resume_cell is not None else cell, home, project,
                              'Use the original challenge value remembered from our previous turn. '
                              'Write it to resumed.txt with no trailing newline using a native tool. '
                              'The original files are gone. Do not print the value.', result["session_id"])
        resumed_file = project / "resumed.txt"
        valid_resume = resumed_file.is_file() and resumed_file.read_text() == nonce
        result["resume"] = resumed
        result["initial_cleanup"] = result.get("cleanup")
        result["cleanup"] = resumed.get("cleanup", "unknown")
        result["resume_assertion"] = valid_resume
        if resumed["status"] != "PASS":
            result.update(status=resumed["status"], reason="resume_" + resumed.get("reason", "failed"))
        elif not (valid_resume and resumed["session_id"] == result["session_id"] and resumed["tool_calls"] and
                  resumed["exact_route_protocol"] and resumed["fresh_tool_result_consumed"]):
            result.update(status="FAIL", reason="resume_identity_context_or_tool_assertion")
        return result


def report(output, results, meta):
    output.mkdir(parents=True, exist_ok=True)
    lane_passed = bool(results) and all(r["status"] == "PASS" for r in results if r.get("selected", True))
    document = {"schema_version": 1, **meta, "results": results, "lane_passed": lane_passed,
                "certified": meta.get("lane") == "release" and bool(results) and
                all(r["status"] == "PASS" for r in results if r.get("required", True))}
    # Aggregators consume JSON alone. Keep it non-certifying until every other
    # required report is published; commit completed JSON last, atomically.
    provisional = {**document, "lane_passed": False, "certified": False,
                   "results": results + [{"id": "report-publication", "required": True,
                                           "selected": True, "status": "NOT_RUN",
                                           "reason": "report_publication_incomplete"}]}
    atomic_json(output / "results.json", provisional)
    suite = ET.Element("testsuite", name="hrx-live-compatibility", tests=str(len(results)))
    failures = 0
    for result in results:
        case = ET.SubElement(suite, "testcase", classname="hrx.live", name=result["id"],
                             time=str(result.get("duration_seconds", 0)))
        if result["status"] != "PASS" and result.get("selected", True):
            failures += 1
            ET.SubElement(case, "failure", type=result["status"],
                          message=result.get("reason", result["status"]))
        elif not result.get("selected", True):
            ET.SubElement(case, "skipped", message="explicitly outside this lane; not release evidence")
    suite.set("failures", str(failures))
    ET.ElementTree(suite).write(output / "junit.xml", encoding="unicode", xml_declaration=True)
    table = ["# HRX live compatibility", "", "Generated: " + meta["timestamp"], "",
             "| Cell | Result | Reason |", "| --- | --- | --- |"]
    table += [f'| {r["id"]} | {r["status"]} | {r.get("reason", "")} |' for r in results]
    (output / "compatibility.md").write_text("\n".join(table) + "\n")
    atomic_json(output / "results.json", document)
    return lane_passed


def scenario_rows(inventory, lane, diagnostic_cells):
    """Required scenario rows. They stay visible as NOT_RUN or BLOCKED when not run,
    so a missing scenario can never be mistaken for a passing one."""
    rows = []
    for name, spec in inventory.get("scenarios", {}).items():
        selected = not diagnostic_cells and lane in spec["lanes"]
        rows.append({"id": name, "required": True, "selected": selected, "status": "NOT_RUN",
                     "reason": "scenario_not_started" if selected else "not_selected_by_lane"})
    return rows


def parser_route(catalog, model, allowed_routes):
    """The --auto parser route: the named model's OpenCode Chat Completions source among
    the routes this CI policy authorizes. Several public payer routes are fine as long
    as exactly one is authorized; zero or several authorized candidates fail closed,
    and no candidate is ever chosen by position."""
    allowed = set(allowed_routes)
    routes = {source.get("gateway_route") for entry in catalog.get("models", [])
              if entry.get("canonical_id") == model
              for source in entry.get("sources", [])
              if source.get("available") and source.get("gateway_route") in allowed
              and (source.get("details", {}).get("harnesses", {})
                   .get("opencode", {}).get("wire_api")) == "chat_completions"}
    return routes.pop() if len(routes) == 1 else None


def run_scenarios(rows, runner, inventory, cells, catalog, root):
    """Run selected scenarios through the same relay, deadline and admission count
    as the cells; their inference (the --auto parser call) counts in the budget."""
    from auto_scenario import auto_scenario
    from broker_scenario import broker_scenario
    by_id = {cell["id"]: cell for cell in cells}
    gateway = runner.gateway
    for row in rows:
        if not row["selected"]:
            continue
        if time.monotonic() >= gateway.deadline:
            row.update(status="NOT_RUN", reason="aggregate_deadline")
            continue
        spec = inventory["scenarios"][row["id"]]
        started = time.monotonic()
        if row["id"] == "auto-parser":
            route = parser_route(catalog, spec["parser_model"], runner.policy["routes"])
            launch = by_id.get(spec["launch_cell"])
            if route is None or launch is None:
                row.update(status="BLOCKED", reason="auto_parser_route_unauthorized_unavailable_or_ambiguous")
                continue
            evidence = auto_scenario(runner, root / "auto-parser", route, launch["harness"],
                                     launch["route"])
        else:
            cell = by_id.get(spec["cell"])
            if cell is None:
                row.update(status="BLOCKED", reason="broker_scenario_cell_missing")
                continue
            previous, gateway.allowed_routes = gateway.allowed_routes, {cell["route"]}
            try:
                evidence = broker_scenario(runner, {"harness": cell["harness"], "route": cell["route"],
                                                    "reasoning": cell.get("reasoning")},
                                           root / "broker-glab-git")
            finally:
                gateway.allowed_routes = previous
        row.pop("reason", None)
        row.update({key: value for key, value in evidence.items() if key != "id"},
                   scenario_evidence_id=evidence.get("id"),
                   duration_seconds=round(time.monotonic() - started, 3))
    return rows


def finalize_gateway(gateway, meta, timeout=5):
    meta["gateway_drained"] = False
    deadline = time.monotonic() + timeout
    gateway.close()
    if gateway.thread.is_alive():
        raise TimeoutError("gateway_listener_not_stopped")
    with gateway.lock:
        if gateway.closed is not True:
            raise ValueError("gateway_admission_not_closed")
        while True:
            if type(gateway.active) is not int or gateway.active < 0:
                raise ValueError("gateway_activity_unknown")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("gateway_requests_not_drained")
            if gateway.active == 0:
                break
            gateway.lock.wait(remaining)
        # Counters are final only after admission has closed and handlers drained.
        meta["inference_request_count"] = gateway.admitted
        meta["admission_attempt_count"] = len(gateway.requests)
        meta["gateway_drained"] = True


def main():
    parser = argparse.ArgumentParser(__doc__)
    parser.add_argument("--lane", choices=["canary", "nightly", "release"], default="canary")
    parser.add_argument("--cell", action="append", help="diagnostic cell ID (repeatable); omitted cells remain uncertified")
    parser.add_argument("--skip-conversation", action="store_true", help="diagnostic only; conversation remains NOT_RUN")
    parser.add_argument("--output", type=Path, default=Path("artifacts/compatibility"))
    parser.add_argument("--inventory", type=Path,
                        default=Path(os.environ.get("HRX_COMPAT_INVENTORY") or HERE / "required.yaml"))
    parser.add_argument("--hrx", default=os.environ.get("HRX_CI_BINARY", str(REPO / "bin/hrx")))
    parser.add_argument("--policy", default=os.environ.get("HRX_CI_POLICY"))
    parser.add_argument("--catalog", type=Path, help="frozen public catalog; no inference in --inventory-only mode")
    parser.add_argument("--inventory-only", action="store_true")
    args = parser.parse_args()
    inventory = json.loads(args.inventory.read_text())
    cells = inventory["cells"]
    if args.cell and set(args.cell) - {c["id"] for c in cells}:
        parser.error("unknown diagnostic cell ID")
    meta = {"timestamp": datetime.now(timezone.utc).isoformat(), "lane": args.lane,
            "os": platform.system().lower(), "inventory_sha256": digest(args.inventory),
            "hrx_sha256": digest(args.hrx) if Path(args.hrx).is_file() else None,
            "commit": subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, capture_output=True, text=True).stdout.strip(),
            "evidence_valid_until": datetime.fromtimestamp(time.time() + 86400, timezone.utc).isoformat(),
            "freshness_seconds": 86400,
            "backend_observability": "requested gateway route; backend failover not independently certified"}
    results = [{**cell, "selected": cell["id"] in args.cell if args.cell else args.lane != "canary" or cell["canary"],
                "status": "NOT_RUN", "reason": "not_selected_by_lane"} for cell in cells]
    scenarios = scenario_rows(inventory, args.lane, args.cell)
    resumes = resume_rows(inventory, args.lane, args.cell)
    finalization = {"id": "run-finalization", "required": True, "selected": True,
                    "status": "NOT_RUN", "reason": "run_not_finalized"}
    results.append({"id": "mixed-pitput-conversation", "required": True,
                    "selected": not args.skip_conversation,
                    "status": "NOT_RUN", "reason": "conversation_not_started"})
    gateway = None
    try:
        upstream = os.environ.get("HRX_CI_GATEWAY", "").rstrip("/")
        if not upstream:
            raise Blocked("missing_gateway_origin")  # set HRX_CI_GATEWAY, e.g. https://gateway.example.com
        parsed = urllib.parse.urlsplit(upstream)
        if parsed.scheme != "https" or parsed.path or parsed.query or parsed.fragment or parsed.username:
            raise Blocked("invalid_https_gateway_origin")
        catalog = json.loads(args.catalog.read_text()) if args.catalog else json.load(
            urllib.request.build_opener(NoRedirect()).open(upstream + "/dashboard/models.json", timeout=15))
        meta["catalog_sha256"] = hashlib.sha256(json.dumps(catalog, sort_keys=True).encode()).hexdigest()
        inventory = resolve_latest_families(inventory, catalog)
        cells = inventory["cells"]
        meta["resolved_inventory_sha256"] = hashlib.sha256(
            json.dumps(inventory, sort_keys=True).encode()).hexdigest()
        for cell, result in zip(cells, results):
            result.update(cell)
            if not result["selected"]:
                continue
            if cell.get("resolution_status") in ("BLOCKED", "UNSUPPORTED"):
                result.update(status=cell["resolution_status"], reason=cell["resolution_reason"])
                continue
            source = capabilities(catalog, cell)
            if source is None:
                result.update(status="UNSUPPORTED", reason="required_route_capability_absent")
            else:
                result["resolved_source"] = source
        if args.inventory_only:
            raise Blocked("inventory_only_no_live_evidence")
        policy = policy_from_file(args.policy)
        meta["config_sha256"] = digest(args.policy)
        if args.lane == "nightly" and not args.cell:
            discovered = select_exploratory_opencode(catalog, cells, policy["routes"])
            meta["exploratory_omissions"] = discovered["omitted"]
            for cell in discovered["cells"]:
                cells.append(cell)
                results.insert(-1, {**cell, "selected": True, "required": False,
                                    "status": "NOT_RUN", "reason": "not_started"})
        key_file = os.environ.get("HRX_CI_KEY_FILE")
        if not key_file:
            raise Blocked("dedicated_ci_key_missing")
        if Path(key_file).stat().st_mode & 0o077:
            raise Blocked("dedicated_ci_key_file_not_private")
        key = Path(key_file).read_text().strip()
        meta["budget"] = verify_key(upstream, key, policy)
        if not Path(args.hrx).is_file():
            raise Blocked("hrx_artifact_missing")
        meta["native_versions"] = {}
        for harness in ("claude", "codex", "opencode"):
            if shutil.which(harness):
                meta["native_versions"][harness] = subprocess.run([harness, "--version"], capture_output=True,
                                                                text=True, timeout=10).stdout.strip()[:100]
        with tempfile.TemporaryDirectory(prefix="hrx-live-ci-") as temp:
            root = Path(temp).resolve()
            lease_file = os.environ.get("HRX_CI_CLAUDE_LEASE_FILE")
            try:
                claude_lease = load_claude_lease(lease_file, upstream) if lease_file else None
            except (OSError, ValueError):
                raise Blocked("claude_lease_invalid_expired_or_wrong_scope") from None
            gateway = Gateway(root, upstream, key, catalog, policy, claude_lease)
            runner = Runner(args.hrx, gateway, policy)
            for cell, result in zip(cells, results):
                if result["status"] in ("UNSUPPORTED", "BLOCKED") or not result["selected"]:
                    continue
                if cell["route"] not in policy["routes"]:
                    result.update(status="BLOCKED", reason="route_not_authorized_by_budget_policy")
                elif cell["harness"] not in meta["native_versions"]:
                    result.update(status="BLOCKED", reason="native_binary_missing")
                elif time.monotonic() >= gateway.deadline:
                    result.update(status="NOT_RUN", reason="aggregate_deadline")
                else:
                    result.pop("reason", None)
                    gateway.allowed_routes = {cell["route"]}
                    cell_started = time.monotonic()
                    result.update(runner.cell(cell, root / cell["id"]))
                    result["attempts"] = 1
                    # A diagnostic retry never turns the original failing run
                    # green, and shares the same aggregate key/deadline limits.
                    if (retry_safe(result) and
                            gateway.admitted < policy["max_requests"] and
                            gateway.deadline - time.monotonic() >= policy["cell_seconds"]):
                        retry = runner.cell(cell, root / (cell["id"] + "-diagnostic"))
                        result["diagnostic_retry"] = retry
                        result["attempts"] = 2
                        result["flaky"] = retry["status"] == "PASS"
                        if retry.get("cleanup") == "unknown":
                            raise Blocked("native_cleanup_unverified")
                    result["duration_seconds"] = round(time.monotonic() - cell_started, 3)
                report(args.output, results + scenarios + resumes + [finalization], meta)
                if result.get("cleanup") == "unknown":
                    raise Blocked("native_cleanup_unverified")
            run_scenarios(scenarios, runner, inventory, cells, catalog, root)
            run_resumes(resumes, runner, cells, catalog, root, capabilities, meta["native_versions"])
            report(args.output, results + scenarios + resumes + [finalization], meta)
            # Mixed conversation remains independently required; per-cell
            # inference cannot stand in for cross-agent communication evidence.
            from conversation import conversation
            gateway.allowed_routes = {c["route"] for c in cells if c["id"] in inventory["conversation"]}
            if args.skip_conversation:
                results[-1].update(selected=False, reason="explicit_diagnostic_omission")
            else:
                results[-1] = conversation(runner, inventory, cells, catalog, root)
            finalize_gateway(gateway, meta)
            gateway = None
    except Blocked as exc:
        finalization.update(status="BLOCKED", reason=str(exc))
        for result in results + scenarios + resumes:
            if result["status"] == "NOT_RUN" and result.get("selected", True):
                result.update(status="BLOCKED", reason=str(exc))
    except Exception as exc:
        finalization.update(status="FAIL", reason="infrastructure_" + type(exc).__name__)
        # Exception messages can include auth headers, model payloads or URLs.
        for result in results + scenarios + resumes:
            if result["status"] == "NOT_RUN" and result.get("selected", True):
                result.update(status="BLOCKED", reason="infrastructure_" + type(exc).__name__)
    finally:
        if gateway:
            meta["inference_request_count"] = gateway.admitted
            meta["admission_attempt_count"] = len(gateway.requests)
            try:
                finalize_gateway(gateway, meta)
            except Exception as exc:
                finalization.update(status="FAIL", reason="gateway_cleanup_" + type(exc).__name__)
    # A retry can release resources, but cannot erase an earlier run failure.
    if finalization["status"] == "NOT_RUN":
        finalization.update(status="PASS")
        finalization.pop("reason", None)
    if args.lane == "release":
        for dimension in inventory["release_dimensions"]:
            if dimension != meta["os"]:
                results.append({"id": "release-os-" + dimension, "required": True,
                                "status": "NOT_RUN", "reason": "separate_native_runner_evidence_required"})
    try:
        return 0 if report(args.output, results + scenarios + resumes + [finalization], meta) else 1
    except OSError:
        # Storage failure may prevent a new failure artifact; never certify it.
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
