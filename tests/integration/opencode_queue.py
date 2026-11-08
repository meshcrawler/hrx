"""Disposable native OpenCode 2 queue/wake contract against local inference."""

import base64
import http.server
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


class NativeOpenCode:
    def __init__(self, root, env=None):
        self.root = Path(root).resolve()
        self.project = self.root / "project"
        self.project.mkdir(parents=True, exist_ok=True)
        self.requests = []
        self.release = threading.Event()
        self.release.set()
        self.entered = threading.Event()
        self.responder = None
        owner = self

        class Model(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if data.get("tools"):
                    owner.requests.append(data)
                    owner.entered.set()
                    owner.release.wait(30)
                reply = owner.responder(data) if owner.responder and data.get("tools") else "fixture handled"
                self.send_response(200)
                if data.get("stream"):
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    delta = reply if isinstance(reply, dict) else {"role": "assistant", "content": reply}
                    finish_reason = "tool_calls" if delta.get("tool_calls") else "stop"
                    for delta, finish in [(delta, None), ({}, finish_reason)]:
                        event = {"id": "fixture", "object": "chat.completion.chunk", "created": 1,
                                 "model": "fixture", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
                        self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                    self.wfile.write(b"data: [DONE]\n\n")
                else:
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(json.dumps({"id": "fixture", "object": "chat.completion", "created": 1,
                                                 "model": "fixture", "choices": [{"index": 0, "message": {
                                                     "role": "assistant", "content": reply}, "finish_reason": "stop"}],
                                                 "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}).encode())

        self.model = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Model)
        threading.Thread(target=self.model.serve_forever, daemon=True).start()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        self.url = f"http://127.0.0.1:{port}"
        self.password = secrets.token_hex(32)
        self.env = dict(env or {"PATH": os.environ["PATH"], "HOME": str(self.root)})
        for kind in ["CONFIG", "DATA", "CACHE", "STATE"]:
            self.env.setdefault("XDG_" + kind + "_HOME", str(self.root / kind.lower()))
        self.env["OPENCODE_SERVER_PASSWORD"] = self.password
        self.env["OPENCODE_CONFIG_CONTENT"] = json.dumps({
            "model": "hrx/fixture", "small_model": "hrx/fixture", "enabled_providers": ["hrx"],
            "provider": {"hrx": {"npm": "@ai-sdk/openai-compatible", "name": "fixture",
                                  "options": {"baseURL": f"http://127.0.0.1:{self.model.server_port}/v1", "apiKey": "fixture"},
                                  "models": {"fixture": {"name": "fixture"}}}}})
        self.log = open(self.root / "opencode.log", "w")
        self.process = subprocess.Popen([shutil.which("opencode"), "serve", "--hostname", "127.0.0.1", "--port", str(port)],
                                        cwd=self.project, env=self.env, stdout=self.log, stderr=self.log)
        self.wait(lambda: self.api("GET", "/api/info"), "private server ready")

    def api(self, method, path, body=None, auth=True):
        headers = {"Content-Type": "application/json"}
        if auth:
            headers["Authorization"] = "Basic " + base64.b64encode(("opencode:" + self.password).encode()).decode()
        request = urllib.request.Request(self.url + path, method=method, headers=headers,
                                         data=None if body is None else json.dumps(body).encode())
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    @staticmethod
    def wait(predicate, description, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                result = predicate()
                if result:
                    return result
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.1)
        raise AssertionError(description + " timed out")

    def create(self):
        return self.api("POST", "/api/session", {"location": {"directory": str(self.project)},
                                                  "permissions": [{"action": "*", "resource": "*", "effect": "allow"}],
                                                  "model": {"providerID": "hrx", "id": "fixture"}})["data"]["id"]

    def prompt(self, session, message, text):
        return self.api("POST", f"/api/session/{session}/prompt", {
            "id": message, "text": text, "delivery": "queue", "resume": True})

    def close(self):
        self.release.set()
        self.process.terminate()
        try:
            self.process.wait(10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
        self.model.shutdown()
        self.model.server_close()
        self.log.close()


def smoke():
    with tempfile.TemporaryDirectory(prefix="hrx-opencode-queue-") as root:
        native = NativeOpenCode(root)
        try:
            try:
                native.api("GET", "/api/session", auth=False)
                raise AssertionError("unauthenticated sessions accepted")
            except urllib.error.HTTPError as error:
                assert error.code == 401
            session = native.create()
            native.release.clear()
            native.prompt(session, "msg_fixture_first", "first controlled busy turn")
            assert native.entered.wait(20), "idle session did not wake"
            accepted = native.prompt(session, "msg_fixture_second", "second queued notification")
            assert accepted["data"]["sessionID"] == session
            assert accepted["data"]["delivery"] == "queue"
            time.sleep(0.5)
            assert len(native.requests) == 1, "queue interrupted active request"
            native.release.set()
            native.wait(lambda: len(native.requests) >= 2, "queued turn execution")
            native.wait(lambda: not native.api("GET", f"/api/session/{session}/inbox")["data"], "inbox drain")
            before = len(native.requests)
            try:
                native.prompt(session, "msg_fixture_second", "second queued notification")
                print("duplicate prompt accepted")
            except urllib.error.HTTPError as error:
                print("duplicate prompt status:", error.code)
            time.sleep(1)
            assert len(native.requests) == before, "duplicate ID executed twice"
            print("PASS: authenticated private server, idle wake, busy queue, no duplicate execution")
        finally:
            native.close()


if __name__ == "__main__":
    smoke()
