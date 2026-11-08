"""Private GitLab TUI driver, launched inside the Linux subreaper boundary.

Discard terminal contents; answer cursor-position queries only. No automatic
trust confirmations or permission overrides. The outer test verifies the prompt
at its loopback provider and the observer's correlated turn events.
"""

import errno
import fcntl
import json
import re
import os
import pty
import select
import signal
import struct
import sys
import termios


def main():
    if not sys.platform.startswith("linux") or len(sys.argv) < 2:
        return 2
    pid, master = pty.fork()
    if pid == 0:
        os.execvp(sys.argv[1], sys.argv[1:])
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))

    def stop(*_):
        # The enclosing subreaper owns/reaps the TUI after this driver exits.
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    tail = b""
    diagnostic = os.environ.get("HRX_TUI_DIAGNOSTIC")
    diagnostic_tail = b""
    try:
        while True:
            child, status = os.waitpid(pid, os.WNOHANG)
            if child:
                return os.waitstatus_to_exitcode(status)
            if not select.select([master], [], [], 0.1)[0]:
                continue
            try:
                chunk = os.read(master, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                _, status = os.waitpid(pid, 0)
                return os.waitstatus_to_exitcode(status)
            if not chunk:
                _, status = os.waitpid(pid, 0)
                return os.waitstatus_to_exitcode(status)
            if diagnostic:
                diagnostic_tail = (diagnostic_tail + chunk)[-8192:]
            data = tail + chunk
            queries = data.count(b"\x1b[6n")
            if queries:
                os.write(master, b"\x1b[1;1R" * queries)
            tail = data[-3:]
    finally:
        os.close(master)
        # Opt-in only for isolated no-input native fixtures. Never capture the
        # default driver's terminal stream or persist an unbounded transcript.
        if diagnostic:
            text = diagnostic_tail.decode("utf-8", errors="replace")
            text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)
            lines = [line.strip() for line in text.splitlines()
                     if any(word in line.lower() for word in ("error", "invalid", "failed", "unexpected", "usage:"))]
            lines = [re.sub(r"(?i)bearer\s+[^\s]+", "Bearer [redacted]", line)[:1024] for line in lines[-4:]]
            fd = os.open(diagnostic, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w") as output:
                json.dump({"error_lines": lines, "captured_bytes": len(diagnostic_tail)}, output)


if __name__ == "__main__":
    sys.exit(main())
