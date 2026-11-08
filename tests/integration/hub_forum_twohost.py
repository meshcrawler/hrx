"""Physical two-host Pitput/HRX exchange against a prepared disposable LXC.

Requires the fixture services, certificates, SSH key, and remote Python venv
specified in docs/hub.md. Runs no paid inference and uses isolated HRX homes.
"""

import argparse
from pathlib import Path
import json, os, subprocess, sys, time, ssl, urllib.request, secrets, signal, shlex, shutil

sys.path.insert(0, str(Path(__file__).resolve().parent))
from pitput_exchange import private, pitput, run

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--fixture-dir", type=Path, required=True)
parser.add_argument("--credential-dir", type=Path, required=True)
parser.add_argument("--hrx", type=Path, required=True)
parser.add_argument("--ssh-jump", required=True, help="SSH host that can reach the container, e.g. root@pve1")
parser.add_argument("--container-address", required=True, help="container IPv4 address reachable from --ssh-jump")
options = parser.parse_args()
p = options.fixture_dir.resolve()
credentials = options.credential_dir.resolve()
for name in ["cert.pem", "admin.token", "viewer.token"]:
    shutil.copyfile(credentials / name, p / name)
    (p / name).chmod(0o600)
hrx = str(options.hrx.resolve())
worker = str(Path(__file__).resolve().with_name("pitput_exchange.py"))
run_key = secrets.token_hex(6)
topic = "/b/exchange/conversation-" + run_key
ssh = [
    "ssh",
    "-o",
    "BatchMode=yes",
    "-o",
    "UserKnownHostsFile=" + str(p / "known_hosts"),
    "-o",
    "StrictHostKeyChecking=yes",
    "-o",
    "ProxyJump=" + options.ssh_jump,
    "-i",
    str(p / "ssh-key"),
    "root@" + options.container_address,
]


def remote(source):
    return subprocess.run(
        ssh + ["/opt/hrx-fixture/venv/bin/python -"],
        input=source,
        text=True,
        capture_output=True,
        check=True,
    )


config = p / "config.toml"
private(
    config,
    '[claude]\nmodel="fixture"\n[claude.provider]\nbase_url="https://127.0.0.1:28793/v1"\nenv_key="HRX_FIXTURE_KEY"\n',
)
bin = p / "bin"
bin.mkdir(exist_ok=True)
private(
    bin / "claude",
    "#!/bin/sh\nexec "
    + shlex.quote(sys.executable)
    + " "
    + shlex.quote(worker)
    + ' --worker "$@"\n',
)
(bin / "claude").chmod(0o700)


def env_for(name):
    home = p / ("retry-" + name)
    home.mkdir(mode=0o700, exist_ok=True)
    private(
        home / ".config/pitput/enrollment.token", (p / "enrollment.token").read_text()
    )
    private(
        home / ".config/pitput/enrollment.toml",
        'url="http://127.0.0.1:28792"\ntoken_file="enrollment.token"\n',
    )
    return dict(
        PATH=str(bin)
        + ":"
        + str(Path(sys.executable).parent)
        + ":"
        + os.environ["PATH"],
        LANG="en_US.UTF-8",
        NO_COLOR="1",
        HOME=str(home),
        XDG_CONFIG_HOME=str(home / ".config"),
        XDG_STATE_HOME=str(p / "state"),
        XDG_CACHE_HOME=str(home / "cache"),
        HRX_CONFIG=str(config),
        HRX_FIXTURE_KEY="fixture-key",
        HRX_INTEGRATION_CA=str(p / "cert.pem"),
    )


collector_env = env_for("collector")
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
    env=collector_env,
)
from datetime import datetime, timezone

started_at = datetime.now(timezone.utc)
children = []


def spawn(args, env, name, cwd=None):
    log = open(p / (name + ".log"), "w")
    c = subprocess.Popen(
        args, env=env, cwd=cwd, stdout=log, stderr=log, start_new_session=True
    )
    log.close()
    children.append(c)
    return c


