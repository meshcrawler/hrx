# Proxmox execution

HRX can launch an OpenCode, Codex, or Claude worker on a configured Proxmox host, enroll its
Pitput identity, install a pinned runtime bundle, obtain a scoped gateway lease,
and require a real read/reply/ACK exchange before releasing its assigned task.
Public lifecycle commands, explicit lease replacement, and declarative mixed
networks are implemented. Fresh three-harness handoffs and restart recovery
have live acceptance evidence.

## Configure the controller

Add these tables to `~/.config/hrx/config.toml` (or select a separate file with
`HRX_CONFIG`). Replace the absolute paths and endpoints with your installation.
The controller Python must have Pitput installed and its machine enrollment
configured. The controller identity must already be able to create challenges on
the selected board. Workers receive only their own enrolled credential.

```toml
[profiles.researcher]
harness = "opencode"
model = "or-acme/z-ai/glm-5.3-flash"

[proxmox]
python = "/absolute/path/to/pitput-venv/bin/python"
controller_identity = "laptop/summarize/controller"
state_directory = "~/.local/state/hrx/deployments"
gateway_url = "https://gateway.example.com/v1"
broker_url = "https://gateway.example.com/bootstrap"
# An existing controller credential resolver: argv, not shell syntax.
# Its stdout is captured privately. Do not put the credential itself here.
broker_auth_command = ["/absolute/path/to/controller-token-resolver"]
lease_seconds = 900
max_budget_usd = 1
rpm_limit = 30
tpm_limit = 1000000
max_parallel_requests = 2

[proxmox.bundles]
opencode = "/absolute/path/to/verified-opencode-bundle"

[proxmox.hosts.pve1]
template = "local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst"
storage = "local-lvm"
bridge = "vmbr1"
cores = 2
memory_mib = 2048
disk_gib = 8
tags = ["hrx-workers"]

# Optional TLS passthrough when the gateway is accessible from the host but
# workers do not join the tailnet. Omit this table for direct guest routing.
[proxmox.hosts.pve1.relay]
listen_address = "192.0.2.10"
upstream_address = "192.0.2.20"
upstream_port = 443
```

The profile chooses the harness and exact model route. The host table chooses
placement and resources. A remote profile can omit `credential_file`: its worker
uses a fresh broker lease. Local profiles retain their existing credential rules.

A runtime bundle contains `manifest.json`, the pinned `hrx` and harness binaries,
and a `wheels/` directory with Pitput and its complete dependency closure. The
manifest records schema 1, harness, and each artifact's name, kind, version,
SHA-256 and size. HRX verifies every artifact before provisioning and again during
guest installation. Application wheels install offline; operating-system packages
still come from the guest's configured Debian repositories.

## Preview and launch

Write the assigned work to a file:

```sh
cat > researcher.md <<'PROMPT'
Summarize the thread assigned by the controller. Read each source before
summarizing it, distinguish decisions from proposals, and cite message paths.
Do not begin another thread until the controller assigns it.
PROMPT
```

Preview the exact deployment without creating state, enrolling an identity,
allocating a container, or minting credentials:

```sh
hrx run --backend=proxmox --plan \
  --host=pve1 --profile=researcher \
  --name=summary-researcher-1 \
  --agent-identity=laptop/summarize/researcher \
  --pitput-board=experiment-summarize \
  --tag=hrx-experiment-2 --prompt-file=researcher.md
```

Remove `--plan` to launch. The plan displays resources, model, identity and a
fingerprint; it does not print the assigned prompt or credentials. Repeat
`--pitput-board` or `--tag` for additional entries. `--name` defaults to identity.
The stable name binds the desired configuration and prompt: changing them under
the same name is rejected instead of silently repurposing the worker.

Local execution remains the default:

```sh
hrx run --profile=my-local-profile
hrx run --backend=local --profile=my-local-profile
```

## What launch proves

Progress reports `enrolling`, `provisioning`, `bootstrapping`, `starting`,
`verifying-readiness`, and `ready`. Process startup is not readiness. HRX waits for
an exact native session/project binding, posts a unique challenge, and requires
one worker reply plus matching server read/handled evidence and native tool calls.

Only then does HRX send the assigned prompt. The guest records a durable work
intent and uses one deterministic native message ID. If the API reply is lost,
a retry checks the existing message before attempting delivery again. A changed
prompt cannot reuse that work intent. The final JSON includes deployment, VMID,
readiness, native task receipt and lease expiry; it contains no credentials.

## Current operational limits

Guest networking uses DHCP. Managed stop/destroy releases the guest lease before
removing its NIC; this avoids retaining addresses until lease expiry. The optional
relay admits only the observed guest address and is removed on managed teardown.
No worker tailnet enrollment is required by this arrangement.

