"""T2 guest-lifecycle controller for two mayor-provisioned disposable LXC guests.

See docs/internal/t2-guest-lifecycle.md. The controller never creates, modifies or destroys
guests and never touches the host. It acts only as the unprivileged `hrx-fixture`
user over SSH with pinned host keys, and re-validates each guest's run marker and the
input expiry before every destructive operation. A guest restart counts only when the
guest's PID 1 start time changed AND the /run/hrx-fixture nonce is gone; boot_id is
recorded but never sufficient. Guest restart is not physical-host outage evidence.
"""

import argparse
import datetime
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time


USER = "hrx-fixture"
MAX_LIFETIME = datetime.timedelta(hours=24)
RUN_ID = re.compile(r"t2-[A-Za-z0-9.-]{1,60}")
RESOURCE_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
EVIDENCE = ("T2 disposable LXC guest restart on a running host kernel; not physical-host outage, "
            "boot autostart, Darwin or live-model evidence")
REMOTE_RUN = "/home/hrx-fixture/run"


class Refused(RuntimeError):
    """A safety check failed; the operation was not attempted."""


def load_input(text, now):
    """Validate the mayor-supplied run input; reject anything stale or ambiguous."""
    try:
        spec = json.loads(text)
    except ValueError:
        raise Refused("run input is not JSON") from None
    if not isinstance(spec, dict) or set(spec) != {"run_id", "guests", "hub_port", "expires_at"}:
        raise Refused("run input must have exactly run_id, guests, hub_port, expires_at")
    if not isinstance(spec["run_id"], str) or not RUN_ID.fullmatch(spec["run_id"]):
        raise Refused("invalid run_id")
    guests = spec["guests"]
    if not isinstance(guests, dict) or set(guests) != {"hub", "agent"}:
        raise Refused("guests must be exactly hub and agent")
    addresses = set()
    for name, guest in guests.items():
        if not isinstance(guest, dict) or set(guest) != {"address", "resource_id"}:
            raise Refused(f"{name}: guest needs exactly address and resource_id")
        try:
            addresses.add(str(ipaddress.ip_address(guest["address"])))
        except (ValueError, TypeError):
            raise Refused(f"{name}: address must be an IP address") from None
        if not isinstance(guest["resource_id"], str) or not RESOURCE_ID.fullmatch(guest["resource_id"]):
            raise Refused(f"{name}: invalid resource_id")
    if len(addresses) != 2:
        raise Refused("hub and agent must be different guests")
    if type(spec["hub_port"]) is not int or not 1024 <= spec["hub_port"] <= 65535:
        raise Refused("hub_port must be an unprivileged TCP port")
    expiry = parse_expiry(spec["expires_at"])
    if not now < expiry <= now + MAX_LIFETIME:
        raise Refused("expires_at must be in the future and at most 24 hours ahead")
    return spec


def parse_expiry(value):
    if not isinstance(value, str) or not value.endswith("Z"):
        raise Refused("expires_at must be an ISO-8601 UTC time ending in Z")
    try:
        return datetime.datetime.fromisoformat(value[:-1]).replace(tzinfo=datetime.timezone.utc)
    except ValueError:
        raise Refused("invalid expires_at") from None


class Ssh:
    """Run Python on a guest as the fixture user; the key is referenced by path only."""

    def __init__(self, key_path, known_hosts):
        self.key_path, self.known_hosts = str(key_path), str(known_hosts)

    def options(self):
        return ["-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o",
                f"UserKnownHostsFile={self.known_hosts}", "-o", "IdentitiesOnly=yes", "-o",
                "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3",
                "-i", self.key_path]

    def python(self, address, source, timeout=120):
        result = subprocess.run(["ssh", *self.options(), f"{USER}@{address}", "python3", "-I", "-"],
                                input=source, capture_output=True, text=True, timeout=timeout)
        return result.returncode, result.stdout, result.stderr

    def copy(self, address, local, remote):
        result = subprocess.run(["scp", "-q", *self.options(), str(local), f"{USER}@{address}:{remote}"],
                                capture_output=True, text=True, timeout=300)
        if result.returncode:
            raise RuntimeError(f"copy to {address} failed: {result.stderr[-1000:]}")


def remote(script, **params):
    """Prefix a guest script with its parameters as JSON; never splice values into code."""
    return f"import json\nP = json.loads({json.dumps(json.dumps(params))})\n" + script


EVIDENCE_SCRIPT = r'''
from pathlib import Path
stat = Path("/proc/1/stat").read_text()
marker = Path("/run/hrx-fixture/restart-marker")
print(json.dumps({
    "pid1_start": int(stat.rsplit(")", 1)[1].split()[19]),
    "marker": marker.read_text() if marker.exists() else None,
    "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
    "run_marker": Path("/etc/hrx-fixture-run").read_text().strip(),
    "machine_id": Path("/etc/machine-id").read_text().strip(),
    "hostname": __import__("socket").gethostname(),
}))
'''

