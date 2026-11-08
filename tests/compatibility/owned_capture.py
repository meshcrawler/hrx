"""Capture a native command without losing ownership when its leader exits."""
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from conversation import SUPERVISOR, read_summary, running, stop_supervised
import ownership


def capture(command, *, env, cwd, deadline, abort, grace=10, capture_stderr=False):
    if ownership.available(sys.platform).mechanism != ownership.LINUX_SUBREAPER:
        raise RuntimeError("linux_subreaper_required")
    # Files cannot deadlock on an orphan retaining an inherited pipe writer.
    with tempfile.TemporaryDirectory(prefix="hrx-capture-") as temp, tempfile.TemporaryFile() as output, \
            tempfile.TemporaryFile() as errors:
        summary = Path(temp) / "supervisor.json"
        process = subprocess.Popen(
            [sys.executable, "-I", str(SUPERVISOR), "--summary", str(summary),
             "--grace", str(grace), "--", *command],
            cwd=cwd, env=env, stdout=output, stderr=errors if capture_stderr else subprocess.DEVNULL,
            stdin=subprocess.DEVNULL, start_new_session=True)
        timed_out = False
        try:
            while running(process):
                if read_summary(str(summary) + ".main") is not None:
                    break
                if time.monotonic() >= deadline:
                    timed_out = True
                    break
                if abort():
                    break
                time.sleep(.05)
        finally:
            cleanup = stop_supervised(process, summary, {"signals": []}, grace=grace)
        main = read_summary(str(summary) + ".main")
        output.seek(0)
        errors.seek(max(0, errors.tell() - 65536))
        return {"stdout": output.read(), "exit_code": main.get("status") if main else None,
                **({"stderr": errors.read()} if capture_stderr else {}),
                "timed_out": timed_out, "cleanup": cleanup,
                "supervisor_exit": process.returncode, "supervisor_summary": read_summary(summary)}