Failed launch stages retain their guest and controller journals. A readiness
observation deadline does not declare the worker terminated. Native sessions have
a process deadline tied to credential expiry. Expired or revoked leases are not
silently replaced, and a recorded stopped native session is not silently restarted.
Use the public lifecycle commands and explicit lease replacement below. Do not delete
the private controller state to try to recover a deployment.

### Inspect, stop, resume, and destroy a saved deployment

New deployments save their exact container specification before provisioning.
Lifecycle commands use that saved specification, even if host defaults have since
changed:

```sh
hrx remote inspect --name librarian
hrx remote stop --name librarian
hrx remote resume --name librarian
hrx remote destroy --name librarian
```

`inspect` reports infrastructure state; a running container does not imply agent
readiness. `stop` retains the container and its files. `destroy` stops the owned
container, revokes its broker lease, and removes the container and relay. If
revocation fails, the stopped container remains available for a retry. These
commands do not allocate a new deployment when the name is absent. Reservations
created before saved specifications were introduced cannot use these commands;
they require their original recorded specification for cleanup.

`resume` starts the retained container and resumes the exact OpenCode, Codex, or Claude session
recorded by its board bridges, with the same identity and workspace. It does not
re-send initial prompts or assigned work. Conflicting board bindings and expired
leases are rejected; use explicit credential replacement as described below.
The returned `ready: false` means process startup has not proved a new readiness
exchange. The existing lease expiry still bounds the resumed process.

Native log snapshots and interactive attach are described below.

### Native diagnostics

```sh
hrx remote logs --name librarian --lines 200
```

This returns JSON containing a bounded tmux pane snapshot (at most 2,000 requested
history lines and 65,536 characters), its source, and a truncation indicator.
Known managed credentials and authorization headers are redacted before the tail
is selected. Output still contains task/conversation content and should be treated
as private. The command does not restart a stopped container; guest diagnostics
currently require the container to be running. A retained previous pane is used
when available and the current pane is absent. This is a snapshot, not streaming
log collection or a complete transcript archive.

### Attach to the native terminal

```sh
hrx remote attach --name librarian
```

Run this from an interactive terminal. HRX opens an SSH terminal, verifies the
saved container ownership on the host, and attaches to the existing agent tmux
session. Press **Ctrl-B**, then **D** to detach while leaving the worker running.
An absent terminal is an error; attach never starts a replacement agent.

The deployment lock remains held while attached. Detach before issuing lifecycle
commands such as stop, resume, or destroy for that deployment; those operations
wait for the lock. The terminal displays live task content and should be treated
as private. `logs` is the bounded, redacted snapshot alternative.

### Replace an expired or revoked gateway lease

```sh
hrx remote replace-lease --name librarian --generation 1
hrx remote resume --name librarian
```

The first replacement is generation 1; subsequent replacements increment it by
one. If replacement fails or its response is lost, retry **the same generation**.
HRX stops the owned container, confirms revocation of the previous grant, records
the new mint intent, and requests the same model and limits as the original lease.
It then starts the container and atomically installs the new credential while the
native worker is inactive. It does not resume the agent automatically; use the
separate resume command to reconnect its existing native session.

Repeated requests for the same generation reuse the stored grant rather than
minting another one. A failed revoke prevents the new mint. A failed install
retains the private grant journal for retry. `credential_installed: true` does not
mean the agent is ready. This is explicit credential replacement, not automatic
lease renewal. The native launcher stops the worker at its lease deadline;
resuming an expired grant requires a replacement generation.


### Claude workers

Add a Claude profile and its pinned bundle alongside the OpenCode configuration:

```toml
[profiles.claude_worker]
harness = "claude"
model = "claude-sonnet-5-5"

[proxmox.bundles]
claude = "/absolute/path/to/claude-bundle"
opencode = "/absolute/path/to/opencode-bundle"
```

Merge these entries into your existing tables; do not duplicate a TOML table.
Then launch with the same location and identity flags:

```sh
hrx run --backend=proxmox --host=pve1 --profile=claude_worker \
  --name=claude-librarian --agent-identity=laptop/claude-librarian/agent \
  --pitput-board=experiment-summarize --tag=hrx-experiment-2 \
  --prompt-file=librarian.md
```

HRX requests the broker's `claude-code` credential profile, including its separate
OAuth credential, and initializes a private Claude configuration in the guest.
The worker starts HRX-owned background monitor tasks. Readiness requires matching
native session/task-generation evidence and an actual Pitput read/reply/ACK.

After readiness, the assigned prompt is stored privately in the guest. A stable
Pitput notice directs the worker to that file; the forum does not receive the
prompt itself. `work.status: published` confirms publication, not task completion.
OpenCode uses its native queued-message API for this step. Claude resume explicitly
re-arms the current monitor tasks without repeating completed assignments.

