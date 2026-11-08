"""Identify fixture processes across PID reuse and guest restarts."""

import json
import os
from pathlib import Path


def identity(pid):
    stat = Path(f"/proc/{pid}/stat")
    fields = stat.read_text().rsplit(")", 1)[1].split()
    init = Path("/proc/1/stat").read_text().rsplit(")", 1)[1].split()
    return {"pid": pid, "start": int(fields[19]), "group": int(fields[2]),
            "session": int(fields[3]), "init_start": int(init[19]),
            "uid": stat.stat().st_uid}, fields[0]


def save(path, pid):
    record, _ = identity(pid)
    if record["uid"] != os.getuid() or record["group"] != pid or record["session"] != pid:
        raise RuntimeError("fixture process is not an owned session leader")
    path.write_text(json.dumps(record))


def matching(path):
    record = json.loads(path.read_text())
    keys = {"pid", "start", "group", "session", "init_start", "uid"}
    if (not isinstance(record, dict) or set(record) != keys
            or any(type(value) is not int for value in record.values())
            or record["pid"] <= 1 or record["uid"] != os.getuid()
            or record["group"] != record["pid"] or record["session"] != record["pid"]):
        raise RuntimeError("invalid fixture process identity")
    try:
        actual, state = identity(record["pid"])
    except (FileNotFoundError, ProcessLookupError):
        return None
    return record if actual == record and state != "Z" else None


def signal_owned(path, sig):
    record = matching(path)
    if record is None:
        return False
    try:
        os.killpg(record["group"], sig)
    except ProcessLookupError:
        return False
    return True
