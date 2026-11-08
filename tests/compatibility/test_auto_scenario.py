"""Deterministic coverage for auto_scenario: no network beyond loopback, no spend."""
from datetime import datetime, timezone
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

import auto_scenario as auto_scenario_module
from gateway import Gateway
from run import Runner
from auto_scenario import auto_scenario, changed_paths, check_translation, proposal_argv, snapshot

REPO = Path(__file__).resolve().parents[2]
PARSER, LAUNCH = "fixture-inkling", "fixture-launch"

FAKE_HRX = r'''#!PYTHON
"""Emulates the hrx --auto terminal contract; MODE selects a misbehaviour."""
import json, os, re, shlex, ssl, subprocess, sys, urllib.request
MODE = "MODE_VALUE"
args = sys.argv[1:]
request, model = args[1], args[args.index("--model") + 1]
config = open(os.path.join(os.environ["XDG_CONFIG_HOME"], "hrx/config.toml")).read()
base = re.search(r'base_url = "([^"]+)"', config).group(1)
parser = re.search(r'\[auto\][^\[]*model = "([^"]+)"', config).group(1)
if MODE == "no_inference":
    found = re.match(r"(\S+) as (\S+) using pitput board (\S+) and gitlab\. Task: (.*)", request)
    p = {"harness": found[1], "agent_identity": found[2], "pitput_boards": [found[3]], "task": found[4]}
else:
    if MODE == "substitute":
        parser = "substituted-route"
    body = {"model": parser, "messages": [{"role": "system", "content": "schema"},
            {"role": "user", "content": json.dumps({"request": request})}]}
    context = ssl.create_default_context(cafile=os.environ["SSL_CERT_FILE"])
    call = urllib.request.Request(base + "/chat/completions", data=json.dumps(body).encode(),
                                  headers={"Authorization": "Bearer " + os.environ["HRX_CI_KEY"],
                                           "Content-Type": "application/json"})
    try:
        data = json.load(urllib.request.urlopen(call, context=context, timeout=10))
    except Exception:
        print("hrx: auto inference returned an HTTP error (body suppressed)")
        sys.exit(1)
    p = json.loads(data["choices"][0]["message"]["content"])
if MODE == "echo_key":
    print(os.environ["HRX_CI_KEY"])
if p.get("question"):
    print("hrx: " + p["question"] + " (enter n to cancel)\n> ", end="", flush=True)
    sys.stdin.readline()
    print("hrx: Cancelled.")
    sys.exit(0)
argv = [p["harness"], "--model", model, "--agent-identity", p["agent_identity"], "--pitput",
        "--pitput-board", p["pitput_boards"][0], "--gitlab", "--no-yolo", "--", "--", p["task"]]
if MODE == "yolo":
    argv[argv.index("--no-yolo")] = "--yolo"
print("hrx: Proposed launch (quoted arguments, never evaluated as shell):")
print("  hrx " + " ".join(shlex.quote(a) for a in argv))
while True:
    print("Launch? [y/n/reprompt] ", end="", flush=True)
    answer = sys.stdin.readline().strip()
    if answer in ("y", "yes") or (MODE == "invalid_is_cancel" and answer not in ("n", "no")):
        if MODE != "invalid_is_cancel":
            subprocess.run([p["harness"]])
        print("hrx: Cancelled.")
        sys.exit(0)
    if answer in ("n", "no"):
        if MODE == "launch_on_cancel":
            subprocess.run([p["harness"]])
        if MODE == "enroll_on_cancel":
            path = os.path.join(os.environ["XDG_CONFIG_HOME"], "hrx/client-tokens/x/broker.token")
            os.makedirs(os.path.dirname(path))
            open(path, "w").write("token")
        if MODE == "broker_on_cancel":
            urllib.request.urlopen(os.environ["HRX_GITLAB_BROKER_URL"] + "/v1/enroll", data=b"{}").close()
        print("hrx: Cancelled.")
        sys.exit(0)
'''


def catalog():
    def model(route, harnesses):
        return {"canonical_id": route, "available": True, "sources": [
            {"gateway_route": route, "available": True, "provider": "fixture", "account": "fixture",
             "details": {"harnesses": harnesses}}]}
    return {"schema_version": 1, "generated_at": datetime.now(timezone.utc).isoformat(),
            "coverage": {"providers": [{"source": "gateway", "ok": True}]},
            "models": [model(PARSER, {"opencode": {"wire_api": "chat_completions"}}),
                       model(LAUNCH, {"claude": {"wire_api": "messages"},
                                      "opencode": {"wire_api": "chat_completions"}})]}


