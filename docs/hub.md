# HRX Hub

This guide covers the optional multi-host hub: its contract, opt-in deployment,
and recovery. Standalone hrx needs no hub.

## Goal

Deliver an optional PostgreSQL-backed HRX hub that securely coordinates agents
across computers and presents one live dashboard. Verify launch, notifications,
exit, resume, and recovery across multiple hosts while preserving standalone HRX
with no hub or database dependency.

## Ownership and availability

Hosts publish observations through an authenticated API. Only the hub connects
to PostgreSQL. Host IDs are stable random installation identifiers; host display
names and canonical agent identities are separate fields. Agent identity prefixes
are explicitly granted at enrollment and are not inferred from a hostname.

Local mode remains the default. Opting into exclusive hub ownership means a launch
must acquire its identity before starting a harness. Renewals have a bounded
deadline; losing ownership or reaching the local conservative lease deadline
terminates the supervised harness and its HRX-owned Pitput bridge/server. A hub
outage does not affect machines that have not opted in.

Leases use database time and monotonically increasing generations. An old run
cannot release or renew a successor's lease. This fences hub state, not arbitrary
external side effects: GitLab and Pitput do not yet enforce these generation
tokens. Stopping the supervised process is the local enforcement mechanism.

## State

- `hosts`: installation ID, credential digest, display name, allowed identity
  prefixes, revocation and last contact. Raw credentials are never stored.
- `runs`: immutable host/run identity, last accepted observation sequence,
  allowlisted metadata, and terminal state. Exit is irreversible for a run.
- `leases`: identity, owning host/run, generation, and database expiry.
- `ingest_receipts`: per-host sequence and payload digest for idempotent retries.
- `schema_migrations`: transactional, versioned PostgreSQL migrations.

The local monitor remains the source of process observations. A collector sends
bounded, durable batches, retrying the same sequence and payload until accepted.
The hub stores client observation times separately from receipt times. Delayed
backlog must not renew presence or an identity lease. Heartbeats and lease renewal
are separate from activity ingestion.

An ended run cannot become live through a late event, heartbeat, or PID reuse.
Expired host presence is `stale`, not a confirmed exit. Resuming creates a new run
while retaining the native session UUID and explicitly reacquiring the identity.

## API v1

All modifying endpoints require a bearer credential. Host credentials grant only
their enrolled host and allowed identity prefixes; administrator and viewer
credentials are separate. Remote transports require TLS (or an explicit private
loopback tunnel). Redirects are not followed by clients.

| Endpoint | Authority | Purpose |
| --- | --- | --- |
| `POST /api/v1/hosts/enroll` | Administrator | Enroll a host with a client-generated credential digest |
| `POST /api/v1/hosts/revoke` | Administrator | Revoke a host and its current ownership |
| `POST /api/v1/hosts/rotate` | Administrator | Replace a host credential |
| `POST /api/v1/heartbeat` | Host | Report host reachability using server receipt time |
| `POST /api/v1/observations` | Host | Idempotently ingest an ordered metadata batch |
| `POST /api/v1/leases/acquire` | Host | Acquire an authorized agent identity for one run |
| `POST /api/v1/leases/renew` | Owning host | Renew the exact run/generation |
| `POST /api/v1/leases/release` | Owning host | Release the exact run/generation |
| `GET /api/v1/agents` | Viewer or administrator | Read the pooled dashboard snapshot |
| `GET /api/v1/host` | Host | Read own identity, permitted prefixes, and ingestion checkpoint |
| `GET /healthz` | Public | Minimal process health; no credentials/configuration |

Bodies and batches are bounded. Unknown fields are rejected on writes. Responses
and logs contain neither credentials nor arbitrary caller metadata. Unsupported
protocol versions, conflicting retries, unauthorized scopes, and stale lease
generations produce explicit errors without applying partial changes.
An acquisition rejected because another run holds the identity returns HTTP 409
with an `owner` object containing its host ID, run ID, generation, and expiry.
Only callers authorized for that identity receive these details. The CLI prints
them so an operator can identify the competing installation without distributing
viewer credentials to agents. Other sequence/stale-generation conflicts retain
the generic error response.