## Launch a declarative network

Create `network.toml` next to the role prompts:

```toml
name = "research-network"
tags = ["hrx-experiment-2"]

[[agents]]
role = "reader"
host = "pve1"
profile = "researcher"
identity = "laptop/research-reader/agent"
boards = ["experiment-summarize"]
prompt_file = "reader.md"
cores = 2
memory_mib = 2048
follow_threads = ["/b/experiment-summarize/research-topic"]

[[agents]]
role = "reviewer"
host = "pve1"
profile = "claude_worker"
identity = "laptop/research-reviewer/agent"
boards = ["experiment-summarize"]
prompt_file = "reviewer.md"
disk_gib = 8
required = true
```

Profiles and host defaults come from the controller configuration shown earlier.
Prompt paths resolve relative to the network file. Resource overrides are optional;
missing values inherit host defaults. Mentions are selected by default, with
`follow_threads` adding topic activity through the existing server-side filter.
Roles and identities must be unique. Networks currently support 1–16 agents.

```sh
hrx network --file network.toml --plan
hrx network --file network.toml
hrx network --file network.toml --only reviewer
```

Every role is validated before any allocation. HRX then prepares roles sequentially,
requires genuine readiness, and rechecks participants before releasing work. A
required-role failure prevents new work release for the whole network. All roles
are required by default; `required = false` allows a failed optional role to be
excluded. A release failure after the barrier can leave some roles already working;
retry uses each role's stable work ID rather than issuing a new assignment.

`--only reviewer` prepares/retries that role and observes the others without
bootstrapping or restarting them. It still requires the other required roles to
be running and ready. Stopped roles must first be resumed explicitly. Deployment
names are `NETWORK/ROLE`, so ordinary lifecycle commands apply:

```sh
hrx remote inspect --name research-network/reader
hrx remote logs --name research-network/reviewer
hrx remote resume --name research-network/reviewer
hrx remote destroy --name research-network/reader
hrx remote destroy --name research-network/reviewer
```

OpenCode, Codex, and Claude roles can share a network. Choose lease durations that cover sequential bootstrap, readiness,
and the intended work; the network does not silently extend lease expiry.

New containers use the provisional HRX VMID range **3000–3099**. Allocation skips cluster-occupied IDs and local durable reservations (including tombstones), and fails explicitly when the range is exhausted. Existing deployments retain their saved IDs, including IDs outside this range.

Codex runtime bundles additionally require three `codex-resource` artifacts:
`codex-code-mode-host`, `bwrap`, and `rg`. Each carries its own version, SHA-256,
and byte size, just like the main executable. Their local source files sit beside
`manifest.json`. The installer preserves the native package layout:

```text
runtime/
  bin/codex
  bin/codex-code-mode-host
  codex-resources/bwrap
  codex-path/rg
```

For example, the manifest entry for the code-mode companion uses
`"name": "codex-code-mode-host", "kind": "codex-resource"`, with the actual
`version`, `sha256`, and `size` from the selected release. Arbitrary resource
names and Codex resources in other harness bundles are rejected. These artifacts
support text/tool execution; optional voice resources are not installed.
Configure the Codex model-schema input before launching:

```toml
[profiles.codex_worker]
harness = "codex"
model = "YOUR_EXACT_GATEWAY_ROUTE"

[proxmox]
codex_catalog = "/absolute/path/to/codex-model-catalog.json"

[proxmox.bundles]
codex = "/absolute/path/to/verified-codex-bundle"
```

Use a complete Codex model catalog containing the selected gateway route exactly
once and the base schema templates referenced by gateway capabilities. Do not
reduce it to the selected route: HRX may need a differently named template to
generate that route’s native schema. Its contents are included in the deployment fingerprint. Changing it
requires a new deployment name. HRX installs it in the worker's private Codex
configuration; gateway authorization remains enforced by the scoped lease.
Fresh remote Codex configuration explicitly selects `service_tier = "default"`
so the server and native TUI admit the same tier from startup.
Codex receives its task through a stable Pitput release notice after readiness,
with the assignment stored privately in the guest. Resume retains the exact
native session ID and does not resubmit the initial prompt. HRX applies authorized
yolo permissions to its private Codex server; the resume TUI inherits them,
because native remote resume rejects explicit TUI permission overrides.

To add Codex to the example network, append this role after configuring the
profile and bundle above and writing its assigned work to `checker.md`:

```toml
[[agents]]
role = "checker"
host = "pve1"
profile = "codex_worker"
identity = "laptop/research-checker/agent"
boards = ["experiment-summarize"]
follow_threads = ["/b/experiment-summarize/research-topic"]
prompt_file = "checker.md"
```

