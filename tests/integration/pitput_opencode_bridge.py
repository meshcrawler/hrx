"""Real disposable Pitput/Postgres and OpenCode; deterministic local model.

The native OpenCode agent executes the reply/ACK shell tools itself. No paid
model, production forum, user profile or installed launcher is changed.
"""

import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time
import unittest

from opencode_queue import NativeOpenCode
from pitput_exchange import PitputIntegration, pitput, private, run


def describe_requests(requests):
    """Name each model turn by its last message, so an unexpected turn shows its origin
    (the busy prompt, a queued notification, or a trailing tool-result continuation)."""
    lines = []
    for request in requests:
        last = (request.get("messages") or [{}])[-1]
        content = last.get("content", "")
        if not isinstance(content, str):
            content = json.dumps(content)
        lines.append(f"{last.get('role')}: {content[:160]!r}")
    return "model turns since the busy prompt: " + "; ".join(lines)


def continued(requests, fragment):
    """True once a model turn has carried a tool result mentioning fragment."""
    for request in list(requests):
        last = (request.get("messages") or [{}])[-1]
        content = last.get("content", "")
        if last.get("role") == "tool" and fragment in (content if isinstance(content, str)
                                                        else json.dumps(content)):
            return True
    return False


class OpenCodeBridgeIntegration(PitputIntegration):
    def test_hrx_native_runtime(self):
        env = self.agent_env("opencode")
        home = Path(env["HOME"])
        private(home / "bin/pitput", f"#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput \"$@\"\n")
        (home / "bin/pitput").chmod(0o700)
        env.update(PATH=str(home / "bin") + os.pathsep + self.env["PATH"],
                   HRX_NATIVE_SESSION_TESTS="1", HRX_PITPUT_INTEGRATION="1")
        pitput(self.controller, *self.prefix, "create", "/b/exchange/opencode-runtime",
               "--author", "test/controller/agent", "--title", "Runtime fixture",
               "--body", "@test/opencode/agent HRX native runtime notification", "--request-id", "opencode-runtime-seed")
        binary = self.root / "hrx-native.test"
        run(["go", "test", "-c", "-race", "-o", str(binary), "./internal/app"], env=os.environ.copy(), timeout=90)
        process = subprocess.Popen(
            # Three sequential modes, each bounded by its own 80 s context in the test.
            [str(binary), "-test.run=^TestNativeOpenCodePitputBridge$", "-test.timeout=270s", "-test.v"],
            env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )
        self.addCleanup(lambda: process.poll() is None and process.kill())
        deadline = time.monotonic() + 45
        while not list((home / "state/hrx/pitput").glob("*/boards/exchange/state/*/cursor.json")):
            if process.poll() is not None or time.monotonic() > deadline:
                self.fail("HRX did not prepare its initial cursor: " + process.communicate(timeout=5)[0])
            time.sleep(0.1)
        # This arrives after the persisted snapshot but before/while the private
        # server and TUI start. It must not be lost with skipped historical seed.
        pitput(self.controller, *self.prefix, "post", "/b/exchange/opencode-runtime",
               "--author", "test/controller/agent", "--body", "Current runtime notification",
               "--request-id", "opencode-runtime-current")
        output = process.communicate(timeout=280)[0]
        self.assertEqual(process.returncode, 0, output)
        self.assertIn("PASS", output)
        print(output)

    def test_native_opencode_delivery(self):
        env = self.agent_env("opencode")
        home = Path(env["HOME"])
        identity = "test/opencode/agent"
        pitput(env, "join", "exchange", "--identity", identity)
        native = NativeOpenCode(home, env)
        self.addCleanup(native.close)
        session = native.create()
        password = home / "password"
        private(password, native.password)
        config = home / "bridge.toml"
        private(config, f'''backend = "opencode"
opencode_url = "{native.url}"
opencode_password_file = "{password}"
opencode_project_directory = "{native.project}"
state_directory = "{home / 'bridge-state'}"
retry_seconds = 0.1
monitor_timeout = 1
[[agents]]
agent_id = "{identity}"
board = "exchange"
thread_id = "{session}"
''')
        replies = set()

        def respond(data):
            messages = data["messages"]
            if messages[-1]["role"] == "tool":
                return "Handled fixture notification and acknowledged it."
            text = json.dumps(messages[-1].get("content", ""))
            # The helper command is launcher-generated; forum content is not
            # interpreted as a command by this deterministic model fixture.
            match = re.search(r"Helper ACK: (.+?)(?:\\n|\"$)", text)
            event = re.search(r"Event key: ([a-f0-9]+)", text)
            if not match or not event:
                return "Controlled busy turn completed."
            author = "test/peer/agent" if "Agent: test/peer/agent" in text else identity
            topic = "/b/restricted/opencode-peer" if author == "test/peer/agent" else "/b/exchange/opencode-native"
            key = event.group(1)
            if (author, key) in replies:
                return "Already handled."
            replies.add((author, key))
            ack = match.group(1).replace('\\"', '"')
            reply = shlex.join([sys.executable, "-m", "ysg_mail.pitput", "--agent", author,
                                "post", topic, "--author", author,
                                "--body", "Native OpenCode handled " + key,
                                "--request-id", "opencode-" + key])
            return {"role": "assistant", "tool_calls": [{"index": 0, "id": "call_" + key[:12],
                    "type": "function", "function": {"name": "shell", "arguments": json.dumps({
                        "command": reply + " && " + ack, "workdir": str(native.project)})}}]}

        native.responder = respond
        output = open(home / "bridge.log", "w+")
        self.addCleanup(output.close)

        def start_bridge():
            process = subprocess.Popen([sys.executable, "-m", "ysg_mail.pitput", "bridge", "run", "--config", str(config)],
                                       env=env, stdout=output, stderr=output, start_new_session=True)
            self.processes.append(process)
            return process

        bridge = start_bridge()
        pitput(self.controller, *self.prefix, "create", "/b/exchange/opencode-native", "--author", "test/controller/agent",
               "--title", "OpenCode integration", "--body", "@" + identity + " First notification", "--request-id", "native-first")
        status = lambda: pitput(env, "bridge", "status", "--config", str(config))

        def replied(count):
            messages = pitput(self.controller, *self.prefix, "read", "/b/exchange/opencode-native")
            return sum(m["author"] == identity for m in messages) >= count

        native.wait(lambda: replied(1), "native agent reply", timeout=90)
        native.wait(lambda: status()["agents"][0]["pending"] is None, "first handling ACK")
        # The ACK happens inside the agent's shell tool; OpenCode then sends that tool's
        # output back to the model. Wait for that continuation so it cannot land later.
        native.wait(lambda: continued(native.requests, "/b/exchange/opencode-native/"),
                    "first handling continuation")
        # Two concurrently registered identities have separate native targets,
        # source credentials, selected boards, cursor/spool and handling ACKs.
        pitput(env, "join", "restricted", "--identity", "test/peer/agent")
        peer_session = native.create()
        peer_config = home / "peer-bridge.toml"
        private(peer_config, config.read_text().replace('board = "exchange"', 'board = "restricted"')
                .replace(identity, "test/peer/agent").replace(session, peer_session)
                .replace(str(home / "bridge-state"), str(home / "peer-state")))
        peer = subprocess.Popen([sys.executable, "-m", "ysg_mail.pitput", "bridge", "run", "--config", str(peer_config)],
                                env=env, stdout=output, stderr=output, start_new_session=True)
        self.processes.append(peer)
        pitput(self.controller, *self.prefix, "create", "/b/restricted/opencode-peer", "--author", "test/controller/agent",
               "--title", "Peer isolation", "--body", "@test/peer/agent Isolated peer event", "--request-id", "peer-seed")
        native.wait(lambda: any(m["author"] == "test/peer/agent" for m in
                                pitput(self.controller, *self.prefix, "read", "/b/restricted/opencode-peer")), "peer reply")
        native.wait(lambda: pitput(env, "bridge", "status", "--config", str(peer_config))["agents"][0]["pending"] is None,
                    "peer handling ACK")
        # Job 3143 counted exactly this trailing peer tool-result turn inside the busy
        # window below; wait for it before measuring that window.
        native.wait(lambda: continued(native.requests, "/b/restricted/opencode-peer/"),
                    "peer handling continuation")
        for target, excluded in [(session, "Agent: test/peer/agent"), (peer_session, "Agent: " + identity)]:
            self.assertNotIn(excluded, json.dumps(native.api("GET", f"/api/session/{target}/message")))
        peer.terminate()
        peer.wait(10)
        # A turn held inside the real OpenCode provider must finish before its
        # queued Pitput notification starts another turn.
        native.entered.clear()
        native.release.clear()
        native.prompt(session, "msg_fixture_busy", "Controlled busy turn")
        self.assertTrue(native.entered.wait(20))
        before = len(native.requests)
        pitput(self.controller, *self.prefix, "post", "/b/exchange/opencode-native", "--author", "test/controller/agent",
               "--body", "@" + identity + " Busy notification", "--request-id", "native-busy")
        native.wait(lambda: status()["agents"][0]["pending"] is not None and
                    status()["agents"][0]["status"] == "notified", "busy queued notification")
        self.assertEqual(len(native.requests), before, describe_requests(native.requests[before - 1:]))
        # Admission survives consumer restart while the original turn is held.
        bridge.terminate()
        bridge.wait(10)
        bridge = start_bridge()
        time.sleep(0.5)
        self.assertEqual(len(native.requests), before)
        native.release.set()
        native.wait(lambda: replied(2), "busy notification reply", timeout=60)
        native.wait(lambda: status()["agents"][0]["pending"] is None, "busy handling ACK")
        # A stopped native server cannot lose the next forum event. The bridge
        # retries its durable pending state when that exact session returns.
        native.process.terminate()
        native.process.wait(10)
        pitput(self.controller, *self.prefix, "post", "/b/exchange/opencode-native", "--author", "test/controller/agent",
               "--body", "@" + identity + " Recover after server outage", "--request-id", "native-outage")
        native.wait(lambda: status()["agents"][0]["last_error"] is not None, "visible native outage")
        native.process = subprocess.Popen(native.process.args, env=native.env, cwd=native.project,
                                          stdout=native.log, stderr=native.log)
        try:
            native.wait(lambda: replied(3), "notification after native restart", timeout=60)
        except AssertionError as error:
            # Show whether delivery was still retrying or had paused after the outage.
            raise AssertionError(f"{error}; bridge state: {json.dumps(status()['agents'][0])}") from None
        native.wait(lambda: status()["agents"][0]["pending"] is None, "recovery handling ACK")
        self.server.terminate()
        self.server.wait(10)
        native.wait(lambda: status()["agents"][0]["last_error"] is not None, "visible Pitput outage")
        self.start_server()
        pitput(self.controller, *self.prefix, "post", "/b/exchange/opencode-native", "--author", "test/controller/agent",
               "--body", "@" + identity + " Recover after source outage", "--request-id", "source-outage")
        native.wait(lambda: replied(4), "notification after Pitput restart", timeout=60)
        native.wait(lambda: status()["agents"][0]["pending"] is None, "source recovery handling ACK")
        bridge.terminate()
        bridge.wait(10)
        bridge = start_bridge()
        time.sleep(1)
        self.assertFalse(replied(5), "handled notification replayed on restart")
        self.assertIsNone(bridge.poll())
        bridge.terminate()
        bridge.wait(10)
        # Fresh native successor keeps the exact remote monitor and its handled
        # boundary, even though the session and immutable transport config change.
        old_config = config
        successor = native.create()
        config = home / "successor-bridge.toml"
        private(config, old_config.read_text().replace(session, successor))
        cursor_before = next((home / "bridge-state").glob("*/cursor.json")).read_bytes()
        pitput(env, "bridge", "prepare", "--config", str(config))
        self.assertEqual(next((home / "bridge-state").glob("*/cursor.json")).read_bytes(), cursor_before)
        pitput(self.controller, *self.prefix, "post", "/b/exchange/opencode-native", "--author", "test/controller/agent",
               "--body", "Current work for successor", "--request-id", "native-successor")
        bridge = start_bridge()
        native.wait(lambda: replied(5), "successor notification", timeout=60)
        native.wait(lambda: status()["agents"][0]["pending"] is None, "successor handling ACK")
        self.assertIn("Pitput attention notification", json.dumps(native.api("GET", f"/api/session/{successor}/message")))
        time.sleep(1)
        self.assertFalse(replied(6), "handled predecessor history replayed to successor")
        stale = pitput(env, "bridge", "status", "--config", str(old_config), check=False)
        self.assertIn("target_mapping_changed", stale.stdout)
        bridge.terminate()
        bridge.wait(10)
        print("PASS: native reply+ACK, two identities, busy queue, pending restart, server/source outages, successor continuity")


if __name__ == "__main__":
    names = sys.argv[1:] or ["test_native_opencode_delivery", "test_hrx_native_runtime"]
    suite = unittest.TestSuite([OpenCodeBridgeIntegration(name) for name in names])
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    sys.exit(not result.wasSuccessful())
