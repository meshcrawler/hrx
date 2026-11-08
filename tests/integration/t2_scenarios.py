"""T2 scenarios, composed from the guest toolkit (t2_guest.py) by the controller.

Every signal or restart re-validates the target guest first (Controller.revalidate);
reads never change guest state. Hub state is read from inside a guest, because the
runner reaches guests only on SSH.
"""

import json
from pathlib import Path
import time

from t2_guest_lifecycle import REMOTE_RUN

DISPATCH = 'import sys\nsys.path.insert(0, P["run"])\nimport t2_guest\nt2_guest.dispatch(P)\n'
ROOT = Path(__file__).resolve().parents[2]
ARTIFACTS = {"bin/hrx": ROOT / "bin" / "hrx", "bin/app.test": ROOT / "bin" / "app.test",
             "t2_guest.py": Path(__file__).with_name("t2_guest.py"),
             "t2_processes.py": Path(__file__).with_name("t2_processes.py"),
             "hub_partition.py": Path(__file__).with_name("hub_partition.py")}
OWNERSHIP_REFUSED = "cannot acquire hub identity ownership"


def call(c, guest, command, rpc_timeout=120, **params):
    if command == "wait":
        rpc_timeout = max(rpc_timeout, params.get("timeout", 15) + 30)
    out = c.run_on(guest, DISPATCH, timeout=rpc_timeout, run=REMOTE_RUN, command=command, params=params)
    return json.loads(out.strip().splitlines()[-1])


def signal(c, guest, name, sig):
    c.revalidate(guest)
    return call(c, guest, "send", name=name, signal=sig)


def hub_url(c):
    return f"https://{c.address('hub')}:{c.spec['hub_port']}"


def rows(c, via="agent"):
    seen = call(c, via, "snapshot", url=hub_url(c))["agents"]
    assert single_owner(seen), seen
    return seen


def wait_for(c, predicate, seconds, label, via="agent"):
    deadline, seen = time.monotonic() + seconds, []
    while time.monotonic() < deadline:
        seen = rows(c, via)
        if predicate(seen):
            return seen
        time.sleep(2)
    summary = [{k: r.get(k) for k in ("agent", "run_id", "status", "host_id")} for r in seen]
    raise AssertionError(f"timed out waiting for {label}: {summary}")


def run_of(seen, identity, status="running"):
    return next(r for r in seen if r["agent"] == identity and r["status"] == status)


def single_owner(seen):
    owners = [r["agent"] for r in seen if r.get("ownership", {}).get("state") == "held"]
    return len(owners) == len(set(owners))


def setup(c):
    c.state = {}
    for guest in ("hub", "agent"):
        c.revalidate(guest)
        c.run_on(guest, "from pathlib import Path\nfor d in (P['run'], P['run'] + '/bin'):\n"
                        "    Path(d).mkdir(mode=0o700, exist_ok=True)\n", run=REMOTE_RUN)
        for remote, local in ARTIFACTS.items():
            c.ssh.copy(c.address(guest), local, f"{REMOTE_RUN}/{remote}")
        call(c, guest, "prepare")
    call(c, "hub", "hub_start", rpc_timeout=180, address=c.address("hub"), port=c.spec["hub_port"])
    call(c, "agent", "install_credentials", files=call(c, "hub", "credentials"))
    for guest in ("hub", "agent"):
        call(c, guest, "enroll", url=hub_url(c))
        call(c, guest, "collector")
    call(c, "hub", "agent", name="alpha", identity="fixture/alpha/agent")
    call(c, "agent", "agent", name="beta", identity="fixture/beta/agent")
    call(c, "agent", "agent", name="gamma", identity="fixture/gamma/agent")
    seen = wait_for(c, lambda s: len([r for r in s if r["status"] == "running"]) == 3, 60, "three runs")
    live = [r for r in seen if r["status"] == "running"]
    assert len({r["host_id"] for r in live}) == 2, live
    c.state["live"] = {r["agent"]: r for r in live}
    c.record("bootstrap: three runs on two guests", "PASS", "distinct hub host IDs")


def exclusion(c):
    call(c, "agent", "agent", name="rival", identity="fixture/alpha/agent")
    result = call(c, "agent", "wait", name="rival", timeout=20)
    assert result["exited"] and OWNERSHIP_REFUSED in result["log"], result
    c.record("owner lease exclusion across guests", "PASS")


def collector_recovery(c):
    gamma = c.state["live"]["fixture/gamma/agent"]["run_id"]
    signal(c, "agent", "collector", "SIGKILL")
    assert call(c, "agent", "wait", name="collector", timeout=15)["exited"]
    checkpoint = call(c, "agent", "outbox")
    signal(c, "agent", "gamma", "SIGTERM")
    assert call(c, "agent", "wait", name="gamma", timeout=15)["exited"]
    local = call(c, "agent", "monitor_state", run_id=gamma)
    assert local.get("status") == "exited", local
    assert next(r for r in rows(c, "hub") if r["run_id"] == gamma)["status"] != "exited"
    call(c, "agent", "collector", name="collector-restarted")
    seen = wait_for(c, lambda s: any(r["run_id"] == gamma and r["status"] == "exited" for r in s), 60,
                    "restarted collector forwards the exit")
    published = next(r for r in seen if r["run_id"] == gamma)
    assert published["ended_at"] == local["ended_at"] and published["last_activity"] == local["last_activity"]
    assert call(c, "agent", "outbox")["next_sequence"] > checkpoint["next_sequence"]
    c.record("collector recovery from checkpoint", "PASS", "original exit and activity timestamps forwarded")


