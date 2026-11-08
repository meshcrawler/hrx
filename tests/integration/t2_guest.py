"""Guest-side toolkit for the T2 controller. Runs as hrx-fixture inside a guest.

Each command takes JSON parameters and prints one JSON line. Everything lives under the
run directory; nothing outside it is created or changed. Fixture credentials are
disposable per run.
"""

import json
import os
from pathlib import Path
import secrets
import signal
import ssl
import subprocess
import sys
import time
import urllib.request
import t2_processes


PG_BIN = Path("/usr/lib/postgresql/17/bin")
FAKE_CLAUDE = "#!/bin/sh\necho ready\nexec sleep 600\n"
CONFIG = '[claude]\nmodel = "fixture-model"\n'


def private(path, text):
    path.write_text(text)
    path.chmod(0o600)


def environment(run, config="config.toml"):
    return dict(PATH=f"{run / 'bin'}:/usr/local/bin:/usr/bin:/bin", HOME=str(run / "home"),
                HRX_CONFIG=str(run / config), XDG_STATE_HOME=str(run / "state"),
                SSL_CERT_FILE=str(run / "cert.pem"), HRX_HUB_PROCESS_FIXTURE="1", HRX_MONITOR="on")


def spawn(run, name, args, env):
    with open(run / (name + ".log"), "w") as log:
        child = subprocess.Popen(args, env=env, stdout=log, stderr=log, start_new_session=True)
    t2_processes.save(run / (name + ".pid"), child.pid)
    return child.pid


def prepare(run, P):
    for directory in (run, run / "bin", run / "home"):
        directory.mkdir(mode=0o700, exist_ok=True)
    claude = run / "bin" / "claude"
    claude.write_text(FAKE_CLAUDE)
    claude.chmod(0o700)
    # scp modes depend on umask; make the copied binaries explicitly executable.
    for binary in ("hrx", "app.test"):
        (run / "bin" / binary).chmod(0o700)
    return {"prepared": str(run)}


def hub_start(run, P):
    """Create the PostgreSQL cluster, tokens and certificate once, then start services."""
    if not (run / "pg").exists():
        subprocess.run([str(PG_BIN / "initdb"), "-D", str(run / "pg"), "-U", "fixture", "--auth=trust",
                        "-E", "UTF8"], check=True, capture_output=True)
        for name in ("admin", "viewer"):
            private(run / (name + ".token"), secrets.token_hex(32))
        private(run / "database", f"host={run} user=fixture dbname=postgres sslmode=disable")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=hrx-t2-hub", "-addext", f"subjectAltName=IP:{P['address']},IP:127.0.0.1",
                        "-keyout", str(run / "key.pem"), "-out", str(run / "cert.pem")],
                       check=True, capture_output=True)
        (run / "key.pem").chmod(0o600)
    return hub_services(run, P)


def hub_services(run, P):
    """Start PostgreSQL and the hub from existing state (also after a guest restart)."""
    subprocess.run([str(PG_BIN / "pg_ctl"), "-D", str(run / "pg"), "-l", str(run / "pg.log"), "-o",
                    f"-k {run} -c listen_addresses=''", "-w", "start"], check=True, capture_output=True)
    spawn(run, "hub", [str(run / "bin" / "hrx"), "hub", "serve", "--database-url-file", str(run / "database"),
                       "--admin-token-file", str(run / "admin.token"), "--viewer-token-file",
                       str(run / "viewer.token"), "--listen", f"0.0.0.0:{P['port']}",
                       "--tls-cert", str(run / "cert.pem"), "--tls-key", str(run / "key.pem")],
          dict(os.environ, SSL_CERT_FILE=str(run / "cert.pem")))
    context = ssl.create_default_context(cafile=str(run / "cert.pem"))
    deadline = time.monotonic() + 30
    while True:
        try:
            urllib.request.urlopen(f"https://127.0.0.1:{P['port']}/healthz", context=context, timeout=2)
            return {"hub": "healthy"}
        except OSError:
            if time.monotonic() >= deadline:
                raise SystemExit("hub never became healthy: " + (run / "hub.log").read_text()[-2000:])
            time.sleep(0.3)


def credentials(run, P):
    """Disposable per-run fixture material the other guest needs."""
    return {name: (run / name).read_text() for name in ("cert.pem", "admin.token", "viewer.token")}


def install_credentials(run, P):
    for name, text in P["files"].items():
        private(run / name, text)
    return {"installed": sorted(P["files"])}


def enroll(run, P):
    if not (run / "config.toml").exists():
        private(run / "config.toml", CONFIG)
    result = subprocess.run([str(run / "bin" / "hrx"), "hub", "enroll", "--url", P["url"], "--admin-token-file",
                             str(run / "admin.token"), "--prefix", "fixture/"], env=environment(run),
                            capture_output=True, text=True)
    if result.returncode:
        raise SystemExit(f"enroll exited {result.returncode}: {result.stderr[-2000:]}")
    return {"enrolled": P["url"]}


