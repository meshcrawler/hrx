"""Two-namespace hub reliability suite: a CI service container acts as host B.

Host B (GitLab service, separate network/PID/UTS/mount namespaces) runs disposable
PostgreSQL 17, the TLS hub on its own interface, a collector and one agent. Host A
(the job container) enrolls separately, reaches the hub only over the network path
to `hostb`, and drives ownership exclusion, collector recovery, owned restart and a
partition/reconnect through the existing PartitionProxy.

Control of host B uses a private spool directory in the shared checkout; it is never
the path under test. Every scenario uses inert fixture harnesses and no paid
inference. This is namespace simulation evidence: it is NOT physical two-host, host
reboot/suspension or Darwin evidence.
"""

import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import ssl
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from hub_partition import PartitionProxy  # noqa: E402


PORT = 18790
FAKE_CLAUDE = "#!/bin/sh\necho ready\nexec sleep 180\n"
# Enrollment updates an existing private HRX config; the hub process fixture builds
# its launch directly, so a minimal harness section is enough.
MINIMAL_CONFIG = '[claude]\nmodel = "fixture-model"\n'
EVIDENCE = "two-namespace simulation; not physical two-host, reboot/suspension or Darwin evidence"
GO_PROBE = """package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(os.Args[1])
	if err != nil {
		fmt.Println("go error:", err)
		return
	}
	response.Body.Close()
	fmt.Println("go status:", response.StatusCode)
}
"""


def control_dir(root):
    return Path(root) / ".twons"


def host_b(root, lifetime=3600):
    """Service entry: execute spooled scripts from host A until told to stop."""
    control = control_dir(root)
    deadline = time.monotonic() + lifetime
    # Services start before checkout; wait (bounded) for host A to create the spool.
    while not (control / "requests").is_dir():
        if time.monotonic() >= deadline:
            return 1
        time.sleep(1)
    done = set()
    while time.monotonic() < deadline and not (control / "stop").exists():
        (control / "heartbeat").write_text(str(time.time()))
        # As PID 1 of the service, reap any orphaned fixture processes.
        try:
            while os.waitpid(-1, os.WNOHANG)[0]:
                pass
        except ChildProcessError:
            pass
        for request in sorted((control / "requests").glob("*.py")):
            if request.name in done:
                continue
            done.add(request.name)
            try:
                result = subprocess.run([sys.executable, "-I", str(request)], capture_output=True,
                                        text=True, timeout=120, env=dict(os.environ, TWONS_ROOT=str(root)))
                outcome = {"returncode": result.returncode, "stdout": result.stdout[-20000:],
                           "stderr": result.stderr[-20000:]}
            except subprocess.TimeoutExpired as error:
                outcome = {"returncode": None, "stdout": str(error.stdout)[-20000:], "stderr": "timeout"}
            temporary = control / "responses" / (request.stem + ".tmp")
            temporary.write_text(json.dumps(outcome))
            temporary.rename(control / "responses" / (request.stem + ".json"))
        time.sleep(0.2)
    return 0


class HostB:
    """Host A's handle on host B: send Python source, wait for its result."""

    def __init__(self, root):
        self.control = control_dir(root)
        for name in ("requests", "responses", "shared"):
            (self.control / name).mkdir(parents=True, exist_ok=True)
        os.chmod(self.control, 0o700)
        self.sequence = 0

    def wait_ready(self, seconds=240):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            beat = self.control / "heartbeat"
            if beat.exists() and time.time() - float(beat.read_text() or 0) < 5:
                return
            time.sleep(1)
        raise AssertionError("host B agent never became ready")

    def run(self, source, seconds=150):
        self.sequence += 1
        name = f"{self.sequence:04d}"
        (self.control / "requests" / (name + ".tmp")).write_text(source)
        (self.control / "requests" / (name + ".tmp")).rename(self.control / "requests" / (name + ".py"))
        response = self.control / "responses" / (name + ".json")
        deadline = time.monotonic() + seconds
        while not response.exists():
            if time.monotonic() >= deadline:
                raise AssertionError(f"host B request {name} timed out")
            time.sleep(0.2)
        result = json.loads(response.read_text())
        if result["returncode"] != 0:
            raise AssertionError(f"host B request {name} failed: {result}")
        return result["stdout"]


