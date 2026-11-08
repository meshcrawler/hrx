# hrx

hrx is a launcher for AI coding agents. It starts **Codex**, **Claude Code** or
**OpenCode** against one LiteLLM-style model gateway, checks that the exact
model route you asked for exists, is authorized for your key and is declared
compatible with that harness, and then replaces itself with the unmodified
harness. One model catalog, exact routes, fast completion, no new credentials.

Around that core it adds what you need once agents stop being a single
conversation:

- [Launch profiles](docs/profiles.md) that pin harness, exact route, reasoning
  and a private credential reference.
- Agent identities, Pitput message-board delivery and optional
  [GitLab service accounts](docs/gitlab-service-account-broker.md).
- [Proxmox workers and mixed agent networks](docs/proxmox.md):
  `hrx run --backend=proxmox` and `hrx network --file network.toml` provision
  isolated LXCs, install pinned runtimes, and verify real Pitput activity
  before releasing work. Local execution remains the default.
- [Natural-language launches](docs/auto.md):
  `hrx --auto "claude as host/role/agent using pitput"` proposes arguments and
  asks yes, no or reprompt before launching.
- A live [agent dashboard](docs/dashboard.md), an optional multi-host
  [hub](docs/hub.md), and `hrx sweep` to clean up what launches left behind.

## Quick start

Requirements: Go 1.26+, at least one harness, and an HTTPS gateway that serves
the OpenAI-compatible `/v1/models` endpoint plus the hrx model catalog at
`/dashboard/models.json`.

```sh
make test
make install          # ~/.local/bin/hrx and zsh completion
hrx refresh
hrx doctor
hrx models
hrx codex --model gpt-6.1-sol --reasoning high --dry-run
hrx codex --model gpt-6.1-sol --reasoning high
hrx codex --model gpt-6.1-sol --reasoning high -- exec 'Explain this repo'
hrx claude --model claude-sonnet-5
hrx opencode --model glm-5p3
```

