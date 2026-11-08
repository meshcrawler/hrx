"""Bounded PTY driver for the isolated offline matrix worker."""
import errno
import fcntl
import json
import os
import pty
import select
import signal
import struct
import sys
import termios
import time

root = os.environ["HRX_MATRIX_DIR"]
with open(root + "/fixture.json") as source:
    comms = json.load(source)["cell"]["communications"] != "off"

# Do not leave a newly forked child untracked if cancellation arrives at fork.
old_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM, signal.SIGINT})
pid, terminal = pty.fork()
if pid == 0:
    for sig in (signal.SIGINT, signal.SIGQUIT, signal.SIGTSTP, signal.SIGTERM):
        signal.signal(sig, signal.SIG_DFL)
    signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
    os.execv(sys.argv[1], [sys.argv[1], "_matrix-child"])


def cancelled(*_):
    raise SystemExit(124)


signal.signal(signal.SIGTERM, cancelled)
signal.signal(signal.SIGINT, cancelled)
deadline = time.monotonic() + float(os.environ["HRX_MATRIX_TIMEOUT"])
data = b""
done = False
group = None
phase = "start"


def check_time():
    if time.monotonic() >= deadline:
        raise TimeoutError("offline PTY deadline")


def drain():
    if select.select([terminal], [], [], 0)[0]:
        try:
            os.read(terminal, 4096)
        except OSError:
            pass


def read_terminal():
    # Linux reports a PTY whose child side has closed as EIO rather than EOF.
    try:
        return os.read(terminal, 4096)
    except OSError as error:
        if error.errno != errno.EIO:
            raise
        return b""


def child_status():
    try:
        found, status = os.waitpid(pid, os.WNOHANG)
    except ChildProcessError:
        return "reaped"
    return "running" if not found else "status %d" % status


def until(marker):
    global data, phase
    phase = "waiting for %r" % marker
    while marker not in data:
        check_time()
        if select.select([terminal], [], [], .02)[0]:
            chunk = read_terminal()
            assert chunk, ("terminal closed", marker, child_status(), data[-1000:])
            data += chunk
            assert len(data) <= 65536, "output limit"


def wait(stopped=False):
    global phase
    phase = "waiting for worker " + ("stop" if stopped else "exit")
    while True:
        found, status = os.waitpid(pid, os.WNOHANG | (os.WUNTRACED if stopped else 0))
        if found:
            return status
        check_time()
        if not stopped:
            drain()
        time.sleep(.01)


def send(target, sig):
    try:
        os.kill(target, sig)
    except (ProcessLookupError, PermissionError):
        pass


try:
    signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
    until(b"ready")
    group = os.tcgetpgrp(terminal)
    assert group > 0 and group != pid
    os.write(terminal, b"\x1a")
    assert os.WIFSTOPPED(wait(True))
    assert os.tcgetpgrp(terminal) == pid
    assert "release" not in open(root + "/events").read()
    os.kill(pid, signal.SIGCONT)
    until(b"continued")
    assert os.tcgetpgrp(terminal) == group
    fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack("HHHH", 41, 113, 0, 0))
    until(b"size:113x41")
    os.write(terminal, b"input-sentinel\n")
    until(b"input-confirmed")
    os.write(terminal, b"\x03")
    status = wait()
    done = True
    assert os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGINT
    print("offline-probe-ok")
except TimeoutError:
    try:
        foreground = os.tcgetpgrp(terminal)
    except OSError as error:
        foreground = "unavailable (%s)" % error.errno
    # Signal state of the worker and its groups separates "SIGCONT never sent"
    # from "delivered but not handled" (Linux /proc only).
    signals = {}
    for entry in os.listdir("/proc") if os.path.isdir("/proc") else []:
        if not entry.isdigit():
            continue
        try:
            with open("/proc/%s/stat" % entry) as source:
                fields = source.read().rsplit(")", 1)[1].split()
            if int(fields[2]) not in {pid, group} and int(fields[1]) != pid:
                continue
            with open("/proc/%s/status" % entry) as source:
                signals[entry] = {key: value.strip() for key, _, value in
                                  (line.partition(":") for line in source)
                                  if key in ("Name", "State", "SigPnd", "ShdPnd", "SigBlk", "SigIgn", "SigCgt")}
        except (OSError, ValueError, IndexError):
            continue
    # Name the stuck phase; the Go executor reports this on timeout.
    # Diagnostic only, after capturing state: a newline lets a worker blocked in
    # read() return; 'continued' appearing only now means a deferred handler.
    after = b""
    try:
        os.write(terminal, b"\n")
        probe_until = time.monotonic() + 1
        while time.monotonic() < probe_until:
            if select.select([terminal], [], [], .05)[0]:
                chunk = read_terminal()
                if not chunk:
                    break
                after += chunk
    except OSError:
        pass
    sys.stderr.write("offline PTY deadline %s; worker %s; foreground %s (initial %s); recent %r; signals %r; after newline %r\n"
                     % (phase, child_status(), foreground, group, data[-400:], signals, after[-400:]))
    sys.exit(124)
finally:
    # Cancellation must reap the worker and its foreground group, including
    # when it arrives before the harness has announced readiness.
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    if not done:
        try:
            current = os.tcgetpgrp(terminal)
            if current > 0:
                group = current
        except OSError:
            pass
        send(pid, signal.SIGTERM)
        send(pid, signal.SIGCONT)
        cleanup = time.monotonic() + 2
        while time.monotonic() < cleanup:
            try:
                if os.waitpid(pid, os.WNOHANG)[0]:
                    done = True
                    break
            except ChildProcessError:
                done = True
                break
            drain()
            time.sleep(.01)
        if group is not None and group > 0:
            send(-group, signal.SIGKILL)
        if not done:
            send(pid, signal.SIGKILL)
    os.close(terminal)
    if not done:
        # Closing the PTY allows Darwin's terminal-exit path to complete.
        os.waitpid(pid, 0)
