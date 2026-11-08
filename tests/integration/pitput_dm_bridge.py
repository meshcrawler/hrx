"""Real Pitput/Postgres direct message -> HRX DM bridge -> private Codex -> model fixture.

Run with: make test-pitput-dm PITPUT_SOURCE=/path/to/pitput
No live profiles, forum messages, or paid model endpoints are used. The DM body is a
random sentinel that must never appear in the session notification.
"""

import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import time
import unittest

from opencode_queue import NativeOpenCode
from pitput_exchange import PitputIntegration, pitput, private, run


class DirectMessageBridgeIntegration(PitputIntegration):
    def test_native_codex_direct_message(self):
        env = self.agent_env("codex")
        home = Path(env["HOME"])
        private(home / "bin/pitput",
                f"#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput \"$@\"\n")
        (home / "bin/pitput").chmod(0o700)
        sentinel = "dm-body-sentinel-" + secrets.token_hex(8)
        env.update(PATH=str(home / "bin") + os.pathsep + self.env["PATH"],
                   HRX_NATIVE_SESSION_TESTS="1", HRX_PITPUT_DM_INTEGRATION="1",
                   HRX_PITPUT_DM_SENTINEL=sentinel)
        binary = self.root / "hrx-native.test"
        run(["go", "test", "-c", "-race", "-o", str(binary), "./internal/app"],
            env=os.environ.copy(), timeout=120)
        process = subprocess.Popen([str(binary), "-test.run=^TestNativePitputDirectMessageQueue$",
                                    "-test.timeout=120s", "-test.v"], env=env,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        deadline = time.monotonic() + 60
        ready = home / "dm-bridge-ready"
        state_root = home / "state/hrx/pitput"
        while not (ready.exists() and list(state_root.glob("*/boards/@dm/state/*/cursor.json"))):
            if process.poll() is not None or time.monotonic() > deadline:
                self.fail("HRX did not prepare its DM consumer: " + process.communicate(timeout=5)[0])
            time.sleep(0.1)
        receipt = pitput(self.controller, *self.prefix, "dm", "send", "test/codex/agent",
                         "--body", sentinel, "--request-id", "native-dm-1")
        self.assertTrue(receipt["stored"])
        output = process.communicate(timeout=150)[0]
        self.assertEqual(process.returncode, 0, output)
        # A skipped test is not proof.
        self.assertIn("--- PASS: TestNativePitputDirectMessageQueue", output)
        self.assertNotIn(sentinel, output)

    def test_native_opencode_direct_message_reply_and_ack(self):
        env = self.agent_env("opencode-dm")
        home = Path(env["HOME"])
        identity = "test/opencodedm/agent"
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
thread_id = "{session}"
source = "dm"
''')
        handled = set()

        def respond(data):
            messages = data["messages"]
            if messages[-1]["role"] == "tool":
                return "Replied to the direct message and acknowledged it."
            text = json.dumps(messages[-1].get("content", ""))
            ack = re.search(r"Helper ACK: (.+?)(?:\\n|\"$)", text)
            message = re.search(r"Message: (dm-[a-z2-7]{12})", text)
            if not ack or not message or message.group(1) in handled:
                return "Nothing to handle."
            handled.add(message.group(1))
            reply = shlex.join([sys.executable, "-m", "ysg_mail.pitput", "--agent", identity,
                                "dm", "reply", message.group(1), "--body", "handled by OpenCode",
                                "--request-id", "opencode-dm-" + message.group(1)])
            return {"role": "assistant", "tool_calls": [{"index": 0, "id": "call_dm",
                    "type": "function", "function": {"name": "shell", "arguments": json.dumps({
                        "command": reply + " && " + ack.group(1).replace('\\"', '"'),
                        "workdir": str(native.project)})}}]}

        native.responder = respond
        log = open(home / "bridge.log", "w+")
        self.addCleanup(log.close)
        bridge = subprocess.Popen([sys.executable, "-m", "ysg_mail.pitput", "bridge", "run",
                                   "--config", str(config)], env=env, stdout=log, stderr=log,
                                  start_new_session=True)
        self.processes.append(bridge)
        sentinel = "dm-body-sentinel-" + secrets.token_hex(8)
        sent = pitput(self.controller, *self.prefix, "dm", "send", identity, "--body", sentinel,
                      "--request-id", "opencode-dm-1")
        status = lambda: pitput(env, "bridge", "status", "--config", str(config))["agents"][0]

        def replied():
            thread = pitput(self.controller, *self.prefix, "dm", "read", sent["conversation"])
            return any(item["from"] == identity for item in thread)

        native.wait(replied, "native direct-message reply", timeout=90)
        native.wait(lambda: status()["pending"] is None and status()["status"] == "idle",
                    "direct-message handling ACK")
        self.assertEqual(status()["source"], "dm")
        unhandled = pitput(env, "--agent", identity, "dm", "inbox", "--unhandled")
        self.assertEqual(unhandled, [])
        self.assertNotIn(sentinel, json.dumps(native.api("GET", f"/api/session/{session}/message")))


def blocked_reason(harness):
    """Missing prerequisites are reported as BLOCKED, never as a skipped success."""
    import importlib.util
    import shutil
    if importlib.util.find_spec("ysg_mail.direct_messages") is None:
        return "pitput_without_direct_messages"
    if not any(shutil.which(name) for name in ("initdb", "pg_ctl")) and not list(
            Path("/opt/homebrew/opt").glob("postgresql*/bin/initdb")):
        return "postgresql_17_binaries_missing"
    if harness == "claude":
        # Claude delivery is its own background monitor (`monitor --include-dms`); proving
        # it needs a real, paid Claude session, which only the mayor schedules.
        return "paid_claude_session_requires_mayor_authorization"
    if shutil.which(harness) is None:
        return harness + "_binary_missing"
    return None


def main():
    import argparse
    from xml.sax.saxutils import quoteattr
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--junit", type=Path)
    arguments = parser.parse_args()
    cases = {"codex": "test_native_codex_direct_message",
             "opencode": "test_native_opencode_direct_message_reply_and_ack", "claude": None}
    results = {}
    for harness, name in cases.items():
        reason = blocked_reason(harness)
        if reason:
            results[harness] = ("BLOCKED", reason)
            continue
        # A suite runs the class fixtures (disposable PostgreSQL, Pitput server, gateway).
        outcome = unittest.TextTestRunner(verbosity=2).run(
            unittest.TestSuite([DirectMessageBridgeIntegration(name)]))
        results[harness] = ("PASS", "") if outcome.wasSuccessful() else ("FAIL", "see job log")
    for harness, (status, reason) in results.items():
        print(f"{status:7} pitput-dm-native-{harness}" + (f": {reason}" if reason else ""))
    if arguments.junit:
        lines = [f'<testsuite name="pitput-dm-native" tests="{len(results)}">']
        for harness, (status, reason) in results.items():
            lines.append(f'  <testcase classname="pitput-dm-native" name="{harness}">')
            if status == "FAIL":
                lines.append(f"    <failure message={quoteattr(reason)}/>")
            elif status == "BLOCKED":
                lines.append(f"    <error message={quoteattr('BLOCKED: ' + reason)}/>")
            lines.append("  </testcase>")
        arguments.junit.parent.mkdir(parents=True, exist_ok=True)
        arguments.junit.write_text("\n".join([*lines, "</testsuite>"]) + "\n")
    statuses = {status for status, _ in results.values()}
    return 1 if "FAIL" in statuses else 2 if "BLOCKED" in statuses else 0


if __name__ == "__main__":
    sys.exit(main())
