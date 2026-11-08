# HRX Agent Watch

```sh
make install
hrx dashboard
```

Open **http://127.0.0.1:8789**. The frontend is embedded in HRX: no Node runtime,
frontend build, hosted service, or hub is needed. The dashboard listens on loopback
only; another address can be selected with `--listen 127.0.0.1:PORT`.

The live table shows agent, host, harness, model, context estimate, observed
compactions, activity, last seen, Pitput forums, and credential metadata. See
[native context metrics](context-metrics.md) for supported versions, accuracy,
privacy and rollout requirements. It polls every three seconds, updates ages each second, and sorts by
latest observed activity. The sort control switches to last-seen order. Search,
host and state filters, and pause/resume controls work without changing the data.
The default **Active** view hides exited and history-only runs and says how many
it hides; **Show all sessions** (or `?status=all`) lists everything. The Agent
column stays pinned while the wide table scrolls sideways.
The compact console defaults to the system light/dark preference, including
changes while open. The Color theme selector offers Light, Dark, or System;
explicit choices persist locally in this browser when storage is available.
The dark palette follows Gateway Pulse (purple-black surfaces, pink accents,
cyan healthy indicators, and rose errors) without changing table density.
Status and host have separate columns; historical sessions
are called out explicitly. Expand a GitLab profile for scope and expiry details,
or open "How to read this table" for signal definitions.
On mobile the table scrolls horizontally. Host failures preserve the last snapshot
and mark affected rows unavailable; a disconnected browser never implies a new
heartbeat.

## What the signals mean

- A row identifies a **launch/run**, not a unique agent name. Concurrent launches
  on different hosts, or multiple sessions under one identity, stay separate.
- Agent names come from the broker identity, then Yesod identity, then Claude's
  explicit `--name`. Otherwise the row is unnamed. Display names alone are not
  verified registrations.
- New HRX launches write a private local observation before starting the harness.
  The collector checks the launcher/harness PID and OS process start time, so PID
  reuse normally does not look like an agent remaining alive. Process observation
  is advisory liveness, not proof that the model is currently doing work.
- Tracked foreground launches stay supervised until the harness exits, including
  launches without a terminal. Normal exits, signals, and startup failures write
  an explicit `ended_at` timestamp and mark the run exited. Late hooks cannot
  revive it. Pitput servers/bridges stop and local identity locks release before
  HRX finishes. Credentials, enrollment, durable cursors, and session history remain
  available for resume. Process checks still detect older or abruptly killed runs.
- **Last seen** is the latest successful process observation or activity event.
  **Last activity** changes only on a recorded event, not every collector poll.
- Claude Code reports user submissions, completed assistant turns, and completed
  tools through native hooks. No message text, tool payload, transcript, or output
  is read or saved by those activity hooks.
- Codex reports launch and trusted SessionStart events; HRX-managed Pitput launches
  also bind the session UUID through the native server API. OpenCode reports
  launch with its native session ID. Neither currently reports per-turn activity.
  This dashboard does not consume Pitput messages or board activity.
  Its Forums column shows launch metadata, not message activity.
- Existing session-index entries appear as **history only**. A past launch is not
  evidence that a process is still running. Older entries may lack an agent name
  or credential metadata. Relaunch existing agents through the updated HRX to
  collect live observations; no retroactive activity is fabricated.
  Detached native launches (`--bg`, `--background`, `--tmux`) also remain history
  only: the launcher PID does not establish their background runtime's lifetime.
- On macOS and Linux, the collector also recovers already-running HRX launches
  that predate monitor files. It requires a same-user harness process with matching
  `HRX_SESSION_METADATA`, and binds the recovered row to its PID and OS start time.
  Identity and GitLab host/account are recovered from explicit HRX environment
  fields; tokens and arbitrary environment values are discarded. Existing
  monitor IDs and `HRX_MONITOR=off` are respected. Recovered rows show **process
  started (recovered)**, not invented message activity. Their profile, scopes,
  expiry, and session ID remain unknown unless recorded. Recovery does not install
  activity hooks into an already-running harness. Relaunch for per-turn telemetry.