### Wire contract and compatibility

Requests use JSON and `Authorization: Bearer TOKEN`; writes include `version: 1`.
Host and run IDs are 32 lowercase hexadecimal characters. Agent identities are
at most 256 bytes and have
three slash-separated components; enrollment prefixes end at a component
boundary, for example `laptop/` or `laptop/project/`. Timestamps use RFC 3339 with
optional fractional seconds. Credential expiry metadata is a string because
GitLab supplies date-only expiry values.

| Operation | JSON request fields | Success response |
| --- | --- | --- |
| Enroll | `version`, `host_id`, `name`, `prefixes` (1-32 strings), `token_digest` (SHA-256, 64 lowercase hex characters) | `{"ok":true}` |
| Revoke | `version`, `host_id` | `{"ok":true}` |
| Rotate | `version`, `host_id`, `token_digest` | `{"ok":true}` |
| Heartbeat | `version`, `live_runs` (up to 1,000 run IDs belonging to this host) | `{"ok":true}` |
| Observations | `version`, `sequence` (positive integer), `records` (1-200 observations) | `{"ok":true}` |
| Acquire | `version`, `identity`, `run_id`; omit `generation` or send zero | Lease object |
| Renew | `version`, `identity`, `run_id`, `generation` from the acquired lease | Lease object |
| Release | `version`, `identity`, `run_id`, `generation` from the acquired lease | `{"ok":true}` |
| Read own host | No body | `host_id`, `name`, `prefixes`, `last_sequence` |
| Read agents | No body | `version`, `generated_at`, `agents`, `sources` |

A lease object contains `identity`, `host_id`, `run_id`, `generation`,
`expires_at`, and `server_time`. Clients measure elapsed time locally and derive
the lease duration from the two server timestamps; a host's wall clock cannot
extend ownership. An acquire retry for the same host/run returns its original
live lease and does not extend it. Renewal is the explicit extension operation.

Each observation contains `run_id`, `host_id`, `host`, `agent`, `harness`,
`model`, `started_at`, `last_seen`, `last_activity`, `activity`, `status`, and
`credential`. Optional fields are `session_id`, `pid`, and `ended_at`.
The server replaces `host` with the enrolled display name. Harness values are
`codex`, `claude`, or `opencode`; write-side status values are `running`,
`exited`, or `unknown`. `ended_at` is allowed only for exited observations.
Credential fields are `provider`, optional `host`, `username`, `profile`,
`scopes`, and `expires_at`. Process-start fingerprints stay local. Arbitrary
metadata, credentials, environment variables, and message content are not part
of this contract, and unknown JSON fields are rejected.

A sequence identifies one immutable batch for a host. Retry the exact sequence
and payload after a lost response. Conflicting payloads, stale sequence numbers,
and attempts to change a run's identity/harness fail. Accepted exits cannot be
revived. The host endpoint reports the checkpoint for reconciliation; collectors
must retain their pending batch rather than inventing a newer sequence to bypass
a conflict. Hub receipt and liveness timestamps are stored separately from the
host's reported activity timestamps.

Read-side agent rows add `source`, `source_status`, and optional `ownership`
(`state`, `generation`, optional `expires_at`). Ownership states are `held`,
`inactive`, and `superseded`. A superseded row retains its own generation and
omits the successor's expiry. Read-side status may be `stale`; it does not prove
process death. Sources provide connection status, last successful contact, and
bounded diagnostic messages. Heartbeats never update genuine activity time.

