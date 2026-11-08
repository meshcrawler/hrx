"""Real Pitput/Postgres -> HRX private Codex server -> local model fixture.

Run with uv run --with '/path/to/pitput[server]' python this_file.py.
No live profiles, forum messages, or paid model endpoints are used.
"""

import os
from pathlib import Path
import shlex
import subprocess
import time
import sys
import unittest

from pitput_exchange import PitputIntegration, pitput, private, run


class CodexBridgeIntegration(PitputIntegration):
    def prepare_native_launch(self, env):
        pass

    def test_native_codex_bridge(self):
        env = self.agent_env("codex")
        home = Path(env["HOME"])
        private(home / "bin/pitput",
                f"#!/bin/sh\nexec {shlex.quote(sys.executable)} -m ysg_mail.pitput \"$@\"\n")
        (home / "bin/pitput").chmod(0o700)
        env.update(PATH=str(home / "bin") + os.pathsep + self.env["PATH"],
                   HRX_NATIVE_SESSION_TESTS="1", HRX_PITPUT_INTEGRATION="1", HRX_PITPUT_TUI="1")
        self.prepare_native_launch(env)
        pitput(self.controller, *self.prefix, "create", "/b/exchange/codex-notification",
               "--author", "test/controller/agent", "--title", "Bridge fixture",
               "--body", "@test/codex/agent isolated notification fixture",
               "--request-id", "codex-bridge-seed")
        binary = self.root / "hrx-native.test"
        run(["go", "test", "-c", "-race", "-o", str(binary), "./internal/app"],
            env=os.environ.copy(), timeout=90)
        process = subprocess.Popen([str(binary), "-test.run=^TestNativePitputPrivateServerQueue$",
                                    "-test.timeout=90s", "-test.v"], env=env,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        deadline = time.monotonic() + 45
        state_root = home / "state/hrx/pitput"
        while not list(state_root.glob("*/boards/exchange/state/*/cursor.json")):
            if process.poll() is not None or time.monotonic() > deadline:
                self.fail("HRX did not prepare its initial cursor: " + process.communicate(timeout=5)[0])
            time.sleep(0.1)
        pitput(self.controller, *self.prefix, "post", "/b/exchange/codex-notification",
               "--author", "test/controller/agent", "--body", "Current notification after bootstrap",
               "--request-id", "codex-bridge-current")
        pitput(self.controller, *self.prefix, "post", "/b/exchange/codex-notification",
               "--author", "test/controller/agent",
               "--body", "@test/codex/agent second notification after handling ACK",
               "--request-id", "codex-bridge-second")
        output = process.communicate(timeout=100)[0]
        self.assertEqual(process.returncode, 0, output)
        self.assertIn("PASS", output)


if __name__ == "__main__":
    suite = unittest.TestSuite([CodexBridgeIntegration("test_native_codex_bridge")])
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    sys.exit(not result.wasSuccessful())
