"""Per-agent ownership boundary: a Linux child subreaper that owns every descendant.

    python3 -I subreaper.py --summary FILE [--grace SECONDS] -- COMMAND [ARGS...]

The supervisor marks itself PR_SET_CHILD_SUBREAPER, then runs COMMAND as its child
in a new session (COMMAND stays a session leader, as when launched directly).
Orphaned descendants, including double-forked setsid daemons, are re-parented to
the supervisor rather than to init, so the supervisor is a persistent boundary.

On SIGTERM it signals its process tree, escalates to SIGKILL after the grace
period, and reaps until wait() reports no children (ECHILD): the kernel's proof
that no descendant remains. Only then does it write FILE and exit: 0 when every
descendant exited without SIGKILL, 1 when it had to force them. Without subreaper
support it refuses to run COMMAND (exit 2), so cleanup can never be called clean
without the boundary. When COMMAND itself exits, FILE.main records its status.
"""

import argparse
import ctypes
import json
import os
from pathlib import Path
import signal
import sys
import time

PR_SET_CHILD_SUBREAPER = 36


def direct_children():
    me = os.getpid()
    path = Path(f"/proc/{me}/task/{me}/children")
    try:
        return [int(pid) for pid in path.read_text().split()]
    except OSError:
        children = []
        for entry in Path("/proc").iterdir():
            if entry.name.isdigit():
                try:
                    fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
                except OSError:
                    continue
                if int(fields[1]) == me:
                    children.append(int(entry.name))
        return children


def write(path, record):
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(record))
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--summary", type=Path, required=True)
    parser.add_argument("--grace", type=float, default=10.0)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("a command is required after --")
    libc = ctypes.CDLL(None, use_errno=True)
    if not sys.platform.startswith("linux") or libc.prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
        write(args.summary, {"subreaper": False, "complete": False, "forced": False})
        return 2
    stop = {"requested": False}

    def request_stop(*_):
        stop["requested"] = True

    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    leader = os.fork()
    if leader == 0:
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        signal.signal(signal.SIGINT, signal.SIG_DFL)
        os.setsid()
        try:
            os.execvp(command[0], command)
        finally:
            os._exit(127)
    leader_reaped, forced, term_at, termed = False, False, None, set()
    while True:
        try:
            pid, status = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            break  # ECHILD: no descendant of this boundary remains.
        if pid:
            if pid == leader:
                leader_reaped = True
                write(Path(str(args.summary) + ".main"), {"status": os.waitstatus_to_exitcode(status)})
            continue
        if stop["requested"]:
            now = time.monotonic()
            if term_at is None:
                term_at = now
                if not leader_reaped:
                    try:  # Safe: the leader is our unreaped child, so its group ID is reserved.
                        os.killpg(leader, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
            escalate = now - term_at >= args.grace
            for child in direct_children():
                if escalate:
                    forced = True
                    sig = signal.SIGKILL
                elif child not in termed:
                    termed.add(child)
                    sig = signal.SIGTERM
                else:
                    continue
                try:  # Our own unreaped children: their PIDs cannot have been reused.
                    os.kill(child, sig)
                except ProcessLookupError:
                    pass
        time.sleep(0.1)
    # Once ECHILD proves the boundary empty, a late controller stop must not
    # terminate Python during finalization after it resets callable handlers.
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    write(args.summary, {"subreaper": True, "complete": True, "forced": forced})
    return 1 if forced else 0


if __name__ == "__main__":
    sys.exit(main())
