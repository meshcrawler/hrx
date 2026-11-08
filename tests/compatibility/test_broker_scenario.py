"""Deterministic coverage for broker_scenario: loopback broker, fake GitLab, no real accounts."""
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

from broker_scenario import FixtureBlocked, broker_scenario, evaluate, load_fixture
from owned_capture import capture
from gateway import Gateway
from run import Runner
from test_auto_scenario import LAUNCH, PARSER, REPO, Upstream, catalog

HOST, PROJECT, PROJECT_ID, IDENTITY = "gitlab.fixture.test", "ci/hrx-ci-disposable-broker", 42, "ci/hrx-broker/agent"
OLD = "2020-01-01T00:00:00.000Z"

FAKE_GLAB = r'''#!PYTHON
import json, os, sys, urllib.parse
STATE = "STATE_PATH"
state = json.load(open(STATE))
args = sys.argv[1:]
include = "-i" in args
args = [a for a in args if a != "-i"]
method = "GET"
if "--method" in args:
    method = args[args.index("--method") + 1]
path = args[-1]
def respond(code, body):
    # Like glab api -i: status line and headers on stdout even for errors.
    if include:
        sys.stdout.write("HTTP/2.0 %d X\r\nCache-Control: max-age=0, private\r\nCf-Cache-Status: DYNAMIC\r\n"
                         "Etag: W/\"%s\"\r\n\r\n" % (code, abs(hash(body)) % 10**8))
    sys.stdout.write(body + "\n")
    sys.exit(0 if code < 400 else 1)
user = state["tokens"].get(os.environ.get("GITLAB_TOKEN", ""))
if state["mode"] == "token_ignored":
    user = user or next(iter(state["tokens"].values()), None)
if user is None or os.environ.get("GITLAB_HOST") != "HOST_VALUE" or args[0] != "api":
    sys.exit(1)
def save():
    json.dump(state, open(STATE, "w"))
base = "projects/%d" % PROJECT_ID_VALUE
if path == "user":
    print(json.dumps(user))
elif path == base:
    print(json.dumps({"path_with_namespace": "PROJECT_VALUE", "topics": ["hrx-disposable-ci"],
                      "visibility": "public" if state["mode"] == "public_project" else "private", "archived": False}))
elif path.startswith(base + "/repository/branches/"):
    name = urllib.parse.unquote(path.rsplit("/", 1)[1].split("?", 1)[0])
    if state["mode"] == "cleanup_network_failure" and state.get("deleted") and method == "GET":
        sys.exit(1)  # Transport failure: no status line, no body.
    if state["mode"] == "single_errors" and state.get("deleted") and method == "GET":
        respond(500, '{"message":"500 Internal Server Error"}')
    if state["mode"] == "slow_single" and state.get("deleted") and method == "GET":
        import time; time.sleep(1.2)
    if state["mode"] == "mismatched_status" and state.get("deleted") and method == "GET" and include \
            and name not in state["branches"]:
        # A 404 status line with a success exit is an inconsistent pairing.
        sys.stdout.write("HTTP/2.0 404 X\r\n\r\n{}\n")
        sys.exit(0)
    if name not in state["branches"]:
        respond(404, '{"message":"404 Branch Not Found"}')
    if method == "DELETE":
        if state["mode"] == "delete_fails" or (state["mode"] == "stale_delete_fails" and name.endswith("/stale")):
            sys.exit(1)
        state.setdefault("deleted_branches", []).append({"name": name, "commit": state["branches"][name]})
        del state["branches"][name]
        state["deleted"] = True
        save()
    else:
        respond(200, json.dumps({"name": name, "commit": state["branches"][name]}))
elif path.startswith(base + "/repository/branches?"):
    if state["mode"] == "cleanup_auth_failure" or (state["mode"] == "listing_fails_after_delete" and state.get("deleted")):
        sys.exit(1)
    page = int(urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)["page"][0])
    items = [{"name": n, "commit": c} for n, c in state["branches"].items()]
    if state["mode"] == "stale_search" and "search=" in path:
        items = state.setdefault("search_snapshot", items)
        save()
    if state["mode"] == "slow_listing" and state.get("deleted"):
        import time; time.sleep(3)
    if state["mode"] == "stale_listing" and state.get("deleted"):
        # Job 4345: complete listings kept returning the pre-delete snapshot.
        items += state.get("deleted_branches", [])
    if state["mode"] == "delayed_visibility" and state.get("deleted"):
        if page == 1:
            state["show_deleted"] = state.get("visibility_polls", 0) < 2
            state["visibility_polls"] = state.get("visibility_polls", 0) + 1
            save()
        if state.get("show_deleted"):
            items += state.get("deleted_branches", [])
    # Small pages deliberately exercise pagination rather than first-page absence.
    respond(200, json.dumps(items[(page - 1) * 2:page * 2]))
elif path.startswith(base + "/events?"):
    print(json.dumps(state["events"]))
else:
    sys.exit(1)
'''

