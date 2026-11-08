package app

const PrimeGuide = `# hrx: agent guide

hrx is a harness launcher. It starts Codex, Claude Code or OpenCode
sessions against the configured model gateway, validates the exact model route first, and
can attach an agent identity, Pitput delivery, GitLab service-account
credentials and Yesod communications. It also launches agents on Proxmox guests,
singly or as a declarative network. hrx never invents models, credentials or
access: every route must be authorized for the key in use.

If you are reading this from inside a session, hrx probably launched you and is
still supervising it: its session hooks run the installed hrx binary. Never
overwrite that binary in place (see "Installing hrx" below).

## Concepts

- Harness: codex, claude or opencode. hrx options go before "--"; anything
  after "--" is passed to the native harness.
- Route: an exact gateway model string, e.g. claude-teams/claude-opus-5-5.
  Bare canonical names are rejected with candidates; nothing is substituted.
- Profile: [profiles.NAME] in ~/.config/hrx/config.toml with exactly harness,
  model, optional reasoning and optional credential_file ({"key": "..."},
  mode 0600). A profile does not choose identity, board or permissions.
- Agent identity: HOST/ROLE/AGENT (e.g. laptop/dev/agent). It is stable and
  distinct from the native session ID. Only one local launch per identity runs
  at a time.

## Look before launching (no inference, no agent started)

  hrx doctor [--harness NAME] [--model NAME] [--json]
  hrx catalog --latest --search TEXT       models and routes you can use
  hrx models --harness opencode            what Tab completion offers
  hrx profiles list | show NAME | validate NAME [--offline]
  hrx claude --model ROUTE --dry-run       resolved launch, provenance, no start
  hrx --profile NAME --dry-run
  hrx --auto "REQUEST" --print             best-guess command; launches nothing

## Launch one local agent

  hrx codex|claude|opencode --model ROUTE [--reasoning LEVEL] [-- NATIVE_ARGS]
  hrx --profile NAME [LAUNCH_OPTIONS] [-- NATIVE_ARGS]
  hrx --auto "opencode as laptop/dev/agent using pitput board team-board"

--auto makes one small-model parse request and always asks y/n/reprompt before
launching; it rejects piped stdin. Scripts must use explicit arguments.

Common launch options:
  --agent-identity ID            identity for Pitput and GitLab
  --pitput --pitput-board BOARD  automatic Pitput delivery; repeat the board flag
  --pitput-follow-thread PATH    also wake on that topic (mentions are the default)
  --pitput-all-activity          whole-board delivery (explicit opt-in only)
  --pitput-no-dm | --pitput-dm-only
  --gitlab                       broker-issued GitLab credentials for git and glab
  --yesod-mail | --yesod-bbs | --yesod-comms
  --yolo | --no-yolo             permission prompts off or on
  --resume SESSION_ID | --continue

Example:
  hrx opencode --model ROUTE --gitlab --agent-identity laptop/dev/agent \
    --pitput --pitput-board team-board

## Launch remote agents and groups of agents (Proxmox)

One worker:
  hrx run --backend=proxmox --plan --host HOST --profile NAME --name NAME \
    --agent-identity ID --pitput-board BOARD --prompt-file FILE

A network of 1-16 agents, declared in TOML ([[agents]] with role, host,
profile, identity, boards, prompt_file, optional follow_threads, resources and
required = false):
  hrx network --file network.toml --plan
  hrx network --file network.toml
  hrx network --file network.toml --only ROLE     retry or prepare one role

Always run --plan first: it shows resources, model, identity and a fingerprint
without creating anything. Launch then proves readiness through a real Pitput
challenge (read, reply, ACK) before it releases the prompt. Every role is
validated before any allocation, and all roles are required unless marked
required = false. The stable name binds configuration and prompt, so changing
either needs a new name. Network deployments are named NETWORK/ROLE. New guests
use VMIDs 3000-3099.

Lifecycle, using the deployment name:
  hrx remote inspect|logs|attach|stop|resume|destroy --name NAME
  hrx remote replace-lease --name NAME --generation N   then: hrx remote resume

- resume reopens the exact recorded native session. It never re-sends prompts.
- Leases are not extended silently. When one expires, replace it explicitly,
  retrying the same generation on failure.
- To recover an interrupted launch, rerun the same command with the same name.
- Destroy every deployment when its work is finished.

## Pitput delivery: what a launched agent must do

Notifications arrive as turns. Run the notification's exact Read and Handle ACK
commands, copying the handle character for character. Until you ACK, no further
notifications reach you. Queue acceptance is not handling. Never delete bridge
state or cursors (under ~/.local/state/hrx/pitput/) to get past a refusal such as
a mapping or selection change. hrx injects "pitput prime" guidance at launch;
run "pitput prime" yourself if that guidance is missing.

## Credentials and safety

- Never print, post or commit gateway keys, Pitput tokens or GitLab tokens.
- Broker GitLab tokens are short-lived. hrx refreshes them for git and glab in
  long sessions, but it never replaces an explicitly set GITLAB_TOKEN.
- Remote deployment state (the [proxmox] state_directory) holds
  ownership reservations, idempotency keys and credential journals. Never
  delete or edit it to recover; use the lifecycle commands.
- Communications launches reject detached, tmux, remote-control and forking
  options. Start a separate hrx launch for each separately owned agent.

## Observe

  hrx dashboard [--hub]          http://127.0.0.1:8789, live agents and models
  hrx hub status                 multi-host coordination, when enrolled

## Clean up

  hrx sweep --host root@pve1 --dashboard https://dashboard.example.com/hrx

Lists what hrx left behind: dashboard rows whose controller went quiet,
dashboard sources that stopped answering, and HRX-owned containers (tagged
hrx-managed or owner-hrx) that are stopped or idle with no live agent row.
It changes nothing. Show the plan to the operator and get approval before
rerunning with --apply ID; a container is backed up before it is destroyed.
Tags life-prod, hrx-infra and hrx-keep put a container out of reach.

## Workspace convention

When [agents] workspace_root is set in the HRX configuration, launched agents are
told to work in <workspace_root>/<agent-name>/, with git worktrees in
worktrees/<repo>/<task>/ and scratch files and logs there too. Preserve the
source checkout. Explicit user workspace instructions win.

## Installing hrx

Build from main. Write the new binary to ~/.local/bin/.hrx.new, check it, then
mv -f it over ~/.local/bin/hrx (an atomic rename) and keep the old binary.
Verify with "hrx --version" and a --dry-run launch.

## More

  hrx --help, the README, and docs/ in the hrx repository: proxmox.md (remote
  workers and networks), profiles.md, auto.md, hub.md and
  gitlab-service-account-broker.md.
`
