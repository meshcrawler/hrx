"""T2 controller safety: input, revalidation, restart proof, key handling, teardown."""

import datetime
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import t2_guest_lifecycle as t2
import t2_processes


NOW = datetime.datetime(2026, 10, 24, 12, 0, tzinfo=datetime.timezone.utc)


def spec_text(**overrides):
    spec = {"run_id": "t2-20261024-01", "hub_port": 18790, "expires_at": "2026-10-24T18:00:00Z",
            "guests": {"hub": {"address": "10.0.0.11", "resource_id": "scratch-ct-9001"},
                       "agent": {"address": "10.0.0.12", "resource_id": "scratch-ct-9002"}}}
    spec.update(overrides)
    return json.dumps(spec)


class Guest:
    def __init__(self, marker="t2-20261024-01", machine="m"):
        self.run_marker, self.machine = marker, machine
        self.pid1, self.nonce, self.boot_id = 100, None, "boot-1"
        self.reachable, self.reboots, self.torn_down = True, 0, 0
        # How a reboot behaves: "restart" (real), "marker-only", "pid-only", "boot-id-only".
        self.mode, self.pending, self.down_probes = "restart", False, 0
        self.teardown = {"remaining": [], "run_dir_removed": True, "postgres_stopped": True, "errors": []}
        self.timeouts_while_booting = 0


class FakeSsh:
    def __init__(self, guests):
        self.guests, self.calls = guests, []

    def python(self, address, source, timeout=120):
        guest = self.guests[address]
        self.calls.append((address, source))
        if source == "print('ok')":
            if guest.pending and guest.down_probes >= 1 and guest.timeouts_while_booting:
                guest.timeouts_while_booting -= 1
                raise subprocess.TimeoutExpired(["ssh"], timeout)
            if guest.pending:
                guest.down_probes += 1
                if guest.down_probes == 1:
                    return 255, "", "unreachable"
                guest.pending = False
                if guest.mode in ("restart", "pid-only"):
                    guest.pid1 += 1
                if guest.mode in ("restart", "marker-only"):
                    guest.nonce = None
                if guest.mode == "boot-id-only":
                    guest.boot_id = "boot-2"
            return 0, "ok\n", ""
        if "restart-marker\").write_text" in source:
            guest.nonce = json.loads(json.loads(source.splitlines()[1][len("P = json.loads("):-1]))["nonce"]
            return 0, "", ""
        if "systemctl" in source and "reboot" in source:
            guest.reboots += 1
            guest.pending, guest.down_probes = True, 0
            return 255, "", "connection closed"
        if "pid1_start" in source:
            return 0, json.dumps({"pid1_start": guest.pid1, "marker": guest.nonce, "boot_id": guest.boot_id,
                                  "run_marker": guest.run_marker, "machine_id": guest.machine,
                                  "hostname": "h"}) + "\n", ""
        if "remaining" in source:
            guest.torn_down += 1
            return 0, json.dumps(guest.teardown) + "\n", ""
        raise AssertionError("unexpected remote script")


class Clock:
    def __init__(self, at):
        self.now = at.timestamp()

    def __call__(self):
        self.now += 1
        return self.now


def controller(hub=None, agent=None, at=NOW):
    hub, agent = hub or Guest(machine="hub"), agent or Guest(machine="agent")
    ssh = FakeSsh({"10.0.0.11": hub, "10.0.0.12": agent})
    directory = tempfile.mkdtemp()
    spec = t2.load_input(spec_text(), NOW)
    return t2.Controller(spec, ssh, directory, clock=Clock(at), sleep=lambda s: None), hub, agent, ssh


