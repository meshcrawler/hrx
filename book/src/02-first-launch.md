# Installing hrx and Launching Your First Agent {#installing-hrx-and-launching-your-first-agent}

In this chapter you will build and install hrx, point each harness at your
gateway, run the read-only health check, and launch a real session. Along the
way you will meet the three habits the rest of the book relies on: refresh,
dry-run, then launch.

::: warning
Most commands in this chapter are free: they read configuration or ask the
gateway what your key may do. The few that start a harness send real
inference requests and spend money. They are marked when they appear.
:::

## What hrx Expects from Your Gateway

hrx does not ship a model list. Everything it knows about models comes from
your gateway at run time, so the gateway has to publish two things:

1. **The standard `/v1/models` endpoint.** Called with your key, it returns
   the routes that key may call. Every OpenAI-compatible gateway has this.
2. **A catalog at `/dashboard/models.json`** on the same host. For each model
   identity it lists the routes that serve it, the provider account behind
   each route, context and output limits, reasoning levels, and which harnesses
   each route has been declared compatible with, optionally with the date of
   the last live test.

The first tells hrx what you *may* use. The second tells it what *exists* and
what *works with which harness*. hrx intersects them. A route you are allowed
to call but that nobody has declared compatible with Codex will not be offered
to Codex. A model in the catalog that your key cannot reach is listed as
`not-authorized`, so you can tell "missing" apart from "forbidden".

::: note
The catalog format is documented in Appendix B, with a small script that builds
one from a LiteLLM configuration. If you run LiteLLM already, publishing the
catalog is a matter of serving one more JSON file from the gateway host.
:::

The gateway must be reachable over HTTPS. hrx refuses a base URL that is plain
HTTP, embeds credentials, or carries a query string or fragment.

## Building and Installing

hrx is a single Go binary. You need Go 1.26 or later and `make`:

```sh
git clone https://example.com/hrx.git
cd hrx
make test
make install
```

`make install` puts the binary in `~/.local/bin/hrx` and a zsh completion
script in `~/.local/share/zsh/site-functions/_hrx`. To install somewhere else,
set the prefix:

```sh
make install PREFIX=/opt/hrx
```

Make sure the `bin` directory is on your `PATH`, then check the version:

```sh
$ hrx --version
hrx 0.1.2 (dbf201ff1db4)
```

The part in parentheses is the Git revision the binary was built from. Every
public command also prints this line to standard error, which keeps standard
output clean for JSON and scripts. When you report a problem, or compare your
output with this book, start with this line.

::: warning
Once you run agents under hrx, the running sessions call the installed `hrx`
binary from their hooks. Replacing the binary in place while sessions are
running can crash them on some systems. Build the new binary next to the old
one and rename it over the top, which is atomic:

```sh
go build -o ~/.local/bin/.hrx.new ./cmd/hrx
~/.local/bin/.hrx.new --version
mv -f ~/.local/bin/.hrx.new ~/.local/bin/hrx
```
:::

### Tab completion

hrx's completion offers models and reasoning levels from its local cache, so
it works offline and never calls the gateway while you type. For zsh, add the
site-functions directory to `fpath` before completion is initialized (before
Oh My Zsh or `compinit`):

```zsh
fpath=("$HOME/.local/share/zsh/site-functions" $fpath)
autoload -Uz compinit
compinit
```

Completion works with plain zsh. It is noticeably better with
[fzf-tab](https://github.com/Aloxaf/fzf-tab), which turns the candidate list
into a fuzzy picker and shows each model's authorization and test status next
to its name. If you use fzf-tab, load it after `compinit` and before any
autosuggestion or syntax-highlighting plugins.

## Connecting Each Harness to the Gateway

hrx reuses the gateway configuration and credential you already have for each
harness. It never creates a key and never copies a key into its own files. What
it needs is slightly different for each harness.

### Codex

Codex keeps its providers in `$CODEX_HOME/config.toml` (by default
`~/.codex/config.toml`). hrx uses the provider named by the top-level
`model_provider` key. That provider must use the Responses API:

```toml
model_provider = "gateway"

[model_providers.gateway]
name = "Model gateway"
base_url = "https://gateway.example.com/v1"
wire_api = "responses"
env_key = "GATEWAY_API_KEY"
```

The credential can be an environment variable (`env_key`, as here) or a
helper command that prints the key. To use a different provider without
changing `model_provider`, set `HRX_CODEX_PROVIDER=NAME` or, permanently,
`provider_name` under `[codex]` in hrx's own configuration.

To keep two Codex setups side by side, point `CODEX_HOME` at the other one.
hrx keeps a separate cache for each configuration, so they never mix:

```sh
CODEX_HOME="$HOME/.codex-work" hrx refresh
CODEX_HOME="$HOME/.codex-work" hrx codex --model gpt-6.1-sol
```

### OpenCode

hrx reads the `litellm` provider from your existing OpenCode configuration in
`~/.config/opencode/opencode.json`:

```json
{
  "provider": {
    "litellm": {
      "options": {
        "baseURL": "https://gateway.example.com/v1",
        "apiKey": "{env:GATEWAY_API_KEY}"
      }
    }
  }
}
```

The key can be inline, an `{env:NAME}` reference or an absolute
`{file:/path}` reference. At launch, hrx gives OpenCode a separate, temporary
provider that reads the key from the child process's environment only. Your
original file is never rewritten, and only that one provider is enabled for the
session.

### Claude Code

Claude Code has no gateway provider of its own to reuse, so you describe the
gateway in hrx's configuration file, `~/.config/hrx/config.toml` (or
`$XDG_CONFIG_HOME/hrx/config.toml`, or the path in `$HRX_CONFIG`):

