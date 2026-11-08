"""Explicit two-computer smoke against a provisioned disposable hub.

Requires private configs/certificates and helper binaries on both computers.
See docs/hub.md. No production state or paid inference is used.
"""

import argparse
from pathlib import Path
import os, subprocess, time, json, ssl, urllib.request, urllib.error, signal, shutil
import tomllib
from hub_partition import PartitionProxy

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--local-dir", type=Path, required=True)
parser.add_argument("--ssh-host", required=True)
parser.add_argument("--container", type=int, required=True)
parser.add_argument("--hrx", required=True, help="local hrxfixture binary")
parser.add_argument("--helper", required=True, help="local internal/app test binary")
parser.add_argument("--remote-dir", default="/opt/hrx-fixture")
parser.add_argument(
    "--exercise-outage",
    action="store_true",
    help="stop/restart the disposable hrx-fixture-hub systemd unit",
)
parser.add_argument(
    "--exercise-database-outage",
    action="store_true",
    help="stop/restart PostgreSQL 17 in the disposable container",
)
parser.add_argument("--hub-url", default="https://127.0.0.1:18790")
parser.add_argument("--exercise-collector-restart", action="store_true")
parser.add_argument("--exercise-process-failures", action="store_true")
parser.add_argument("--exercise-host-suspension", action="store_true")
parser.add_argument("--exercise-host-reboot", action="store_true")
parser.add_argument("--exercise-network-partition", action="store_true")
args = parser.parse_args()
if args.exercise_network_partition and any(
    (
        args.exercise_outage,
        args.exercise_database_outage,
        args.exercise_host_suspension,
        args.exercise_host_reboot,
        args.exercise_process_failures,
    )
):
    parser.error("run network partition separately from other failure scenarios")
if args.exercise_process_failures and (
    args.exercise_outage or args.exercise_database_outage
):
    parser.error("run process failures separately from service outages")
if args.exercise_host_reboot and any(
    (
        args.exercise_outage,
        args.exercise_database_outage,
        args.exercise_host_suspension,
        args.exercise_process_failures,
    )
):
    parser.error("run host reboot separately from other failure scenarios")
p = args.local_dir.resolve()
env = dict(
    os.environ,
    HRX_CONFIG=str(p / "config.toml"),
    XDG_STATE_HOME=str(p / "state"),
    HRX_INTEGRATION_CA=str(p / "cert.pem"),
    HRX_HUB_PROCESS_FIXTURE="1",
    HRX_MONITOR="on",
)
bin = p / "bin"
bin.mkdir(exist_ok=True)
(bin / "claude").write_text("#!/bin/sh\necho ready\nexec sleep 180\n")
(bin / "claude").chmod(0o700)
env["PATH"] = str(bin) + ":" + env["PATH"]
children = []
proxy = None
if args.exercise_network_partition:
    proxy = PartitionProxy(args.hub_url, p / "cert.pem")
    original = (p / "config.toml").read_text()
    original_url = tomllib.loads(original)["hub"]["url"]
    partition_config = p / "partition.toml"
    partition_config.write_text(original.replace(original_url, proxy.url))
    partition_config.chmod(0o600)
    env["HRX_CONFIG"] = str(partition_config)


def spawn(args, name, overrides=None):
    log = open(p / (name + ".log"), "w")
    child = subprocess.Popen(
        args,
        env=dict(env, **(overrides or {})),
        stdout=log,
        stderr=log,
        start_new_session=True,
    )
    children.append(child)
    log.close()
    return child


remote = """from pathlib import Path
import subprocess,os
p=Path('/opt/hrx-fixture'); b=p/'bin';b.mkdir(exist_ok=True)
(b/'claude').write_text('#!/bin/sh\\necho ready\\nexec sleep 180\\n');(b/'claude').chmod(0o700)
e=dict(os.environ,HRX_CONFIG=str(p/'config.toml'),XDG_STATE_HOME=str(p/'state'),SSL_CERT_FILE=str(p/'cert.pem'),HRX_HUB_PROCESS_FIXTURE='1',HRX_MONITOR='on',PATH=str(b)+':'+os.environ['PATH'])
for name,args in [('collector',[str(p/'hrx'),'hub','collect']),('agent',[str(p/'hrx.test'),'-test.run=^TestHubProcessFixture$'])]:
 if name=='agent':
  import time;time.sleep(2);e['HRX_HUB_FIXTURE_IDENTITY']='fixture/remote/agent'
 with open(p/(name+'.log'),'w') as log:
  c=subprocess.Popen(args,env=e,stdout=log,stderr=log,start_new_session=True)
 (p/(name+'.pid')).write_text(str(c.pid))
 (p/(name+'.boot')).write_text(Path('/proc/sys/kernel/random/boot_id').read_text())
"""