B_PRELUDE = r'''
import json, os, secrets, signal, ssl, subprocess, time, urllib.request
from pathlib import Path
root = Path(os.environ["TWONS_ROOT"])
shared = root / ".twons" / "shared"
b = Path("/tmp/hrx-twons-b")
# The production binary: the hrxfixture build deliberately dials loopback only.
HRX = str(root / "bin" / "hrx")
HELPER = str(root / "bin" / "app.test")
def check(args, env=None):
    """Run a setup command; on failure, surface its own output instead of a bare traceback."""
    result = subprocess.run(args, env=env, capture_output=True, text=True)
    if result.returncode:
        raise SystemExit(f"{args[1:3]} exited {result.returncode}: {result.stderr[-3000:]} {result.stdout[-1000:]}")
    return result
def spawn(args, name, env):
    log = open(b / (name + ".log"), "w")
    child = subprocess.Popen(args, env=env, stdout=log, stderr=log, start_new_session=True)
    (b / (name + ".pid")).write_text(str(child.pid))
    return child
def host_env():
    bin_dir = b / "bin"
    return dict(PATH=str(bin_dir) + ":/usr/local/bin:/usr/bin:/bin", HOME=str(b / "home"),
                HRX_CONFIG=str(b / "config.toml"), XDG_STATE_HOME=str(b / "state"),
                HRX_INTEGRATION_CA=str(shared / "cert.pem"), SSL_CERT_FILE=str(shared / "cert.pem"),
                HRX_HUB_PROCESS_FIXTURE="1", HRX_MONITOR="on")
'''

B_SETUP = B_PRELUDE + r'''
b.mkdir(mode=0o700, exist_ok=True)
(b / "home").mkdir(mode=0o700, exist_ok=True)
(b / "bin").mkdir(mode=0o700, exist_ok=True)
(b / "bin" / "claude").write_text(FAKE)
(b / "bin" / "claude").chmod(0o700)
pg = b / "pg"
check(["initdb", "-D", str(pg), "-U", "fixture", "--auth=trust", "-E", "UTF8"])
check(["pg_ctl", "-D", str(pg), "-l", str(b / "pg.log"), "-o", f"-k {b} -c listen_addresses=''", "-w", "start"])
for name in ("admin", "viewer"):
    token = shared / (name + ".token")
    token.write_text(secrets.token_hex(32))
    token.chmod(0o600)
database = b / "database"
database.write_text(f"host={b} user=fixture dbname=postgres sslmode=disable")
database.chmod(0o600)
check(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=hostb",
       "-addext", "subjectAltName=DNS:hostb,IP:127.0.0.1", "-keyout", str(b / "key.pem"),
       "-out", str(shared / "cert.pem")])
spawn([HRX, "hub", "serve", "--database-url-file", str(database), "--admin-token-file", str(shared / "admin.token"),
       "--viewer-token-file", str(shared / "viewer.token"), "--listen", "0.0.0.0:PORT",
       "--tls-cert", str(shared / "cert.pem"), "--tls-key", str(b / "key.pem")], "hub",
      dict(os.environ, SSL_CERT_FILE=str(shared / "cert.pem")))
context = ssl.create_default_context(cafile=str(shared / "cert.pem"))
deadline = time.monotonic() + 30
while True:
    try:
        urllib.request.urlopen("https://127.0.0.1:PORT/healthz", context=context, timeout=1)
        break
    except OSError:
        if time.monotonic() >= deadline:
            raise SystemExit("hub never became healthy: " + (b / "hub.log").read_text()[-2000:])
        time.sleep(0.2)
env = host_env()
(b / "config.toml").write_text(MINIMAL_HRX_CONFIG)
(b / "config.toml").chmod(0o600)
check([HRX, "hub", "enroll", "--url", "https://127.0.0.1:PORT", "--admin-token-file",
       str(shared / "admin.token"), "--prefix", "fixture/"], env)
spawn([HRX, "hub", "collect"], "collector", env)
outbox = b / "state" / "hrx" / "hub" / "outbox.json"
deadline = time.monotonic() + 15
while not outbox.exists():
    if time.monotonic() >= deadline:
        raise SystemExit("host B collector never initialized")
    time.sleep(0.1)
spawn([HELPER, "-test.run=^TestHubProcessFixture$"], "remote-agent",
      dict(env, HRX_HUB_FIXTURE_IDENTITY="fixture/remote/agent"))
print(json.dumps({"hostname": os.uname().nodename}))
'''.replace("FAKE", repr(FAKE_CLAUDE)).replace("MINIMAL_HRX_CONFIG", repr(MINIMAL_CONFIG)).replace("PORT", str(PORT))