```toml
[claude]
model = "claude-sonnet-5"

[claude.provider]
base_url = "https://gateway.example.com/v1"
env_key = "GATEWAY_API_KEY"
```

Give exactly one credential handle:

- `env_key`: the name of an environment variable that holds the key.
- `credential_file`: a private JSON file of the form `{"key": "..."}`,
  readable only by you (mode `0600`).
- An `[claude.provider.auth]` table with `command`, `args` and optional
  `timeout_ms`: a helper program that prints the key. This is the best choice
  when the key lives in a password manager or on another machine.

The same `[opencode]` and `[opencode.provider]` tables override OpenCode's
settings if you prefer to keep everything in one place.

::: warning
Never put a literal key in hrx's configuration file or in a helper's
arguments. Configuration files get copied, committed and pasted into chats.
A handle that points at the key is safe to share; the key is not.
:::

At launch hrx clears inherited Claude Code routing and API-key settings and
pins the session, including subagents, to the selected route. If your Claude
Code settings contain conflicting routing or an `apiKeyHelper`, hrx stops and
explains the conflict instead of silently changing how you authenticate.

## Refresh: Fetching the Catalog

With at least one harness connected, fetch the catalog:

```sh
hrx refresh
hrx refresh --harness claude
hrx refresh --harness opencode
```

Plain `hrx refresh` refreshes Codex. Each harness has its own cache, because
each has its own key and its own compatibility rules. A refresh contacts the
gateway, runs your credential helper if you have one, and writes a cache file
under `~/.cache/hrx` with mode `0600`. The cache contains route names and
capabilities, never key values.

The cache is considered fresh for 30 minutes. After that, Tab completion shows
nothing rather than offering stale choices. This is deliberate: an empty menu
is a prompt to refresh, while a stale one invites you to pick a route that may
no longer work.

## Doctor: The Read-Only Health Check

Before your first launch, and whenever something feels wrong, run the doctor:

```sh
hrx doctor
hrx doctor --harness claude
hrx doctor --harness codex --model gpt-6.1-sol --reasoning high
hrx doctor --harness opencode --json --timeout 20
```

The doctor checks, for each harness: that the executable exists and reports a
version; that the configuration parses; whether the cache is present and
fresh; that the credential handle resolves; that the gateway is reachable;
that the gateway accepts your key; and that the selected or default route is
authorized and declared compatible. With `--model` it also checks the reasoning
level.

Doctor sends **no inference requests** and has no option to. It runs harnesses
only with `--version`, reads your key in memory, and makes `GET` requests to the
catalog and `/v1/models`. It does not write the cache, edit configuration or
start an agent. The whole run is bounded, 45 seconds by default.

Output is one line per check, with a fix where there is one:

```text
FAIL codex     authorization  not-authorized: Gateway rejected this key (HTTP 401/403).
     Fix: Check the existing key expiry, budget and authorization; do not rotate automatically.
WARN codex     cache          stale: Completion cache is stale; launch will revalidate.
     Fix: Run hrx refresh --harness codex
```

`FAIL` means a launch would fail; `WARN` means it would probably succeed but
something deserves attention. The exit status is 0 when nothing failed, 1 when
a check failed and 2 for bad arguments, so the doctor works in scripts and CI.
`--json` prints a stable structure with a `schema_version`, an overall `ok`,
and one entry per check.