- Pitput boards come from explicit saved HRX launch settings, or the matching
  runtime's verified enrollment at launch. Hover a board to distinguish
  **configured** from **enrollment verified at launch**. Neither verifies current
  membership, recent communication, or live delivery readiness; the UI explicitly
  says **Delivery unverified**. Board names are plain text because no viewer route
  is configured. Search includes collapsed boards; expand the additional-board
  control for long lists.
- **None configured** means this observed launch did not enable Pitput.
  **Unknown** means an older/recovered record lacks that observation. Historical
  session records expose configured boards when saved, not verified enrollment.
  Resume uses the existing saved `pitput_boards`; each new run takes a separate
  snapshot, and exited runs retain theirs.
- Pitput enrollment and Codex bridge configuration alone are not HRX launch records.
  They do not establish harness liveness. The collector never scans other agents'
  profiles or private transcripts to guess their boards.
  A shared Codex daemon being alive does not prove that each configured thread is
  running. Agents started outside HRX need an explicit runtime association before
  they can be represented as live HRX runs.
- The additive API field `pitput` is omitted for unknown legacy observations, or
  contains `enabled`, optional canonical `identity`, and `boards` entries with
  `name` (`/b/BOARD`) and `membership` (`configured` or `joined`, both at launch).
  It survives monitor-file serialization, collector pooling, and hub JSON
  storage without moving between host/run/session identities. It contains no
  tokens, message bodies, DM correspondents, or invented active-delivery signal.
  Existing saved session fields are unchanged for compatibility with DM work.
  Upgrade the hub before enabling collectors that send `pitput`: old hub binaries
  strictly reject unknown top-level observation fields. New hubs still accept
  old collector payloads, and unknown nested Pitput capability fields are ignored
  and not re-exported.
- Credential cells show GitLab host, account, profile, scopes, and expiry recorded
  at launch. When HRX refreshes a broker token in a long session, the run's expiry
  is updated to the new token's. Expired dates are highlighted. They never show tokens, private keys,
  provisioning credentials, or arbitrary environment variables. They do not probe
  GitLab or guarantee that a token has not been revoked. Unmanaged credentials
  remain explicitly unknown.

Local state lives under `$XDG_STATE_HOME/hrx/monitor` (normally
`~/.local/state/hrx/monitor`), in a private directory with private files. It holds
a persistent random host ID and one file per run. Existing session history remains
in its own store. Monitoring failure does not block a standalone harness launch. Set
`HRX_MONITOR=off` to disable new run observations; normal session indexing still
works. Standalone launches never contact a dashboard or hub. Explicitly enabling
hub ownership requires local monitoring and a reachable authenticated hub at
launch. Local history has no automatic retention limit; the hub compacts its
completed observation metadata according to the policy in [hub setup](hub.md).

## Pool multiple hosts today

Run `hrx dashboard` on each host whose local observations you want to collect.
The aggregating dashboard accepts repeatable, explicitly configured source feeds:

```sh
# In another terminal: private access to the remote host's loopback collector.
ssh -N -L 18789:127.0.0.1:8789 desktop

# On the viewing computer:
hrx dashboard --source desktop=http://127.0.0.1:18789
```

Use the same source arguments when restarting the dashboard. Sources are not
discovered automatically. Each refresh fetches sources concurrently with a two-second
timeout and a response-size bound. The viewer requests `/api/v1/local`, never
another viewer's aggregated endpoint, so sources cannot recursively aggregate
one another. Remote timestamps require reasonably synchronized host clocks.
Responses older than 15 seconds or more than 30 seconds in the future are rejected.
Redirects and credentials embedded in source URLs are rejected.

