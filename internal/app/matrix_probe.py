"""Offline-only fake harness: no network access or real credentials are used."""
import json
import os
import select
import signal
import sys

root = os.environ["HRX_MATRIX_DIR"]
with open(root + "/fixture.json") as source:
    fixture = json.load(source)
cell = fixture["cell"]
args = sys.argv[1:]
with open(root + "/argv.json") as source:
    assert args == json.load(source)


def option(name):
    return args[args.index(name) + 1]


harness = cell["harness"]
route = cell["route"]
reasoning = fixture["reasoning"]
prompt = ""
permission = {"codex": "--yolo", "claude": "--dangerously-skip-permissions", "opencode": "--auto"}[harness]
assert (permission in args) == (cell["permissions"] == "yolo")
if cell["session"] != "fresh":
    assert "offline-session" in args
    assert "--continue" not in args and "--last" not in args

if harness == "codex":
    assert 'model_provider="fixture"' in args
    assert 'model_providers.fixture={name="hrx LiteLLM",base_url="http://127.0.0.1:1/v1",wire_api="responses",env_key="HRX_OFFLINE_KEY"}' in args
    assert os.environ["HRX_OFFLINE_KEY"] == "offline-key-sentinel"
    assert option("--model") == route
    assert "model_reasoning_effort=" + json.dumps(reasoning) in args
    catalogs = [arg.split("=", 1)[1] for arg in args if arg.startswith("model_catalog_json=")]
    with open(json.loads(catalogs[-1])) as source:
        prompt = json.load(source)["models"][0]["base_instructions"]
elif harness == "claude":
    assert option("--model") == route
    if reasoning:
        assert option("--effort") == reasoning
    assert os.environ["ANTHROPIC_MODEL"] == route
    assert os.environ["ANTHROPIC_BASE_URL"] == "http://127.0.0.1:1"
    assert os.environ["ANTHROPIC_CUSTOM_HEADERS"] == "x-litellm-api-key: Bearer offline-key-sentinel"
    assert os.environ.get("ANTHROPIC_AUTH_TOKEN") == "offline-key-sentinel" and not os.environ.get("ANTHROPIC_API_KEY")
    if "--append-system-prompt" in args:
        prompt = option("--append-system-prompt")
else:
    config = json.loads(os.environ["OPENCODE_CONFIG_CONTENT"])
    assert config["model"] == "hrx/" + route
    if cell["execution"] == "one-shot":
        assert option("--model") == config["model"]
    assert os.environ["HRX_GATEWAY_KEY"] == "offline-key-sentinel"
    provider = config["provider"]["hrx"]
    assert provider["options"]["baseURL"] == "http://127.0.0.1:1/v1"
    if reasoning:
        assert provider["models"][route]["options"]["reasoningEffort"] == reasoning
    if "--file" in args:
        with open(option("--file")) as source:
            prompt = source.read()
    elif "--prompt" in args:
        prompt = option("--prompt")

if cell["communications"] != "off":
    for name in ("YESOD_AGENT_IDENTITY", "YESOD_MAIL_IDENTITY", "YESOD_MAIL_SELF"):
        assert os.environ[name] == fixture["agent"]
    assert fixture["agent"] in prompt
    assert ("### Yesod mail" in prompt) == (cell["communications"] in ("mail", "both"))
    assert ("### Yesod bulletin" in prompt) == (cell["communications"] in ("bulletin", "both"))
else:
    assert "### Yesod mail" not in prompt and "### Yesod bulletin" not in prompt

with open(root + "/events", "a") as target:
    target.write("launch\n")
if cell["execution"] == "interactive":
    signal.signal(signal.SIGINT, signal.SIG_DFL)
    signal.signal(signal.SIGTSTP, signal.SIG_DFL)
    # Handlers write unbuffered: a print here can re-enter a stdout flush that the
    # stop interrupted and kill the fixture ("reentrant call inside BufferedWriter").
    signal.signal(signal.SIGCONT, lambda *_: os.write(1, b"continued\n"))
    signal.signal(signal.SIGWINCH, lambda *_: os.write(1, b"size:%dx%d\n" % tuple(os.get_terminal_size())))
    assert os.isatty(0) and os.isatty(1) and os.isatty(2)
    print("ready", flush=True)
    # Never block in read() before input is ready: a blocking read gives CPython no
    # checkpoint, so a SIGCONT landing just before it would defer the 'continued'
    # handler until input arrives. Short select polls keep handlers prompt.
    while not select.select([0], [], [], 0.05)[0]:
        pass
    assert sys.stdin.readline().strip() == "input-sentinel"
    print("input-confirmed", flush=True)
    while True:
        signal.pause()
print("offline-probe-ok")
sys.exit(37)