::: tip
An accepted key is not proof that inference will work. Budgets, concurrency
limits and provider outages can still stop a request, and the doctor cannot see
them from a `/v1/models` response. If a launch passes every check and then hits
HTTP 429, the limit is on the gateway or provider side.
:::

## Dry Run: See the Launch Before You Pay for It

Every launch command accepts `--dry-run`. It does everything a launch does,
including the live authorization check, and then prints the result instead of
starting the harness:

```sh
hrx codex --model gpt-6.1-sol --reasoning high --dry-run
hrx claude --model claude-sonnet-5 --dry-run
hrx opencode --model glm-5p3 --dry-run
```

The output shows the harness, the exact route the name resolved to, the
provider account behind it, the reasoning level and where each default came
from. It never shows credential values or the arguments of your credential
helper.

Make dry-run a habit for any launch you have not done before. It costs nothing,
and it catches the two most expensive mistakes: the wrong route, and the wrong
payer.

## Your First Session

Now launch for real. This starts a harness and spends money.

```sh
cd ~/src/my-project
hrx claude --model claude-sonnet-5
```

hrx validates the route, prepares Claude Code's environment, and replaces
itself with `claude`. From here on you are in an ordinary Claude Code session.
When you exit, the shell gets Claude Code's exit status, because hrx is no
longer running.

The other harnesses work the same way:

```sh
hrx codex --model gpt-6.1-sol --reasoning high
hrx opencode --model glm-5p3
```

If you leave out `--model`, hrx uses the harness's default: Codex's `model`
setting, or the `model` key in the harness's hrx section. OpenCode has no
default unless you set one, so it needs `--model`.

### Passing arguments to the harness

Options for hrx go first. Anything after `--` goes to the harness unchanged.
This is how you run one-shot tasks:

```sh
# Codex: a read-only exploration with the exec subcommand.
hrx codex --model gpt-6.1-sol --reasoning high -- exec --sandbox read-only \
  'Explain the entry points and the test layout. Do not change files.'

# Claude Code: print mode.
hrx claude --model claude-sonnet-5 -- -p 'Summarize the open TODOs in this repo.'

# OpenCode: the run subcommand, without external plugins.
hrx opencode --model glm-5p3 -- run --pure 'Describe the project structure.'
```

hrx does not add permissions. The first example is read-only because it asks
Codex for a read-only sandbox, not because of anything hrx did.

hrx rejects passthrough options that would change the model or provider, such
as Codex's `-m` or `--profile`, Claude Code's `--model` or OpenCode's agent
selection. They would bypass the route hrx just validated. Choose the model
through hrx instead.

### Permission prompts

Each harness asks before running commands or editing files, unless you tell it
not to. hrx's `--yolo` maps to each harness's own switch:

| hrx | What the harness receives |
|---|---|
| `hrx codex --yolo` | `codex --yolo`: no approvals and no sandbox |
| `hrx claude --yolo` | `claude --dangerously-skip-permissions` |
| `hrx opencode --yolo` | `opencode --auto`: explicit deny rules still apply |

To make it the default, set `yolo = true` under `[permissions]` in hrx's
configuration; `--no-yolo` then restores prompts for a single launch.

::: warning
Removing permission prompts lets the model run any command your user can.
Use it in a disposable checkout, a container, or a remote worker
(Chapter 8), and never on a repository or machine you cannot afford to lose.
:::

### Resuming a session

Session IDs belong to the harness, but hrx gives all three the same options:

```sh
hrx claude --model claude-sonnet-5 --resume SESSION_ID
hrx codex --model gpt-6.1-sol --continue
```

`--resume` reopens one exact session; `--continue` picks the most recent one
for this project. hrx translates them into each harness's native form, and for
OpenCode it first checks that the session exists and belongs to this project,
so a typo cannot silently start a new session.

## The Whole Command Line

Here is hrx's complete usage summary. Most of it is the subject of later
chapters; for now, notice the shape: a harness or subcommand, hrx options, and
then `--` and whatever the harness should receive.

```{.text include="generated/hrx-help.txt"}
```

## Summary

You installed hrx, connected each harness to your gateway with the credential
it already used, and learned the rhythm that the rest of the book assumes:

1. `hrx refresh` to fetch what your key can use.
2. `hrx doctor` when something seems wrong.
3. `--dry-run` before any launch you have not done before.
4. Launch, with hrx options before `--` and harness options after.

So far you have been choosing models by name and trusting that the name
resolved sensibly. The next chapter looks at how that resolution works, how to
read the catalog, and what to do when the model you want is not there.