def collector(run, P):
    env = environment(run, P.get("config", "config.toml"))
    pid = spawn(run, P.get("name", "collector"), [str(run / "bin" / "hrx"), "hub", "collect"], env)
    outbox = run / "state" / "hrx" / "hub" / "outbox.json"
    deadline = time.monotonic() + 20
    while not outbox.exists():
        if (time.monotonic() >= deadline
                or t2_processes.matching(run / (P.get("name", "collector") + ".pid")) is None):
            raise SystemExit("collector never initialized")
        time.sleep(0.2)
    return {"pid": pid, "outbox": json.loads(outbox.read_text())}


def agent(run, P):
    env = dict(environment(run, P.get("config", "config.toml")), HRX_HUB_FIXTURE_IDENTITY=P["identity"])
    return {"pid": spawn(run, P["name"], [str(run / "bin" / "app.test"), "-test.run=^TestHubProcessFixture$"], env)}


def wait(run, P):
    path = run / (P["name"] + ".pid")
    deadline = time.monotonic() + P.get("timeout", 15)
    while t2_processes.matching(path) and time.monotonic() < deadline:
        time.sleep(0.2)
    return {"exited": t2_processes.matching(path) is None,
            "log": (run / (P["name"] + ".log")).read_text()[-3000:]}


def send(run, P):
    """Signal one of this run's own processes, identified by its pid file."""
    return {"signalled": t2_processes.signal_owned(
        run / (P["name"] + ".pid"), getattr(signal, P["signal"]))}


def snapshot(run, P):
    request = urllib.request.Request(P["url"] + "/api/v1/agents", headers={
        "Authorization": "Bearer " + (run / "viewer.token").read_text().strip()})
    context = ssl.create_default_context(cafile=str(run / "cert.pem"))
    with urllib.request.urlopen(request, context=context, timeout=5) as response:
        return {"agents": json.load(response)["agents"]}


def monitor_state(run, P):
    path = run / "state" / "hrx" / "monitor" / (P["run_id"] + ".json")
    return json.loads(path.read_text()) if path.exists() else {"missing": True}


def outbox(run, P):
    return json.loads((run / "state" / "hrx" / "hub" / "outbox.json").read_text())


PROXY = r'''
import sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from hub_partition import PartitionProxy
run = Path(sys.argv[1])
proxy = PartitionProxy(sys.argv[2], run / "cert.pem")
state = "heal"
(run / "proxy.state").write_text(state)
(run / "proxy.url").write_text(proxy.url)
while True:
    command = (run / "proxy.command").read_text().strip() if (run / "proxy.command").exists() else state
    if command == "stop":
        proxy.close()
        break
    if command in ("partition", "heal") and command != state:
        getattr(proxy, command)()
        state = command
        (run / "proxy.state").write_text(state)
    time.sleep(0.1)
'''


def proxy(run, P):
    """Start a loopback partition proxy to the hub and a config that routes through it."""
    (run / "proxy.py").write_text(PROXY)
    for stale in ("proxy.url", "proxy.command", "proxy.state"):
        (run / stale).unlink(missing_ok=True)
    spawn(run, "proxy", [sys.executable, "-I", str(run / "proxy.py"), str(run), P["url"]], dict(os.environ))
    deadline = time.monotonic() + 15
    while not (run / "proxy.url").exists():
        if time.monotonic() >= deadline:
            raise SystemExit("proxy never started")
        time.sleep(0.1)
    url = (run / "proxy.url").read_text()
    private(run / "partition.toml", (run / "config.toml").read_text().replace(P["url"], url))
    return {"proxy": url}


def proxy_control(run, P):
    """Ask the proxy daemon to partition, heal or stop, and wait until it has applied it."""
    if P["action"] not in ("partition", "heal", "stop"):
        raise SystemExit("unknown proxy command")
    (run / "proxy.command").write_text(P["action"])
    if P["action"] == "stop":
        return {"proxy": "stop"}
    deadline = time.monotonic() + 10
    while (run / "proxy.state").read_text().strip() != P["action"]:
        if time.monotonic() >= deadline:
            raise SystemExit(f"proxy did not apply {P['action']}")
        time.sleep(0.1)
    return {"proxy": P["action"]}


COMMANDS = {function.__name__: function for function in (
    prepare, hub_start, hub_services, credentials, install_credentials, enroll, collector, agent, wait,
    send, snapshot, monitor_state, outbox, proxy, proxy_control)}


def dispatch(P):
    run = Path(P["run"])
    print(json.dumps(COMMANDS[P["command"]](run, P.get("params", {}))))