class InputTests(unittest.TestCase):
    def test_valid_input(self):
        self.assertEqual(t2.load_input(spec_text(), NOW)["run_id"], "t2-20261024-01")

    def test_rejects_stale_long_lived_or_ambiguous_input(self):
        cases = {
            "expired": spec_text(expires_at="2026-10-24T11:00:00Z"),
            "over 24 hours": spec_text(expires_at="2026-10-25T13:00:00Z"),
            "no Z": spec_text(expires_at="2026-10-24T18:00:00"),
            "extra key": spec_text(extra=1),
            "bad run id": spec_text(run_id="prod-1"),
            "same guest": spec_text(guests={"hub": {"address": "10.0.0.11", "resource_id": "a"},
                                            "agent": {"address": "10.0.0.11", "resource_id": "b"}}),
            "hostname": spec_text(guests={"hub": {"address": "hub.example", "resource_id": "a"},
                                          "agent": {"address": "10.0.0.12", "resource_id": "b"}}),
            "privileged port": spec_text(hub_port=443),
        }
        for name, text in cases.items():
            with self.subTest(name), self.assertRaises(t2.Refused):
                t2.load_input(text, NOW)


class ProcessIdentityTests(unittest.TestCase):
    def test_teardown_signals_only_matching_processes(self):
        records = {pid: {"pid": pid, "start": 1000, "group": pid, "session": pid,
                         "init_start": 500, "uid": os.getuid()} for pid in (41, 42)}
        with tempfile.TemporaryDirectory() as directory:
            run = Path(directory) / "run"
            run.mkdir()
            for pid, record in records.items():
                (run / f"{pid}.pid").write_text(json.dumps(record))

            def actual(pid):
                return {**records[pid], "start": 9999 if pid == 42 else 1000}, "S"

            output = []
            with patch.object(t2_processes, "identity", side_effect=actual), \
                    patch.object(t2_processes.os, "killpg") as kill, \
                    patch("subprocess.run", return_value=subprocess.CompletedProcess([], 0, "", "")), \
                    patch("time.sleep"), patch("builtins.print", side_effect=output.append):
                exec(t2.TEARDOWN, {"P": {"run": str(run)}, "json": json})
            self.assertEqual([call.args[0] for call in kill.call_args_list], [41, 41])
            result = json.loads(output[-1])
            self.assertTrue(result["run_dir_removed"])
            self.assertEqual(result["errors"], [])

    def test_guest_wait_reports_reused_pid_as_exited(self):
        import t2_guest
        record = {"pid": 123, "start": 456, "group": 123, "session": 123,
                  "init_start": 10, "uid": os.getuid()}
        with tempfile.TemporaryDirectory() as directory:
            run = Path(directory)
            (run / "agent.pid").write_text(json.dumps(record))
            (run / "agent.log").write_text("original process")
            with patch.object(t2_processes, "identity", return_value=({**record, "init_start": 11}, "S")):
                self.assertTrue(t2_guest.wait(run, {"name": "agent", "timeout": 0})["exited"])

    def test_reused_pid_or_restarted_guest_is_not_signalled(self):
        record = {"pid": 123, "start": 456, "group": 123, "session": 123,
                  "init_start": 10, "uid": os.getuid()}
        for update in ({"start": 789}, {"init_start": 11}, {"uid": os.getuid() + 1},
                       {"group": 124}, {"session": 124}):
            with self.subTest(update=update), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "p.pid"
                path.write_text(json.dumps(record))
                with patch.object(t2_processes, "identity", return_value=({**record, **update}, "S")), \
                        patch.object(t2_processes.os, "killpg") as kill:
                    self.assertFalse(t2_processes.signal_owned(path, 15))
                    kill.assert_not_called()

    def test_matching_owned_session_can_be_signalled(self):
        record = {"pid": 123, "start": 456, "group": 123, "session": 123,
                  "init_start": 10, "uid": os.getuid()}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "p.pid"
            path.write_text(json.dumps(record))
            with patch.object(t2_processes, "identity", return_value=(record, "S")), \
                    patch.object(t2_processes.os, "killpg") as kill:
                self.assertTrue(t2_processes.signal_owned(path, 15))
                kill.assert_called_once_with(123, 15)
            with patch.object(t2_processes, "identity", return_value=(record, "Z")):
                self.assertIsNone(t2_processes.matching(path))

    def test_numeric_or_invalid_pid_record_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "p.pid"
            path.write_text("123")
            with self.assertRaisesRegex(RuntimeError, "invalid fixture"):
                t2_processes.matching(path)