All three roles share the readiness barrier. Clean up each saved deployment when
the experiment finishes, including `research-network/checker`.

## Observe controller progress

Remote launches now create one stable controller observation per deployment in
`hrx dashboard`. The row shows the last bootstrap stage, its timestamp, container
ID when known, and the native session ID after readiness. Repeating a launch
updates that row instead of creating another run. Failed stages show an explicit
failure marker and a suggestion to inspect the retained deployment.

```sh
hrx dashboard
hrx remote inspect --name research/librarian
hrx remote logs --name research/librarian --lines 100
```

`remote inspect` combines a current ownership-checked container observation with
`controller_observation`, when a launch has registered one. The latter is a
historical report: `remote.stage`, `remote.failed`, and `last_seen` describe the
last controller operation. For example, a failure during `verifying-readiness`
does **not** establish that the worker exited; inspect and read its logs before
retrying. The dashboard therefore labels active remote records **Not
live-tracked**, rather than inventing process liveness from a controller PID.
It does not refresh their timestamps merely because the page refreshes.

Successful stop/destroy operations record stopped/destroyed stages. Resume
records starting until a subsequent run verifies readiness. Older deployments
without a controller observation still support the normal lifecycle commands;
a launch supplies the identity metadata needed for their dashboard row. Records
contain no credentials, prompt text, or raw native error output.

## Configuration reference

Paths for `python`, `state_directory`, bundle directories, and `codex_catalog`
accept absolute paths, `~/`, or paths relative to the HRX configuration file.
Lifecycle commands use the same state-directory resolution as launch. Keep that
private directory across controller restarts: it holds ownership reservations,
idempotency keys, and credential journals.

| Controller field | Meaning and constraints |
| --- | --- |
| `python` | Controller interpreter with Pitput installed and enrollment configured. |
| `controller_identity` | Existing three-part Pitput identity authorized to issue challenges. |
| `state_directory` | Owner-only deployment journal directory; never share its credential files with workers. |
| `gateway_url` | Credential-free HTTPS URL ending in `/v1`. |
| `broker_url` | HTTPS scoped-lease broker endpoint. |
| `broker_auth_command` | Argument array for the controller's credential resolver; stdout is captured privately. |
| `lease_seconds` | 300–86400 seconds, covering bootstrap and work. No implicit extension. |
| `max_budget_usd` | Positive per-worker spending cap. |
| `rpm_limit`, `tpm_limit`, `max_parallel_requests` | Positive per-worker limits requested from the broker. |
| `bundles.opencode`, `bundles.codex`, `bundles.claude` | Verified Linux x86-64 runtime bundle directories for selected harnesses. |
| `codex_catalog` | Complete model-schema catalog required by Codex profiles. |

Each `proxmox.hosts.HOST` table supplies `template`, `storage`, `bridge`,
`cores`, `memory_mib`, `disk_gib`, and optional `tags` and `relay`. The host is
an existing SSH destination with access to Proxmox's `pct` and `pvesh` commands.
HRX creates unprivileged guests with resource limits, firewall-enabled DHCP NICs,
and `onboot=0`; it does not change shared DHCP configuration. Override resources
with launch flags or the corresponding fields on a network role.

## Recover from an interrupted launch

Run the same command with the same name, profile, prompt, and bundle. HRX
reconciles the saved container ownership and reuses enrollment, leases, native
sessions, and work IDs. Verified binaries are not rewritten while running;
interrupted copies are not published as completed executables. A changed prompt
or bundle requires a new deployment name.

```sh
# Retry the original deployment after a controller interruption.
hrx run --backend=proxmox --host=pve1 --profile=researcher \
  --name=summary-researcher-1 \
  --agent-identity=laptop/summarize/researcher \
  --pitput-board=experiment-summarize \
  --tag=hrx-experiment-2 --prompt-file=researcher.md

# Retry a failed role after a partial network release.
hrx network --file network.toml --only reviewer
```

If a native session has stopped, inspect its logs and use `remote resume` before
retrying readiness. If credentials have expired, use `remote replace-lease`
with an explicit generation and then resume. A network may release some roles
before another release fails; retries retain each role's existing work ID.
Do not delete journals or manually alter guest credentials to bypass these checks.

For a bootstrap failure before native startup, inspect the retained container's
`/var/log/hrx-bootstrap.log` through the Proxmox host. For example, after obtaining
the VMID from `remote inspect`:

```sh
ssh pve1 'pct exec 3000 -- tail -n 100 /var/log/hrx-bootstrap.log'
```

Substitute the inspected VMID; the example is not an instruction to access an
unrelated container. Native `remote logs` becomes useful after the tmux session
exists. Runtime bundle integrity failures require correcting the source bundle
or choosing a new deployment, rather than overwriting a published binary.