All successful operations return HTTP 200. Errors have an `error` string, with
the optional scoped `owner` object on acquisition conflicts. Codes are 400 for
invalid input/version, 401 for missing/invalid host authentication, 403 for
denied authority, 404 for unknown endpoints, 405 for unsupported methods, 409 for
ownership/sequence conflicts, 429 for throttling, and 503 for capacity/storage
unavailability. Callers must not depend on the wording of error strings.
Writes are bounded to 1 MiB, each server admits 64 concurrent requests, and each
authenticated principal gets 30 requests/second per server. Rate limits are
per API instance; ownership and observation consistency remain transactional
across instances. Large-fleet snapshot bounds are described below.

API v1 and database schema versions are separate. Clients reject unsupported
snapshot versions; servers reject unsupported write versions. Additive optional
response fields may appear within v1, so readers ignore fields they do not use.
Incompatible request or lifecycle semantics require a new API version. Migrations
are forward-only and transactional; binaries refuse a database schema newer than
they support. There is no automatic downgrade or mixed-schema rollout guarantee:
back up PostgreSQL, stop hub writers, deploy compatible API binaries, migrate at
startup, then restart collectors. Stop coordinated agents before this maintenance
window and resume them afterward. Agent-side native session indexes remain local
and are not migrated by the hub.

## Development startup

The branch includes the host client, collector, supervised ownership, and local
dashboard proxy. Physical cross-host notification/resume and failure-recovery
checks have passed as recorded below. Start with a dedicated PostgreSQL database
and enroll only the installations intended to participate.

Create a PostgreSQL database owned by a dedicated hub role. Put its connection
string in a private file, then create distinct administrator and viewer tokens:

```sh
mkdir -m 700 -p ~/.config/hrx/hub
umask 077
openssl rand -hex 32 > ~/.config/hrx/hub/admin.token
openssl rand -hex 32 > ~/.config/hrx/hub/viewer.token
# Write the database connection string to ~/.config/hrx/hub/database.
hrx hub serve \
  --database-url-file ~/.config/hrx/hub/database \
  --admin-token-file ~/.config/hrx/hub/admin.token \
  --viewer-token-file ~/.config/hrx/hub/viewer.token
```

The default listen address is 127.0.0.1:8790. Schema migrations run transactionally
at startup. A non-loopback listen address requires both `--tls-cert` and
`--tls-key`; use a trusted HTTPS origin or an SSH tunnel to a loopback listener.
Never expose the local dashboard as the hub API.

Enroll each installation once, using an administrator credential supplied
privately by the operator:

```sh
hrx hub enroll --url http://127.0.0.1:8790 \
  --admin-token-file /private/path/admin.token --prefix laptop/
```

Enrollment creates a private host token and updates the existing HRX config
without discarding other settings. Multiple prefixes are supported. Physical
hosts may share a permitted prefix intentionally; the ownership transaction
still admits only one live run for each identity. Do not copy an installation's
state directory to another computer.

The resulting configuration is equivalent to:

```toml
[hub]
enabled = true
url = "http://127.0.0.1:8790"
token_file = "hub/host.token"
viewer_token_file = "hub/viewer.token"
```

Relative credential paths are resolved against the config directory. The viewer
file is provisioned separately; the host credential cannot read all agents.
`hrx dashboard --hub` keeps the viewer token in the local server process and
shows the hub through the existing compact UI. `hrx hub status` prints the same
metadata as JSON. Existing local dashboards and `--source` pooling remain usable.

Launch with a canonical `--agent-identity HOST/ROLE/QUALIFIER`. HRX acquires
ownership before starting the harness and automatically starts one collector per
installation. The collector can also be supervised explicitly:

```sh
hrx hub collect
hrx hub collect --once
```

The default 45-second lease renews every 10 seconds. The launcher reserves five
seconds for termination and refuses leases shorter than 20 seconds. Suspension
followed by SIGCONT terminates a hub-coordinated harness. A child-stop event also
terminates a coordinated run instead of suspending its supervisor again; this
handles queued stop events after host recovery. Start or resume a fresh HRX run
to reacquire ownership. A paused/crashed OS cannot enforce a local
deadline, and external GitLab/Pitput systems do not enforce HRX fencing tokens.
Do not interpret the lease as a guarantee against arbitrary orphaned processes
after an uncatchable supervisor kill.