B_CLEANUP = B_PRELUDE + r'''
stopped = {}
for pid_file in sorted(b.glob("*.pid")):
    pid = int(pid_file.read_text())
    try:
        os.killpg(pid, signal.SIGTERM)
        stopped[pid_file.stem] = "terminated"
    except ProcessLookupError:
        stopped[pid_file.stem] = "already exited"
time.sleep(2)
for pid_file in sorted(b.glob("*.pid")):
    try:
        os.killpg(int(pid_file.read_text()), signal.SIGKILL)
    except ProcessLookupError:
        pass
pg = subprocess.run(["pg_ctl", "-D", str(b / "pg"), "-m", "fast", "-w", "stop"], capture_output=True, text=True)
status = subprocess.run(["pg_ctl", "-D", str(b / "pg"), "status"], capture_output=True, text=True)
# Exited daemons are reparented to the service's PID 1, which reaps them on its next
# loop; give it that chance, then report live processes and zombies separately.
time.sleep(1)
rows = [line.split(None, 2) for line in subprocess.run(["ps", "-eo", "pid=,stat=,comm="], capture_output=True,
                                                       text=True).stdout.splitlines()]
fixture = [row for row in rows if len(row) == 3
           and any(word in row[2] for word in ("hrx", "app.test", "postgres", "sleep"))]
print(json.dumps({"stopped": stopped, "postgres_stopped": status.returncode == 3,
                  "remaining": [" ".join(row) for row in fixture if not row[1].startswith("Z")],
                  "zombies": [" ".join(row) for row in fixture if row[1].startswith("Z")]}))
'''