WRITE_MARKER = r'''
from pathlib import Path
Path("/run/hrx-fixture/restart-marker").write_text(P["nonce"])
'''

REBOOT = r'''
import subprocess
subprocess.Popen(["sudo", "-n", "/usr/bin/systemctl", "reboot"], start_new_session=True)
'''


def restarted(before, after):
    """A restart is proven only by a new PID 1 AND the tmpfs nonce disappearing."""
    return (bool(before.get("marker")) and after.get("marker") is None
            and after.get("pid1_start") != before.get("pid1_start"))


class Controller:
    def __init__(self, spec, ssh, out, clock=time.time, sleep=time.sleep):
        self.spec, self.ssh, self.out = spec, ssh, Path(out)
        self.clock, self.sleep = clock, sleep
        self.expiry = parse_expiry(spec["expires_at"])
        self.results, self.restarts = [], {}
        self.out.mkdir(parents=True, exist_ok=True)

    def address(self, guest):
        return self.spec["guests"][guest]["address"]

    def record(self, name, status, detail=""):
        self.results.append({"check": name, "status": status, "detail": detail})
        print(f"{status}: {name} {detail}".rstrip(), flush=True)

    def run_on(self, guest, script, timeout=120, **params):
        code, stdout, stderr = self.ssh.python(self.address(guest), remote(script, **params), timeout)
        if code:
            raise RuntimeError(f"{guest}: remote step exited {code}: {stderr[-2000:]}")
        return stdout

    def evidence(self, guest):
        return json.loads(self.run_on(guest, EVIDENCE_SCRIPT).strip().splitlines()[-1])

    def revalidate(self, guest):
        """Before any destructive step: input still valid and the guest still ours."""
        now = datetime.datetime.fromtimestamp(self.clock(), datetime.timezone.utc)
        if now >= self.expiry:
            raise Refused("run input expired; refusing guest operation")
        seen = self.evidence(guest)["run_marker"]
        if seen != self.spec["run_id"]:
            raise Refused(f"{guest}: run marker does not match this run; refusing guest operation")

    def preflight(self):
        found = {guest: self.evidence(guest) for guest in ("hub", "agent")}
        for guest, item in found.items():
            if item["run_marker"] != self.spec["run_id"]:
                raise Refused(f"{guest}: run marker does not match this run")
        if found["hub"]["machine_id"] == found["agent"]["machine_id"]:
            raise Refused("hub and agent report the same machine ID")
        self.record("preflight", "PASS", json.dumps({g: {k: v for k, v in e.items() if k != "marker"}
                                                     for g, e in found.items()}))

    def reboot(self, guest, down_timeout=120, up_timeout=300):
        """Restart a guest from inside it, then prove the restart from guest-owned evidence."""
        self.revalidate(guest)
        nonce = secrets.token_hex(16)
        self.run_on(guest, WRITE_MARKER, nonce=nonce)
        before = self.evidence(guest)
        if before["marker"] != nonce:
            raise RuntimeError(f"{guest}: restart marker was not recorded")
        self.revalidate(guest)
        self.ssh.python(self.address(guest), remote(REBOOT), 30)
        self.wait_reachability(guest, False, down_timeout)
        self.wait_reachability(guest, True, up_timeout)
        after = self.evidence(guest)
        self.revalidate(guest)
        if not restarted(before, after):
            raise RuntimeError(f"{guest}: restart not proven (PID 1 and /run marker evidence)")
        self.restarts[guest] = {"before": {k: v for k, v in before.items() if k != "marker"},
                                "after": {k: v for k, v in after.items() if k != "marker"},
                                "boot_id_changed": before["boot_id"] != after["boot_id"]}
        return before, after

    def wait_reachability(self, guest, reachable, timeout):
        deadline = self.clock() + timeout
        while self.clock() < deadline:
            try:
                code, _, _ = self.ssh.python(self.address(guest), "print('ok')", 20)
            except subprocess.TimeoutExpired:
                # A timed-out observation proves neither shutdown nor readiness.
                self.sleep(2)
                continue
            if (code == 0) == reachable:
                return
            self.sleep(2)
        raise RuntimeError(f"{guest}: did not become {'reachable' if reachable else 'unreachable'}")

    def teardown(self):
        """Stop only this run's own processes on each guest, independently."""
        errors, remaining = {}, {}
        for guest in ("agent", "hub"):
            try:
                self.revalidate(guest)
                remaining[guest] = json.loads(self.run_on(guest, TEARDOWN, run=REMOTE_RUN).strip().splitlines()[-1])
            except Exception as error:
                errors[guest] = repr(error)[-1000:]
        clean = not errors and len(remaining) == 2 and all(
            item.get("remaining") == [] and item.get("run_dir_removed") is True
            and item.get("postgres_stopped") is True and item.get("errors") == []
            for item in remaining.values())
        self.record("teardown", "PASS" if clean else "FAIL",
                    json.dumps({"guests": remaining, "errors": errors,
                                "resource_ids": {g: v["resource_id"] for g, v in self.spec["guests"].items()}}))
        return clean

    def write_report(self):
        (self.out / "evidence.json").write_text(json.dumps({
            "evidence": EVIDENCE, "run_id": self.spec["run_id"],
            "resource_ids": {g: v["resource_id"] for g, v in self.spec["guests"].items()},
            "restarts": self.restarts, "results": self.results}, indent=2) + "\n")

    def run(self, scenarios):
        ok, clean = False, False
        try:
            self.preflight()
            for scenario in scenarios:
                scenario(self)
            ok = True
        except Exception as error:
            self.record("suite", "FAIL", repr(error)[-2000:])
        finally:
            try:
                clean = self.teardown()
            except Exception as error:
                self.record("teardown", "FAIL", repr(error)[-2000:])
            finally:
                self.write_report()
        failed = any(result["status"] != "PASS" for result in self.results)
        return 0 if ok and clean and not failed else 1