Codex and OpenCode reuse the gateway provider already in their own
configuration. Claude Code has no built-in gateway: configure `[claude]` and
`[claude.provider]` in `~/.config/hrx/config.toml` before running `hrx claude`
(see [Claude Code and OpenCode](#claude-code-and-opencode)).

Friendly names resolve to exact authorized routes. Public aliases stay intact,
preserving LiteLLM failover. Dry-run validates and prints routing without starting
the harness or exposing credential-helper arguments. Defaults come from the
harness configuration (model) and the gateway capability policy (reasoning).

Public commands print the HRX version to stderr, leaving JSON and other stdout
output intact. `hrx --version` prints it once to stdout. Builds made with
`make build` include the Git revision and dirty-worktree marker. Internal
completion, credential, and session hooks stay quiet.

## Examples

Run these from the project you want the harness to work on. Commands that
launch a harness use real provider quota; listings and `--dry-run` do not send
an inference request. Discovery and dry-run can still contact the gateway and
run your existing credential helper. Availability changes, so use `hrx models`
for your current choices rather than treating the examples as a fixed list.

### Start Codex with Fireworks GLM 5.3

```sh
# Refresh the local list used by completion.
hrx refresh

# Start an interactive coding session.
hrx codex --model glm-5p3 --reasoning low

# Equivalent, but explicitly choose the Fireworks/acme route.
hrx codex --model fw-acme-glm-5p3 --reasoning low

# Inspect the resolved provider and route without starting Codex.
hrx codex --model glm-5p3 --reasoning low --dry-run
```

This uses your existing LiteLLM Codex key, not an OpenAI-hosted GLM model.
No new credential or `--oss` flag is needed. Only `low` is currently advertised
for this GLM route; other GLM accounts/variants need their own compatibility
verification. Leaving off `--reasoning` selects the gateway's default:

```sh
hrx codex --model glm-5p3
```

### Find something to try

Inspect available models and the exact routes your key permits:

```sh
hrx models --refresh
hrx models --json
hrx models --harness claude --refresh
hrx models --harness opencode --refresh
```

`models` lists launchable choices for one harness; `catalog` includes discovered
models that you cannot currently launch, with the reason why:

```sh
# Refresh discovery/authorization and show one row per model identity.
hrx catalog --refresh --compact

# Search across providers, keeping only the latest generations.
hrx catalog --compact --latest --search gpt-

# Inspect GLM's individual routes and their Codex status.
hrx catalog --harness codex --search glm

# See which accounts offer Codex-named models in a compact view.
hrx catalog --compact --search codex

# Export a credential-free catalog snapshot with detailed source evidence.
hrx catalog --output model-catalog.json
```

Searching for `codex` matches catalog text; it does not mean "all models usable
in Codex." Use `--harness codex` to select that harness's status view, or
`hrx models` to see only its launchable choices. `--latest` is optional: omit it
to include superseded generations. Neither flag changes your keys.


### Choose a model and reasoning level

Check the resolved model before starting a session:

```sh
hrx codex --model gpt-6.1-sol --reasoning high --dry-run
```


Start an interactive session, or pass a task to Codex's exec subcommand:

```sh
hrx codex --model gpt-6-astra --reasoning xhigh
hrx codex --model gpt-6.1-sol --reasoning high -- exec 'Explain the architecture without modifying files'
```

Friendly identities keep commands short. An exact route chooses a particular
provider/account when the identity has multiple usable routes:

```sh
hrx codex --model gpt-6.1-sol --reasoning high --dry-run
hrx codex --model tfy-contoso/gpt-6.1-sol --reasoning high --dry-run
```

An ambiguous identity produces an error rather than silently choosing a payer.
Use the route shown by `hrx models`. Public aliases such as `gpt-6-astra` retain
their gateway-managed failover policy.

### One-shot tasks and read-only exploration

Put hrx's model/reasoning options before `--`, then the harness's own arguments:

```sh
# Explore the current repository with GLM, using Codex's read-only sandbox.
hrx codex --model glm-5p3 --reasoning low -- exec --sandbox read-only \
  'Explain the entry points and test layout. Do not change files.'

# Ask for a focused review rather than implementation.
hrx codex --model gpt-6.1-sol --reasoning high -- exec --sandbox read-only \
  'Review the working-tree diff for correctness bugs. Cite files and lines.'

# Request an implementation using your normal Codex permission settings.
hrx codex --model glm-5p3 --reasoning low -- exec \
  'Add regression tests for the bug described in this project. Run focused tests.'
```

hrx does not grant additional filesystem or command permissions. The first two
examples explicitly request a read-only sandbox; the implementation example
uses your normal Codex settings and may edit files. Model/provider/config
overrides after `--` are rejected: select the model through hrx instead.

### Try another harness

```sh
# Claude Code: inspect choices, validate routing, then start a session.
hrx refresh --harness claude
hrx claude --model claude-sonnet-5 --dry-run
hrx claude --model claude-sonnet-5

# OpenCode: use the same GLM identity through its own compatible API path.
hrx refresh --harness opencode
hrx opencode --model glm-5p3 --dry-run
hrx opencode --model glm-5p3

# A one-shot OpenCode task, excluding external plugins for this invocation.
hrx opencode --model glm-5p3 -- run --pure \
  'Explain the project structure without changing files.'
```

Each harness uses its own existing credentials and capability check. A model
being ready in OpenCode does not automatically make it ready in Codex or Claude.
These Claude/OpenCode examples use provider-default reasoning; do not copy
Codex effort flags into them without checking the advertised choices.

### Pick interactively with Tab

In the following examples, press the Tab key where `<Tab>` appears; do not type
the literal angle brackets into your shell:

```text
hrx codex --model <Tab>
hrx codex --model glm-5p3 --reasoning <Tab>
hrx codex --latest --model <Tab>
hrx claude --model <Tab>
hrx opencode --latest --model <Tab>
```

Type `glm` or `sol` into the fuzzy picker to narrow the choices. The reasoning
picker follows the selected model, so GLM currently offers only `low`.
Completion reads local cache only; refresh the relevant harness if it is empty.
See [Zsh and fzf-tab](#zsh-and-fzf-tab) for installation.

### Use a different existing Codex configuration

Use another existing Codex home without changing your default:

```sh
CODEX_HOME="$HOME/.codex-work" hrx refresh
CODEX_HOME="$HOME/.codex-work" hrx codex --model gpt-6.1-sol --reasoning high
```

### Troubleshoot a missing model

```sh
# Diagnose the selected harness/model without starting an agent or inference.
hrx doctor --harness codex --model glm-5p3 --reasoning low

# Refresh the correct harness, not just the default Codex cache.
hrx refresh --harness opencode
hrx models --harness opencode

# Investigate the route-level reason a GLM model is missing from Codex.
hrx catalog --refresh --harness codex --search glm

# Inspect old cached choices during an outage; this does not permit a launch.
hrx models --stale
```

| Status or symptom | Meaning / next step |
| --- | --- |
| `not-authorized` | This harness's key does not permit the route; refresh cannot widen it. |
| `compatibility-unverified` | Access alone is insufficient; the gateway has not declared this harness compatible. |
| `no-gateway-route` | Provider discovery found the model, but there is no matching gateway route. |
| `unavailable` | The shared catalog marks the model or source unavailable. |
| Empty Tab menu | Refresh that harness; completion hides stale or missing cache data. |
| Unsupported reasoning | Choose an advertised level; GLM currently accepts only `low` through hrx. |
| HTTP 429 after launch | A gateway/provider limit was hit; `ready` does not guarantee capacity. |

### Read-only health checks

```sh
# Check all installed harnesses; missing optional harnesses are warnings.
hrx doctor

# Check a particular harness and its default model.
hrx doctor --harness claude

# Check communications prerequisites without allocating an agent name.
hrx doctor --harness codex --yesod-comms

# Machine-readable output and a smaller total time budget.
hrx doctor --harness opencode --json --timeout 20
```

Doctor checks executable discovery/version, configuration, cache schema and
freshness, credential resolution, gateway connectivity, live key authorization,
and selected/default route compatibility. Codex uses the same template-validation
code as launch, without writing a generated catalog. `--model` and `--reasoning` require a
specific `--harness`; an ambiguous identity needs an exact route. Without a
default model, doctor reports the choice count and asks you to select one.
Claude routing overrides are checked too. Yesod flags additionally check
`yesod mail roster --json` and Python 3 availability; no registration or heartbeat is sent.

Doctor performs **no inference** and has no paid-probe mode. It runs installed
harnesses with `--version`, reads the configured credential handle in memory,
and makes GET requests to the catalog and `/v1/models`. It does not write the
catalog cache, edit configuration, rotate keys, or launch an agent. Helpers
remain trusted user-configured programs. Subprocess output, HTTP error bodies,
credential handles and values are not relayed into diagnostics.

The default total deadline is 45 seconds (`--timeout 1..60`); HTTP and version/
registry subprocesses are capped at 5 seconds each, and credential helpers at
their existing bounded timeout. Process cleanup can take an additional second.

Exit codes: **0** means no failed checks (warnings can remain), **1** means a
failed check, and **2** means invalid doctor arguments. JSON has `schema_version`,
`checked_at`, `ok`, and `checks`; each check has a harness, name, status
(`ok`, `warn`, `fail`), state, detail, and optional remedy. A failure also prints
a generic summary to stderr, leaving stdout valid JSON.

Example diagnostics (illustrative, not live-test evidence):

```text
FAIL codex     authorization  not-authorized: Gateway rejected this key (HTTP 401/403).
     Fix: Check the existing key expiry, budget and authorization; do not rotate automatically.
WARN codex     cache          stale: Completion cache is stale; launch will revalidate.
     Fix: Run hrx refresh --harness codex
WARN codex     verification   not-tested: No recent dated end-to-end harness test is recorded; authorization is not verification.
```

An accepted key is not proof that inference will succeed. Budget, concurrency,
provider uptime, and provider-side limits can still prevent a request. Doctor
does not infer those limits from a successful `/v1/models` response. A stale or
missing cache is a warning because live checks can still succeed; doctor does
not silently refresh it. Missing explicitly selected harnesses are failures.

With a communications flag, doctor also checks the read-only help contracts for
`yesod mail claim`, `renew`, and `release` (`--as` and `--token-stdin`). A readable
roster alone is insufficient: an older Yesod installation fails `yesod-leases`.
These checks do not claim an identity or verify that the database migration has
been deployed; actual lease operations still fail closed at launch.

## Install

Requirements: Go 1.26+ and the harness you want to launch. Codex uses the existing Responses-based LiteLLM provider
in `$CODEX_HOME/config.toml` (default `~/.codex/config.toml`). The provider uses
its existing `auth.command` or `env_key`; hrx never creates a credential.

```sh
make test
make install
hrx refresh
```

Installs `~/.local/bin/hrx` and
`~/.local/share/zsh/site-functions/_hrx`.
Use `make install PREFIX=/other/path` to change the prefix; keep its bin on PATH.

## Agent guide

`hrx prime` prints a compact guide to hrx written for agents. It covers concepts,
read-only checks, local launches, remote workers and networks, Pitput duties,
credential safety and installation. It makes no network requests. Point a new
agent at it before it uses hrx.

## Exhaustive test manifest (development)

### Test commands

```sh
make test                 # All Go packages with race detection, vet, Python tests
make test-native          # Installed Codex/Claude/OpenCode, isolated local endpoints
make test-pitput PITPUT_SOURCE=/path/to/pitput
make test-all PITPUT_SOURCE=/path/to/pitput
```

`make test` includes the session store and matrix packages as well as app/catalog
tests. It disables inherited live-probe and native-test switches. The default
suite includes 432 adapter cells, 216 one-shot process cells, and 216 interactive
PTY cells; allow several minutes. Native checks exercise installed CLI contracts
against local endpoints that reject inference, not paid providers.

The Pitput integration requires `uv`, Python 3.12+, OpenSSL, a local Pitput checkout
with its `server` extra, and PostgreSQL binaries (`initdb` and `pg_ctl`). Set
`PG_BIN=/path/to/postgresql/bin` when they are not on PATH; Homebrew PostgreSQL
is also discovered on macOS. A missing dependency fails the explicit integration
target rather than silently skipping it. It creates a disposable PostgreSQL
cluster, starts a real loopback Pitput HTTP service, and removes its private
credentials, profiles and database afterward. It never connects to the shared
Pitput service or uses the user's database, credentials, or enrollment authority.

Three concurrent HRX command-dispatcher processes launch deterministic Claude
harness fixtures that enroll separately, exchange a nonce through alpha -> beta
-> gamma -> alpha, read peer messages, replay pending events, ACK them, and retry
writes without duplication. Tests also cover denied scopes/authorship, invalid
credentials, changed request-ID payloads, history persistence, and pending monitor
recovery across service restart. SessionStart hooks verify separate HRX session
records. The temporary gateway rejects inference.

The integration launcher uses the production `app.Run` path with a test-only
TLS transport that trusts only its temporary CA and permits only loopback
connections. This is necessary on macOS where native trust does not honor
`SSL_CERT_FILE`; the installed HRX executable and system trust are unchanged.
The exchange fixtures prove launcher-to-Pitput transport and protocol behavior.
`make test-pitput-bridge PITPUT_SOURCE=/path/to/pitput` additionally exercises
the real Pitput bridge and native Codex server against local model fixtures: queue
delivery, handling ACK, private credential inheritance, and reconnect after resume.
These tests do not claim model reasoning quality or distributed ownership leases.

Live compatibility checks against real providers are opt-in and run only in
the project's own CI with a disposable gateway lease.

`hrx matrix --input FILE` emits a deterministic JSON manifest without running
tests, refreshing authorization, invoking credential helpers, or making provider
requests. Every cell starts as `not-run`, including expected-rejection cases.

Freeze the three key-specific caches and installed versions first:

```sh
hrx models --json > codex-snapshot.json
hrx models --harness claude --json > claude-snapshot.json
hrx models --harness opencode --json > opencode-snapshot.json
jq -n --arg hrx "$(hrx --version)" \
  --arg codex "$(codex --version)" --arg claude "$(claude --version)" \
  --arg opencode "$(opencode --version)" \
  --slurpfile c codex-snapshot.json --slurpfile a claude-snapshot.json \
  --slurpfile o opencode-snapshot.json \
  '{hrx_version:$hrx,harness_versions:{codex:$codex,claude:$claude,opencode:$opencode},snapshots:{codex:$c[0],claude:$a[0],opencode:$o[0]}}' \
  > matrix-input.json
hrx matrix --input matrix-input.json > matrix-manifest.json
jq '.cells | group_by(.harness) | map({harness:.[0].harness,cells:length})' matrix-manifest.json
```

Each harness/route/provider/account/reasoning tuple expands into all 48 choices:
four communications modes, two permission modes, three session modes, and two
execution modes. Omitted reasoning is distinct from explicitly passing the default.
Account routes are not collapsed by model identity. Exact duplicate source rows
are collapsed; conflicting declarations fail generation. Unauthorized or
unsupported tuples remain explicit expected-rejection cases, not silent omissions.
Public failover aliases retain unknown backend coverage until actual evidence
identifies a backend. A manifest is a plan, not a capability-verification result.

Run bounded offline checks against that frozen catalog:

```sh
hrx matrix --input matrix-input.json --offline --max-cells 100 --wall-time 2m --cell-time 15s > matrix-report.json
jq '{partial,counts,stop_reason}' matrix-report.json
```

This uses disposable fake harness/Yesod executables, dummy credentials and isolated
home/session state. It does not invoke installed harnesses or contact providers.
Python 3 is required for positive process checks. The CLI executor handles
one-shot cells, interactive PTY cells, and explicit rejection cases. Skipped cells
remain `not-run`; the report retains the full manifest. Exit 1 means failed or
unfinished work, with valid JSON still written to stdout. No live mode exists.
The report's `runner` records the actual runner version, executable SHA-256,
platform and embedded VCS metadata separately from caller-supplied manifest
versions. Offline results explicitly identify their fake-harness execution mode.

Every cell has a stable ID in the manifest. To rerun specific combinations, pass
`--cell ID` once per selection (duplicate IDs execute only once):

```sh
hrx matrix --input matrix-input.json > matrix-manifest.json
jq -r '.cells[] | [.id,.harness,.communications,.session,.execution] | @tsv' matrix-manifest.json
hrx matrix --input matrix-input.json --offline --max-cells 2 --cell CELL_ID_1 --cell CELL_ID_2 > matrix-report.json
```

Selections still retain the complete manifest and mark other cells `not-run`;
they never claim exhaustive coverage. Unknown IDs fail before any cell runs.

The synthetic offline regression matrix can be run without installed harnesses,
Yesod, credentials, or provider access:

```sh
go test -race ./internal/app -run '^TestMatrix(AdaptersEveryOptionCell|OneShotProcesses|InteractivePTY)$' -count=1 -timeout=420s
```

It covers 432 adapter cells and runs all 216 one-shot cells through disposable fake
harness/Yesod executables. With Python available, all 216 interactive cells also
run under PTYs. Process checks include argument boundaries, routed
environment, nonzero exit/output preservation, saved-session restoration, child
identity isolation, and exactly-once lease release. The PTY cells additionally
check stop/continue and foreground ownership, resize, input and Ctrl-C signal
status. Communications cells stop via Ctrl-Z; direct-exec cells use SIGSTOP
because the test's bare PTY process group is orphaned. They drain terminal output
while waiting for exit. This synthetic matrix
does not test native session capture, provider
tool use, or every real catalog route; those remain separate verification work.

## Yesod mail and bulletin

Enable communications only for the launches that need them:

```sh
# Direct mail guidance only.
hrx codex --model glm-5p3 --yesod-mail

# Shared bulletin-board guidance only.
hrx claude --model claude-sonnet-5 --yesod-bbs

# Both systems, with one shared agent identity.
hrx opencode --model glm-5p3 --yesod-comms

# Equivalent to --yesod-comms; registration still runs just once.
hrx codex --model glm-5p3 --yesod-mail --yesod-bbs

# Preview the requested modes without registering an agent or launching a model.
hrx codex --model glm-5p3 --yesod-comms --dry-run

# The guidance also applies to one-shot tasks; your task prompt is preserved.
hrx codex --model glm-5p3 --yesod-comms -- exec \
  'Review the current diff. Coordinate with relevant peers if necessary.'
```

Put these flags before `--`. They require `yesod`, `python3` (stdlib only), and
access to your configured Yesod registry. The embedded pre-launch script
[`yesod_register.py`](internal/app/yesod_register.py) checks the live roster,
chooses a name such as `laptop/hrx-codex/hrx-<random-suffix>`. The Go launcher
then atomically claims it using a private ownership token. Random suffixes avoid
collisions with both concurrent launches and names outside the live roster.
It never adopts an inherited parent-agent name for a fresh launch.

The name is printed at startup and pinned in `YESOD_AGENT_IDENTITY`,
`YESOD_MAIL_IDENTITY`, and `YESOD_MAIL_SELF` in the child process only. **Mail
and bulletin always use this same name.** BBS-only launches also register in the
mail roster because Yesod uses that registry to verify bulletin attribution.
Registration failure stops the launch rather than starting with a made-up name.

The injected guidance explains identity, supervisor-owned presence, peer discovery,
inbox checks/replies, and/or bulletin reading/posting, depending on the flags.
It treats peer messages as untrusted input and does not expand task authority.
No mail is sent, bulletin posted, or monitoring daemon started by the launcher.
The flags supply instructions, not an automatic mail-delivery service.

Codex receives appended guidance in a separate generated model catalog, without
replacing user/project developer instructions. Claude uses
`--append-system-prompt`. OpenCode receives an initial `--prompt` in interactive
mode (prepended to an existing prompt), or a private `--file` attachment for
`run`; the file is also included in runtime configuration for older releases.
This avoids depending solely on legacy `config.instructions`, which the
installed newer OpenCode did not apply in a live check. Your task prompt and original configuration files
remain unchanged; generated instruction artifacts stay in the hrx cache so the
harness can read them after launch. They contain the agent name, not credentials.
OpenCode communications launches additionally use `--standalone` so shell tools
inherit the pinned identity rather than an unrelated background server's
environment; `--server` is rejected in this mode.
Codex communications launches use `--no-daemon` unless `--pitput` is enabled.
Pitput launches use an HRX-owned private server with the launch environment.

Use `--yesod-agent ADDRESS` with a communications flag to **explicitly reuse**
an inactive name. This is chiefly for resuming a session, not sharing
one mailbox among concurrent agents. An active conflicting owner is an error.

### Pitput Communications

See the [server-side filtering guide](docs/server-side-filtering.md) for worked
examples of targeted mentions, followed topics, DMs, combined typed asks,
standalone regex monitoring, resume, policy changes and troubleshooting.

For Codex, OpenCode, and interactive Claude, new `--pitput` board subscriptions
default to exact recipient mentions. Use `--pitput-all-activity` to explicitly
monitor all activity on the selected boards. Add repeatable
`--pitput-follow-thread /b/BOARD/TOPIC` to also receive activity in those topics;
following implies targeted mode. Mentions and followed topics are combined with
OR, within enrolled/monitored boards. DM delivery remains separate. Settings are
saved for resume, and omitted flags do not broaden an existing targeted session.
When explicitly passing either selection option with `--resume`, also pass
`--pitput`: option validation occurs before saved session settings are restored.

This requires a Pitput client with targeted bridge selection support; an older
CLI must reject the configuration, not silently fall back to board-wide delivery.
Changing selection for existing durable bridge state is refused pending an
explicit policy migration. Do not delete state to bypass that refusal. The new-subscription default is targeted. Interactive Claude uses HRX-owned compact task bindings;
noninteractive Claude retains its manual monitoring path. Existing broad subscriptions are not
automatically migrated; policy migration remains a rollout gate.

Use `--pitput` before the harness passthrough separator for automatic Codex/OpenCode
notifications and platform-specific guidance, without requiring Yesod registration:

```sh
hrx codex --model glm-5p3 --pitput \
  --agent-identity myhost/developer/agent --pitput-board dev
hrx claude --model claude-sonnet-5 --pitput
hrx opencode --model glm-5p3 --pitput \
  --agent-identity myhost/developer/agent --pitput-board dev
hrx codex --model glm-5p3 --pitput --dry-run
```

HRX requests `pitput prime --platform codex|claude|opencode` using the actual
launch adapter. Older CLIs fall back to plain `pitput prime`; missing, empty,
failing, or timed-out priming falls back to built-in guidance. Priming has a
three-second total deadline and a 64 KiB output limit. Dry runs report
`"pitput": true` without invoking Pitput or writing instruction artifacts.
The flag composes with Yesod guidance in the same injection, preserving the
user task and routing settings. Pitput is restored for newly indexed sessions;
for older sessions repeat `--pitput`.

The guidance covers a stable agent identity distinct from the native session
ID, explicit authorized boards, private enrollment profiles, status checks,
idempotent write retries, and handling acknowledgments. A Yesod identity may
be reused as the agent's own Pitput identity, but it still requires a separate
Pitput enrollment. For Codex and OpenCode, `--pitput-board BOARD` enrolls explicitly requested
boards using the machine's existing enrollment authority. Repeat it for multiple
boards. If omitted, HRX monitors the identity's existing enrolled boards. New
identities require at least one board. `--agent-identity` also works without
`--gitlab`. HRX does not post introductions or change server-side permissions.

Private Pitput **direct messages** are delivered by default for Codex and OpenCode. HRX
adds one direct-message bridge per identity, so it does not multiply with boards. Its
notifications carry the sender and `dm-`/`dc-` handles, never the body, plus exact
`pitput dm read`/`reply` and helper-ACK commands. Before launch, HRX runs a read-only
`pitput dm inbox` probe. If the server or CLI lacks direct messages, it prints a notice
and delivers boards only, so existing board launches never break.

- `--pitput-no-dm` turns direct messages off for this launch.
- `--pitput-dm-only` launches with no boards at all. If the identity is not yet
  enrolled, it uses `pitput join --dm-only`, which requires the machine enrollment
  authority's `allow_dm_only`.

Resume keeps the chosen mode. For Claude, the guidance asks for `--include-dms` on its
one monitor. See Pitput's `docs/direct-messages.md` for the full contract.

- **Codex:** automatically starts a private app server with the launch
  credentials, attaches the terminal, discovers the loaded root UUID through
  the runtime API, and connects one Pitput bridge per selected board.
  Session capture for this path does not depend on trusting a hook. A private
  queue adapter connects the existing Pitput bridge to the correct Unix socket.
  The shared daemon and bridge are untouched. Explicit `--no-daemon` and
  noninteractive `exec` are rejected with `--pitput`.
- **Claude:** interactive launches use HRX-owned compact background-task bindings
  with generation fencing and exact task/output checks. Noninteractive launches
  retain manual monitoring guidance. Session prompt snapshots are disabled so a
  resumed snapshot cannot replace current launch guidance. This path has
  an end-to-end acceptance test against a disposable Pitput service.
- **OpenCode 2.1.2:** starts an authenticated private loopback server with the
  launch environment, validates the exact root session/project, and attaches
  the terminal. Each board gets a durable bridge using queued prompts that wake
  idle sessions without interrupting busy turns. Noninteractive `run` and
  externally supplied `--server` are rejected with `--pitput`.
  See [OpenCode bridge operation and validation](docs/opencode-pitput.md).

Codex and OpenCode bridge configs and durable cursors live under
`~/.local/state/hrx/pitput/IDENTITY_HASH/SESSION_UUID/BOARD/` (or
`$XDG_STATE_HOME/hrx/pitput/...`). Use `pitput bridge status --config PATH/bridge.toml`
to inspect a connection. New board subscriptions use server-side recipient-mention
filtering, excluding self-authored posts. Explicitly followed topics are OR
alternatives; `--pitput-all-activity` requests broader board activity.
HRX prevents simultaneous local launches of the same identity and stops its
server and bridges on exit. Resume with `hrx codex --resume UUID` or
`hrx opencode --resume SESSION_ID` to restore the
identity, selected boards, and durable bridge state. An already-running old HRX
session needs to exit and resume once to acquire this connection.

As with Yesod communications, detached/forked modes and OpenCode `--server`
are rejected for this option. Attention should target relevant mentions or
followed topics; the supported remote targeted bridge combines these with OR.
Older installed tools must reject unsupported selection rather than broaden it.
Queue acceptance is not handling.

For the explicit one-board/DM/typed-ask preview, use
`--pitput-combined-preview` with `--pitput` and exactly one `--pitput-board`.
Codex, interactive Claude and OpenCode use one combined identity consumer.
The service must explicitly enable `combined_attention_preview`; DM-disabled
and multiple-board preview launches are rejected. See the
[worked combined examples and identity migration limits](docs/server-side-filtering.md#combined-board-dm-and-typed-ask-preview)
before enabling it.

### Presence cleanup

Communications launches require a Yesod release exposing `mail claim`, `mail
renew`, and `mail release`; hrx fails closed with older releases. This prerequisite
is being shipped separately; do not install this hrx development version over a
working communications setup before the Yesod lease API is available.

hrx stays alive as a thin supervisor, renewing a 180-second lease every 45 seconds.
It releases presence on exit and on post-claim launch failures, preserving the
mailbox and unread/history messages. The token stays private to hrx and travels to
Yesod only on stdin, never in harness arguments, prompts, environment, or metadata.
No-communications launches retain their original direct-exec behavior.
Communications launches reject detached/background, tmux, remote-control, and
session-forking options: those can outlive or change the identity ownership of
the supervised process. Start a separate hrx launch for a separately owned agent.

Cleanup calls are bounded to 10 seconds. A renewal failure terminates the child
process group; shutdown escalates to SIGKILL after a three-second grace period.
Remaining child-group processes are killed before releasing the identity.
Ctrl-Z stops the supervisor with the harness; resume verifies ownership before
continuing. A long suspension can expire the lease, in which case the old child
is terminated rather than running with a reclaimed name. SIGKILL or host loss
cannot execute cleanup: lease expiry allows later reuse, not immediate release.

## HRX-managed Claude configuration

A fresh Claude Code configuration stops at first-run screens before it reads a prompt.
Claude Code 2.1.306 shows theme onboarding, then project trust, then bypass acceptance
when launched with `--yolo`. For CI and other disposable homes, HRX can manage a separate
configuration and answer those screens once:

```sh
hrx claude --claude-config-dir /path/to/new/dir --claude-init theme,trust,bypass --yolo
```

- `--claude-config-dir PATH`: HRX creates `PATH` (0700) with a provenance marker
  (`.hrx-claude-config.json`) and passes it as the child's `CLAUDE_CONFIG_DIR`. HRX refuses
  any directory it did not create, including your `~/.claude`, an existing or empty
  directory, a symlink, or anything accessible to group or others. It also refuses a
  different `CLAUDE_CONFIG_DIR` in the environment. Resuming a session reuses its
  recorded directory, and never recreates it if it is missing.
- `--claude-init theme,trust,bypass` writes only the named answers:
  - `theme`: onboarding completion;
  - `trust`: trust for exactly the canonical launch directory;
  - `bypass`: the bypass acceptance, allowed only when the launch's effective permission
    mode is `--yolo`.

  None of these implies another.

`--claude-init` **initializes once**. Each answer is written the first time it is
requested and recorded in the marker (trust per directory). Later launches never
re-apply it, because Claude owns its configuration and may migrate or rewrite these
keys. It is not a repair or reset command: if Claude loses an answer, its first-run screen
appears again and stays visible. Trust for a new directory is a new, explicit request.
Without these flags HRX writes nothing to any Claude configuration.

## Permission bypass and resume

### One permission flag across harnesses

`--yolo` reduces or removes permission prompts. Set `[permissions] yolo = true`
in HRX config to make it the default; `--no-yolo` keeps prompts for one launch.
Without that configuration, the default remains normal harness permissions;
do not use it for untrusted tasks or code. It does not widen your gateway key.

| hrx invocation | Native translation |
| --- | --- |
| `hrx codex --yolo` | `codex --yolo` (bypasses approvals and sandboxing) |
| `hrx claude --yolo` | `claude --dangerously-skip-permissions` |
| `hrx opencode --yolo` | `opencode --auto` (explicit permission denies still apply) |

```sh
# Inspect the selection without launching in bypass mode.
hrx codex --model glm-5p3 --yolo --dry-run

# Interactive launches with communications and reduced permission prompts.
hrx codex --model glm-5p3 --yesod-comms --yolo
hrx claude --model claude-sonnet-5 --yesod-mail --yolo
hrx opencode --model glm-5p3 --yesod-bbs --yolo

# OpenCode's --auto is placed after run for one-shot commands.
hrx opencode --model glm-5p3 --yolo -- run 'Run the project tests.'
```

Do not combine `--yolo` with explicit sandbox or approval-mode options. With neither the flag nor a configured default, hrx leaves normal permissions intact. OpenCode's installed `--auto`
is deliberately not described as overriding every deny rule.

### Resume a session or continue the latest

Replace `SESSION_ID` with an existing session identifier from that harness:

```sh
hrx codex --model glm-5p3 --resume SESSION_ID
hrx claude --model claude-sonnet-5 --resume SESSION_ID
hrx opencode --model glm-5p3 --resume SESSION_ID

# Continue the latest session selected by each harness's native behavior.
hrx codex --model glm-5p3 --continue
hrx claude --model claude-sonnet-5 --continue
hrx opencode --model glm-5p3 --continue

# Resume with a one-shot follow-up.
hrx codex --model glm-5p3 --resume SESSION_ID -- exec 'Continue the review.'
hrx claude --model claude-sonnet-5 --resume SESSION_ID -- -p 'Continue the review.'
hrx opencode --model glm-5p3 --resume SESSION_ID -- run 'Continue the review.'
```

| hrx option | Codex | Claude | OpenCode |
| --- | --- | --- | --- |
| `--resume ID` | `resume ID` / `exec resume ID` | `--resume ID` | `--session ID` |
| `--continue` | Indexed ID via `resume ID` | Indexed ID via `--resume ID` | Indexed ID via `--session ID` |

Do not combine `--resume` and `--continue`, or mix them with native resume flags
after `--`. Session IDs belong to their harness, not hrx; this does not migrate
history between harnesses. OpenCode resume first checks the ID through its public
session API, refusing missing or cross-project targets rather than allowing its
CLI to create a replacement session. Native exact-ID/continue forms are normalized
to the same hrx resolution path; interactive session pickers require an explicit ID.
Recognized native option values and literal text after a native `--` remain data: an output
filename named `resume` or a prompt containing `--continue` does not select a
session. Keep hrx options before the wrapper's passthrough separator as usual.
OpenCode root options may precede `run`; hrx inserts model, permissions, session,
and communications arguments after that subcommand. For example:

```sh
hrx opencode --continue --yesod-comms -- --log-level INFO run 'Continue the task'
```

### Private session index (development)

When a new interactive terminal session exits, HRX prints its exact resume
command and project directory. This uses the session ID captured from that launch,
not the latest session from another agent. If capture was unavailable, HRX says
so. One-shot and redirected launches retain their existing output behavior.

Resume restores the saved exact gateway route, reasoning, communications modes,
Yesod address, GitLab plugin/agent identity, and Pitput setting. Older records
lacking GitLab/Pitput fields still need explicit flags. Explicit `--model`, `--reasoning`, communications flags, and
`--yesod-agent` override their saved counterparts. Gateway authorization and
capabilities are checked again; an unavailable route is an error, not a silent
provider substitution. Permission bypass is never stored in session history;
the current `[permissions] yolo` setting applies unless `--yolo` or `--no-yolo`
overrides it.

The versioned index lives under `$XDG_STATE_HOME/hrx/sessions` (default
`~/.local/state/hrx/sessions`), using private directories/files, locking and atomic
replacement. It contains only session IDs and launch metadata, not keys, lease
tokens, prompts, or transcripts. `--continue` resolves the latest indexed session
for the current harness and canonical project, with deterministic tie-breaking.
Codex `--cd`/`-C` and OpenCode's directory argument affect that project selection.

Codex and Claude receive a SessionStart hook through a launch-local configuration
overlay. The hook records the actual native ID and working directory. Codex needs
you to review/trust the hrx hook in `/hooks`; hrx never bypasses hook trust. If the
hook is disabled, untrusted, or fails, that session is not automatically indexed.
OpenCode uses its public session create/get API; creation allocates an empty native
session without sending an inference prompt. Its index update is coupled to
process startup: a startup failure restores prior metadata under the index lock,
so it cannot replace the session selected by `--continue`. An empty native session
may remain after a failed fresh launch, but it is not indexed as an hrx launch.
Dry-run does not install hooks or
create sessions. These adapters still require end-to-end installed-harness
verification before this development build is promoted.

Opt-in native contract checks use disposable home/state directories and a local
HTTP fixture, never gateway/provider credentials or paid inference:

```sh
HRX_NATIVE_SESSION_TESTS=1 go test ./internal/app \
  -run '^TestNative(OpenCodeEmptySessionContract|OpenCodeResumeRetainsContext|ClaudeSessionStartCapture|CodexDoesNotBypassHookTrust)$' \
  -count=1 -timeout=180s
```

These verify empty OpenCode session creation/lookup, Claude's startup-hook capture,
and actual OpenCode/Claude resume/continue with the same native ID, selected model, and retained
user messages (observed at the local rejecting endpoint),
and Codex's refusal to run an untrusted hook. The Codex negative trust check does
not prove trusted-hook capture or full inference/resume behavior.

```sh
hrx codex --continue
hrx claude --resume SESSION_ID
hrx opencode --resume SESSION_ID --reasoning high -- run 'Continue the review.'
```

An exact-ID legacy session without metadata needs explicit `--model`, and an
explicit `--yesod-agent` if communications are enabled. Missing metadata for
`--continue` is an error; hrx does not substitute a different project's latest.

### Preserve the Yesod name when resuming

Save the `hrx: Yesod agent=...` name printed by the original launch. Pass that
same name along with the desired communications modes:

```sh
hrx codex --model glm-5p3 --resume SESSION_ID \
  --yesod-comms --yesod-agent laptop/hrx-codex/REPLACE_WITH_ORIGINAL_NAME

hrx claude --model claude-sonnet-5 --continue \
  --yesod-mail --yesod-agent laptop/hrx-claude/REPLACE_WITH_ORIGINAL_NAME
```

For indexed sessions, the saved name and communications flags restore automatically;
the examples above are explicit overrides or legacy reconstruction. hrx claims
the inactive name with a fresh ownership token and refuses an active conflicting
owner. Claude communications launches disable its system-prompt snapshot so the
current injected identity cannot disagree with an older saved system prompt.

## Claude Code and OpenCode

Each harness gets its own authorization check and completion cache:

```sh
hrx refresh --harness claude
hrx models --harness claude
hrx claude --model claude-sonnet-5 --dry-run
hrx claude --model claude-sonnet-5 -- -p 'Explain this repo without changing files'

hrx refresh --harness opencode
hrx models --harness opencode
hrx opencode --model glm-5p3 --dry-run
hrx opencode --model glm-5p3
hrx opencode --model fw-acme-kimi-k2p7-code -- run 'Explain this repo without changing files'
```

`hrx claude --model <Tab>` and `hrx opencode --model <Tab>` use the same fzf-tab
picker as Codex, filtered for the chosen harness. Each list contains only the
routes your gateway declares compatible with that harness and your key permits,
for example Anthropic routes for Claude and Fireworks or OpenRouter routes that
declare tools and text output for OpenCode. Provider-declared compatibility is
not a claim that every model has been live-tested, and completion does not
guarantee provider uptime. When the gateway groups a route under a different
model identity (for example `fw-acme-kimi-k2p7-code` under `kimi-k3`),
completion follows that shared identity. Use the exact route to be explicit.
More routes appear when the gateway declares compatibility and your key permits
them. Reasoning choices are model-and-harness-specific; these initial adapters
use provider defaults and **do not yet advertise selectable reasoning levels**.
`--reasoning` fails explicitly rather than silently ignoring an unsupported value.

Claude Code has no gateway provider of its own, so `hrx claude` requires a
`[claude.provider]` table in hrx configuration (below); there is no built-in
default gateway. Claude keeps your existing subscription login (OAuth/keychain).
hrx resolves the gateway key through the configured credential handle and
supplies it in a separate `x-litellm-api-key` header. It clears inherited API-key/provider overrides,
pins the selected model and subagent aliases, and preserves normal permissions
and settings. Conflicting routing/auth environment settings or `apiKeyHelper`
in Claude settings fail with an explanation instead of silently changing auth.
Managed enterprise policies remain authoritative; hrx is a launcher,
not a security boundary against plugins, hooks, or managed configuration.

OpenCode reuses `provider.litellm.options.apiKey` from
`$XDG_CONFIG_HOME/opencode/opencode.json` (default `~/.config/opencode/opencode.json`).
The existing inline key stays there; hrx does not copy it to another file.
`{env:NAME}` and absolute `{file:/path}` references are also supported. Runtime
configuration uses a separate `hrx` provider and `{env:HRX_GATEWAY_KEY}`; the key
exists only in the child environment. Models and limits come from LiteLLM.
Only this provider is enabled for the invocation; built-in agents and the small
model are pinned to the selection. Your original OpenCode configuration is not
rewritten. Use `-- --pure` (or `-- run --pure ...`) to omit external plugins.

OpenCode needs an explicit `--model` unless you set a default below. Harness
settings live in `~/.config/hrx/config.toml` (`$XDG_CONFIG_HOME` respected), or
`$HRX_CONFIG`. Claude Code requires them:

```toml
[claude]
model = "claude-sonnet-5"
[claude.provider]
base_url = "https://gateway.example.com/v1"
env_key = "GATEWAY_API_KEY"
```

OpenCode's section is optional and overrides its own `litellm` provider:

```toml
[opencode]
model = "glm-5p3"
[opencode.provider]
base_url = "https://gateway.example.com/v1"
credential_file = "~/.config/opencode/opencode.json"
```

Provide exactly one credential handle: `credential_file`, `env_key`, or an
`[...provider.auth]` table containing `command`, `args`, and optional
`timeout_ms`. Never place a literal key in hrx configuration or helper
arguments. Omit the `[opencode]` section to use OpenCode's own configuration.
hrx never creates or widens a key.

Passthrough is harness-specific: Claude's `-p`/`-c` and OpenCode's `run`/`-c`
work normally. Routing overrides (`--model`, `--effort`, `--variant`, agent
selection, settings files, and remote attachment) must not bypass validation.
Run administrative OpenCode subcommands directly, not through hrx.


## Zsh and fzf-tab

`make install` writes the zsh completion to
`~/.local/share/zsh/site-functions/_hrx`. Put this **before** Oh My Zsh or
`compinit`:

```zsh
fpath=("$HOME/.local/share/zsh/site-functions" $fpath)
```

If your framework does not initialize completion:

```zsh
autoload -Uz compinit
compinit
```

The generated file can also be sourced after compinit:

```zsh
source "$HOME/.local/share/zsh/site-functions/_hrx"
```

Open a new terminal (or run `exec zsh`), then:

```sh
hrx refresh
hrx codex --model <Tab>
hrx codex --model gpt-6.1-sol --reasoning <Tab>
```

Completion works with plain zsh. With [fzf-tab](https://github.com/Aloxaf/fzf-tab)
the candidates open in a fuzzy picker: type to narrow them and press Enter to
insert the selection. The first Tab may insert a common prefix such as `gpt-`;
a second opens the picker. Efforts depend on the selected model: Sol 6.1
advertises low, medium, high, xhigh and max, not ultra.

Load fzf-tab after completion setup and before any autosuggestion or
syntax-highlighting widget wrappers. A typical configuration:

```zsh
zstyle ':completion:*' menu no
zstyle ':completion:*:descriptions' format '[%d]'
source "$HOME/.oh-my-zsh/custom/plugins/fzf-tab/fzf-tab.plugin.zsh"
```

Run `disable-fzf-tab` to disable fuzzy completion in the current shell.

## One source of truth

### Latest-only mode

```sh
hrx catalog --latest --search gpt-
hrx models --harness opencode --latest
hrx opencode --latest --model <Tab>
```

`--latest` hides older numeric generations within the same family and variant:
for example, `gpt-5.6-sol` replaces `gpt-5.5-sol`, but not Terra or Astra.
Versions compare numerically (`5.10` is newer than `5.9`). This optional display
heuristic recognizes GPT, Claude, GLM, Kimi, DeepSeek and MiniMax version naming;
unknown names, dated snapshots and distinct suffixes remain separate. It is not
a provider deprecation declaration or a quality/cost recommendation.

Completion/listings compare only models usable by that harness. The full catalog
also retains an older generation when it is a selected harness's newest usable
option; inaccessible or unverified successors cannot remove your only choice.
All provider routes for a retained identity remain visible. Filtering happens
before catalog search; JSON/export counts reflect the filtered view.

Leave off `--latest` to see everything. The cache stays exhaustive, and an
explicit older `--model` remains launchable even with `--latest` present.
`NEWER FAMILY MODEL` explains the numeric successor even when an older usable
fallback is retained. A known successor is not a claim that your key or harness
can use it.

The catalog table collapses identical visible rows (model, route,
provider/account and harness statuses). Different accounts and routes remain
separate. JSON and file exports retain every upstream source and its evidence;
the source count therefore can exceed the number of table rows.

### Compact model catalog

Use `--compact` for one row per model identity with provider/accounts aggregated:

```sh
hrx catalog --compact
hrx catalog --compact --latest --search codex
hrx catalog --compact --harness codex --search gpt-
hrx catalog --details --harness codex --search glm
hrx catalog --compact --stale --harness codex
```

Illustrative output:

```text
MODEL        SOURCES          codex    claude  opencode  NEWER FAMILY MODEL
example-sol  OR-DF,OR-M,TF-T   TF-T[D]  -       OR-M[D]   -
```

`SOURCES` includes all catalog sources matching the search, including sources
with no gateway route. Harness columns list only provider/accounts with at
least one ready route for that harness's key; `-` means none. This does not
claim every route from that account is usable or guarantee provider uptime.
Evidence badges are separate from authorization:

| Badge | Meaning |
| --- | --- |
| `D` | Declared compatible, with no recorded live test. |
| `V` | Shared capability has `evidence: live-tested` and an RFC3339 `tested_at` within the last 7 days. |
| `U` | A live-test claim has no date, an invalid date, or a future date. |
| `S` | Recorded live test is older than 7 days. |
| `cached-` prefix | Authorization or discovery cache is stale; historical information only. |

The 7-day evidence window is a display policy, not an automatic compatibility
revocation. Refreshing authorization does **not** refresh a test timestamp.
Only dated harness-level evidence from the shared catalog qualifies for `V`;
discovery dates and HTTP acceptance never do. Existing shared capabilities
without these fields remain `D` (or `U` for undated test claims). hrx does not
invent test dates or maintain a second verification registry. A provider can
show multiple badges if its routes have different evidence.

Completion descriptions show authorization, declaration, test state/date and
numeric successors. Reasoning completion identifies the declared default.
Descriptions are display-only: selecting an entry inserts only the exact model
or reasoning value, including routes containing a colon. Both remain entirely
cache-local and work with standard zsh completion and fzf-tab.


Shorthand: `OAI` = OpenAI, `ANT` = Anthropic and `CGPT` = ChatGPT are built in.
Provider accounts are shown as `provider/account` in full unless you give them
short labels in hrx configuration:

```toml
[catalog.labels]
"openrouter/acme" = "OR-A"
"fireworks/acme" = "FW-A"
"truefoundry/contoso" = "TF-C"
```

Only the labels present in a result are explained below its table. Unlabeled
accounts keep their full names rather than receiving potentially ambiguous
initials.

Omit `--compact` for route-level statuses, or use `--details` for exact upstream,
per-harness authorization, compatibility, evidence, test dates, wire API,
reasoning choices/defaults, and context/output limits (0 means unknown).
`--details` takes precedence over `--compact`. These flags affect text display
only: `--json` and `--output` continue exporting the full underlying evidence.
Search also matches account names. `--stale` explicitly allows historical
listings and visibly marks them; completion and launches still reject stale data.
JSON adds `authorization_checked_at`, per-harness `stale`, per-row
`superseded_by`, and separate access/evidence fields. Legacy `verified_at` is
retained for compatibility but means **authorization checked**, not inference
verified. Prefer `authorization_checked_at` in new consumers.

Authorization accepts exact route IDs and account-scoped trailing wildcards
returned by the current key, including `tfy-contoso/*` and
`fw-acme-*`. Such grants authorize only matching routes in the shared
catalog; they never imply Codex compatibility. A model can therefore move from
`not-authorized` to `compatibility-unverified` after a key expansion without
appearing in launch completion.

The full catalog includes every discovered identity and provider source, not
just launchable models, across all three harnesses:

```sh
hrx catalog --refresh
hrx catalog --search claude
hrx catalog --harness opencode --search deepseek
hrx catalog --json
hrx catalog --output model-catalog.json
```

Statuses explain inclusion and exclusion: `ready`, `not-authorized`,
`compatibility-unverified`, `no-gateway-route`, and `unavailable`.
JSON includes exact route, upstream model, provider/account, available limits,
per-harness reasoning choices/evidence, discovery coverage and timestamps.
Unknown compatibility or effort enums stay unknown; discovery is not a live
inference guarantee. `ready` means authorized and declared compatible.
The JSON export is exhaustive for the gateway's discovered sources at its
recorded timestamp, not a claim of every model worldwide. Provider discovery
failures are visible in coverage. Catalog listing is offline unless `--refresh`
is supplied, and never changes keys or permissions.

Configured wildcard routes are expanded by the gateway against the matching
provider account's discovery. hrx recognizes namespace grants such as
`or-acme/*` only for discovered concrete routes; it never treats that
grant as access to other accounts or arbitrary undeclared models.

hrx consumes the dashboard's `/dashboard/models.json` and intersects its
identities/routes with the existing client's authenticated `/v1/models`.
There is no hardcoded model list in the launcher.

The LiteLLM repo owns `dashboard/harness-capabilities.json`. Its collector
publishes sanitized `sources[].details.harnesses` for exact discovered
routes. Discovery alone does not establish tool support or reasoning levels;
unknown capabilities are excluded, not guessed. Shared catalog or key-scope
changes flow through on the next refresh without updating hrx. Multiple
permitted routes without a public alias are ambiguous: choose an exact route.

Codex needs richer metadata than standard /v1/models. hrx generates a private
Codex catalog from the configured `model_catalog_json` (or
`$CODEX_HOME/models_cache.json`), emitting only permitted gateway routes and
replacing IDs, labels and reasoning metadata from the shared source. Your
original cache stays untouched.

The gateway can specify `catalog_template` for a new route. Sol 6.1 and the
verified Fireworks GLM 5.3 route borrow the Sol instruction/tool template.
Borrowed templates use the gateway's context limit (32K fallback when unknown),
text input only, and do not inherit OpenAI-specific service tiers, verbosity,
search, Responses-lite, or code-mode-only tool orchestration. Missing templates
fail explicitly.

Fireworks GLM 5.3 passed a real Codex text and shell-tool/result smoke test:

```sh
hrx refresh
hrx codex --model fw-acme-glm-5p3 --reasoning low
```

Only `low` is currently verified for this route. `--oss` is not needed: that
Codex flag selects local Ollama/LM Studio instead of the LiteLLM gateway.

## Freshness and security

- Tab only reads local configuration/cache: no HTTP, SSH or auth helper.
- `hrx models` reads cache; `--refresh` updates it and `--json` prints JSON.
- Freshness is capped at 30 minutes, including gateway snapshot age. Stale
  completion is empty; run `hrx refresh` before your next session.
- `hrx models --stale` explicitly permits old data with a warning. Every launch
  revalidates live authorization; there is no stale launch override.
- Failed discovery, malformed data and credential/refresh failures stop launch.
  A failed refresh preserves the last good cache.
- Atomic mode-0600 files live under `$XDG_CACHE_HOME/hrx` (default
  `~/.cache/hrx`). They never contain key values. Generated Codex catalogs
  contain local instruction templates, not authentication data.
- Cache identity is bound to harness, provider, endpoint, credential handle and
  config path. Different CODEX_HOME configurations do not share a cache.
- Credential output and HTTP error bodies are suppressed. Redirects are refused;
  HTTP responses and timeouts are bounded.
- hrx replaces itself with the harness using exec: PID, terminal streams, signals and
  exit status are preserved. The harness is not wrapped in a shell.

Put harness arguments after `--`. Model, provider and profile/config overrides
are rejected there because they bypass validated routing. Other Codex options
and subcommands pass through unchanged. hrx supports Unix and macOS;
no Windows adapter is claimed.

## Development

```sh
make test
go test ./internal/catalog -run '^$' -bench BenchmarkResolve -benchmem
```

Tests cover authorization, deduplication, ambiguity, freshness, invalid metadata,
atomic caching, error redaction, offline completion, generated catalog filtering,
and process/stream/signal preservation. Live probes are opt-in and send small
paid requests:

```sh
HRX_PROBE_MODEL=tfy-contoso/gpt-6.1-sol \
HRX_PROBE_EFFORTS=low,medium,high,xhigh,max \
go test ./internal/app -run '^TestLiveReasoningProbe$' -v -count=1
```

Terminal screenshots are real CLI/ZLE sessions captured in an isolated
pseudo-terminal and rendered with pyte/Pillow. Regenerate them:

```sh
hrx refresh
uv run --with pillow --with pyte python scripts/capture_screenshots.py
```

Codex's supported launch settings are documented in the
[official configuration reference](https://developers.openai.com/codex/config-reference/).

## Optional GitLab service account plugin

If your normal Codex default uses OpenAI directly, set
`HRX_CODEX_PROVIDER=litellm-codex` (or another configured LiteLLM provider name)
for HRX launches. This overrides HRX's provider selection without modifying
your Codex configuration or default provider.

Launch an agent with broker-managed GitLab credentials using
`--gitlab --agent-identity=HOST/ROLE/AGENT` (`--plugin=gitlab-service-account-broker`
remains supported). With host enrollment configured, first launch automatically
creates the private agent broker credential and subsequent launches reuse it.
Broker tokens are short-lived; HRX refreshes them for `git` and `glab` during long
sessions, so a session no longer needs relaunching when its launch token expires.
See [broker setup and lifecycle](docs/gitlab-service-account-broker.md).

## Live Agent Dashboard

Run `hrx dashboard` and open `http://127.0.0.1:8789` for a live table of agents,
models, harnesses, host identity, last-seen ages, and nonsecret credential metadata.
Rows sort by observed activity. Local operation needs no hub; additional private
host feeds can be pooled with `--source NAME=URL`.
See [dashboard signals and source pooling](docs/dashboard.md).

## Optional Multi-Host Hub

`hrx hub` coordinates agents on multiple computers through an authenticated
PostgreSQL-backed API. Enroll each computer once with permitted agent-identity
prefixes, then launch using the usual harness commands. `hrx dashboard --hub`
shows the shared fleet with host identity, activity, credential metadata, stale
presence, and ownership state.

Standalone mode remains the default and has no hub or database dependency.
Explicitly enabling hub mode requires ownership before launch and stops a
coordinated agent if its lease expires. Native sessions, GitLab credentials,
and Pitput delivery stay on the agent's computer.

See [startup and enrollment commands, configuration, and recovery](docs/hub.md).

## Wave Terminal Labels

New named-agent launches in verified local Wave terminals label unnamed panes
with the role, model, and harness. Existing titles are preserved; other terminals
are unaffected. Disable with `[wave] enabled = false` in your HRX configuration.
See [Wave labeling behavior and limitations](docs/wave.md).

## Agent workspace guidance (opt-in)

Set a workspace root to give named agent launches and Pitput/Yesod joins shared
workspace guidance in Codex, Claude, and OpenCode:

```toml
[agents]
workspace_root = "~/hrx-agents"
```

Agents are then asked to use `<workspace_root>/<agent-name>/` as their
workspace home, put isolated Git worktrees under `worktrees/<repo>/<task>/`,
and keep scratch files and logs there, preserving the source checkout and
existing work. Resumed agents retain their recorded owned worktree. Without
`workspace_root`, no workspace guidance is injected. This is priming guidance;
HRX does not change the launch working directory or move existing worktrees.
Explicit user workspace instructions win.

OpenCode launches automatically include the bundled [action guard](docs/opencode-pitput.md#bundled-action-guard); installing the HRX binary is sufficient.

### Claude Teams Opus route (0.1.2)

If your gateway publishes a `claude-teams/claude-opus-5-5` route, use
`hrx claude --model claude-teams/claude-opus-5-5` to keep its configured
subscription-to-Teams failover. The unprefixed
`claude-opus-5-5` is a different route and is not substituted automatically.
HRX supplies a provider-declared Messages capability for this exact Anthropic
route and its two known upstreams when discovery omits the capability.
Gateway availability and key authorization are still required; explicit gateway
capabilities take precedence. This declaration is not evidence of a live inference
probe. Both `hrx models` and `hrx catalog` include the route when eligible.

## License

Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