Set `enabled = false` for standalone launches. A manually supervised collector
must also be stopped when changing its configuration. No database or network
connection is required by standalone mode.

## Diagnostics and recovery

Schema v2 compacts completed observation metadata after 30 days, in transactions
of at most 500 runs and 500 receipts once per minute. Active ownership is never
compacted. Minimal run tombstones (host/run identity, agent, harness, terminal
state) and acquisition receipts remain to fence delayed retries. The newest
ingestion receipt for each host remains indefinitely, allowing its pending
outbox to retry after a long outage; older expired receipt retries fail closed.
This bounds retained observation history by age, not the necessary fencing
records. Stale runs are not presumed dead or silently removed.

Snapshots are bounded to 5,000 runs, 1,000 hosts, and 3 MiB of stored observation
metadata. Capacity overflow returns an explicit error rather than a misleading
partial fleet. This first release does not paginate large fleets. Schema v1
upgrades and repeated startup preserve existing records; older binaries reject
the newer schema rather than trying to downgrade it.

Collector state lives beside the monitor directory under the XDG state root:
`hrx/hub/outbox.json`, `collector.lock`, and `collector.log`. It retains one
exact pending batch and bounded fingerprints, not an audit log of every model
event. Archiving old local runs prunes their fingerprints. Capacity errors and
forwarding interruptions appear in the collector log; pending batches survive
restart. A copied/rolled-back checkpoint is rejected instead of silently replayed.
Every delivery first persists its exact pending payload. A failed disk write
blocks publication even if the batch is still in memory; restoring writable
storage permits delivery and checkpoint advancement. The race-enabled forwarder
suite exercises this failure/recovery boundary as well as lost-response replay.
Batch formation respects both the 200-record cap and the 1 MiB encoded request
limit, including JSON escaping overhead. A single oversized observation fails
explicitly instead of creating an endlessly rejected multi-record batch.

An unreachable hub prevents new acquisitions. Existing agents stop at their
conservative ownership deadline; observations retry when connectivity returns.
The dashboard preserves cached rows as unreachable when its hub fetch fails.
Heartbeat freshness never substitutes for model/message activity.

Use `hrx hub revoke --host-id ID --admin-token-file FILE` to revoke an
installation. Revocation stops renewal but preserves its existing expiry window
before another owner can acquire. `hrx hub rotate --host-id ID
--new-token-file FILE --admin-token-file FILE` changes the credential digest;
privately install the matching token on that host and restart its collector.
Do not delete checkpoints or re-enroll a replacement over an active installation.

Enrollment is retryable with the same private token, host ID, and prefixes.
Rotation preserves the installation ID and ingestion checkpoint. Generate a new
private token, run the administrator `rotate` command, install that token at the
host's configured `token_file`, and restart its collector. Until the file is
updated, that host's requests fail authentication; coordinated agents stop when
renewal is denied. Plan rotation around agent shutdown/resume rather than
expecting an already-running launcher to reload credentials.

Revocation is permanent for that installation ID. For replacement, revoke the
old host and enroll a fresh installation with its own state directory and token.
Grant the replacement the intended identity prefixes explicitly. Keep the old
state as history; do not copy its host-ID file, outbox, or token to the replacement.
For a replacement on the same computer, use a new `XDG_STATE_HOME` and
`HRX_CONFIG` path consistently for enrollment and subsequent launches. Native
session data is separate and must be restored explicitly when needed. Existing
ownership still observes its expiry window before the replacement can acquire.

`tests/integration/hub_credentials.py` verifies this workflow through the actual
CLI against a disposable PostgreSQL hub. It covers enrollment
retry, preservation of unrelated config, private token permissions, old-token
rejection after rotation, collection with the new token, permanent revocation,
fresh-installation replacement, and CLI secret redaction:

```sh
python3 tests/integration/hub_credentials.py \
  --fixture-dir /tmp/hrx-twohost-forum --hrx bin/hrx-integration
```