setup_remote = """from pathlib import Path
import os,subprocess,time
p=Path('/opt/hrx-fixture'); home=p/'forum-beta-retry';home.mkdir(mode=0o700,exist_ok=True)
b=p/'forum-bin';b.mkdir(mode=0o700,exist_ok=True)
(b/'claude').write_text('#!/bin/sh\\nexec /opt/hrx-fixture/venv/bin/python /opt/hrx-fixture/pitput_exchange.py --worker "$@"\\n');(b/'claude').chmod(0o700)
config=p/'forum-config.toml';config.write_text('[claude]\\nmodel="fixture"\\n[claude.provider]\\nbase_url="https://127.0.0.1:8793/v1"\\nenv_key="HRX_FIXTURE_KEY"\\n');config.chmod(0o600)
c=home/'.config/pitput';c.mkdir(parents=True,mode=0o700,exist_ok=True)
(c/'enrollment.token').write_text((p/'forum-enrollment.token').read_text());(c/'enrollment.token').chmod(0o600)
(c/'enrollment.toml').write_text('url="http://127.0.0.1:8792"\\ntoken_file="enrollment.token"\\n');(c/'enrollment.toml').chmod(0o600)
e=dict(PATH=str(b)+':/opt/hrx-fixture/venv/bin:/usr/local/bin:/usr/bin:/bin',HOME=str(home),XDG_CONFIG_HOME=str(home/'.config'),XDG_STATE_HOME=str(p/'forum-state'),XDG_CACHE_HOME=str(home/'cache'),HRX_CONFIG=str(config),HRX_FIXTURE_KEY='fixture-key',SSL_CERT_FILE=str(p/'cert.pem'),TEST_IDENTITY='test/beta/agent',TEST_PEER='test/gamma/agent',TEST_STAGE='1')
subprocess.run([str(p/'hrx'),'hub','enroll','--url','https://127.0.0.1:8790','--admin-token-file',str(p/'admin.token'),'--token-file',str(p/'forum-host.token'),'--prefix','test/'],env=e,check=True)
for name,args in [('collector',[str(p/'hrx'),'hub','collect']),('beta',[str(p/'hrx'),'claude','--agent-identity','test/beta/agent','--pitput','--','-p','exchange fixture'])]:
 if name=='beta':time.sleep(2)
 with (p/('forum-'+name+'.log')).open('w') as log:child=subprocess.Popen(args,env=e,cwd=home,stdout=log,stderr=log,start_new_session=True)
 (p/('forum-'+name+'.pid')).write_text(str(child.pid))
"""
try:
    spawn([hrx, "hub", "collect"], collector_env, "collector")
    time.sleep(2)
    controller = env_for("controller")
    pitput(controller, "join", "exchange", "--identity", "test/controller/agent")
    nonce = secrets.token_hex(12)
    pitput(
        controller,
        "--agent",
        "test/controller/agent",
        "create",
        topic,
        "--author",
        "test/controller/agent",
        "--title",
        "Physical two-host HRX exchange",
        "--body",
        "@test/alpha/agent " + json.dumps({"nonce": nonce, "chain": []}),
        "--request-id",
        "seed-" + run_key,
    )
    print(
        remote(
            setup_remote.replace(
                "TEST_STAGE='1')",
                "TEST_STAGE='1',TEST_RUN_KEY="
                + repr(run_key)
                + ",TEST_TOPIC="
                + repr(topic)
                + ")",
            )
        ).stdout.strip(),
        flush=True,
    )
    runs = []
    for stage, name, peer in [(0, "alpha", "beta"), (2, "gamma", "alpha")]:
        env = env_for(name)
        env.update(
            TEST_IDENTITY="test/" + name + "/agent",
            TEST_PEER="test/" + peer + "/agent",
            TEST_STAGE=str(stage),
            TEST_RUN_KEY=run_key,
            TEST_TOPIC=topic,
        )
        runs.append(
            (
                spawn(
                    [
                        hrx,
                        "claude",
                        "--agent-identity",
                        "test/" + name + "/agent",
                        "--pitput",
                        "--",
                        "-p",
                        "exchange fixture",
                    ],
                    env,
                    name,
                    cwd=env["HOME"],
                ),
                name,
            )
        )
    for child, name in runs:
        child.wait(timeout=80)
        assert child.returncode == 0, (p / (name + ".log")).read_text()[-2000:]
        report = json.loads((p / (name + ".log")).read_text().splitlines()[-1])
        assert report["identity"] == "test/" + name + "/agent"
    messages = pitput(controller, "--agent", "test/controller/agent", "read", topic)
    assert len(messages) == 4
    assert [m["author"] for m in messages[1:]] == [
        "test/alpha/agent",
        "test/beta/agent",
        "test/gamma/agent",
    ]
    assert json.loads(messages[-1]["body"].split(" ", 1)[1])["nonce"] == nonce
    print(
        "PASS: real Pitput messages and ACKs crossed this host -> LXC -> this host without duplicate replies",
        flush=True,
    )
    deadline = time.monotonic() + 20
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
        ) as response:
            rows = [
                r
                for r in json.load(response)["agents"]
                if r["agent"] in {"test/alpha/agent", "test/beta/agent", "test/gamma/agent"}
                and datetime.fromisoformat(r["started_at"].replace("Z", "+00:00"))
                >= started_at
            ]
        if len(rows) == 3 and all(r["status"] == "exited" for r in rows):
            break
        time.sleep(0.5)
    else:
        raise AssertionError("missing final hub exits")
    assert len({r["host_id"] for r in rows}) == 2
    assert all(r["session_id"].startswith("fixture-") for r in rows)
    (p / "evidence.json").write_text(
        json.dumps({"agents": rows, "message_count": len(messages)}, indent=2)
    )
    print(
        "PASS: three exited HRX runs with captured sessions and exactly two physical host IDs in shared hub",
        flush=True,
    )
finally:
    for child in reversed(children):
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
    remote("""from pathlib import Path
import os,signal
p=Path('/opt/hrx-fixture')
for name in ['beta','collector']:
 try:os.kill(int((p/('forum-'+name+'.pid')).read_text()),signal.SIGTERM)
 except (FileNotFoundError,ProcessLookupError):pass
""")