class RevalidationTests(unittest.TestCase):
    def test_expired_input_blocks_reboot_before_any_command(self):
        control, hub, agent, ssh = controller(at=NOW + datetime.timedelta(hours=7))
        with self.assertRaisesRegex(t2.Refused, "expired"):
            control.reboot("agent")
        self.assertEqual(agent.reboots, 0)

    def test_foreign_marker_blocks_reboot(self):
        control, hub, agent, ssh = controller(agent=Guest(marker="someone-else"))
        with self.assertRaisesRegex(t2.Refused, "marker"):
            control.reboot("agent")
        self.assertEqual(agent.reboots, 0)

    def test_preflight_refuses_a_foreign_or_duplicate_guest(self):
        control, *_ = controller(hub=Guest(marker="t2-other"))
        with self.assertRaises(t2.Refused):
            control.preflight()
        control, *_ = controller(hub=Guest(machine="same"), agent=Guest(machine="same"))
        with self.assertRaisesRegex(t2.Refused, "machine ID"):
            control.preflight()


class RestartProofTests(unittest.TestCase):
    def test_probe_timeout_is_retried_not_treated_as_shutdown(self):
        for reachable in (True, False):
            control, _, _, ssh = controller()
            with patch.object(ssh, "python", side_effect=[
                subprocess.TimeoutExpired("ssh", 20),
                (0 if reachable else 255, "", ""),
            ]) as probe:
                control.wait_reachability("agent", reachable, 60)
            self.assertEqual(probe.call_count, 2)

    def test_real_restart_is_proven_without_boot_id_change(self):
        control, hub, agent, ssh = controller()
        control.reboot("agent")
        self.assertEqual(agent.reboots, 1)
        self.assertFalse(control.restarts["agent"]["boot_id_changed"])

    def test_partial_or_boot_id_only_evidence_is_rejected(self):
        for mode in ("marker-only", "pid-only", "boot-id-only"):
            with self.subTest(mode):
                agent = Guest(machine="agent")
                agent.mode = mode
                control, *_ = controller(agent=agent)
                with self.assertRaisesRegex(RuntimeError, "restart not proven"):
                    control.reboot("agent")

    def test_restart_predicate(self):
        self.assertTrue(t2.restarted({"marker": "n", "pid1_start": 1}, {"marker": None, "pid1_start": 2}))
        self.assertFalse(t2.restarted({"marker": None, "pid1_start": 1}, {"marker": None, "pid1_start": 2}))


class KeyHandlingTests(unittest.TestCase):
    def test_ssh_passes_the_key_path_only(self):
        with tempfile.TemporaryDirectory() as directory:
            key = Path(directory) / "key"
            key.write_text("-----BEGIN SECRET KEY MATERIAL-----")
            seen = []

            def fake_run(argv, **kwargs):
                seen.append((argv, kwargs.get("input")))
                return subprocess.CompletedProcess(argv, 0, "", "")

            with patch.object(t2.subprocess, "run", fake_run):
                t2.Ssh(key, Path(directory) / "known_hosts").python("10.0.0.12", "print(1)")
            argv, stdin = seen[0]
            self.assertIn(str(key), argv)
            self.assertNotIn("SECRET", " ".join(argv) + (stdin or ""))
            self.assertIn("StrictHostKeyChecking=yes", argv)
            self.assertIn("hrx-fixture@10.0.0.12", argv)