HTTP sources must use literal loopback IP addresses. HTTPS origins are also
supported when provided by an authenticated private transport/proxy; the collector
itself checks the incoming Host header for loopback, so such a proxy must forward
the loopback Host. The built-in server is not a public endpoint and has no network
registration or authentication service. It rejects cross-origin reads and DNS
rebinding hostnames. Do not expose it publicly as a substitute for a hub.

API version 1:

- `GET /api/v1/local`: only this host's observations and local collector health.
- `GET /api/v1/agents`: local plus configured source observations and source health.
- Both return `version`, `generated_at`, `agents`, and `sources`. Rows carry
  `host_id`, `run_id`, and `source`; identical agent names never overwrite one
  another. `session_id` is optional and separate from launch identity.
- The serialization schema contains only approved metadata. Unknown fields from
  remote feeds, including any supplied token fields, are not forwarded.
- Credentials and activity are observations, not authorization decisions.

Do not copy a monitor directory to a different host: that would duplicate its
stable host ID. A fresh installation creates its own identity.

## Optional PostgreSQL Hub

`hrx hub` adds authenticated shared registration, durable observation forwarding,
and exclusive agent-identity ownership. Only the hub connects to PostgreSQL;
enrolled hosts use its API. Standalone operation remains the default and does
not need enrollment, a database, or hub connectivity.

`hrx dashboard --hub` reads the fleet through a server-side viewer credential.
It shows host/run identity, stale presence, and lease state separately from
activity. Host credentials have explicit identity-prefix publishing scopes;
viewer credentials read the whole hub fleet and cannot publish observations.
Tokens, native sessions, and Pitput delivery remain on their owning computers.

Unlike read-only `--source` pooling, enabling hub ownership changes launch and
outage behavior: new launches require an available hub, and running coordinated
agents terminate when their leases expire. This applies only to opted-in hosts.
No availability-oriented mode that permits duplicate ownership is offered.

See [hub startup, enrollment, API, recovery policy, and verification](hub.md).

## Tests

```sh
make test-dashboard
HRX_NATIVE_SESSION_TESTS=1 go test ./internal/app -run '^TestNativeClaudeDashboardActivity$' -count=1
```

The native test uses an isolated local inference fixture; it makes no paid model
request. Store/API tests cover concurrent events, PID reuse, activity versus
heartbeats, host collisions, stale sources, secret-field stripping, and HTTP
boundaries. Existing broker and session tests cover the launch integration.

Optional browser checks against a running dashboard:

```sh
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
  node tests/integration/dashboard_browser.cjs
```

The script defaults to installed Chrome; set `HRX_BROWSER_CHANNEL` for another
Playwright channel and `HRX_DASHBOARD_URL` for another local address. It intercepts
the browser's API requests with explicit test fixtures, leaving real observations
unchanged. It covers live sorting, filters, pause/resume, outage states, expired
credentials, untrusted text rendering, and desktop/mobile layout. Set
`HRX_SCREENSHOT_DIR` to capture fixture screenshots.

## Remote workers started outside `hrx run`

Proxmox rows name their backend and the collector that observed them, for
example `pve1 (Proxmox, via this collector)`: the controller reports these
guests; no collector runs on the Proxmox host. A controller that starts guests
itself, such as an experiment driver, can record the same rows with the hidden
`hrx _remote-observation` command. It reads one JSON object on stdin:

```json
{"operation":"register","deployment":"<32 hex>","host":"pve2","name":"NETWORK/ROLE","identity":"HOST/ROLE/AGENT","harness":"opencode","model":"ROUTE"}
{"operation":"stage","deployment":"<32 hex>","stage":"ready","vmid":3004,"session":"ses_..."}
```

Stages are the `hrx run` stages (for example `provisioning`, `ready`, `stopped`,
`destroyed`). Registration never asserts liveness, unknown fields are rejected,
and `stopped`/`destroyed` mark the row exited.
