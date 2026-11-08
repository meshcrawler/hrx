# Natural-language launches

`--auto` translates a request with a small model through your configured LiteLLM
gateway, validates the proposed settings locally, and asks before launching:

```sh
hrx --auto "claude as laptop/project-mayor/agent using pitput"
hrx --auto="codex using gpt-6.1-sol. Task: Read the handoff and fix the failing tests"
hrx --auto "opencode as laptop/dev/agent using pitput board pitput-dev" --model fw-acme-glm-5p3
```

The first example asks which board to join. Supply `pitput-dev` (or the actual
board you want), review the revised command, then choose `y`, `n`, or `reprompt`.
`reprompt` asks for a correction and returns to a fresh preview; it never launches
without a new yes. EOF cancels with an error; scripts/piped stdin are rejected.
Use ordinary explicit HRX arguments for automation.

To see the best guess without any prompt or launch, add `--print`:

```sh
hrx --auto "opencode as laptop/dev/agent using pitput board pitput-dev" --print
```

It makes one parser request and writes the validated proposal to stdout as a
single quoted `hrx ...` command line; notices go to stderr. It asks nothing, so
it also works without a terminal. If the request is missing something (for
example which board to join), it prints the question on stderr and exits
non-zero instead of guessing; add the detail to the request and run it again.
`--print` is never sent to the parser or passed to the launch, and it never
enrolls an identity, starts an agent, or contacts Pitput or GitLab.

Parsing itself makes an inference request. Do not put secrets in the request or
corrections. HRX sends only your request/corrections, explicit supported option
values, and the supported proposal schema; it does not send project files,
environment variables, credentials, saved sessions, or other agent records.
Gateway credentials are used only in the HTTP Authorization header.

## Gateway configuration

Configure `~/.config/hrx/config.toml` (also respects `HRX_CONFIG` and
`XDG_CONFIG_HOME`):

```toml
[auto]
provider_harness = "opencode"
model = "fw-acme-inkling"
```

The parser reuses that harness's existing gateway URL and credential handle.
It uses chat completions independently of the harness being launched. The
provider harness defaults to `opencode`, whose key typically authorizes these
routes. You may explicitly choose `codex` or `claude` if its configured key has
the required access. No new credentials are provisioned for parsing.

Before inference, HRX joins the fresh dashboard catalog with the gateway's
`/v1/models` authorization response. `auto.model` must resolve to an authorized
chat-completions route. If omitted, exactly one route with canonical identity
`inkling` must exist; when several exist, HRX lists the choices and asks you to
set `auto.model`. It does not assume a free route actually works or guess which
account you want. An explicitly configured alternative model is allowed, but
HRX never changes models after a failure.

Catalog authorization is necessary but does not guarantee upstream
availability: a free route can be advertised and still return HTTP 403. If that
happens, choose a route you have verified, such as a paid Fireworks route,
explicitly in `auto.model`. Route names here are examples, not identifiers
hardcoded in HRX.

## Validation and precedence

The model returns an allowlisted JSON object, not executable shell text. HRX
accepts harness, model, reasoning, agent identity, Pitput flag/boards, GitLab
flag, task text, or a clarification question. Unknown fields, duplicate keys,
nulls, control characters, malformed/truncated output, and unsupported options
are rejected. Tasks become one literal argument, with a native argument
separator for Codex/Claude or OpenCode's `--prompt`; they cannot introduce
harness subcommands or options. The preview uses shell-safe quoting, but HRX
launches an argument array directly.

Supported explicit flags after `--auto REQUEST` are `--model`, `--reasoning`,
`--agent-identity`, `--pitput`, repeatable `--pitput-board`, `--gitlab`,
`--yolo`, `--no-yolo`, and `--dry-run`. Explicit scalar settings fill missing
inferred settings; differing values fail with a conflict instead of silently
overriding either value. Explicit boards must match any inferred board list.
Boolean flags enable their named integration. Resume, continue, Yesod, arbitrary
plugins, and harness passthrough are intentionally outside this initial schema;
use a normal explicit launch for those. `--auto` must be the first argument.

Omitted launch models/reasoning use the selected harness's existing defaults.
They are resolved and printed explicitly before confirmation. A harness with no
default model asks for clarification. Unsupported models and reasoning levels
fail local catalog/capability validation. Pitput needs an exact identity and at
least one explicit board; neither is guessed from a project folder. GitLab
uses that same identity through the normal broker integration. Standalone
identities still follow the normal parser's requirement for Pitput or GitLab.

Auto launches explicitly use `--no-yolo`, even if `permissions.yolo` is true.
The model cannot enable dangerous permission modes. To request them, supply
`--yolo` as an explicit CLI flag outside the natural-language request; the
preview displays a warning. `--no-yolo` retains the harness's configured
permission behavior and does not impose a separate HRX sandbox.

No board joining, agent registration, broker enrollment/provisioning, priming,
session capture or harness execution occurs before yes. Read-only gateway
catalog/authentication calls and ordinary model-cache writes occur during
validation. After approval, the validated argv goes through the same `Run`
pipeline as an explicit launch, including fresh catalog validation and all
integration/lifecycle checks. Later failures can therefore still prevent a
launch safely. `--dry-run` still requires confirmation and then uses the normal
launcher's dry-run behavior.

## Bounds and failure behavior

Each inference has a 30-second deadline, a 32 KiB response limit, a 2048-token
output cap, no redirects, no retries, and no fallback. Catalog/auth retrieval
uses the existing bounded HRX refresh path. Requests and replies are limited to
8 KiB each, cumulative conversation to 32 KiB, and the loop to eight proposals
or clarifications. Invalid confirmation answers do not count as approval.
HTTP errors report status without echoing response bodies or credentials.

## Verification

```sh
go test -race ./internal/app -run '^TestAuto' -count=1
```

Tests include all three harnesses through a local TLS gateway, real local config
loading/catalog validation, exact identity/board handling, no/yes/reprompt,
permission isolation, shell literals, schema failures, unsupported routing,
HTTP failures/timeout, response bounds, noninteractive rejection, and the
pre-approval boundary. The launch callback records accepted argv instead of
starting a production harness or touching Pitput/GitLab.

Optional live translation only (no registration or launch):

```sh
HRX_AUTO_LIVE_TEST=1 HRX_AUTO_LIVE_ROUTE=fw-acme-inkling \
  go test ./internal/app -run '^TestAutoLiveTranslation$' -v -count=1
```

`HRX_AUTO_LIVE_ROUTE` is a test-only explicit override, revalidated against live
authorization. It is not a production fallback or launcher setting.