class TeardownTests(unittest.TestCase):
    def test_incomplete_cleanup_report_cannot_pass(self):
        clean = {"remaining": [], "run_dir_removed": True,
                 "postgres_stopped": True, "errors": []}
        for update in ({"run_dir_removed": False}, {"postgres_stopped": False},
                       {"remaining": None}, {"errors": ["inspection failed"]}):
            with self.subTest(update=update):
                control, *_ = controller()
                with patch.object(control, "revalidate"), patch.object(
                    control, "run_on", return_value=json.dumps({**clean, **update})
                ):
                    self.assertFalse(control.teardown())

    def test_remote_cleanup_failures_are_reported_and_state_preserved(self):
        for failure in ("pg", "pg_stop", "ps", "remove", None):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                run = Path(directory) / "run"
                (run / "pg").mkdir(parents=True)
                calls, output = [], []

                def fake_run(argv, **kwargs):
                    calls.append(argv)
                    if argv[0] == "ps":
                        if failure == "ps":
                            raise subprocess.CalledProcessError(1, argv)
                        return subprocess.CompletedProcess(argv, 0, "", "")
                    if failure == "pg":
                        raise subprocess.TimeoutExpired(argv, 30)
                    if failure == "pg_stop":
                        if argv[-1] == "stop":
                            raise subprocess.CalledProcessError(1, argv)
                        return subprocess.CompletedProcess(argv, 0, "", "")
                    return subprocess.CompletedProcess(argv, 3, "", "")

                import shutil
                original_remove = shutil.rmtree

                def remove(path):
                    if failure == "remove":
                        raise PermissionError("injected removal failure")
                    return original_remove(path)

                with patch("subprocess.run", side_effect=fake_run), \
                        patch("time.sleep"), patch("shutil.rmtree", side_effect=remove), \
                        patch("builtins.print", side_effect=output.append):
                    exec(t2.TEARDOWN, {"P": {"run": str(run)}, "json": json})
                result = json.loads(output[-1])
                self.assertTrue(any(call[0] == "ps" for call in calls))
                self.assertEqual(result["run_dir_removed"], failure is None)
                self.assertEqual(bool(result["errors"]), failure is not None)

    def test_each_guest_is_torn_down_independently(self):
        control, hub, agent, ssh = controller(hub=Guest(marker="t2-other", machine="hub"))
        self.assertFalse(control.teardown())
        self.assertEqual((agent.torn_down, hub.torn_down), (1, 0))
        detail = json.loads(control.results[-1]["detail"])
        self.assertIn("hub", detail["errors"])
        self.assertEqual(detail["resource_ids"]["agent"], "scratch-ct-9002")

    def test_run_writes_evidence_and_fails_on_any_failure(self):
        control, *_ = controller()

        def failing(c):
            raise RuntimeError("scenario broke")

        self.assertEqual(control.run([failing]), 1)
        report = json.loads((control.out / "evidence.json").read_text())
        self.assertIn("scenario broke", json.dumps(report))
        self.assertEqual(report["evidence"], t2.EVIDENCE)

    def test_clean_run_passes(self):
        control, *_ = controller()
        self.assertEqual(control.run([lambda c: c.record("noop", "PASS")]), 0)


class VerifiedTeardownTests(unittest.TestCase):
    def check(self, guest_field, value, expect_message):
        agent = Guest(machine="agent")
        agent.teardown = dict(agent.teardown, **{guest_field: value})
        control, hub, agent, ssh = controller(agent=agent)
        self.assertFalse(control.teardown())
        self.assertEqual((hub.torn_down, agent.torn_down), (1, 1))
        self.assertIn(expect_message, control.results[-1]["detail"])

    def test_unremoved_run_directory_fails(self):
        self.check("run_dir_removed", False, '"run_dir_removed": false')

    def test_unverified_database_shutdown_fails(self):
        self.check("postgres_stopped", False, '"postgres_stopped": false')

    def test_guest_cleanup_error_fails_and_is_preserved(self):
        self.check("errors", ["run directory removal: PermissionError()"], "PermissionError")

    def test_fully_verified_teardown_passes(self):
        control, *_ = controller()
        self.assertTrue(control.teardown())


class ReachabilityTests(unittest.TestCase):
    def test_ssh_timeouts_while_booting_are_retried_until_the_deadline(self):
        agent = Guest(machine="agent")
        agent.timeouts_while_booting = 3
        control, *_ = controller(agent=agent)
        control.reboot("agent")
        self.assertEqual(agent.timeouts_while_booting, 0)
        self.assertIn("agent", control.restarts)


