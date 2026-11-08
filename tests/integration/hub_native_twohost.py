"""Native Codex notification/ACK/exit/resume against the prepared remote fixture.

Run hub_forum_twohost.py first; use the same tunnel and credential directory.
The test binary must be built from ./internal/app with go test -c.
"""

import argparse
from pathlib import Path
import os, subprocess, sys, time, shlex, secrets, json, ssl, urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parent))
from pitput_exchange import private, pitput, run

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--fixture-dir", type=Path, required=True)
parser.add_argument("--hrx", type=Path, required=True)
parser.add_argument("--test-binary", type=Path, required=True)
options = parser.parse_args()
p = options.fixture_dir.resolve()
identity = "test/codex-" + secrets.token_hex(6) + "/agent"
home = p / ("native-" + identity.split("/")[1])
home.mkdir(mode=0o700, exist_ok=True)
private(home / ".config/pitput/enrollment.token", (p / "enrollment.token").read_text())
private(
    home / ".config/pitput/enrollment.toml",
    'url="http://127.0.0.1:28792"\ntoken_file="enrollment.token"\n',
)
private(
    home / "bin/pitput",
    "#!/bin/sh\nexec " + shlex.quote(sys.executable) + ' -m ysg_mail.pitput "$@"\n',
)
(home / "bin/pitput").chmod(0o700)
config = home / "hrx.toml"
private(config, "")
env = dict(
    PATH=str(home / "bin") + ":" + os.environ["PATH"],
    HOME=str(home),
    XDG_CONFIG_HOME=str(home / ".config"),
    XDG_STATE_HOME=str(home / "state"),
    XDG_CACHE_HOME=str(home / "cache"),
    HRX_CONFIG=str(config),
    HRX_INTEGRATION_CA=str(p / "cert.pem"),
    HRX_NATIVE_SESSION_TESTS="1",
    HRX_PITPUT_INTEGRATION="1",
    HRX_PITPUT_TUI="1",
    HRX_HUB_NATIVE_TEST="1",
    HRX_PITPUT_TEST_IDENTITY=identity,
)
hrx = str(options.hrx.resolve())
run(
    [
        hrx,
        "hub",
        "enroll",
        "--url",
        "https://127.0.0.1:28790",
        "--admin-token-file",
        str(p / "admin.token"),
        "--prefix",
        "test/",
    ],
    env=env,
)
controller = dict(
    env,
    HOME=str(p / "retry-controller"),
    XDG_CONFIG_HOME=str(p / "retry-controller/.config"),
)
topic = "/b/exchange/codex-native-" + secrets.token_hex(6)
pitput(
    controller,
    "--agent",
    "test/controller/agent",
    "create",
    topic,
    "--author",
    "test/controller/agent",
    "--title",
    "Physical native Codex bridge",
    "--body",
    "@" + identity + " isolated notification fixture",
    "--request-id",
    "native-seed-" + identity.split("/")[1],
)
pitput(
    controller,
    "--agent",
    "test/controller/agent",
    "post",
    topic,
    "--author",
    "test/controller/agent",
    "--body",
    "@" + identity + " second notification after handling ACK",
    "--request-id",
    "native-next-" + identity.split("/")[1],
)
with (home / "collector.log").open("w") as log:
    collector = subprocess.Popen(
        [hrx, "hub", "collect"], env=env, stdout=log, stderr=log
    )
try:
    deadline = time.monotonic() + 10
    while not (home / "state/hrx/hub/outbox.json").exists():
        assert time.monotonic() < deadline and collector.poll() is None
        time.sleep(0.1)
    result = run(
        [
            str(options.test_binary.resolve()),
            "-test.run=^TestNativePitputPrivateServerQueue$",
            "-test.timeout=90s",
            "-test.v",
        ],
        env=env,
        timeout=100,
        check=False,
    )
    print(result.stdout, flush=True)
    assert result.returncode == 0, "native fixture failed; see test output"
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        req = urllib.request.Request(
            "https://127.0.0.1:28790/api/v1/agents",
            headers={
                "Authorization": "Bearer " + (p / "viewer.token").read_text().strip()
            },
        )
        with urllib.request.urlopen(
            req,
            context=ssl.create_default_context(cafile=str(p / "cert.pem")),
            timeout=5,
        ) as r:
            rows = [x for x in json.load(r)["agents"] if x["agent"] == identity]
        if len(rows) == 2 and all(x["status"] == "exited" for x in rows):
            break
        time.sleep(0.5)
    else:
        raise AssertionError("native exits absent from remote hub")
    sessions = {x.get("session_id") for x in rows}
    assert len(sessions) == 1 and None not in sessions and "" not in sessions, rows
    (p / "native-evidence.json").write_text(json.dumps(rows, indent=2))
    print(
        "PASS: physical remote Pitput + hub, native Codex notification, pending preservation, ACK, exit and same-thread resume",
        flush=True,
    )
finally:
    collector.terminate()
    collector.wait(timeout=10)