def remote_python(source, capture=False):
    return subprocess.run(
        [
            "ssh",
            "-o",
            "BatchMode=yes",
            args.ssh_host,
            f"pct exec {args.container} -- python3 -",
        ],
        input=source.replace(
            "Path('/opt/hrx-fixture')", "Path(" + repr(args.remote_dir) + ")"
        ),
        text=True,
        check=True,
        capture_output=capture,
        timeout=30,
    )


def snapshot():
    req = urllib.request.Request(
        args.hub_url.rstrip("/") + "/api/v1/agents",
        headers={"Authorization": "Bearer " + (p / "viewer.token").read_text().strip()},
    )
    with urllib.request.urlopen(
        req, context=ssl.create_default_context(cafile=str(p / "cert.pem")), timeout=5
    ) as r:
        result = json.load(r)
    identities = {
        "fixture/alpha/agent",
        "fixture/beta/agent",
        "fixture/remote/agent",
        "fixture/failed/agent",
    }
    result["agents"] = [row for row in result["agents"] if row["agent"] in identities]
    return result


def wait_for(predicate, seconds=20):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        s = snapshot()
        if predicate(s):
            return s
        time.sleep(1)
    raise AssertionError("condition timed out")


try:
    spawn([args.hrx, "hub", "collect"], "collector")
    remote_python(remote)
    time.sleep(2)
    a = spawn(
        [args.helper, "-test.run=^TestHubProcessFixture$"],
        "alpha",
        {"HRX_HUB_FIXTURE_IDENTITY": "fixture/alpha/agent"},
    )
    b = spawn(
        [args.helper, "-test.run=^TestHubProcessFixture$"],
        "beta",
        {"HRX_HUB_FIXTURE_IDENTITY": "fixture/beta/agent"},
    )
    s = wait_for(
        lambda s: len([r for r in s["agents"] if r["status"] == "running"]) == 3
    )
    live = [r for r in s["agents"] if r["status"] == "running"]
    assert len({r["host_id"] for r in live}) == 2
    assert all(r["harness"] == "claude" and r["model"] == "fixture-model" for r in live)
    alpha_run = next(r["run_id"] for r in live if r["agent"] == "fixture/alpha/agent")
    print("PASS: three supervised HRX runs on two physical hosts", flush=True)
    rival = spawn(
        [args.helper, "-test.run=^TestHubProcessFixture$"],
        "rival",
        {"HRX_HUB_FIXTURE_IDENTITY": "fixture/remote/agent"},
    )
    assert rival.wait(timeout=10) != 0
    assert "cannot acquire hub identity ownership" in (p / "rival.log").read_text()
    print("PASS: cross-host duplicate identity refused", flush=True)
    if args.exercise_collector_restart:
        children[0].kill()
        children[0].wait(timeout=10)
        checkpoint = json.loads((p / "state/hrx/hub/outbox.json").read_text())
    a.send_signal(signal.SIGTERM)
    a.wait(timeout=10)
    if args.exercise_collector_restart:
        # The exit exists locally, but the dead collector cannot publish it.
        local = json.loads(
            (p / "state/hrx/monitor" / (alpha_run + ".json")).read_text()
        )
        assert local["status"] == "exited"
        assert (
            next(r for r in snapshot()["agents"] if r["run_id"] == alpha_run)["status"]
            != "exited"
        )
        restarted = spawn([args.hrx, "hub", "collect"], "collector-restarted")
    s = wait_for(
        lambda s: any(
            r["run_id"] == alpha_run and r["status"] == "exited" for r in s["agents"]
        )
    )
    if args.exercise_collector_restart:
        published = next(r for r in s["agents"] if r["run_id"] == alpha_run)
        assert published["ended_at"] == local["ended_at"]
        assert published["last_activity"] == local["last_activity"]
        assert restarted.poll() is None
        current = json.loads((p / "state/hrx/hub/outbox.json").read_text())
        assert current["next_sequence"] > checkpoint["next_sequence"]
        print(
            "PASS: killed collector restarts from checkpoint and forwards original exit/activity timestamps",
            flush=True,
        )
    replacement = spawn(
        [args.helper, "-test.run=^TestHubProcessFixture$"],
        "replacement",
        {"HRX_HUB_FIXTURE_IDENTITY": "fixture/alpha/agent"},
    )
    s = wait_for(
        lambda s: any(
            r["agent"] == "fixture/alpha/agent"
            and r["run_id"] != alpha_run
            and r["status"] == "running"
            for r in s["agents"]
        )
    )
    print("PASS: exit forwarded and identity reacquired with a new run", flush=True)
    if args.exercise_network_partition:
        partitioned_runs = {
            r["run_id"]
            for r in s["agents"]
            if r["agent"] in {"fixture/alpha/agent", "fixture/beta/agent"}
            and r["status"] == "running"
        }
        assert len(partitioned_runs) == 2
        proxy.partition()
        try:
            refused = spawn(
                [args.helper, "-test.run=^TestHubProcessFixture$"],
                "partition-refused",
                {"HRX_HUB_FIXTURE_IDENTITY": "fixture/partition-new/agent"},
            )
            assert refused.wait(timeout=10) != 0
            assert (
                "cannot acquire hub identity ownership"
                in (p / "partition-refused.log").read_text()
            )
            started = time.monotonic()
            b.wait(timeout=50)
            replacement.wait(timeout=max(1, 50 - (time.monotonic() - started)))
            assert "ownership lease expired" in (p / "beta.log").read_text()
            assert "ownership lease expired" in (p / "replacement.log").read_text()
            s = wait_for(
                lambda s: (
                    {
                        r["run_id"]
                        for r in s["agents"]
                        if r["run_id"] in partitioned_runs and r["status"] == "stale"
                    }
                    == partitioned_runs
                ),
                15,
            )
            remote_row = next(
                r
                for r in s["agents"]
                if r["run_id"]
                == next(
                    r["run_id"] for r in live if r["agent"] == "fixture/remote/agent"
                )
            )
            assert (
                remote_row["status"] == "running"
                and remote_row["ownership"]["state"] == "held"
            )
        finally:
            proxy.heal()
        s = wait_for(
            lambda s: (
                {
                    r["run_id"]
                    for r in s["agents"]
                    if r["run_id"] in partitioned_runs and r["status"] == "exited"
                }
                == partitioned_runs
            ),
            30,
        )
        print(
            "PASS: partitioned host refuses launches and stops on expiry; remote host keeps ownership; recovery forwards final exits",
            flush=True,
        )
    if args.exercise_host_reboot:
        # Fixture services have no [Install] section; explicitly configure boot
        # activation before testing automatic recovery of this disposable host.
        remote_python("""import subprocess
subprocess.run(['systemctl','add-wants','multi-user.target','hrx-fixture-hub.service','hrx-fixture-pitput.service','hrx-fixture-gateway.service'],check=True)
""")
        remote_run = next(
            r["run_id"] for r in live if r["agent"] == "fixture/remote/agent"
        )
        boot_query = "from pathlib import Path;print(Path('/proc/sys/kernel/random/boot_id').read_text().strip())"
        before = remote_python(boot_query, capture=True).stdout.strip()
        try:
            subprocess.run(
                [
                    "ssh",
                    "-o",
                    "BatchMode=yes",
                    args.ssh_host,
                    f"pct reboot {args.container}",
                ],
                check=True,
                timeout=30,
            )
        except subprocess.TimeoutExpired:
            # The remote operation can outlive the SSH observation. Verify its
            # result via boot ID; never issue a second reboot on timeout.
            pass
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            try:
                after = remote_python(boot_query, capture=True).stdout.strip()
                s = snapshot()
                if after and after != before:
                    break
            except (subprocess.SubprocessError, urllib.error.URLError, TimeoutError):
                pass
            time.sleep(1)
        else:
            raise AssertionError("container boot and hub recovery deadline exceeded")
        remote_python("""from pathlib import Path
import os,subprocess
p=Path('/opt/hrx-fixture')
env=dict(os.environ,HRX_CONFIG=str(p/'config.toml'),XDG_STATE_HOME=str(p/'state'),SSL_CERT_FILE=str(p/'cert.pem'))
with open(p/'collector.log','w') as log:
 c=subprocess.Popen([str(p/'hrx'),'hub','collect'],env=env,stdout=log,stderr=log,start_new_session=True)
(p/'collector.pid').write_text(str(c.pid))
(p/'collector.boot').write_text(Path('/proc/sys/kernel/random/boot_id').read_text())
""")
        s = wait_for(
            lambda s: any(
                r["run_id"] == remote_run
                and r["status"] == "exited"
                and r.get("ownership", {}).get("state") == "inactive"
                for r in s["agents"]
            ),
            60,
        )
        assert any(
            r["run_id"] == alpha_run and r["status"] == "exited" for r in s["agents"]
        )
        reboot_replacement = spawn(
            [args.helper, "-test.run=^TestHubProcessFixture$"],
            "reboot-replacement",
            {"HRX_HUB_FIXTURE_IDENTITY": "fixture/remote/agent"},
        )
        s = wait_for(
            lambda s: any(
                r["agent"] == "fixture/remote/agent"
                and r["run_id"] != remote_run
                and r["status"] == "running"
                for r in s["agents"]
            )
        )
        assert reboot_replacement.poll() is None
        (p / "reboot-evidence.json").write_text(
            json.dumps(
                {
                    "before_boot_id": before,
                    "after_boot_id": after,
                    "previous_run": remote_run,
                    "history_preserved": True,
                    "collector_reconciled_exit": True,
                    "identity_reacquired": True,
                },
                indent=2,
            )
        )
        print(
            "PASS: actual container reboot preserves hub history; restarted collector reconciles old process exit; ownership is reacquired",
            flush=True,
        )
    if args.exercise_host_suspension:
        remote_run = next(
            r["run_id"] for r in live if r["agent"] == "fixture/remote/agent"
        )
        pause = """from pathlib import Path
import os,signal,subprocess,json
p=Path('/opt/hrx-fixture')
pids={int((p/(name+'.pid')).read_text()) for name in ['agent','collector']}
rows=[list(map(int,row.split())) for row in subprocess.check_output(['ps','-axo','pid=,ppid='],text=True).splitlines()]
while True:
 children={pid for pid,parent in rows if parent in pids}
 if children <= pids:break
 pids |= children
(p/'suspended.pids').write_text(json.dumps(sorted(pids)))
for pid in pids:os.kill(pid,signal.SIGSTOP)
"""
        remote_python(pause)
        try:
            s = wait_for(
                lambda s: any(
                    r["run_id"] == remote_run
                    and r["status"] == "stale"
                    and r.get("ownership", {}).get("state") == "inactive"
                    for r in s["agents"]
                ),
                65,
            )
            takeover = spawn(
                [args.helper, "-test.run=^TestHubProcessFixture$"],
                "takeover",
                {"HRX_HUB_FIXTURE_IDENTITY": "fixture/remote/agent"},
            )
            s = wait_for(
                lambda s: any(
                    r["agent"] == "fixture/remote/agent"
                    and r["run_id"] != remote_run
                    and r["status"] == "running"
                    for r in s["agents"]
                )
            )
            successor = next(
                r["run_id"]
                for r in s["agents"]
                if r["agent"] == "fixture/remote/agent" and r["status"] == "running"
            )
        finally:
            remote_python("""from pathlib import Path
import os,signal,json
p=Path('/opt/hrx-fixture')
for pid in json.loads((p/'suspended.pids').read_text()):
 try:os.kill(pid,signal.SIGCONT)
 except ProcessLookupError:pass
""")
        s = wait_for(
            lambda s: any(
                r["run_id"] == remote_run and r["status"] == "exited"
                for r in s["agents"]
            ),
            30,
        )
        assert takeover.poll() is None
        assert (
            next(r for r in s["agents"] if r["run_id"] == successor)["status"]
            == "running"
        )
        print(
            "PASS: suspended host becomes stale; expired ownership transfers; resumed old owner exits without stopping successor",
            flush=True,
        )
    if args.exercise_process_failures:
        # Keep process inspection available, but deliberately omit the harness.
        missing = p / "missing-harness"
        missing.mkdir(exist_ok=True)
        if not (missing / "ps").exists():
            (missing / "ps").symlink_to(shutil.which("ps"))
        failed = spawn(
            [args.helper, "-test.run=^TestHubProcessFixture$"],
            "failed-start",
            {"PATH": str(missing), "HRX_HUB_FIXTURE_IDENTITY": "fixture/failed/agent"},
        )
        assert failed.wait(timeout=10) != 0
        assert "ownership acquired" in (p / "failed-start.log").read_text()
        wait_for(
            lambda s: any(
                r["agent"] == "fixture/failed/agent" and r["status"] == "exited"
                for r in s["agents"]
            )
        )
        recovered = spawn(
            [args.helper, "-test.run=^TestHubProcessFixture$"],
            "recovered-start",
            {"HRX_HUB_FIXTURE_IDENTITY": "fixture/failed/agent"},
        )
        wait_for(
            lambda s: any(
                r["agent"] == "fixture/failed/agent" and r["status"] == "running"
                for r in s["agents"]
            )
        )
        print(
            "PASS: failed harness startup finalizes its run and releases ownership for recovery",
            flush=True,
        )
        beta_run = next(r["run_id"] for r in live if r["agent"] == "fixture/beta/agent")
        processes = subprocess.check_output(["ps", "-axo", "pid=,ppid="], text=True)
        descendants = [
            int(row.split()[0])
            for row in processes.splitlines()
            if len(row.split()) == 2 and int(row.split()[1]) == b.pid
        ]
        assert len(descendants) == 1, descendants
        os.kill(descendants[0], signal.SIGKILL)
        assert b.wait(timeout=10) != 0
        s = wait_for(
            lambda s: any(
                r["run_id"] == beta_run and r["status"] == "exited" for r in s["agents"]
            )
        )
        assert recovered.poll() is None
        print(
            "PASS: abrupt harness kill produces a confirmed exit through its supervisor",
            flush=True,
        )
    if args.exercise_outage or args.exercise_database_outage:
        service = (
            "postgresql@17-main" if args.exercise_database_outage else "hrx-fixture-hub"
        )
        remote_python(
            'import subprocess;subprocess.run(["systemctl","stop",'
            + repr(service)
            + "],check=True)"
        )
        try:
            started = time.monotonic()
            b.wait(timeout=50)
            replacement.wait(timeout=max(1, 50 - (time.monotonic() - started)))
            assert "ownership lease expired" in (p / "beta.log").read_text()
            assert "ownership lease expired" in (p / "replacement.log").read_text()
        finally:
            remote_python(
                'import subprocess;subprocess.run(["systemctl","start",'
                + repr(service)
                + "],check=True)"
            )
        time.sleep(2)
        s = wait_for(lambda s: all(r["status"] == "exited" for r in s["agents"]), 30)
        print(
            "PASS: "
            + service
            + " outage stops coordinated agents; restart preserves and receives final exits",
            flush=True,
        )
    (p / "evidence.json").write_text(json.dumps(s, indent=2))
finally:
    for child in children[1:]:
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
    for child in children[1:]:
        try:
            child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGKILL)
    remote_python("""from pathlib import Path
import os,signal,time
p=Path('/opt/hrx-fixture')
for name in ['agent','collector']:
 if not (p/(name+'.boot')).exists() or (p/(name+'.boot')).read_text()!=Path('/proc/sys/kernel/random/boot_id').read_text():continue
 try:os.kill(int((p/(name+'.pid')).read_text()),signal.SIGTERM)
 except (ProcessLookupError,FileNotFoundError):pass
 time.sleep(4)
""")
    time.sleep(4)
    for child in children[:1]:
        if child.poll() is None:
            child.terminate()
            child.wait(timeout=10)
    if proxy is not None:
        proxy.close()