class ScenarioSafetyTests(unittest.TestCase):
    def test_guest_wait_receives_timeout_with_longer_rpc_deadline(self):
        import t2_scenarios
        c, _ = self.fake()
        with patch.object(c, "run_on", return_value='{"exited": true}') as remote:
            t2_scenarios.call(c, "agent", "wait", name="epsilon", timeout=120)
        self.assertEqual(remote.call_args.kwargs["params"]["timeout"], 120)
        self.assertEqual(remote.call_args.kwargs["timeout"], 150)

    def test_ownership_uses_held_lease_not_running_status(self):
        import t2_scenarios
        seen = [{"agent": "a", "status": "running", "ownership": {"state": "held"}},
                {"agent": "a", "status": "running", "ownership": {"state": "inactive"}}]
        self.assertTrue(t2_scenarios.single_owner(seen))
        seen[1]["ownership"]["state"] = "held"
        self.assertFalse(t2_scenarios.single_owner(seen))

    def test_takeover_waits_for_observed_expiry_before_launch(self):
        import t2_scenarios
        from types import SimpleNamespace
        events = []
        row = {"run_id": "old", "agent": "fixture/beta/agent",
               "ownership": {"state": "held"}, "status": "running"}
        c = SimpleNamespace(state={"live": {"fixture/beta/agent": row}},
                            reboot=lambda guest: ({}, {}), record=lambda *args: None)

        def call(control, guest, command, **kwargs):
            events.append((command, kwargs.get("name")))
            return {"exited": True, "log": t2_scenarios.OWNERSHIP_REFUSED}

        def wait(control, predicate, seconds, label, **kwargs):
            if "expires before" in label:
                expired = {**row, "ownership": {"state": "inactive"}}
                self.assertFalse(predicate([row]))
                self.assertTrue(predicate([expired]))
                events.append(("expiry observed", None))
                return [expired]
            return [{**row, "status": "exited", "ownership": {"state": "inactive"}}]

        with patch.object(t2_scenarios, "rows", return_value=[row]), \
                patch.object(t2_scenarios, "call", side_effect=call), \
                patch.object(t2_scenarios, "wait_for", side_effect=wait):
            t2_scenarios.agent_restart(c)
        self.assertLess(events.index(("expiry observed", None)),
                        events.index(("agent", "beta-takeover")))

    def fake(self, fail_on=None):
        events = []

        class Fake:
            spec = {"hub_port": 18790}
            results = []

            def address(self, guest):
                return {"hub": "10.0.0.11", "agent": "10.0.0.12"}[guest]

            def revalidate(self, guest):
                events.append(("revalidate", guest))

            def record(self, name, status, detail=""):
                self.results.append((name, status))

            def run_on(self, guest, script, timeout=120, **params):
                command = params.get("command")
                events.append((command, guest, params.get("params", {}).get("action")))
                if command == fail_on:
                    raise RuntimeError(f"{command} failed")
                return json.dumps({"agents": [], "proxy": "ok", "signalled": True}) + "\n"

        return Fake(), events

    def test_every_signal_revalidates_the_guest_first(self):
        import t2_scenarios
        c, events = self.fake()
        t2_scenarios.signal(c, "agent", "collector", "SIGKILL")
        self.assertEqual(events[0], ("revalidate", "agent"))
        self.assertEqual(events[1][0], "send")

    def test_reconnect_heals_and_stops_the_proxy_when_it_fails(self):
        import t2_scenarios
        c, events = self.fake()
        with patch.object(t2_scenarios, "rows", side_effect=RuntimeError("hub unreachable")), \
                self.assertRaisesRegex(RuntimeError, "hub unreachable"):
            t2_scenarios.reconnect(c)
        controls = [event[2] for event in events if event[0] == "proxy_control"]
        self.assertEqual(controls, ["heal", "stop"])

    def test_reconnect_keeps_the_original_error_when_cleanup_also_fails(self):
        import t2_scenarios
        c, events = self.fake(fail_on="proxy_control")
        with patch.object(t2_scenarios, "rows", side_effect=RuntimeError("hub unreachable")), \
                self.assertRaisesRegex(RuntimeError, "hub unreachable"):
            t2_scenarios.reconnect(c)
        self.assertIn(("partition proxy heal", "FAIL"), c.results)
        self.assertIn(("partition proxy stop", "FAIL"), c.results)


if __name__ == "__main__":
    unittest.main()