class Suite:
    def __init__(self, root, out):
        self.root, self.out = Path(root).resolve(), Path(out)
        self.out.mkdir(parents=True, exist_ok=True)
        self.a = Path("/tmp/hrx-twons-a")
        self.children, self.results = [], []
        self.b = HostB(self.root)
        self.shared = self.b.control / "shared"
        self.hrx = str(self.root / "bin" / "hrx")
        self.helper = str(self.root / "bin" / "app.test")
        self.hub_url = f"https://hostb:{PORT}"

    def record(self, name, status, detail=""):
        self.results.append({"scenario": name, "status": status, "detail": detail})
        print(f"{status}: {name} {detail}".rstrip(), flush=True)

    def env(self, config=None):
        bin_dir = self.a / "bin"
        return dict(PATH=str(bin_dir) + ":/usr/local/bin:/usr/bin:/bin", HOME=str(self.a / "home"),
                    HRX_CONFIG=str(config or self.a / "config.toml"), XDG_STATE_HOME=str(self.a / "state"),
                    HRX_INTEGRATION_CA=str(self.shared / "cert.pem"), SSL_CERT_FILE=str(self.shared / "cert.pem"),
                    HRX_HUB_PROCESS_FIXTURE="1", HRX_MONITOR="on")

    def spawn(self, args, name, env, **overrides):
        log = open(self.a / (name + ".log"), "w")
        child = subprocess.Popen(args, env=dict(env, **overrides), stdout=log, stderr=log, start_new_session=True)
        log.close()
        self.children.append(child)
        return child

    def agent(self, name, identity, env=None):
        return self.spawn([self.helper, "-test.run=^TestHubProcessFixture$"], name, env or self.env(),
                          HRX_HUB_FIXTURE_IDENTITY=identity)

    def snapshot(self):
        request = urllib.request.Request(self.hub_url + "/api/v1/agents", headers={
            "Authorization": "Bearer " + (self.shared / "viewer.token").read_text().strip()})
        context = ssl.create_default_context(cafile=str(self.shared / "cert.pem"))
        with urllib.request.urlopen(request, context=context, timeout=5) as response:
            return json.load(response)["agents"]

    def wait_for(self, predicate, seconds=30, label="hub condition"):
        deadline = time.monotonic() + seconds
        rows = []
        while time.monotonic() < deadline:
            rows = self.snapshot()
            if predicate(rows):
                return rows
            time.sleep(1)
        summary = [{k: r.get(k) for k in ("agent", "run_id", "status")} for r in rows]
        raise AssertionError(f"timed out waiting for {label}: {summary}")

    def setup(self):
        if self.a.exists():
            shutil.rmtree(self.a)
        for directory in (self.a, self.a / "home", self.a / "bin"):
            directory.mkdir(mode=0o700)
        (self.a / "bin" / "claude").write_text(FAKE_CLAUDE)
        (self.a / "bin" / "claude").chmod(0o700)
        self.b.wait_ready()
        info = json.loads(self.b.run(B_SETUP).strip().splitlines()[-1])
        (self.a / "config.toml").write_text(MINIMAL_CONFIG)
        (self.a / "config.toml").chmod(0o600)
        self.probe_hub()
        enroll = subprocess.run([self.hrx, "hub", "enroll", "--url", self.hub_url, "--admin-token-file",
                                 str(self.shared / "admin.token"), "--prefix", "fixture/"], env=self.env(),
                                capture_output=True, text=True, timeout=60)
        assert enroll.returncode == 0, f"host A enroll exited {enroll.returncode}: {enroll.stderr[-3000:]}"
        self.collector = self.spawn([self.hrx, "hub", "collect"], "collector", self.env())
        outbox = self.a / "state" / "hrx" / "hub" / "outbox.json"
        deadline = time.monotonic() + 15
        while not outbox.exists():
            assert time.monotonic() < deadline, "host A collector never initialized"
            time.sleep(0.1)
        self.alpha = self.agent("alpha", "fixture/alpha/agent")
        self.beta = self.agent("beta", "fixture/beta/agent")
        rows = self.wait_for(lambda rows: len([r for r in rows if r["status"] == "running"]) == 3)
        live = [r for r in rows if r["status"] == "running"]
        assert len({r["host_id"] for r in live}) == 2, live
        self.live = {r["agent"]: r for r in live}
        self.record("three runs on two namespaces", "PASS",
                    f"host B {info['hostname']}; distinct hub host IDs")

    def probe_hub(self):
        """Prove the network path A -> hostb hub works before HRX uses it, with the exact error if not."""
        import socket
        evidence = {}
        try:
            evidence["resolved"] = sorted({info[4][0] for info in socket.getaddrinfo("hostb", PORT)})
            with socket.create_connection(("hostb", PORT), timeout=5):
                evidence["tcp"] = "connected"
            context = ssl.create_default_context(cafile=str(self.shared / "cert.pem"))
            with urllib.request.urlopen(self.hub_url + "/healthz", context=context, timeout=5) as response:
                evidence["healthz"] = response.status
        except Exception as error:  # Record which layer failed, then fail the suite.
            evidence["error"] = repr(error)
            raise AssertionError(f"host A cannot reach the host B hub: {evidence}") from None
        # HRX's hub client is Go and reports every transport error as "hub connection
        # unavailable"; make the same request from Go with the same trust settings so the
        # exact Go error is recorded if it differs from Python's view of the path.
        probe = self.a / "goprobe"
        probe.mkdir(exist_ok=True)
        (probe / "main.go").write_text(GO_PROBE)
        go = subprocess.run(["go", "run", str(probe / "main.go"), self.hub_url + "/healthz"],
                            env=dict(os.environ, **{k: v for k, v in self.env().items() if k not in ("PATH", "HOME")}),
                            capture_output=True, text=True, timeout=120)
        evidence["go_client"] = (go.stdout + go.stderr).strip()[-1500:]
        self.record("host A reaches host B hub over the network", "PASS", json.dumps(evidence))

    def ownership_exclusion(self):
        rival = self.agent("rival", "fixture/remote/agent")
        assert rival.wait(timeout=15) != 0
        assert "cannot acquire hub identity ownership" in (self.a / "rival.log").read_text()
        self.record("owner lease exclusion across namespaces", "PASS",
                    "host A refused an identity owned by host B")

    def collector_recovery(self):
        alpha_run = self.live["fixture/alpha/agent"]["run_id"]
        self.collector.kill()
        self.collector.wait(timeout=10)
        checkpoint = json.loads((self.a / "state/hrx/hub/outbox.json").read_text())
        self.alpha.send_signal(signal.SIGTERM)
        self.alpha.wait(timeout=10)
        local = json.loads((self.a / "state/hrx/monitor" / (alpha_run + ".json")).read_text())
        assert local["status"] == "exited"
        assert next(r for r in self.snapshot() if r["run_id"] == alpha_run)["status"] != "exited"
        self.collector = self.spawn([self.hrx, "hub", "collect"], "collector-restarted", self.env())
        rows = self.wait_for(lambda rows: any(r["run_id"] == alpha_run and r["status"] == "exited" for r in rows))
        published = next(r for r in rows if r["run_id"] == alpha_run)
        assert published["ended_at"] == local["ended_at"] and published["last_activity"] == local["last_activity"]
        current = json.loads((self.a / "state/hrx/hub/outbox.json").read_text())
        assert current["next_sequence"] > checkpoint["next_sequence"]
        self.record("collector recovery from checkpoint", "PASS",
                    "killed collector restarted and forwarded the original exit timestamps")
        self.replacement = self.agent("replacement", "fixture/alpha/agent")
        self.wait_for(lambda rows: any(r["agent"] == "fixture/alpha/agent" and r["run_id"] != alpha_run
                                       and r["status"] == "running" for r in rows))
        self.record("owned restart", "PASS", "identity reacquired with a new run after exit")

    def partition(self):
        proxy = PartitionProxy(self.hub_url, self.shared / "cert.pem")
        # The proxy must never outlive the scenario: heal and close it on every path.
        try:
            config = self.a / "partition.toml"
            original = (self.a / "config.toml").read_text()
            config.write_text(original.replace(self.hub_url, proxy.url))
            config.chmod(0o600)
            env = self.env(config)
            rows = self.snapshot()
            before = {r["run_id"] for r in rows if r["status"] == "running" and r["agent"] != "fixture/remote/agent"}
            # Re-home host A onto the proxied path before cutting it. Stop the agents first and
            # let the still-running collector forward their exits; only then swap collectors.
            for child in (self.beta, self.replacement):
                child.send_signal(signal.SIGTERM)
                child.wait(timeout=10)
            self.wait_for(lambda rows: all(r["status"] == "exited" for r in rows if r["run_id"] in before),
                          label="host A exits forwarded before re-homing")
            self.collector.send_signal(signal.SIGTERM)
            self.collector.wait(timeout=10)
            self.collector = self.spawn([self.hrx, "hub", "collect"], "collector-proxied", env)
            first = self.agent("proxied-alpha", "fixture/alpha/agent", env)
            second = self.agent("proxied-beta", "fixture/beta/agent", env)
            rows = self.wait_for(lambda rows: len([r for r in rows if r["status"] == "running"
                                                   and r["agent"] in {"fixture/alpha/agent", "fixture/beta/agent"}]) == 2,
                                 label="proxied host A agents running")
            partitioned = {r["run_id"] for r in rows if r["status"] == "running"
                           and r["agent"] in {"fixture/alpha/agent", "fixture/beta/agent"}}
            remote_run = self.live["fixture/remote/agent"]["run_id"]
            proxy.partition()
            refused = self.agent("partition-refused", "fixture/partition-new/agent", env)
            assert refused.wait(timeout=15) != 0
            assert "cannot acquire hub identity ownership" in (self.a / "partition-refused.log").read_text()
            started = time.monotonic()
            first.wait(timeout=60)
            second.wait(timeout=max(1, 60 - (time.monotonic() - started)))
            for name in ("proxied-alpha", "proxied-beta"):
                assert "ownership lease expired" in (self.a / (name + ".log")).read_text()
            rows = self.wait_for(lambda rows: {r["run_id"] for r in rows if r["run_id"] in partitioned
                                               and r["status"] == "stale"} == partitioned, 30,
                                 label="partitioned host A runs stale")
            remote = next(r for r in rows if r["run_id"] == remote_run)
            assert remote["status"] == "running" and remote["ownership"]["state"] == "held"
            proxy.heal()
            self.wait_for(lambda rows: {r["run_id"] for r in rows if r["run_id"] in partitioned
                                        and r["status"] == "exited"} == partitioned, 45,
                          label="final exits forwarded after heal")
            self.record("partition and reconnect", "PASS",
                        "partitioned host A refused launches and stopped on expiry; host B kept ownership; "
                        "final exits forwarded after heal")
        except BaseException:
            # Restore the network even while failing, without losing either error.
            for step in (proxy.heal, proxy.close):
                try:
                    step()
                except Exception as error:
                    self.record("partition proxy " + step.__name__, "FAIL", repr(error)[-1000:])
            raise
        else:
            proxy.heal()
            proxy.close()

    def stop_local(self):
        """Stop every host A fixture process; one failure never skips the rest.

        Returns (processes still alive, per-child errors)."""
        errors = []

        def attempt(child, action):
            try:
                action(child)
            except ProcessLookupError:
                pass
            except Exception as error:
                errors.append(f"{child.args[0] if child.args else child.pid}: {error!r}"[-500:])

        for child in self.children:
            if child.poll() is None:
                attempt(child, lambda c: os.killpg(c.pid, signal.SIGTERM))
        time.sleep(2)
        for child in self.children:
            if child.poll() is None:
                attempt(child, lambda c: os.killpg(c.pid, signal.SIGKILL))
            attempt(child, lambda c: c.wait(timeout=10))
        listing = subprocess.run(["ps", "-eo", "pid=,comm="], capture_output=True, text=True).stdout
        leftovers = [line for line in listing.splitlines()
                     if line.split() and any(word in line.split()[-1] for word in ("hrx", "app.test"))]
        return leftovers, errors

    def cleanup(self):
        """Attempt both hosts independently; a failure on one never skips the other."""
        errors, leftovers, remote = {}, None, None
        try:
            leftovers, local_errors = self.stop_local()
            if local_errors:
                errors["host_a"] = local_errors
        except Exception as error:
            errors["host_a"] = repr(error)[-1000:]
        try:
            remote = json.loads(self.b.run(B_CLEANUP).strip().splitlines()[-1])
        except Exception as error:
            errors["host_b"] = repr(error)[-1000:]
        finally:
            try:
                (self.b.control / "stop").write_text("done")
            except OSError as error:
                errors["host_b_stop"] = repr(error)
        clean = (not errors and leftovers == [] and isinstance(remote, dict)
                 and remote.get("postgres_stopped") is True and not remote.get("remaining"))
        self.record("cleanup", "PASS" if clean else "FAIL",
                    json.dumps({"host_a_leftovers": leftovers, "host_b": remote, "errors": errors}))
        return clean

    def write_report(self):
        """Always persist the evidence, including every recorded failure."""
        try:
            for log in sorted(self.a.glob("*.log")):
                shutil.copy(log, self.out / ("host-a-" + log.name))
        except OSError as error:
            self.results.append({"scenario": "log collection", "status": "FAIL", "detail": repr(error)})
        (self.out / "suite.json").write_text(json.dumps({"evidence": EVIDENCE, "results": self.results}, indent=2) + "\n")

    def run(self):
        ok, clean = False, False
        try:
            self.setup()
            for scenario in (self.ownership_exclusion, self.collector_recovery, self.partition):
                scenario()
            ok = True
        except Exception as error:
            self.record("suite", "FAIL", repr(error)[-2000:])
        finally:
            try:
                clean = self.cleanup()
            except Exception as error:  # Keep the original suite error alongside this one.
                self.record("cleanup", "FAIL", repr(error)[-2000:])
            finally:
                self.write_report()
        failed = any(result["status"] != "PASS" for result in self.results)
        return 0 if ok and clean and not failed else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("role", choices=["host-b", "suite"])
    parser.add_argument("--root", type=Path, default=Path(os.environ.get("CI_PROJECT_DIR", ".")))
    parser.add_argument("--out", type=Path, default=Path("artifacts/two-namespace"))
    args = parser.parse_args()
    if args.role == "host-b":
        return host_b(args.root)
    return Suite(args.root, args.out).run()


if __name__ == "__main__":
    sys.exit(main())
