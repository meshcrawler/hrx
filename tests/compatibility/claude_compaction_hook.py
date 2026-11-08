"""Fixture-only hook capture. Never writes raw prompts, summaries or paths."""

import argparse
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import stat
import sys
import time

_spec = importlib.util.spec_from_file_location(
    "claude_compaction_receipts", Path(__file__).with_name("claude_compaction_receipts.py"),
)
receipts = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(receipts)


MAX_JOURNAL_BYTES = 8192
MAX_EVENTS = 8


def open_journal(path):
    """Require an already-created private fixture directory and journal."""
    path = Path(path)
    parent = path.parent.lstat()
    receipts.require(stat.S_ISDIR(parent.st_mode) and parent.st_uid == os.getuid()
                     and stat.S_IMODE(parent.st_mode) == 0o700, "journal_parent")
    fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        receipts.require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                         and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600
                         and info.st_size <= MAX_JOURNAL_BYTES, "journal_file")
        return fd
    except BaseException:
        os.close(fd)
        raise


def read_rows(fd):
    os.lseek(fd, 0, os.SEEK_SET)
    raw = os.read(fd, MAX_JOURNAL_BYTES + 1)
    receipts.require(len(raw) <= MAX_JOURNAL_BYTES and (not raw or raw.endswith(b"\n")),
                     "journal_incomplete")
    rows = [receipts.record(line) for line in raw.splitlines()]
    receipts.require(len(rows) <= MAX_EVENTS, "journal_count")
    return rows


def capture(raw, path, *, expected_cwd):
    """Capture a prospective session; controller must bind its digest later."""
    fd = open_journal(path)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        rows = read_rows(fd)
        receipts.require(len(rows) < MAX_EVENTS, "journal_exhausted")
        try:
            value = receipts.record(raw)
            projected = receipts.hook(raw, expected_session=value.get("session_id"),
                                      expected_cwd=expected_cwd)
            entry = {"sequence": len(rows) + 1, "at_ns": time.monotonic_ns(), **projected}
            valid = True
        except ValueError:
            # Persist refusal even when the native hook caller ignores our exit status.
            entry = {"sequence": len(rows) + 1, "error": "invalid_hook"}
            valid = False
        encoded = json.dumps(entry, sort_keys=True).encode() + b"\n"
        receipts.require(os.lseek(fd, 0, os.SEEK_END) + len(encoded) <= MAX_JOURNAL_BYTES,
                         "journal_exhausted")
        receipts.require(os.write(fd, encoded) == len(encoded), "journal_short_write")
        os.fsync(fd)
        return valid
    finally:
        os.close(fd)


def observed_hooks(path, *, expected_session, expected_cwd):
    receipts.session(expected_session, expected_session)
    fd = open_journal(path)
    try:
        fcntl.flock(fd, fcntl.LOCK_SH | fcntl.LOCK_NB)
        rows = read_rows(fd)
    finally:
        os.close(fd)
    receipts.require(len(rows) == 2, "hook_count")
    previous = 0
    for number, (row, event) in enumerate(zip(rows, ("PreCompact", "PostCompact")), start=1):
        receipts.require(set(row) == {"sequence", "at_ns", "event", "trigger",
                                      "session_digest", "cwd_digest"}, "hook_fields")
        receipts.require(type(row["sequence"]) is int and row["sequence"] == number
                         and type(row["at_ns"]) is int and row["at_ns"] > previous,
                         "hook_order")
        receipts.require(row["event"] == event and row["trigger"] == "auto"
                         and row["session_digest"] == receipts.digest(expected_session)
                         and row["cwd_digest"] == receipts.digest(expected_cwd), "hook_binding")
        previous = row["at_ns"]
    return rows


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--journal", required=True)
    parser.add_argument("--cwd", required=True)
    args = parser.parse_args()
    if (os.environ.get("HRX_CLAUDE_ATTENTION_AUTHORIZED") != "1"
            or os.environ.get("CI") != "true" or sys.platform != "linux"):
        return 2
    try:
        raw = sys.stdin.buffer.read(receipts.MAX_RECORD_BYTES + 1)
        return 0 if capture(raw, args.journal, expected_cwd=args.cwd) else 1
    except (OSError, ValueError):
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
