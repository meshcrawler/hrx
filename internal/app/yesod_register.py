"""Read-only identity selection; the Go supervisor atomically claims the result."""

import argparse
import json
import os
import re
import socket
import subprocess
import sys
import uuid


def command(args, env):
    result = subprocess.run(
        ["yesod", "mail", *args], env=env, stdin=subprocess.DEVNULL,
        capture_output=True, text=True, timeout=20,
    )
    if result.returncode:
        raise RuntimeError("Yesod command failed")
    return result.stdout


def allocate(harness, model, agent=""):
    env = os.environ.copy()
    for key in ("YESOD_AGENT_IDENTITY", "YESOD_MAIL_IDENTITY", "YESOD_MAIL_SELF"):
        env.pop(key, None)
    env.update(NO_COLOR="1", TERM="dumb", COLUMNS="500")
    rows = json.loads(command(["roster", "--json"], env))
    if not isinstance(rows, list) or any(not isinstance(r, dict) or not isinstance(r.get("addr"), str) for r in rows):
        raise RuntimeError("Invalid Yesod roster")
    occupied = {row["addr"] for row in rows}
    if agent:
        if not re.fullmatch(r"[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+", agent):
            raise RuntimeError("Invalid address")
    else:
        slug = lambda s: re.sub(r"[^a-z0-9-]+", "-", s.lower()).strip("-")[:40] or "agent"
        host = slug(socket.gethostname().split(".")[0])
        project = slug(os.path.basename(os.getcwd()))
        for _ in range(8):
            agent = f"{host}/hrx-{harness}/{project}-{uuid.uuid4().hex}"
            if agent not in occupied:
                break
        else:
            raise RuntimeError("Cannot allocate unused address")
    # The roster is a naming hint, never ownership evidence. Claim happens in
    # the launcher with a private per-run token and rejects active conflicts.
    return agent


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--harness", choices=("codex", "claude", "opencode"), required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--agent", default="")
    args = parser.parse_args()
    try:
        print(json.dumps({"agent": allocate(args.harness, args.model, args.agent)}))
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
        # Database/helper diagnostics can contain credentials; never relay them.
        print("Yesod registration failed; check yesod mail roster and the selected identity.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