class Upstream:
    """Fake provider: translates the request correctly unless told otherwise."""

    def __init__(self):
        self.mode, self.calls = "correct", 0
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                owner.calls += 1
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if owner.mode == "forbidden":
                    self.send_response(403)
                    self.send_header("Content-Length", "2")
                    self.end_headers()
                    self.wfile.write(b"{}")
                    return
                request = json.loads(data["messages"][-1]["content"])["request"]
                found = re.match(r"(\S+) as (\S+) using pitput board (\S+) and gitlab\. Task: (.*)", request)
                proposal = {"harness": found[1], "agent_identity": found[2], "pitput": True,
                            "pitput_boards": [found[3]], "gitlab": True, "task": found[4]}
                if owner.mode == "wrong_identity":
                    proposal["agent_identity"] = "ci/someone-else/agent"
                if owner.mode == "question":
                    proposal = {"question": "Which board?"}
                raw = json.dumps({"choices": [{"message": {"content": json.dumps(proposal)},
                                               "finish_reason": "stop"}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class ScenarioTest(unittest.TestCase):
    def setUp(self):
        self.temp = Path(tempfile.mkdtemp(prefix="hrx-auto-scenario-")).resolve()
        self.addCleanup(shutil.rmtree, self.temp, True)
        self.upstream = Upstream()
        self.addCleanup(self.upstream.close)
        self.policy = {"max_requests": 10, "wall_seconds": 120, "cell_seconds": 60, "request_seconds": 10,
                       "max_request_bytes": 100000, "max_output_tokens": 2048, "concurrency": 1,
                       "routes": [PARSER, LAUNCH]}
        self.gateway = Gateway(self.temp, self.upstream.url, "upstream-secret", catalog(), self.policy)
        self.addCleanup(self.gateway.close)

    def fake(self, mode):
        path = self.temp / ("hrx-" + mode)
        path.write_text(FAKE_HRX.replace("PYTHON", sys.executable).replace("MODE_VALUE", mode))
        path.chmod(0o700)
        return path

    def scenario(self, hrx, name, harness="claude"):
        return auto_scenario(Runner(hrx, self.gateway, self.policy), self.temp / name, PARSER, harness, LAUNCH, 30)


class FakeLauncherTests(ScenarioTest):
    def test_honest_translation_and_cancellation_passes(self):
        result = self.scenario(self.fake("honest"), "honest")
        self.assertEqual(result["status"], "PASS", result)
        self.assertEqual(result["inference_requests"], 1)
        self.assertEqual(result["observed_routes"], [PARSER])
        self.assertEqual(result["launch_side_effects"], [])

    def test_false_positives_fail(self):
        for mode, reason in [("no_inference", "no_parser_inference"),
                             ("launch_on_cancel", "side_effect_before_approval"),
                             ("enroll_on_cancel", "side_effect_before_approval"),
                             ("broker_on_cancel", "side_effect_before_approval"),
                             ("yolo", "translation_unexpected_argument"),
                             ("invalid_is_cancel", "invalid_answer_not_refused"),
                             ("echo_key", "credential_in_terminal_output")]:
            with self.subTest(mode=mode):
                result = self.scenario(self.fake(mode), mode)
                self.assertEqual((result["status"], result["reason"]), ("FAIL", reason), result)

    def test_wrong_translation_fails(self):
        for mode, reason in [("wrong_identity", "translation_identity"), ("question", "unexpected_clarification")]:
            with self.subTest(mode=mode):
                self.upstream.mode = mode
                result = self.scenario(self.fake("honest"), mode)
                self.assertEqual((result["status"], result["reason"]), ("FAIL", reason), result)

    def test_provider_rejection_is_blocked_not_pass(self):
        self.upstream.mode = "forbidden"
        result = self.scenario(self.fake("honest"), "forbidden")
        self.assertEqual((result["status"], result["reason"]),
                         ("BLOCKED", "provider_auth_quota_or_availability"), result)

    def test_route_substitution_never_reaches_upstream(self):
        result = self.scenario(self.fake("substitute"), "substitute")
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "no_parser_inference"), result)
        self.assertEqual(self.upstream.calls, 0)

    def test_parser_route_outside_budget_policy_is_blocked(self):
        result = auto_scenario(Runner(self.fake("honest"), self.gateway, self.policy), self.temp / "policy",
                               "unbudgeted-route", "claude", LAUNCH, 30)
        self.assertEqual((result["status"], result["reason"]), ("BLOCKED", "route_not_authorized_by_budget_policy"))
        self.assertEqual(self.upstream.calls, 0)

    def test_missing_binary_is_blocked(self):
        result = self.scenario(self.temp / "absent", "absent")
        self.assertEqual((result["status"], result["reason"]), ("BLOCKED", "hrx_artifact_missing"))

    def test_evidence_is_sanitized(self):
        result = self.scenario(self.fake("honest"), "sanitized")
        text = json.dumps(result)
        self.assertNotIn(self.gateway.secret, text)
        self.assertNotIn("upstream-secret", text)
        self.assertNotIn("Task", text)


class TranslationTests(unittest.TestCase):
    def test_preview_parsing_takes_last_proposal(self):
        output = (b"hrx: Proposed launch (quoted arguments, never evaluated as shell):\r\n  hrx 'claude' 'old'\r\n"
                  b"hrx: Proposed launch (quoted arguments, never evaluated as shell):\r\n"
                  b"  hrx 'claude' '--' '--' 'it'\"'\"'s'\r\n")
        self.assertEqual(proposal_argv(output), ["claude", "--", "--", "it's"])
        self.assertIsNone(proposal_argv(b"  hrx 'claude'\r\n"))

    def test_translation_requires_every_setting_exactly(self):
        good = ["claude", "--model", "m", "--agent-identity", "a/b/c", "--pitput", "--pitput-board", "b",
                "--gitlab", "--no-yolo", "--", "--", "task"]
        self.assertEqual(check_translation(good, "claude", "m", "a/b/c", "b", "task"), (True, None))
        opencode = ["opencode"] + good[1:-3] + ["--", "--prompt", "task"]
        self.assertTrue(check_translation(opencode, "opencode", "m", "a/b/c", "b", "task")[0])
        for broken in (good[:-1] + ["other"], [x for x in good if x != "--gitlab"],
                       good[:1] + ["--yolo"] + good[1:], good[:1] + ["--resume", "x"] + good[1:],
                       good[:1] + ["--model", "m"] + good[1:], ["codex"] + good[1:]):
            self.assertFalse(check_translation(broken, "claude", "m", "a/b/c", "b", "task")[0], broken)

    def test_watched_state_changes_are_reported(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "cache/hrx").mkdir(parents=True)
            (root / ".config/hrx").mkdir(parents=True)
            before = snapshot(root)
            (root / "cache/hrx/models.json").write_text("{}")
            self.assertEqual(changed_paths(before, snapshot(root)), [])
            (root / ".config/hrx/client-tokens").mkdir()
            (root / ".config/hrx/client-tokens/broker.token").write_text("x")
            self.assertEqual(changed_paths(before, snapshot(root)), [".config/hrx/client-tokens/broker.token"])


@unittest.skipUnless(sys.platform.startswith("linux") and (os.environ.get("HRX_CI_BINARY") or shutil.which("go")),
                     "real HRX binary test needs Linux (Go honors SSL_CERT_FILE) and Go or HRX_CI_BINARY")
class RealBinaryTests(ScenarioTest):
    """The actual hrx --auto terminal contract against the local fixture relay."""

    @classmethod
    def setUpClass(cls):
        cls.build = Path(tempfile.mkdtemp(prefix="hrx-auto-build-"))
        cls.hrx = os.environ.get("HRX_CI_BINARY")
        if not cls.hrx:
            cls.hrx = str(cls.build / "hrx")
            subprocess.run(["go", "build", "-o", cls.hrx, "./cmd/hrx"], cwd=REPO, check=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.build, True)

    def test_each_harness_translates_and_cancels_without_side_effects(self):
        for harness in ("claude", "opencode"):
            with self.subTest(harness=harness):
                result = self.scenario(self.hrx, "real-" + harness, harness)
                self.assertEqual(result["status"], "PASS", result)
                self.assertEqual(result["inference_requests"], 1)

    def test_sentinels_detect_a_real_approved_launch(self):
        # Swap the cancelling "n" for approval: HRX must then reach the broker,
        # Pitput or harness, proving the zero-side-effect assertion is not hollow.
        original = auto_scenario_module.Terminal.send
        approve = lambda terminal, text: original(terminal, "y\n" if text == "n\n" else text)
        with mock.patch.object(auto_scenario_module.Terminal, "send", approve):
            result = self.scenario(self.hrx, "real-approved")
        self.assertEqual((result["status"], result["reason"]), ("FAIL", "side_effect_before_approval"), result)
        self.assertTrue(result["launch_side_effects"] or result["filesystem_side_effects"], result)

    def test_wrong_identity_rejected_by_hrx_is_not_pass(self):
        self.upstream.mode = "wrong_identity"
        result = self.scenario(self.hrx, "real-wrong")
        self.assertNotEqual(result["status"], "PASS", result)


if __name__ == "__main__":
    unittest.main()
