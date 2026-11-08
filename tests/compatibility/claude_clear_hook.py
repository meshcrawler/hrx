"""Runner-only projection of a native clear event; never stores transcript content."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys
import uuid


def live_start(stat):
    # comm can contain spaces and parentheses; fields after its final ')' start
    # with state (field 3), so starttime (field 22) is at offset 19.
    fields = stat.rsplit(")", 1)[1].split()
    return None if fields[0] in {"Z", "X"} else int(fields[19])


def process_start(pid):
    try:
        return live_start(Path(f"/proc/{int(pid)}/stat").read_text())
    except FileNotFoundError:
        return None


def project(raw, cwd):
    if len(raw) > 65536:
        raise ValueError("oversized_clear_event")
    event = json.loads(raw)
    if (event.get("hook_event_name") != "SessionStart" or event.get("source") != "clear"
            or event.get("cwd") != cwd):
        raise ValueError("wrong_clear_event")
    session = event["session_id"]
    if str(uuid.UUID(session)) != session:
        raise ValueError("invalid_clear_session")
    return {"source": "clear", "session_digest": hashlib.sha256(session.encode()).hexdigest()}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cwd", required=True)
    args = parser.parse_args()
    if os.environ.get("HRX_CLAUDE_ATTENTION_AUTHORIZED") != "1" or sys.platform != "linux":
        return 2
    try:
        result = project(sys.stdin.buffer.read(65537), args.cwd)
        temporary = args.output.with_suffix(".tmp")
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as stream:
            json.dump(result, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, args.output)
        return 0
    except (ValueError, KeyError, TypeError, OSError):
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
