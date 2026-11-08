from concurrent.futures import ThreadPoolExecutor
import copy
import http.server
import json
from pathlib import Path
import socket
import ssl
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock, patch
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET

from gateway import Gateway, tool_results, load_claude_lease
from run import Runner, Blocked, capabilities, native_evidence, policy_from_file, report
from conversation import Agent, descendants


class EvidenceTests(unittest.TestCase):
    def test_resume_requires_fresh_live_registration(self):
        with tempfile.TemporaryDirectory() as temp:
            agent = Agent.__new__(Agent)
            agent.home = Path(temp)
            agent.identity = "ci/test/agent"
            agent.process = Mock()
            agent.summary = agent.home / "supervisor.json"
            # Liveness is checked without reaping (conversation.running), never poll().
            alive = patch("conversation.running", return_value=True)
            alive.start()
            self.addCleanup(alive.stop)
            agent.previous_updates = {"native-123": "old"}
            index = agent.home / "state/hrx/sessions/index.json"
            index.parent.mkdir(parents=True)
            record = {"native_id": "native-123", "agent_identity": agent.identity, "updated_at": "old"}
            index.write_text(json.dumps({"sessions": [record]}))
            self.assertIsNone(agent.session())
            record["updated_at"] = "fresh"
            index.write_text(json.dumps({"sessions": [record]}))
            self.assertEqual(agent.session()["native_id"], "native-123")
            alive.stop()
            with patch("conversation.running", return_value=False), self.assertRaises(RuntimeError):
                agent.session()
            alive.start()
            # HRX exiting inside a still-running supervisor also counts as exited.
            Path(str(agent.summary) + ".main").write_text('{"status": 0}')
            with self.assertRaises(RuntimeError):
                agent.session()

    def test_claude_lease_rejects_expiry_profile_models_and_private_key_is_discarded(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "lease.json"
            valid = {"expires_at": "2099-01-01T00:00:00Z", "key": "unused-lease-key",
                     "models": ["claude-opus-test"], "gateway": {"origin": "https://gateway.test"},
                     "credentials": {"profile": "claude-code", "claude_code_oauth_token": "oauth-secret"}}
            def write(value):
                path.write_text(json.dumps(value))
                path.chmod(0o600)
            write(valid)
            loaded = load_claude_lease(path, "https://gateway.test")
            self.assertEqual(loaded["models"], {"claude-opus-test"})
            self.assertEqual(set(loaded), {"models", "token", "expires_at"})
            for key, value in (("expires_at", "2000-01-01T00:00:00Z"),
                               ("models", ["open-model"]), ("models", []),
                               ("credentials", {"profile": "other", "claude_code_oauth_token": "oauth-secret"})):
                write({**valid, key: value})
                with self.assertRaises(ValueError):
                    load_claude_lease(path, "https://gateway.test")
            write(valid)
            with self.assertRaises(ValueError):
                load_claude_lease(path, "https://wrong.test")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                load_claude_lease(path, "https://gateway.test")

    def test_native_catalog_template_omits_identity_and_unknown_fields(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            template = root / "models.json"
            template.write_text(json.dumps({"models": [{"slug": "public-model"}],
                                            "identity": {"account_id": "private-account"},
                                            "client_version": "test", "unknown_secret": "private"}))
            gateway = Mock(secret="fixture", cert=root / "cert.pem", url="https://127.0.0.1:1234")
            runner = Runner("hrx", gateway, {})
            with patch.dict("os.environ", {"HRX_CI_CODEX_CATALOG": str(template),
                                           "HRX_CI_CLAUDE_LEASE_FILE": "/private/lease.json",
                                           "CLAUDE_CODE_OAUTH_TOKEN": "private-oauth"}):
                env = runner.configure(root / "home", root / "project", {"route": "exact"})
            self.assertNotIn("HRX_CI_CLAUDE_LEASE_FILE", env)
            self.assertNotIn("CLAUDE_CODE_OAUTH_TOKEN", env)
            self.assertNotIn("private-oauth", json.dumps(env))
            copied = json.loads((root / "home/codex/models_cache.json").read_text())
            self.assertEqual(copied, {"models": [{"slug": "public-model"}], "client_version": "test"})

    def test_resume_rejects_historical_tool_results_without_fresh_roundtrip(self):
        for fresh in (False, True):
            with self.subTest(fresh=fresh), tempfile.TemporaryDirectory() as temp:
                runner = Runner.__new__(Runner)
                original = []
                def launch(cell, home, project, prompt, resume=None):
                    if resume:
                        (project / "resumed.txt").write_text(original[0])
                    else:
                        original.append((project / "challenge.txt").read_text())
                        (project / "result.txt").write_text(original[0][::-1])
                    return {"status": "PASS", "session_id": "session-test", "tool_calls": 1,
                            "tool_results": 3, "exact_route_protocol": True,
                            "fresh_tool_result_consumed": fresh}
                runner.launch = launch
                result = runner.cell({}, Path(temp))
                self.assertEqual(result["status"], "PASS" if fresh else "FAIL")
                if not fresh:
                    self.assertEqual(result["reason"], "resume_identity_context_or_tool_assertion")

    def test_descendants_include_detached_process_groups_not_other_trees(self):
        table = {1: (0, "a"), 2: (1, "b"), 3: (2, "c"), 4: (1, "d")}
        self.assertEqual(set(descendants(2, table)), {2, 3})

    def test_missing_required_cell_prevents_certification(self):
        for status in ("FAIL", "BLOCKED", "UNSUPPORTED", "NOT_RUN"):
            with self.subTest(status=status), tempfile.TemporaryDirectory() as temp:
                results = [{"id": "works", "status": "PASS"}, {"id": "required", "status": status}]
                self.assertFalse(report(Path(temp), results, {"timestamp": "now", "lane": "release"}))
                data = json.loads((Path(temp) / "results.json").read_text())
                self.assertFalse(data["certified"])
                self.assertEqual(len(ET.parse(Path(temp) / "junit.xml").findall("testcase/failure")), 1)

    def test_canary_cannot_certify_omitted_required_cells(self):
        with tempfile.TemporaryDirectory() as temp:
            results = [{"id": "works", "status": "PASS"},
                       {"id": "required-nightly", "status": "NOT_RUN", "selected": False}]
            self.assertTrue(report(Path(temp), results, {"timestamp": "now", "lane": "canary"}))
            self.assertFalse(json.loads((Path(temp) / "results.json").read_text())["certified"])

    def test_catalog_does_not_substitute_route_or_protocol(self):
        cell = {"canonical_model": "kimi-k3", "route": "exact-route", "harness": "codex", "protocol": "responses"}
        catalog = {"models": [{"canonical_id": "kimi-k3", "sources": [{"gateway_route": "other-route",
                   "available": True, "details": {"harnesses": {"codex": {"wire_api": "responses"}}}}]}]}
        self.assertIsNone(capabilities(catalog, cell))
        catalog["models"][0]["sources"][0]["gateway_route"] = "exact-route"
        self.assertIsNotNone(capabilities(catalog, cell))
        cell["protocol"] = "chat_completions"
        self.assertIsNone(capabilities(catalog, cell))

    def test_catalog_capability_respects_provider_and_account_for_shared_route(self):
        cell = {"canonical_model": "fixture", "route": "shared", "harness": "codex",
                "protocol": "responses", "route_policy": {"provider": "chosen", "account": "owned"}}
        def source(provider, account):
            return {"gateway_route": "shared", "available": True, "provider": provider,
                    "account": account, "details": {"harnesses": {"codex": {"wire_api": "responses"}}}}
        sources = [source("other", "owned"), source("chosen", "other"), source("chosen", "owned")]
        catalog = {"models": [{"canonical_id": "fixture", "sources": sources}]}
        for ordered in (sources, list(reversed(sources))):
            catalog["models"][0]["sources"] = ordered
            result = capabilities(catalog, cell)
            self.assertEqual((result["provider"], result["account"]), ("chosen", "owned"))
        catalog["models"][0]["sources"] = sources[:2]
        self.assertIsNone(capabilities(catalog, cell))
        catalog["models"][0]["sources"] = sources
        sources[2]["details"]["harnesses"] = {}
        self.assertIsNone(capabilities(catalog, cell))

    def test_text_claim_is_not_tool_evidence(self):
        self.assertEqual(native_evidence('I ran a tool and wrote the file', 'codex')["tool_calls"], 0)
        evidence = native_evidence(json.dumps({"type": "thread.started", "thread_id": "session-123"}) + '\n' +
                                   json.dumps({"type": "item.completed", "item": {
                                       "type": "command_execution", "status": "completed"}}), "codex")
        self.assertEqual(evidence, {"session_id": "session-123", "tool_calls": 1})

    def test_tool_result_protocols(self):
        for payload in ({"input": [{"type": "function_call_output", "output": "value"}]},
                        {"messages": [{"role": "tool", "content": "value"}]},
                        {"messages": [{"role": "user", "content": [{"type": "tool_result", "content": "value"}]}]}):
            self.assertEqual(tool_results(payload), 1)
        self.assertEqual(tool_results({"messages": [{"role": "user", "content": "tool_result"}]}), 0)

    def test_policy_rejects_missing_unlimited_and_nonfinite(self):
        with self.assertRaises(Blocked):
            policy_from_file(None)
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "policy.json"
            for value in (0, -1, True, float("inf"), float("nan")):
                path.write_text(json.dumps({"daily_budget_usd": value}))
                with self.assertRaises(Blocked):
                    policy_from_file(path)


class RelayTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.forwarded = []
        owner = self

        class Upstream(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                owner.forwarded.append((data, self.headers.get("Authorization")))
                owner.forwarded_headers = dict(self.headers)
                if data.get("reject"):
                    self.send_response(data["reject"])
                    self.end_headers()
                    return
                if data.get("delay"):
                    time.sleep(data["delay"])
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                if data.get("trickle"):
                    try:
                        for _ in range(40):
                            self.wfile.write(b'x')
                            self.wfile.flush()
                            time.sleep(.05)
                    except OSError:
                        pass
                    return
                self.wfile.write(b'{"ok":true}')

        self.upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        thread = threading.Thread(target=self.upstream.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self.upstream.server_close)
        self.addCleanup(self.upstream.shutdown)
        self.policy = {"max_requests": 2, "wall_seconds": 30, "request_seconds": 5,
                       "max_request_bytes": 10000, "max_output_tokens": 100, "concurrency": 1,
                       "routes": ["exact"]}
        self.gateway = Gateway(Path(self.temp.name), f'http://127.0.0.1:{self.upstream.server_port}',
                               "upstream-secret", {"models": []}, self.policy)
        self.addCleanup(self.gateway.close)
        self.context = ssl.create_default_context(cafile=str(self.gateway.cert))

    def request(self, payload, key=None):
        request = urllib.request.Request(self.gateway.url + "/v1/responses", data=json.dumps(payload).encode(),
                                         headers={"Authorization": "Bearer " + (key or self.gateway.secret)})
        return urllib.request.urlopen(request, context=self.context, timeout=5)

    def test_requests_and_output_are_bounded_at_inference_boundary(self):
        for _ in range(2):
            with self.request({"model": "exact", "max_output_tokens": 99999}) as response:
                self.assertEqual(response.status, 200)
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.request({"model": "exact"})
        self.assertEqual(caught.exception.code, 429)
        self.assertEqual(len(self.forwarded), 2)
        self.assertEqual(self.forwarded[0][0]["max_output_tokens"], 100)
        self.assertEqual(self.forwarded[0][1], "Bearer upstream-secret")
        self.assertNotIn("secret", json.dumps(self.gateway.requests))

    def test_three_requests_queue_behind_two_slots(self):
        self.policy.update(concurrency=2, max_requests=3)
        def call():
            with self.request({"model": "exact", "delay": .25}) as response:
                return response.status, response.read()
        with ThreadPoolExecutor(max_workers=3) as pool:
            responses = list(pool.map(lambda _: call(), range(3)))
        self.assertEqual([r[0] for r in responses], [200, 200, 200])
        self.assertEqual(self.gateway.admitted, 3)
        self.assertGreater(max(e["queue_seconds"] for e in self.gateway.requests), .15)
        self.assertFalse(any(e.get("local_limit") for e in self.gateway.requests))

    def test_waiting_request_obeys_deadline_without_provider_call(self):
        self.gateway.active = 1
        self.policy["request_seconds"] = .15
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.request({"model": "exact"})
        self.assertEqual(caught.exception.code, 429)
        self.assertEqual(self.gateway.requests[0]["local_limit"], "admission_deadline")
        self.assertEqual(self.forwarded, [])

    def test_disconnected_queued_client_does_not_spend_request_budget(self):
        self.gateway.active = 1
        connection = self.context.wrap_socket(socket.create_connection(
            ("127.0.0.1", self.gateway.server.server_port)), server_hostname="127.0.0.1")
        body = b'{"model":"exact"}'
        connection.sendall(("POST /v1/responses HTTP/1.0\r\nAuthorization: Bearer " +
                            self.gateway.secret + "\r\nContent-Length: " + str(len(body)) +
                            "\r\n\r\n").encode() + body)
        deadline = time.monotonic() + 2
        while not self.gateway.requests and time.monotonic() < deadline:
            time.sleep(.01)
        connection.close()
        while self.gateway.waiting and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertEqual(self.gateway.requests[0].get("local_limit"), "client_disconnected")
        self.assertEqual(self.gateway.admitted, 0)
        self.assertEqual(self.forwarded, [])

    def test_claude_litellm_header_is_authenticated_but_not_forwarded(self):
        request = urllib.request.Request(self.gateway.url + "/v1/messages?beta=true",
                                         data=json.dumps({"model": "exact"}).encode(),
                                         headers={"x-litellm-api-key": "Bearer " + self.gateway.secret})
        with urllib.request.urlopen(request, context=self.context, timeout=5) as response:
            self.assertEqual(response.status, 200)
        self.assertEqual(self.forwarded[0][1], "Bearer upstream-secret")
        self.assertNotIn(self.gateway.secret, json.dumps(self.forwarded_headers))
        self.assertEqual(self.forwarded_headers.get("X-Litellm-Api-Key"), "Bearer upstream-secret")
        request.headers["X-litellm-api-key"] = "Bearer wrong-key"
        with self.assertRaises(urllib.error.HTTPError) as caught:
            urllib.request.urlopen(request, context=self.context, timeout=5)
        self.assertEqual(caught.exception.code, 401)
        self.assertEqual(len(self.forwarded), 1)

    def test_oauth_is_scoped_to_vendor_messages_and_never_native_artifacts(self):
        self.gateway.claude_routes = {"exact"}
        self.gateway.claude_lease = {"token": "provider-oauth", "models": {"exact"}, "expires_at": time.time() + 60}
        request = urllib.request.Request(self.gateway.url + "/v1/messages", data=b'{"model":"exact"}',
                                         headers={"Authorization": "Bearer " + self.gateway.secret})
        with urllib.request.urlopen(request, context=self.context, timeout=5) as response:
            self.assertEqual(response.status, 200)
        self.assertEqual(self.forwarded[0][1], "Bearer provider-oauth")
        self.assertEqual(self.forwarded_headers.get("X-Litellm-Api-Key"), "Bearer upstream-secret")
        self.assertNotIn("provider-oauth", json.dumps(self.gateway.requests))
        self.gateway.claude_routes = set()
        with urllib.request.urlopen(request, context=self.context, timeout=5) as response:
            self.assertEqual(response.status, 200)
        self.assertEqual(self.forwarded[1][1], "Bearer upstream-secret")

    def test_provider_auth_or_quota_rejection_opens_route_circuit(self):
        for _ in range(2):
            with self.assertRaises(urllib.error.HTTPError) as caught:
                self.request({"model": "exact", "reject": 401})
            self.assertEqual(caught.exception.code, 401)
        self.assertEqual(len(self.forwarded), 1)
        self.assertEqual(self.gateway.admitted, 1)
        self.assertTrue(self.gateway.requests[-1]["provider_circuit_open"])

    def test_missing_expired_or_out_of_scope_vendor_lease_never_reaches_provider(self):
        self.gateway.claude_routes = {"exact"}
        for lease in (None, {"token": "oauth", "models": {"other"}, "expires_at": time.time() + 60},
                      {"token": "oauth", "models": {"exact"}, "expires_at": time.time() - 1}):
            self.gateway.claude_lease = lease
            request = urllib.request.Request(self.gateway.url + "/v1/messages", data=b'{"model":"exact"}',
                                             headers={"Authorization": "Bearer " + self.gateway.secret})
            with self.assertRaises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(request, context=self.context, timeout=5)
            self.assertEqual(caught.exception.code, 403)
        self.assertEqual(self.forwarded, [])
        self.assertEqual(self.gateway.admitted, 0)

    def test_substitution_and_auth_never_reach_upstream(self):
        for payload, key, expected in [({"model": "wrong"}, None, 400),
                                       ({"model": "exact"}, "wrong-secret", 401)]:
            with self.assertRaises(urllib.error.HTTPError) as caught:
                self.request(payload, key)
            self.assertEqual(caught.exception.code, expected)
        self.assertEqual(self.forwarded, [])

    def test_absolute_deadline_stops_continuously_streaming_upstream(self):
        self.policy["request_seconds"] = .2
        started = time.monotonic()
        with self.request({"model": "exact", "trickle": True}) as response:
            response.read()
        self.assertLess(time.monotonic() - started, 1)
        self.assertTrue(self.gateway.requests[0]["transport_error"])


if __name__ == "__main__":
    unittest.main()
