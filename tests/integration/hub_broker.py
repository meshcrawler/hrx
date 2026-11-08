"""Real broker and hub lifecycle, with an isolated GitLab API fixture."""

import hashlib
import http.server
import json
import os
from pathlib import Path
import shlex
import select
import sqlite3
import ssl
import subprocess
import sys
import threading
import time
import urllib.request

import pitput_exchange as exchange


def verify_broker_resume(test):
    minted = []
    accounts = []
    admin = "gitlab-admin-canary"
    client = "broker-client-canary-" * 3
    identity = "test/broker/agent"

    class GitLab(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            if self.headers.get("PRIVATE-TOKEN") != admin:
                self.send_error(403)
                return
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if self.path == "/api/v4/groups/7/service_accounts":
                accounts.append(body["username"])
                result = {"id": 42, "username": body["username"]}
            elif (
                self.path
                == "/api/v4/groups/7/service_accounts/42/personal_access_tokens"
            ):
                minted.append("service-token-canary-" + str(len(minted) + 1))
                result = {
                    "user_id": 42,
                    "token": minted[-1],
                    "expires_at": body["expires_at"],
                }
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps(result).encode())

    gitlab = http.server.ThreadingHTTPServer(("127.0.0.1", 0), GitLab)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(test.root / "cert.pem", test.root / "key.pem")
    gitlab.socket = tls.wrap_socket(gitlab.socket, server_side=True)
    threading.Thread(target=gitlab.serve_forever, daemon=True).start()
    test.addCleanup(gitlab.server_close)
    test.addCleanup(gitlab.shutdown)
    root = test.root / "broker-fixture"
    root.mkdir()
    exchange.private(root / "admin", admin)
    exchange.private(root / "client", client)
    exchange.private(
        root / "config.json",
        json.dumps(
            {
                "gitlab_url": f"https://127.0.0.1:{gitlab.server_port}",
                "gitlab_token_file": str(root / "admin"),
                "state_dir": str(root / "state"),
                "group_id": 7,
                "enrollment_clients": {
                    "fixture": {
                        "sha256": hashlib.sha256(client.encode()).hexdigest(),
                        "prefix": "test/",
                        "profile": "developer",
                    }
                },
            }
        ),
    )
    module = (
        Path(__file__).resolve().parents[2] / "services/gitlab-service-account-broker"
    )
    script = """import json,sys,os
sys.path.insert(0,sys.argv[1])
from broker import Broker,Handler,ThreadingHTTPServer
os.umask(0o077)
server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
server.broker=Broker(json.load(open(sys.argv[2])))
print(server.server_port,flush=True)
server.serve_forever()
"""
    log = open(root / "broker.log", "w+")
    test.addCleanup(log.close)
    broker = subprocess.Popen(
        [sys.executable, "-c", script, str(module), str(root / "config.json")],
        env=dict(test.env, SSL_CERT_FILE=str(test.root / "cert.pem")),
        stdout=subprocess.PIPE,
        stderr=log,
        text=True,
        start_new_session=True,
    )
    test.processes.append(broker)
    if not select.select([broker.stdout], [], [], 10)[0]:
        raise AssertionError("broker startup timed out")
    port = int(broker.stdout.readline().strip())
    env = test.agent_env("broker-agent")
    home = Path(env["HOME"])
    config = home / "hrx.toml"
    exchange.private(
        config,
        f'''[claude]
model="fixture"
[claude.provider]
base_url="https://127.0.0.1:{test.gateway.server_port}/v1"
env_key="HRX_FIXTURE_KEY"
[plugins.gitlab-service-account-broker]
url="http://127.0.0.1:{port}"
enrollment_token_file="{root / "client"}"
''',
    )
    fixture_bin = home / "bin"
    fixture_bin.mkdir()
    harness = """import os,sys,json,shlex,subprocess
args=sys.argv[1:]
assert os.environ['GITLAB_TOKEN']==os.environ['EXPECTED_SERVICE_TOKEN']
assert 'GitLab service account' in args[args.index('--append-system-prompt')+1]
if os.environ['EXPECT_RESUME']=='1':assert args[args.index('--resume')+1]=='broker-native-session'
hook=json.loads(args[args.index('--settings')+1])['hooks']['SessionStart'][0]['hooks'][0]['command']
event={'hook_event_name':'SessionStart','source':'resume' if os.environ['EXPECT_RESUME']=='1' else 'startup','session_id':'broker-native-session','cwd':os.getcwd(),'model':'fixture/route'}
subprocess.run(shlex.split(hook),input=json.dumps(event),text=True,check=True)
"""
    exchange.private(home / "harness.py", harness)
    exchange.private(
        fixture_bin / "claude",
        "#!/bin/sh\nexec "
        + shlex.quote(sys.executable)
        + " "
        + shlex.quote(str(home / "harness.py"))
        + ' "$@"\n',
    )
    (fixture_bin / "claude").chmod(0o700)
    env.update(
        HRX_CONFIG=str(config),
        HRX_FIXTURE_KEY="fixture-key",
        HRX_INTEGRATION_CA=str(test.root / "cert.pem"),
        TEST_IDENTITY=identity,
        PATH=str(fixture_bin) + os.pathsep + test.env["PATH"],
    )
    test.prepare_launch(env, config)
    for iteration in range(2):
        args = (
            ["--gitlab", "--agent-identity", identity]
            if iteration == 0
            else ["--resume", "broker-native-session"]
        )
        result = subprocess.run(
            [exchange.HRX, "claude", *args, "--", "-p", "fixture"],
            env=dict(
                env,
                EXPECT_RESUME=str(iteration),
                EXPECTED_SERVICE_TOKEN=f"service-token-canary-{iteration + 1}",
            ),
            cwd=home,
            capture_output=True,
            text=True,
            timeout=20,
        )
        test.assertEqual(result.returncode, 0, result.stderr)
        test.assertTrue(
            all(
                secret not in result.stdout + result.stderr
                for secret in [admin, client, *minted]
            )
        )
        if iteration == 0:
            with sqlite3.connect(root / "state/accounts.sqlite3") as db:
                db.execute(
                    "UPDATE accounts SET expires_at='2000-01-01' WHERE identity=?",
                    (identity,),
                )
    test.assertEqual(len(accounts), 1)
    test.assertEqual(len(minted), 2)
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        request = urllib.request.Request(
            test.hub_url + "/api/v1/agents",
            headers={"Authorization": "Bearer " + test.viewer.read_text().strip()},
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        rows = [r for r in json.loads(raw)["agents"] if r["agent"] == identity]
        if len(rows) == 2 and all(r["status"] == "exited" for r in rows):
            break
        time.sleep(0.2)
    else:
        raise AssertionError("broker runs not finalized in hub")
    test.assertEqual({r["session_id"] for r in rows}, {"broker-native-session"})
    test.assertEqual(len({r["run_id"] for r in rows}), 2)
    test.assertEqual({r["ownership"]["generation"] for r in rows}, {1, 2})
    test.assertTrue(
        all(
            r["credential"]["provider"] == "gitlab"
            and r["credential"]["username"] == accounts[0]
            for r in rows
        )
    )
    stored = exchange.run(
        [
            str(Path(test.pg_ctl).with_name("psql")),
            "-h",
            str(test.pg_socket),
            "-U",
            "fixture",
            "-d",
            "postgres",
            "-Atc",
            "SELECT record::text FROM hrx_hub_runs",
        ],
        env=test.env,
    ).stdout
    log.flush()
    logs = (
        (root / "broker.log").read_text()
        + (home / "collector.log").read_text()
        + (test.root / "hub.log").read_text()
    )
    enrolled = list((home / "client-tokens").rglob("broker.token"))
    test.assertEqual(len(enrolled), 1)
    enrolled_token = enrolled[0].read_text().strip()
    test.assertTrue(
        all(
            secret not in raw + stored + logs
            for secret in [admin, client, enrolled_token, *minted]
        )
    )
