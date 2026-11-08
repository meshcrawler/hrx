"""Real PostgreSQL hub and Pitput with three isolated HRX installations.

Uses the production CLI dispatcher and deterministic harnesses, with no paid
inference. The separate two-computer smoke verifies physical host isolation.
"""

import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import time
import unittest
import urllib.request

import pitput_exchange as exchange
from pitput_codex_bridge import CodexBridgeIntegration
from hub_broker import verify_broker_resume


class HubPitputIntegration(CodexBridgeIntegration):
    def test_hub_combined(self):
        self.test_hub_exchange()
        self.test_hub_native_bridge()
        verify_broker_resume(self)

    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.admin = cls.root / "hub-admin"
        cls.viewer = cls.root / "hub-viewer"
        exchange.private(cls.admin, secrets.token_hex(32))
        exchange.private(cls.viewer, secrets.token_hex(32))
        database = cls.root / "hub-database"
        exchange.private(
            database,
            f"host={cls.pg_socket} user=fixture dbname=postgres sslmode=disable",
        )
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        cls.hub_url = f"http://127.0.0.1:{port}"
        env = dict(cls.env, HRX_INTEGRATION_CA=str(cls.root / "cert.pem"))
        log = open(cls.root / "hub.log", "w")
        cls.addClassCleanup(log.close)
        server = subprocess.Popen(
            [
                exchange.HRX,
                "hub",
                "serve",
                "--database-url-file",
                str(database),
                "--admin-token-file",
                str(cls.admin),
                "--viewer-token-file",
                str(cls.viewer),
                "--listen",
                f"127.0.0.1:{port}",
            ],
            env=env,
            stdout=log,
            stderr=log,
            start_new_session=True,
        )
        cls.processes.append(server)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if server.poll() is not None:
                raise AssertionError(
                    "hub startup failed: " + (cls.root / "hub.log").read_text()
                )
            try:
                with urllib.request.urlopen(cls.hub_url + "/healthz", timeout=1):
                    return
            except OSError:
                time.sleep(0.1)
        raise AssertionError("hub startup timed out")

    def prepare_launch(self, env, config):
        exchange.run(
            [
                exchange.HRX,
                "hub",
                "enroll",
                "--url",
                self.hub_url,
                "--admin-token-file",
                str(self.admin),
                "--prefix",
                "test/",
            ],
            env=env,
        )
        log = open(Path(env["HOME"]) / "collector.log", "w")
        self.addCleanup(log.close)
        collector = subprocess.Popen(
            [exchange.HRX, "hub", "collect"],
            env=env,
            stdout=log,
            stderr=log,
            start_new_session=True,
        )
        self.processes.append(collector)
        # Wait for this collector to open its durable outbox before launching.
        outbox = Path(env["XDG_STATE_HOME"]) / "hrx/hub/outbox.json"
        deadline = time.monotonic() + 10
        while not outbox.exists():
            if collector.poll() is not None or time.monotonic() >= deadline:
                raise AssertionError("collector failed to initialize")
            time.sleep(0.1)
        return ["--agent-identity", env["TEST_IDENTITY"]]

    def test_hub_exchange(self):
        self.test_three_hrx_processes_exchange_and_capture_sessions()
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            req = urllib.request.Request(
                self.hub_url + "/api/v1/agents",
                headers={"Authorization": "Bearer " + self.viewer.read_text().strip()},
            )
            with urllib.request.urlopen(req, timeout=5) as response:
                snapshot = json.load(response)
            rows = snapshot["agents"]
            if len(rows) == 3 and all(row["status"] == "exited" for row in rows):
                self.assertEqual(len({r["host_id"] for r in rows}), 3)
                self.assertEqual(
                    {r["agent"] for r in rows},
                    {"test/alpha/agent", "test/beta/agent", "test/gamma/agent"},
                )
                self.assertTrue(
                    all(r["session_id"].startswith("fixture-") for r in rows)
                )
                self.assertTrue(all(r["model"] == "fixture" for r in rows))
                return
            time.sleep(0.2)
        self.fail("hub did not receive all final Pitput-agent exits")

    def prepare_native_launch(self, env):
        config = Path(env["HOME"]) / "hrx.toml"
        exchange.private(config, "")
        env.update(
            HRX_CONFIG=str(config),
            HRX_INTEGRATION_CA=str(self.root / "cert.pem"),
            HRX_HUB_NATIVE_TEST="1",
            TEST_IDENTITY="test/codex/agent",
        )
        self.prepare_launch(env, config)

    def test_hub_native_bridge(self):
        self.test_native_codex_bridge()
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            req = urllib.request.Request(
                self.hub_url + "/api/v1/agents",
                headers={"Authorization": "Bearer " + self.viewer.read_text().strip()},
            )
            with urllib.request.urlopen(req, timeout=5) as response:
                rows = [
                    row
                    for row in json.load(response)["agents"]
                    if row["agent"] == "test/codex/agent"
                ]
            if len(rows) == 2 and all(row["status"] == "exited" for row in rows):
                self.assertNotEqual(rows[0]["run_id"], rows[1]["run_id"])
                return
            time.sleep(0.2)
        self.fail("hub did not retain both native Codex run incarnations")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--hrx", required=True)
    args = parser.parse_args()
    exchange.HRX = str(Path(args.hrx).resolve())
    suite = unittest.TestSuite([HubPitputIntegration("test_hub_combined")])
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    sys.exit(not result.wasSuccessful())