def agent_restart(c):
    beta = c.state["live"]["fixture/beta/agent"]["run_id"]
    held = next(r for r in rows(c, "hub") if r["run_id"] == beta)
    assert held["ownership"]["state"] == "held", held
    call(c, "hub", "agent", name="beta-before-expiry", identity="fixture/beta/agent")
    refused = call(c, "hub", "wait", name="beta-before-expiry", timeout=20)
    assert refused["exited"] and OWNERSHIP_REFUSED in refused["log"], refused
    before, after = c.reboot("agent")
    assert call(c, "agent", "wait", name="beta", timeout=5)["exited"], "pre-restart process survived"
    # While the agent guest's collector is gone, ownership must expire before another
    # guest can take the identity; it is never owned twice.
    expired_rows = wait_for(c, lambda s: any(
        r["run_id"] == beta and r.get("ownership", {}).get("state") == "inactive" for r in s),
        180, "previous ownership lease expires before takeover", via="hub")
    expired = next(r for r in expired_rows if r["run_id"] == beta)
    call(c, "hub", "agent", name="beta-takeover", identity="fixture/beta/agent")
    seen = wait_for(c, lambda s: any(r["agent"] == "fixture/beta/agent" and r["run_id"] != beta
                                     and r["status"] == "running"
                                     and r.get("ownership", {}).get("state") == "held" for r in s), 180,
                    "identity reacquired on the hub guest after expiry", via="hub")
    assert single_owner(seen), seen
    call(c, "agent", "collector", name="collector-after-restart")
    seen = wait_for(c, lambda s: any(r["run_id"] == beta and r["status"] == "exited" for r in s), 90,
                    "pre-restart run reconciled as exited", via="hub")
    assert single_owner(seen), seen
    c.record("agent guest restart", "PASS", json.dumps({
        "restart": "new PID 1 and lost /run marker", "before_expiry_refused": True,
        "before": held["ownership"], "expired": expired["ownership"],
        "reconciled_old_run": beta, "ownership_check": "unique held owner in every sampled snapshot"}))


def hub_restart(c):
    known = {r["run_id"] for r in rows(c, "hub")}
    c.reboot("hub")
    call(c, "hub", "hub_services", rpc_timeout=180, port=c.spec["hub_port"])
    call(c, "hub", "collector", name="collector-hub-after-restart")
    seen = wait_for(c, lambda s: known <= {r["run_id"] for r in s}
                    and not any(r["status"] == "running" and r["host_id"] == c.state["live"]["fixture/alpha/agent"]["host_id"]
                                for r in s), 120, "history persists and pre-restart hub-guest runs reconcile")
    call(c, "agent", "agent", name="delta", identity="fixture/delta/agent")
    seen = wait_for(c, lambda s: any(r["agent"] == "fixture/delta/agent" and r["status"] == "running" for r in s),
                    60, "agent guest launches again after the hub restart")
    assert single_owner(seen), seen
    c.record("hub guest restart", "PASS",
             "restart proven; services restarted by the controller (not boot autostart); history persists; "
             "pre-restart runs reconciled; agent guest reconnects")


def reconnect(c):
    call(c, "agent", "proxy", url=hub_url(c))
    try:
        delta = next(r["run_id"] for r in rows(c) if r["agent"] == "fixture/delta/agent" and r["status"] == "running")
        signal(c, "agent", "delta", "SIGTERM")
        wait_for(c, lambda s: any(r["run_id"] == delta and r["status"] == "exited" for r in s), 60,
                 "agent guest exit forwarded before re-homing")
        signal(c, "agent", "collector-after-restart", "SIGTERM")
        call(c, "agent", "collector", name="collector-proxied", config="partition.toml")
        call(c, "agent", "agent", name="epsilon", identity="fixture/epsilon/agent", config="partition.toml")
        call(c, "hub", "agent", name="anchor", identity="fixture/anchor/agent")
        seen = wait_for(c, lambda s: {"fixture/epsilon/agent", "fixture/anchor/agent"}
                        <= {r["agent"] for r in s if r["status"] == "running"}, 60, "proxied and anchor runs")
        epsilon, anchor = run_of(seen, "fixture/epsilon/agent")["run_id"], run_of(seen, "fixture/anchor/agent")["run_id"]
        call(c, "agent", "proxy_control", action="partition")
        call(c, "agent", "agent", name="refused", identity="fixture/refused/agent", config="partition.toml")
        refused = call(c, "agent", "wait", name="refused", timeout=20)
        assert refused["exited"] and OWNERSHIP_REFUSED in refused["log"], refused
        stopped = call(c, "agent", "wait", name="epsilon", timeout=120)
        assert stopped["exited"] and "ownership lease expired" in stopped["log"], stopped
        seen = wait_for(c, lambda s: any(r["run_id"] == epsilon and r["status"] == "stale" for r in s), 60,
                        "partitioned run stale", via="hub")
        held = next(r for r in seen if r["run_id"] == anchor)
        assert held["status"] == "running" and held["ownership"]["state"] == "held", held
        call(c, "agent", "proxy_control", action="heal")
        wait_for(c, lambda s: any(r["run_id"] == epsilon and r["status"] == "exited" for r in s), 90,
                 "final exit forwarded after heal", via="hub")
    except BaseException:
        # Restore the path even while failing, without losing the original error.
        try:
            call(c, "agent", "proxy_control", action="heal")
        except Exception as error:
            c.record("partition proxy heal", "FAIL", repr(error)[-1000:])
        raise
    finally:
        try:
            call(c, "agent", "proxy_control", action="stop")
        except Exception as error:
            c.record("partition proxy stop", "FAIL", repr(error)[-1000:])
    c.record("partition and reconnect", "PASS",
             "partitioned agent guest refused launches and stopped on expiry; hub guest kept ownership; "
             "final exit forwarded after heal")


SCENARIOS = [setup, exclusion, collector_recovery, agent_restart, hub_restart, reconnect]