TEARDOWN = r'''
import os, shutil, signal, subprocess, sys, time
from pathlib import Path
run = Path(P["run"])
sys.path.insert(0, str(run))
errors = []
for pid_file in sorted(run.glob("*.pid")) if run.exists() else []:
    try:
        from t2_processes import signal_owned
        signal_owned(pid_file, signal.SIGTERM)
    except ProcessLookupError:
        pass
    except Exception as error:
        errors.append(f"TERM {pid_file.name}: {error!r}")
time.sleep(2)
for pid_file in sorted(run.glob("*.pid")) if run.exists() else []:
    try:
        from t2_processes import signal_owned
        signal_owned(pid_file, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except Exception as error:
        errors.append(f"KILL {pid_file.name}: {error!r}")
postgres_stopped = not (run / "pg").exists()
if (run / "pg").exists():
    try:
        pg = ["/usr/lib/postgresql/17/bin/pg_ctl", "-D", str(run / "pg")]
        status = subprocess.run(pg + ["status"], capture_output=True, timeout=30)
        if status.returncode == 0:
            subprocess.run(pg + ["-m", "fast", "-w", "stop"], capture_output=True,
                           timeout=30, check=True)
            status = subprocess.run(pg + ["status"], capture_output=True, timeout=30)
        postgres_stopped = status.returncode == 3
        if not postgres_stopped:
            errors.append(f"postgres stop unverified: status exit {status.returncode}")
    except Exception as error:
        errors.append(f"postgres cleanup: {error!r}")
remaining = None
try:
    rows = subprocess.run(["ps", "-u", "hrx-fixture", "-o", "pid=,stat=,args="],
                          capture_output=True, text=True, timeout=30, check=True).stdout
    remaining = [line.strip() for line in rows.splitlines()
                 if line.split() and not line.split()[1].startswith("Z")
                 and (str(run) in line or Path(line.split()[2]).name
                      in {"hrx", "app.test", "postgres", "sleep"})]
except Exception as error:
    errors.append(f"process inspection: {error!r}")
# Preserve state when process shutdown cannot be verified.
if postgres_stopped and remaining == []:
    try:
        if run.exists():
            shutil.rmtree(run)
    except Exception as error:
        errors.append(f"run directory removal: {error!r}")
print(json.dumps({"remaining": remaining, "run_dir_removed": not run.exists(),
                  "postgres_stopped": postgres_stopped, "errors": errors}))
'''


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", type=Path, default=Path("artifacts/t2"))
    args = parser.parse_args()
    for variable in ("HRX_T2_INPUT", "HRX_T2_SSH_KEY", "HRX_T2_KNOWN_HOSTS"):
        if not os.environ.get(variable):
            print(f"FAIL: {variable} is not provided; the mayor supplies run-scoped inputs", flush=True)
            return 1
    now = datetime.datetime.now(datetime.timezone.utc)
    spec = load_input(Path(os.environ["HRX_T2_INPUT"]).read_text(), now)
    ssh = Ssh(os.environ["HRX_T2_SSH_KEY"], os.environ["HRX_T2_KNOWN_HOSTS"])
    from t2_scenarios import SCENARIOS
    return Controller(spec, ssh, args.out).run(SCENARIOS)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Refused as error:
        print(f"REFUSED: {error}", flush=True)
        sys.exit(1)