FAKE_GIT = r'''#!PYTHON
import json, os, subprocess, sys
STATE, REAL = "STATE_PATH", "REAL_GIT"
args = sys.argv[1:]
options = []
while args and args[0] == "-c":
    options += args[:2]
    args = args[2:]
command = args[0] if args else ""
if command not in ("clone", "ls-remote", "push"):
    os.execv(REAL, [REAL] + sys.argv[1:])
state = json.load(open(STATE))
fill = subprocess.run([REAL] + options + ["credential", "fill"], input="protocol=https\nhost=HOST_VALUE\n\n",
                      capture_output=True, text=True, stdin=None)
password = dict(line.split("=", 1) for line in fill.stdout.splitlines() if "=" in line).get("password")
user = state["tokens"].get(password)
if fill.returncode or user is None:
    sys.exit(128)
if command == "ls-remote":
    if state["mode"] in ("ls_remote_fails", "cleanup_git_auth_failure") and state.get("deleted"):
        sys.exit(128)
    if state["mode"] == "slow_ls_remote" and state.get("deleted"):
        import time; time.sleep(3)
    refs = [a for a in args if a.startswith("refs/heads/")]
    for ref in refs:
        name = ref[len("refs/heads/"):]
        if name in state["branches"]:
            print(state["branches"][name]["id"] + "\t" + ref)
    sys.exit(0)
if command == "clone":
    if args[-2] != "https://HOST_VALUE/PROJECT_VALUE.git":
        sys.exit(128)
    target = args[-1]
    subprocess.run([REAL, "init", "-q", "-b", "main", target], check=True)
    subprocess.run([REAL, "-c", "user.name=seed", "-c", "user.email=seed@example.test", "commit", "-q",
                    "--allow-empty", "-m", "seed"], cwd=target, check=True)
    sys.exit(0)
if state["mode"] == "push_rejected":
    sys.exit(1)
branch = args[-1]
sha = subprocess.run([REAL, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
author = subprocess.run([REAL, "log", "-1", "--format=%an"], capture_output=True, text=True).stdout.strip()
state["branches"][branch] = {"id": sha, "author_name": author, "committed_date": "2099-01-01T00:00:00.000Z"}
author_id = user["id"] + (1 if state["mode"] == "wrong_author" else 0)
state["events"].insert(0, {"author_id": author_id, "push_data": {"ref": branch}})
json.dump(state, open(STATE, "w"))
'''


