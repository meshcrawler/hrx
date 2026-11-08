"""Opt-in broker/glab/git scenario against a disposable GitLab CI project.

HRX launches a scripted stand-in for the harness with --gitlab, so the real
broker retrieval, glab environment and Git credential helper are exercised
without inference. The stand-in proves service-account identity, rejected bad
credentials, private noninteractive clone, branch push with attribution, and a
resumed launch on the same account, then deletes only its own branches (and
stale branches under the same prefix) in that project.

The controller never sees the GitLab token. Missing fixture configuration is
BLOCKED, never a pass. Only a project carrying the disposable topic is touched.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sys
import time

from owned_capture import capture

FIXTURE_ENV = "HRX_BROKER_SCENARIO_FIXTURE"
EVIDENCE_RESERVE = 10  # Seconds kept after stand-in commands for saving evidence and teardown.
DISPOSABLE_TOPIC = "hrx-disposable-ci"
DEFAULT_PREFIX = "hrx-ci-broker/"

# Runs as the "harness" inside HRX's credentialed child environment. It only
# records parsed facts; tokens and raw command output never reach its file.
STAND_IN = r'''#!PYTHON
import hashlib, json, os, pathlib, subprocess, sys, tempfile, time, urllib.parse
spec = json.loads(os.environ["HRX_BROKER_SCENARIO_SPEC"])
out = pathlib.Path(os.environ["HRX_BROKER_SCENARIO_EVIDENCE"])
facts = {"mode": spec["mode"], "resume_argument": "--resume" in sys.argv,
         "host": os.environ.get("GITLAB_HOST"), "token_present": bool(os.environ.get("GITLAB_TOKEN")),
         "noninteractive": os.environ.get("GIT_TERMINAL_PROMPT") == "0" and os.environ.get("GLAB_NO_PROMPT") == "true"}

def save():
    out.write_text(json.dumps(facts))
    out.chmod(0o600)

# The controller's phase budget minus its evidence/teardown reserve: no command may
# outlive it, so the stand-in always has time to save its facts.
STOP = time.monotonic() + spec.get("budget_seconds", 10 ** 9)

def run(args, env=None, cwd=None, until=None):
    """Exit code and stdout; 124 when the command would or did exceed its limit."""
    limit = min(STOP, until if until is not None else STOP)
    remaining = min(60, limit - time.monotonic())
    if remaining <= 0:
        return 124, ""
    try:
        done = subprocess.run(args, env=env, cwd=cwd, stdin=subprocess.DEVNULL, capture_output=True,
                              text=True, timeout=remaining)
    except subprocess.TimeoutExpired:
        return 124, ""
    return done.returncode, done.stdout

def api(path, method="GET", until=None):
    code, text = run(["glab", "api", "--method", method, path], until=until)
    diagnostic = {"method": method, "exit_code": code, "response_bytes": len(text.encode()),
                  "response_kind": "command_error"}
    if "cleanup_api" in facts and len(facts["cleanup_api"]) < 12:
        facts["cleanup_api"].append(diagnostic)
    if code:
        return None
    try:
        parsed = json.loads(text) if text.strip() else {}
        diagnostic["response_kind"] = type(parsed).__name__ if text.strip() else "empty_success"
        if isinstance(parsed, list):
            diagnostic["items"] = len(parsed)
        return parsed
    except ValueError:
        diagnostic["response_kind"] = "invalid_json"
        return None

def branch_path(name):
    return "projects/%d/repository/branches/%s" % (spec["project_id"], urllib.parse.quote(name, safe=""))

def scoped_branches(until=None):
    branches = []
    # Only a complete successful listing proves absence. A failed lookup is
    # ambiguous (404, expired auth, network failure), never cleanup evidence.
    for page in range(1, 101):
        # GitLab's search-filtered listing can retain deleted branches. Enumerate
        # the private disposable project and apply ownership filtering locally.
        batch = api("projects/%d/repository/branches?per_page=100&page=%d"
                    % (spec["project_id"], page), until=until)
        if not isinstance(batch, list):
            return None
        if not batch:
            return branches
        branches.extend(batch)
    return None

def http(path, until=None):
    """GET through glab with the response status and headers (`glab api -i` prints
    them to stdout even for errors). status is None when no status line parsed."""
    code, text = run(["glab", "api", "-i", path], until=until)
    head, _, body = text.replace("\r\n", "\n").partition("\n\n")
    lines = head.splitlines()
    status = None
    if lines and lines[0].startswith("HTTP/"):
        parts = lines[0].split()
        status = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else None
    headers = {}
    for line in lines[1:]:
        key, sep, value = line.partition(":")
        if sep:
            headers[key.strip().lower()] = value.strip()
    return code, status, headers, body

def digest(value):
    return hashlib.sha256(value.encode()).hexdigest()[:12] if value else None

def listing_meta(until):
    """Which layer is stale: HTTP caching metadata and a body digest, values hashed."""
    code, status, headers, body = http("projects/%d/repository/branches?per_page=100&page=1" % spec["project_id"],
                                       until)
    return {"exit_code": code, "status": status, "etag": digest(headers.get("etag")), "age": headers.get("age"),
            "cache_control": (headers.get("cache-control") or "")[:80] or None,
            "cf_cache_status": (headers.get("cf-cache-status") or "")[:20] or None, "body": digest(body)}

def absence(name, until):
    """Two independent reads per target, each within the verification budget. Git's
    ref advertisement must exit 0 listing nothing; a cache-busted single-branch GET
    must pair exit 1 with HTTP 404 (glab -i). Exit 0 with 200 is present. Any other
    pairing, a timeout (124) or an unparsed status is unknown, never absence."""
    code, out = run(["git", "ls-remote", "--heads", url, "refs/heads/" + name], until=until)
    git = ("absent" if code == 0 and not out.strip() else "present" if code == 0 else "unknown")
    code, status, _, _ = http(branch_path(name) + "?hrx_nocache=" + os.urandom(8).hex(), until)
    rest = ("absent" if (code, status) == (1, 404) else "present" if (code, status) == (0, 200)
            else "unknown")
    return git, rest

def cleanup():
    facts["cleanup_verified"] = False
    facts["cleanup_api"] = []
    existing = scoped_branches()
    if existing is None:
        facts["cleanup_stage"] = "initial_listing_unverified"
        return
    facts["cleanup_initial_branch_count"] = len(existing)
    targets = set(spec["cleanup"])
    for branch in existing:
        name = branch.get("name", "")
        committed = branch.get("commit", {}).get("committed_date", "")
        if name.startswith(spec["prefix"]) and committed and committed < spec["stale_before"]:
            targets.add(name)
    removed = True
    for branch in existing:
        name = branch.get("name", "")
        if name in targets and api(branch_path(name), "DELETE") is None:
            removed = False
    # The REST branch list can stay stale after successful deletes (job 4345: eight
    # identical listings). Each target must be absent in BOTH independent sources
    # within the verification budget; the list is a recorded diagnostic only.
    deadline = min(time.monotonic() + spec.get("verify_seconds", 30), STOP)
    attempts, results, on_time = [], {}, False
    while removed:
        results = {name: absence(name, deadline) for name in sorted(targets)}
        # Evidence completed after the deadline is late and never accepted.
        on_time = time.monotonic() <= deadline
        listed = scoped_branches(deadline)
        attempts.append({"git_absent": sum(r[0] == "absent" for r in results.values()),
                         "rest_404": sum(r[1] == "absent" for r in results.values()),
                         "unknown": sum("unknown" in r for r in results.values()),
                         "on_time": on_time,
                         "listed_targets": (sum(b.get("name") in targets for b in listed)
                                            if listed is not None else None),
                         "listing": listing_meta(deadline)})
        done = on_time and all(r == ("absent", "absent") for r in results.values())
        # An error or lateness is final at once; only a consistent "present" may converge.
        if done or attempts[-1]["unknown"] or not on_time or time.monotonic() >= deadline:
            break
        time.sleep(min(1, max(0, deadline - time.monotonic())))
    facts["cleanup_deletes_verified"] = removed
    facts["cleanup_target_count"] = len(targets)
    facts["cleanup_verification_attempts"] = len(attempts)
    facts["cleanup_attempts"] = attempts[:3] + attempts[3:][-5:]
    facts["cleanup_listing_converged"] = bool(attempts) and attempts[-1]["listed_targets"] == 0
    facts["cleanup_stage"] = "verification"
    facts["cleanup_verification_on_time"] = on_time
    facts["cleanup_verified"] = bool(removed and (not targets or (results and on_time and
                                     all(r == ("absent", "absent") for r in results.values()))))

user = api("user")
facts["user"] = {k: user.get(k) for k in ("id", "username", "name")} if isinstance(user, dict) else None
bad = dict(os.environ, GITLAB_TOKEN="hrx-invalid-" + os.urandom(12).hex())
facts["invalid_token_rejected"] = run(["glab", "api", "user"], env=bad)[0] != 0
project = api("projects/%d" % spec["project_id"])
facts["project_verified"] = isinstance(project, dict) and project.get("path_with_namespace") == spec["project_path"] \
    and DISPOSABLE_TOPIC in (project.get("topics") or []) and project.get("visibility") == "private" \
    and not project.get("archived")
if not facts["project_verified"] or not facts["user"]:
    save()
    sys.exit(3)
url = "https://%s/%s.git" % (spec["host"], spec["project_path"])
if spec["mode"] == "janitor":
    cleanup()
    save()
    sys.exit(0)
anonymous = {k: v for k, v in os.environ.items() if not k.startswith("GIT_CONFIG_")}
anonymous.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
facts["anonymous_rejected"] = run(["git", "-c", "credential.helper=", "ls-remote", url], env=anonymous)[0] != 0
work = tempfile.mkdtemp(prefix="hrx-broker-", dir=out.parent)
repo = os.path.join(work, "repo")
facts["clone"] = run(["git", "clone", "--depth", "1", url, repo])[0] == 0
if facts["clone"]:
    name, email = facts["user"]["name"], "%s@noreply.%s" % (facts["user"]["username"], spec["host"])
    run(["git", "switch", "-c", spec["branch"]], cwd=repo)
    pathlib.Path(repo, "hrx-broker-scenario.txt").write_text(spec["branch"] + "\n")
    run(["git", "add", "hrx-broker-scenario.txt"], cwd=repo)
    committed = run(["git", "-c", "user.name=" + name, "-c", "user.email=" + email, "commit", "-m",
                     "HRX broker scenario " + spec["branch"]], cwd=repo)[0] == 0
    sha = run(["git", "rev-parse", "HEAD"], cwd=repo)[1].strip()
    facts["push"] = committed and run(["git", "push", "-o", "ci.skip", "origin", spec["branch"]], cwd=repo)[0] == 0
    remote = api(branch_path(spec["branch"])) if facts["push"] else None
    commit = (remote or {}).get("commit", {})
    facts["remote_commit_matches"] = bool(sha) and commit.get("id") == sha
    facts["commit_author_matches"] = commit.get("author_name") == name
    facts["push_event_author"] = None
    for _ in range(10 if facts["push"] else 0):
        events = api("projects/%d/events?action=pushed&per_page=50" % spec["project_id"]) or []
        for event in events:
            if (event.get("push_data") or {}).get("ref") == spec["branch"]:
                facts["push_event_author"] = event.get("author_id")
        if facts["push_event_author"] is not None:
            break
        time.sleep(1.5)
if spec["mode"] == "resume":
    cleanup()
save()
'''


class FixtureBlocked(Exception):
    pass


def load_fixture(path=None):
    """Validate the explicit, private fixture description; never guess one."""
    path = path or os.environ.get(FIXTURE_ENV)
    if not path:
        raise FixtureBlocked("broker_fixture_config_missing")
    path = Path(path)
    if not path.is_file() or path.stat().st_mode & 0o077:
        raise FixtureBlocked("broker_fixture_config_missing_or_not_private")
    try:
        fixture = json.loads(path.read_text())
    except ValueError:
        raise FixtureBlocked("broker_fixture_config_invalid") from None
    host, project = fixture.get("gitlab_host"), fixture.get("project_path")
    if not isinstance(host, str) or not re.fullmatch(r"[A-Za-z0-9.-]+(:\d+)?", host):
        raise FixtureBlocked("broker_fixture_host_invalid")
    if not isinstance(project, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+", project) or \
            "disposable" not in project.rsplit("/", 1)[-1]:
        raise FixtureBlocked("broker_fixture_project_not_disposable")
    if isinstance(fixture.get("project_id"), bool) or not isinstance(fixture.get("project_id"), int) or \
            fixture["project_id"] <= 0:
        raise FixtureBlocked("broker_fixture_project_id_invalid")
    if not isinstance(fixture.get("identity"), str) or not re.fullmatch(r"[A-Za-z0-9_.:-]+(/[A-Za-z0-9_.:-]+){2}",
                                                                         fixture["identity"]):
        raise FixtureBlocked("broker_fixture_identity_invalid")
    broker = fixture.get("broker_url")
    if not isinstance(broker, str) or not re.fullmatch(r"https://[^/?#@]+/?|http://127\.0\.0\.1:\d+/?", broker):
        raise FixtureBlocked("broker_fixture_url_invalid")
    token = Path(str(fixture.get("client_token_file", "")))
    if not fixture.get("client_token_file") or not token.is_file() or token.stat().st_mode & 0o077:
        raise FixtureBlocked("broker_fixture_client_token_missing_or_not_private")
    prefix = fixture.get("branch_prefix", DEFAULT_PREFIX)
    if not isinstance(prefix, str) or not re.fullmatch(r"hrx-ci-[a-z0-9-]+/", prefix):
        raise FixtureBlocked("broker_fixture_branch_prefix_invalid")
    stale = fixture.get("stale_hours", 6)
    if isinstance(stale, bool) or not isinstance(stale, (int, float)) or stale < 1:
        raise FixtureBlocked("broker_fixture_stale_hours_invalid")
    return {**fixture, "branch_prefix": prefix, "stale_hours": stale, "broker_url": broker.rstrip("/")}


def private(path, text, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(text)
    path.chmod(mode)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None


def evaluate(fixture, initial, resumed, continuity):
    """Pure judgement over stand-in facts; returns (status, reason)."""
    for label, facts in (("initial", initial), ("resume", resumed)):
        if facts is None:
            return "FAIL", label + "_harness_not_reached"
        if not facts.get("token_present") or facts.get("host") != fixture["gitlab_host"]:
            return "FAIL", label + "_credential_not_injected"
        if not facts.get("noninteractive"):
            return "FAIL", label + "_interactive_prompts_possible"
        if not facts.get("user") or not isinstance(facts["user"].get("id"), int):
            return "FAIL", label + "_service_account_unverified"
        if not facts.get("invalid_token_rejected"):
            return "FAIL", label + "_invalid_token_not_rejected"
        if not facts.get("project_verified"):
            return "FAIL", label + "_project_not_disposable"
        if not facts.get("anonymous_rejected"):
            return "FAIL", label + "_anonymous_access_not_rejected"
        if not facts.get("clone"):
            return "FAIL", label + "_clone"
        if not (facts.get("push") and facts.get("remote_commit_matches")):
            return "FAIL", label + "_push"
        if not facts.get("commit_author_matches") or facts.get("push_event_author") != facts["user"]["id"]:
            return "FAIL", label + "_attribution"
    if not resumed.get("resume_argument"):
        return "FAIL", "resume_not_requested"
    if resumed["user"] != initial["user"]:
        return "FAIL", "resume_account_changed"
    if not continuity.get("client_token_unchanged") or continuity.get("pending_enrollment"):
        return "FAIL", "resume_reenrolled"
    if not resumed.get("cleanup_verified"):
        return "FAIL", "cleanup_unverified"
    return "PASS", None


def broker_scenario(runner, cell, root, fixture=None, timeout=180, verify_seconds=30):
    """Run the opt-in scenario; returns sanitized PASS/FAIL/BLOCKED evidence.

    runner is run.Runner; cell supplies the harness name and a route the relay
    authorizes (no inference occurs). fixture defaults to load_fixture().
    """
    result = {"id": "broker-glab-git-" + cell["harness"], "required": True}
    try:
        fixture = fixture or load_fixture()
    except FixtureBlocked as exc:
        return {**result, "status": "BLOCKED", "reason": str(exc)}
    if not sys.platform.startswith("linux"):
        return {**result, "status": "BLOCKED", "reason": "linux_subreaper_required"}
    if not Path(runner.hrx).is_file():
        return {**result, "status": "BLOCKED", "reason": "hrx_artifact_missing"}
    if cell["route"] not in runner.policy["routes"]:
        return {**result, "status": "BLOCKED", "reason": "route_not_authorized_by_budget_policy"}
    for tool in ("glab", "git"):
        if not shutil.which(tool):
            return {**result, "status": "BLOCKED", "reason": tool + "_missing"}
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    home, project = root / "home", root / "project"
    project.mkdir(parents=True, exist_ok=True, mode=0o700)
    env = runner.configure(home, project, cell)
    config = home / ".config/hrx/config.toml"
    private(config, config.read_text() + '[plugins.gitlab-service-account-broker]\nurl = "' +
            fixture["broker_url"] + '"\nclient_tokens_dir = "client-tokens"\n')
    token = home / ".config/hrx/client-tokens" / fixture["identity"] / "broker.token"
    for parent in reversed(token.relative_to(home / ".config/hrx").parents):
        (home / ".config/hrx" / parent).mkdir(mode=0o700, exist_ok=True)
    private(token, Path(fixture["client_token_file"]).read_text())
    stand_in = home / "stand-in" / cell["harness"]
    private(stand_in, STAND_IN.replace("PYTHON", shutil.which("python3") or "/usr/bin/python3")
            .replace("DISPOSABLE_TOPIC", json.dumps(DISPOSABLE_TOPIC)), 0o700)
    env["PATH"] = str(stand_in.parent) + os.pathsep + env["PATH"]
    run_id = time.strftime("%Y%m%d%H%M%S", time.gmtime()) + "-" + secrets.token_hex(3)
    branches = [fixture["branch_prefix"] + run_id + "-" + phase for phase in ("initial", "resume")]
    stale_before = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() - fixture["stale_hours"] * 3600))
    deadline = time.monotonic() + timeout
    session = "hrx-broker-" + run_id
    result["process_phases"] = {}

    def launch(mode, branch, resume=False):
        evidence = root / (mode + ".json")
        # Janitor performs scoped cleanup only; it gets a separate bounded window.
        phase_deadline = time.monotonic() + 30 if mode == "janitor" else deadline
        spec = {"mode": mode, "branch": branch, "cleanup": branches, "prefix": fixture["branch_prefix"],
                "stale_before": stale_before, "host": fixture["gitlab_host"], "project_id": fixture["project_id"],
                "project_path": fixture["project_path"], "verify_seconds": verify_seconds,
                # Every stand-in command must finish before this, leaving the reserve
                # for saving evidence and supervised teardown.
                "budget_seconds": round(max(1, phase_deadline - time.monotonic() - EVIDENCE_RESERVE), 3)}
        args = [runner.hrx, cell["harness"], "--model", cell["route"], "--gitlab",
                "--agent-identity", fixture["identity"], "--no-yolo"]
        if resume:
            args += ["--resume", session]
        args += ["--", "-p", "broker scenario"]
        child = dict(env, HRX_BROKER_SCENARIO_SPEC=json.dumps(spec), HRX_BROKER_SCENARIO_EVIDENCE=str(evidence))
        captured = capture(args, cwd=project, env=child, deadline=phase_deadline,
                           abort=lambda: False, capture_stderr=True)
        result["process_phases"][mode] = {key: captured.get(key) for key in
                                        ("cleanup", "exit_code", "timed_out", "supervisor_exit",
                                         "supervisor_summary")}
        try:
            facts = json.loads(evidence.read_text()) if evidence.is_file() else None
        except (OSError, ValueError):
            facts = None
        if captured["cleanup"] != "clean":
            result["status"] = "FAIL"
            result.setdefault("reason", mode + "_process_cleanup_" + captured["cleanup"])
            return facts, None
        if captured["timed_out"]:
            result["status"] = "FAIL"
            result.setdefault("reason", mode + "_process_timeout")
            return facts, None
        stderr = captured["stderr"]
        # HRX diagnostics never contain credentials; only a category is kept.
        refused = re.search(rb"GitLab broker (request failed|refused credentials \(HTTP (\d+)\))", stderr)
        if captured["exit_code"] != 0 and not refused:
            result["status"] = "FAIL"
            reason = mode + "_process_exit"
            if captured["exit_code"] == 3 and facts:
                if facts.get("project_verified") is False:
                    reason = mode + "_project_not_disposable"
                elif not facts.get("user"):
                    reason = mode + "_service_account_unverified"
            result.setdefault("reason", reason)
            return facts, None
        return facts, refused

    before = digest(token)
    initial = resumed = None
    try:
        initial, refused = launch("initial", branches[0])
        if result.get("status") == "FAIL":
            return result
        if initial is None and refused:
            status = refused.group(2)
            reason = "broker_unavailable" if not status or status.startswith(b"5") else "broker_refused_credentials"
            return {**result, "status": "BLOCKED", "reason": reason}
        if initial and initial.get("project_verified"):
            resumed, _ = launch("resume", branches[1], resume=True)
    finally:
        pushed = initial and initial.get("push")
        owned = all(p.get("cleanup") == "clean" for p in result["process_phases"].values())
        timed_out = any(p.get("timed_out") for p in result["process_phases"].values())
        cleaned = bool(resumed and resumed.get("cleanup_verified"))
        if not owned:
            result["janitor_skipped"] = "process_ownership_not_clean"
            cleaned = False
        elif (pushed or timed_out) and not cleaned:
            janitor, _ = launch("janitor", branches[0])
            result["janitor_cleanup_verified"] = bool(
                janitor and janitor.get("cleanup_verified") and
                result["process_phases"]["janitor"]["cleanup"] == "clean" and
                result["process_phases"]["janitor"]["exit_code"] == 0 and
                not result["process_phases"]["janitor"]["timed_out"])
            cleaned = result["janitor_cleanup_verified"]
        if not cleaned:
            result["owned_branch_candidates"] = branches
            result["initial_push_recorded"] = bool(pushed)
    if result.get("status") == "FAIL":
        return result
    continuity = {"client_token_unchanged": before is not None and digest(token) == before,
                  "pending_enrollment": token.with_name("broker.token.pending").exists()}
    status, reason = evaluate(fixture, initial, resumed, continuity)
    account = (initial or {}).get("user") or {}
    result.update(status=status, service_account_id=account.get("id"),
                  phases={label: {k: v for k, v in (facts or {}).items() if k not in ("user", "host")}
                          for label, facts in (("initial", initial), ("resume", resumed))}, **continuity)
    if reason:
        result["reason"] = reason
    return result