class FakeBroker:
    def __init__(self, client_secret, state_path):
        self.mode, self.calls, self.secret, self.state_path = "honest", 0, client_secret, state_path
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def reply(self, status, data=None):
                raw = json.dumps(data or {}).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                owner.calls += 1
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path != "/v1/credentials" or body.get("identity") != IDENTITY:
                    return self.reply(404)
                if owner.mode == "down" or (owner.mode == "second_down" and owner.calls == 2):
                    return self.reply(503)
                if self.headers.get("Authorization") != "Bearer " + owner.secret:
                    return self.reply(401)
                changed = owner.mode == "account_changes" and owner.calls > 1
                user = {"id": 9 if changed else 7, "username": "svc_other" if changed else "svc_ci",
                        "name": "HRX other" if changed else "HRX ci/hrx-broker/agent"}
                token = "glpat-fixture-" + user["username"]
                state = json.loads(owner.state_path.read_text())
                state["tokens"][token] = user
                owner.state_path.write_text(json.dumps(state))
                self.reply(200, {"identity": IDENTITY, "username": user["username"], "user_id": user["id"],
                                 "token": token, "host": HOST, "expires_at": "2099-01-01", "scopes": ["api"],
                                 "profile": "developer", "ssh_hosts": []})

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def fixture_file(directory, **overrides):
    token = directory / "client.token"
    token.write_text("fixture-client-secret-" + "x" * 32)
    token.chmod(0o600)
    data = {"gitlab_host": HOST, "project_id": PROJECT_ID, "project_path": PROJECT, "identity": IDENTITY,
            "broker_url": "http://127.0.0.1:1", "client_token_file": str(token)}
    data.update(overrides)
    path = directory / "fixture.json"
    path.write_text(json.dumps(data))
    path.chmod(0o600)
    return path


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.temp = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.temp, True)

    def test_missing_or_unsafe_fixture_is_blocked(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("HRX_BROKER_SCENARIO_FIXTURE", None)
            with self.assertRaisesRegex(FixtureBlocked, "broker_fixture_config_missing"):
                load_fixture()
        cases = [({"project_path": "acme/hrx"}, "not_disposable"),
                 ({"broker_url": "http://broker.example"}, "url_invalid"),
                 ({"branch_prefix": "main"}, "branch_prefix_invalid"),
                 ({"identity": "short"}, "identity_invalid"),
                 ({"project_id": True}, "project_id_invalid")]
        for overrides, reason in cases:
            with self.subTest(reason=reason), self.assertRaisesRegex(FixtureBlocked, reason):
                load_fixture(fixture_file(self.temp, **overrides))
        path = fixture_file(self.temp)
        path.chmod(0o644)
        with self.assertRaisesRegex(FixtureBlocked, "not_private"):
            load_fixture(path)

    def test_valid_fixture_defaults(self):
        fixture = load_fixture(fixture_file(self.temp))
        self.assertEqual((fixture["branch_prefix"], fixture["stale_hours"]), ("hrx-ci-broker/", 6))

    def test_scenario_without_fixture_is_blocked_before_any_launch(self):
        runner = mock.Mock(hrx="/nonexistent")
        with mock.patch.dict(os.environ, {"HRX_BROKER_SCENARIO_FIXTURE": ""}):
            result = broker_scenario(runner, {"harness": "claude", "route": LAUNCH}, self.temp / "r")
        self.assertEqual((result["status"], result["reason"]), ("BLOCKED", "broker_fixture_config_missing"))


class EvaluationTests(unittest.TestCase):
    fixture = {"gitlab_host": HOST}

    def facts(self, **overrides):
        facts = {"token_present": True, "host": HOST, "noninteractive": True, "user": {"id": 7, "username": "svc"},
                 "invalid_token_rejected": True, "project_verified": True, "anonymous_rejected": True,
                 "clone": True, "push": True, "remote_commit_matches": True, "commit_author_matches": True,
                 "push_event_author": 7, "resume_argument": True, "cleanup_verified": True}
        facts.update(overrides)
        return facts

    def test_every_missing_proof_fails(self):
        continuity = {"client_token_unchanged": True, "pending_enrollment": False}
        self.assertEqual(evaluate(self.fixture, self.facts(), self.facts(), continuity), ("PASS", None))
        for key, value in [("token_present", False), ("host", "other.host"), ("noninteractive", False),
                           ("invalid_token_rejected", False), ("anonymous_rejected", False), ("clone", False),
                           ("push", False), ("remote_commit_matches", False), ("commit_author_matches", False),
                           ("push_event_author", 8), ("project_verified", False), ("user", None)]:
            with self.subTest(key=key):
                self.assertEqual(evaluate(self.fixture, self.facts(**{key: value}), self.facts(), continuity)[0],
                                 "FAIL")
        for resumed, reason in [(None, "resume_harness_not_reached"),
                                (self.facts(resume_argument=False), "resume_not_requested"),
                                (self.facts(user={"id": 9, "username": "x"}, push_event_author=9),
                                 "resume_account_changed"),
                                (self.facts(cleanup_verified=False), "cleanup_unverified")]:
            self.assertEqual(evaluate(self.fixture, self.facts(), resumed, continuity), ("FAIL", reason))
        self.assertEqual(evaluate(self.fixture, self.facts(), self.facts(),
                                  {"client_token_unchanged": False})[1], "resume_reenrolled")


@unittest.skipUnless(sys.platform.startswith("linux") and (os.environ.get("HRX_CI_BINARY") or shutil.which("go"))
                     and shutil.which("git"),
                     "real HRX binary test needs Linux (Go honors SSL_CERT_FILE), git, and Go or HRX_CI_BINARY")
class RealBinaryTests(unittest.TestCase):
    """The real HRX broker retrieval and Git helper against loopback fixtures."""

    @classmethod
    def setUpClass(cls):
        cls.build = Path(tempfile.mkdtemp(prefix="hrx-broker-build-"))
        cls.hrx = os.environ.get("HRX_CI_BINARY")
        if not cls.hrx:
            cls.hrx = str(cls.build / "hrx")
            subprocess.run(["go", "build", "-o", cls.hrx, "./cmd/hrx"], cwd=REPO, check=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.build, True)

    def setUp(self):
        self.temp = Path(tempfile.mkdtemp(prefix="hrx-broker-scenario-")).resolve()
        self.addCleanup(shutil.rmtree, self.temp, True)
        self.state = self.temp / "gitlab.json"
        self.write_state("honest")
        bin_dir = self.temp / "fake-bin"
        bin_dir.mkdir()
        for name, text in (("glab", FAKE_GLAB), ("git", FAKE_GIT)):
            script = (text.replace("PYTHON", sys.executable).replace("STATE_PATH", str(self.state))
                      .replace("REAL_GIT", shutil.which("git")).replace("HOST_VALUE", HOST)
                      .replace("PROJECT_ID_VALUE", str(PROJECT_ID)).replace("PROJECT_VALUE", PROJECT))
            (bin_dir / name).write_text(script)
            (bin_dir / name).chmod(0o700)
        patch = mock.patch.dict(os.environ, {"PATH": str(bin_dir) + os.pathsep + os.environ["PATH"]})
        patch.start()
        self.addCleanup(patch.stop)
        self.fixture_path = fixture_file(self.temp)
        self.broker = FakeBroker(Path(json.loads(self.fixture_path.read_text())["client_token_file"]).read_text(),
                                 self.state)
        self.addCleanup(self.broker.close)
        self.fixture = load_fixture(fixture_file(self.temp, broker_url=self.broker.url))
        self.upstream = Upstream()
        self.addCleanup(self.upstream.close)
        self.policy = {"max_requests": 10, "wall_seconds": 300, "cell_seconds": 120, "request_seconds": 10,
                       "max_request_bytes": 100000, "max_output_tokens": 2048, "concurrency": 1,
                       "routes": [PARSER, LAUNCH]}
        self.gateway = Gateway(self.temp, self.upstream.url, "upstream-secret", catalog(), self.policy)
        self.addCleanup(self.gateway.close)

    def write_state(self, mode):
        branches = {"main": {"id": "a", "author_name": "x", "committed_date": OLD},
                    "hrx-ci-broker/stale": {"id": "b", "author_name": "x", "committed_date": OLD},
                    "feature/keep": {"id": "c", "author_name": "x", "committed_date": OLD}}
        self.state.write_text(json.dumps({"mode": mode, "tokens": {}, "branches": branches, "events": []}))

    def run_scenario(self, mode="honest", broker="honest", verify_seconds=30):
        state = json.loads(self.state.read_text())
        state["mode"] = mode
        self.state.write_text(json.dumps(state))
        self.broker.mode, self.broker.calls = broker, 0
        runner = Runner(self.hrx, self.gateway, self.policy)
        result = broker_scenario(runner, {"harness": "claude", "route": LAUNCH, "reasoning": None},
                                 self.temp / ("run-" + mode + "-" + broker), self.fixture, 120,
                                 verify_seconds=verify_seconds)
        self.assertEqual(self.upstream.calls, 0, "broker scenario must not make inference requests")
        text = json.dumps(result)
        self.assertNotIn("glpat-fixture", text)
        self.assertNotIn("fixture-client-secret", text)
        return result

    def branches(self):
        return set(json.loads(self.state.read_text())["branches"])

    def test_honest_run_passes_and_cleans_only_its_scope(self):
        result = self.run_scenario()
        self.assertEqual(result["status"], "PASS", result)
        self.assertEqual(result["service_account_id"], 7)
        self.assertEqual(self.broker.calls, 2)
        self.assertEqual(self.branches(), {"main", "feature/keep"})
        self.assertTrue(all(p["cleanup"] == "clean" and p["exit_code"] == 0
                            for p in result["process_phases"].values()), result)

    def test_delayed_listing_visibility_does_not_decide_cleanup(self):
        # Verification rests on the two per-target sources; a briefly stale list
        # is recorded, never decisive.
        result = self.run_scenario(mode="delayed_visibility")
        self.assertEqual(result["status"], "PASS", result)
        resume = result["phases"]["resume"]
        self.assertTrue(resume["cleanup_verified"])
        self.assertGreaterEqual(resume["cleanup_verification_attempts"], 1)
        self.assertEqual(self.branches(), {"main", "feature/keep"})

    def test_stale_complete_listing_is_resolved_by_independent_sources(self):
        # Job 4345: deletes succeeded but every complete listing still showed the
        # targets. Git's ref advertisement and a cache-busted single GET both show
        # absence, so cleanup is verified, and the stale listing is recorded.
        result = self.run_scenario("stale_listing")
        self.assertEqual(result["status"], "PASS", result)
        resume = result["phases"]["resume"]
        self.assertTrue(resume["cleanup_verified"])
        self.assertFalse(resume["cleanup_listing_converged"])
        attempt = resume["cleanup_attempts"][0]
        self.assertEqual((attempt["git_absent"], attempt["rest_404"], attempt["unknown"]),
                         (resume["cleanup_target_count"],) * 2 + (0,))
        self.assertGreater(attempt["listed_targets"], 0)
        listing = attempt["listing"]
        self.assertEqual((listing["status"], listing["cf_cache_status"]), (200, "DYNAMIC"))
        self.assertRegex(listing["etag"], r"^[0-9a-f]{12}$")
        self.assertNotIn("W/", json.dumps(resume), "header values are digests, never raw")
        self.assertEqual(self.branches(), {"main", "feature/keep"})

    def test_unknown_independent_source_fails_at_once(self):
        for mode in ("ls_remote_fails", "single_errors"):
            with self.subTest(mode=mode):
                self.write_state("honest")
                result = self.run_scenario(mode)
                self.assertEqual(result["status"], "FAIL")
                resume = result["phases"]["resume"]
                self.assertFalse(resume["cleanup_verified"])
                self.assertEqual(resume["cleanup_verification_attempts"], 1, "errors are final immediately")
                self.assertGreater(resume["cleanup_attempts"][0]["unknown"], 0)

    def test_late_or_timed_out_sources_never_verify(self):
        # Review 945: the verification budget bounds every command. A source slower
        # than the remaining budget times out (unknown); absence that would arrive
        # after the deadline is never accepted.
        for mode in ("slow_ls_remote", "slow_single"):
            with self.subTest(mode=mode):
                self.write_state("honest")
                started = time.monotonic()
                result = self.run_scenario(mode, verify_seconds=2)
                resume = result["phases"]["resume"]
                self.assertEqual(result["status"], "FAIL")
                self.assertFalse(resume["cleanup_verified"])
                self.assertEqual(resume["cleanup_verification_attempts"], 1)
                attempt = resume["cleanup_attempts"][0]
                self.assertTrue(attempt["unknown"] > 0 or not attempt["on_time"], attempt)
                self.assertLess(time.monotonic() - started, 100, "verification must respect its budget")

    def test_slow_diagnostics_do_not_block_on_time_evidence(self):
        result = self.run_scenario("slow_listing", verify_seconds=2)
        resume = result["phases"]["resume"]
        self.assertTrue(resume["cleanup_verified"], resume)
        self.assertTrue(resume["cleanup_verification_on_time"])
        attempt = resume["cleanup_attempts"][0]
        self.assertIsNone(attempt["listed_targets"], "an over-budget listing is recorded as incomplete")
        self.assertEqual(attempt["listing"]["exit_code"], 124)

    def test_status_must_pair_with_exit_code(self):
        result = self.run_scenario("mismatched_status")
        resume = result["phases"]["resume"]
        self.assertFalse(resume["cleanup_verified"])
        self.assertGreater(resume["cleanup_attempts"][0]["unknown"], 0)

    def test_stale_search_results_do_not_corrupt_cleanup_verification(self):
        result = self.run_scenario(mode="stale_search")
        self.assertEqual(result["status"], "PASS", result)
        self.assertTrue(result["phases"]["resume"]["cleanup_verified"])
        self.assertEqual(self.branches(), {"main", "feature/keep"})

    def test_unowned_initial_process_prevents_resume(self):
        for cleanup in ("failed", "unknown"):
            with self.subTest(cleanup=cleanup), mock.patch("broker_scenario.capture", return_value={
                    "cleanup": cleanup, "exit_code": 0, "timed_out": False}) as execute:
                result = self.run_scenario()
                self.assertEqual((result["status"], result["reason"]),
                                 ("FAIL", "initial_process_cleanup_" + cleanup))
                self.assertEqual(execute.call_count, 1)

    def test_lost_resume_ownership_prevents_janitor_launch(self):
        def lose_ownership(*args, **kwargs):
            result = capture(*args, **kwargs)
            if "--resume" in args[0]:
                result["cleanup"] = "unknown"
            return result

        with mock.patch("broker_scenario.capture", side_effect=lose_ownership) as execute:
            result = self.run_scenario()
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "resume_process_cleanup_unknown"))
        self.assertEqual(execute.call_count, 2)
        self.assertNotIn("janitor", result["process_phases"])
        self.assertEqual(result["janitor_skipped"], "process_ownership_not_clean")
        self.assertTrue(result["initial_push_recorded"])
        self.assertEqual(len(result["owned_branch_candidates"]), 2)

    def test_timeout_gets_separate_bounded_janitor_window(self):
        deadlines = {}

        def timeout_after_push(*args, **kwargs):
            phase = json.loads(kwargs["env"]["HRX_BROKER_SCENARIO_SPEC"])["mode"]
            deadlines[phase] = kwargs["deadline"]
            result = capture(*args, **kwargs)
            if phase == "initial":
                result["timed_out"] = True
            return result

        with mock.patch("broker_scenario.capture", side_effect=timeout_after_push):
            result = self.run_scenario()
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "initial_process_timeout"))
        self.assertTrue(result["janitor_cleanup_verified"], result)
        self.assertEqual(set(deadlines), {"initial", "janitor"})
        self.assertLess(deadlines["janitor"], deadlines["initial"])
        self.assertEqual(self.branches(), {"main", "feature/keep"})

    def test_successful_facts_cannot_hide_native_exit_failure(self):
        def fail_after_evidence(*args, **kwargs):
            result = capture(*args, **kwargs)
            if json.loads(kwargs["env"]["HRX_BROKER_SCENARIO_SPEC"])["mode"] == "initial":
                result["exit_code"] = 7
            return result

        with mock.patch("broker_scenario.capture", side_effect=fail_after_evidence):
            result = self.run_scenario()
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "initial_process_exit"))
        self.assertTrue(result["janitor_cleanup_verified"], result)
        self.assertEqual(self.branches(), {"main", "feature/keep"})

    def test_false_positives_fail(self):
        for mode, broker, reason in [("push_rejected", "honest", "initial_push"),
                                     ("wrong_author", "honest", "initial_attribution"),
                                     ("token_ignored", "honest", "initial_invalid_token_not_rejected"),
                                     ("public_project", "honest", "initial_project_not_disposable"),
                                     ("delete_fails", "honest", "cleanup_unverified"),
                                     ("stale_delete_fails", "honest", "cleanup_unverified"),
                                     ("cleanup_auth_failure", "honest", "cleanup_unverified"),
                                     # Network/auth failures in the DECISIVE reads stay mandatory FAILs.
                                     ("cleanup_network_failure", "honest", "cleanup_unverified"),
                                     ("cleanup_git_auth_failure", "honest", "cleanup_unverified"),
                                     ("honest", "account_changes", "resume_account_changed")]:
            with self.subTest(mode=mode, broker=broker):
                self.write_state(mode)
                result = self.run_scenario(mode, broker)
                self.assertEqual((result["status"], result["reason"]), ("FAIL", reason), result)

    def test_failed_diagnostic_listing_is_not_decisive(self):
        # Review 966: the post-delete listing is diagnostic only. If it fails while
        # BOTH direct reads prove absence, cleanup is verified and the failure recorded.
        self.write_state("listing_fails_after_delete")
        result = self.run_scenario("listing_fails_after_delete")
        self.assertEqual(result["status"], "PASS", result)
        resume = result["phases"]["resume"]
        attempt = resume["cleanup_attempts"][0]
        self.assertTrue(resume["cleanup_verified"])
        self.assertEqual((attempt["git_absent"], attempt["rest_404"], attempt["unknown"]),
                         (resume["cleanup_target_count"],) * 2 + (0,))
        self.assertIsNone(attempt["listed_targets"])
        self.assertFalse(resume["cleanup_listing_converged"])

    def test_decisive_read_failures_are_unknown_not_absence(self):
        for mode in ("cleanup_network_failure", "cleanup_git_auth_failure"):
            with self.subTest(mode=mode):
                self.write_state(mode)
                resume = self.run_scenario(mode)["phases"]["resume"]
                self.assertFalse(resume["cleanup_verified"])
                self.assertGreater(resume["cleanup_attempts"][0]["unknown"], 0)

    def test_public_project_is_never_written(self):
        self.write_state("public_project")
        self.run_scenario("public_project")
        self.assertEqual(self.branches(), {"main", "hrx-ci-broker/stale", "feature/keep"})

    def test_broker_outage_and_refusal_are_blocked(self):
        for broker, reason in [("down", "broker_unavailable")]:
            result = self.run_scenario(broker=broker)
            self.assertEqual((result["status"], result["reason"]), ("BLOCKED", reason), result)
        self.broker.secret = "different-client-secret"
        result = self.run_scenario(broker="refuse")
        self.assertEqual((result["status"], result["reason"]), ("BLOCKED", "broker_refused_credentials"), result)

    def test_failed_resume_still_runs_janitor(self):
        result = self.run_scenario(broker="second_down")
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "resume_harness_not_reached"), result)
        self.assertTrue(result["janitor_cleanup_verified"], result)
        self.assertEqual(self.branches(), {"main", "feature/keep"})


if __name__ == "__main__":
    unittest.main()
