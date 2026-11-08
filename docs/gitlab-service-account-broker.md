# GitLab service accounts for HRX

The optional `gitlab-service-account-broker` plugin obtains a GitLab service
account token before launching Codex, Claude Code, or OpenCode. The standalone
Python 3.11+ broker uses SQLite to keep a stable agent-to-account mapping. It
supports instance service accounts and group service accounts. It does not need
third-party Python dependencies.

## Everyday use

Configure `~/.config/hrx/config.toml` once (`XDG_CONFIG_HOME` and `HRX_CONFIG`
are supported). This file contains settings and credential paths, never tokens:

```toml
[codex]
provider_name = "litellm-codex"

[permissions]
yolo = true

[plugins.gitlab-service-account-broker]
url = "http://127.0.0.1:8743"
client_tokens_dir = "client-tokens"
enrollment_token_file = "enrollment.token"
```

Relative paths resolve beside config.toml; absolute paths and `~/` are supported.
The provider name selects an existing provider from Codex's config without
changing Codex's normal default. Environment overrides remain available:
`HRX_CODEX_PROVIDER`, `HRX_GITLAB_BROKER_URL`, and
`HRX_GITLAB_BROKER_TOKEN_FILE`. An explicit token-file override must exist;
HRX never silently enrolls a replacement for it.

```sh
hrx codex --model gpt-6.1-sol --gitlab \
  --agent-identity laptop/pitput-mayor-2/agent --pitput

# Keep approval prompts for just this launch.
hrx codex --model gpt-6.1-sol --gitlab \
  --agent-identity laptop/pitput-mayor-2/agent --pitput --no-yolo
```

`--pitput` automatically connects interactive Codex sessions to the Pitput bridge.
HRX starts a private Codex server with this launch's GitLab credentials, connects
the terminal to it, and registers the actual loaded thread with one bridge per
selected board. It does not reuse the shared daemon's credential environment.
Use `--pitput-board pitput-dev` for explicit enrollment/selection; an existing
identity can omit this flag to monitor its already enrolled boards. On exit HRX
stops both server and bridge; `hrx codex --resume UUID` restores the connection.
Old running sessions must exit and resume once after updating HRX.

`--gitlab` is shorthand for `--plugin=gitlab-service-account-broker`. Put HRX
options before the harness passthrough separator `--`. The default-permission
setting maps to Codex `--yolo`, Claude `--dangerously-skip-permissions`, and
OpenCode `--auto` (which still respects explicit denies). Set `yolo = false`
to restore normal permissions by default. No config means no bypass by default.

## Automatic host enrollment

The operator provisions one host enrollment credential in `enrollment.token`
(mode 0600). The broker config authorizes its SHA256 digest under a host policy:

```json
"enrollment_clients": {
  "laptop": {
    "sha256": "REPLACE_WITH_SHA256_OF_HOST_ENROLLMENT_CREDENTIAL",
    "prefix": "laptop/",
    "profile": "developer"
  }
}
```

Policies accept canonical HOST/ROLE/AGENT identities within that host prefix.
The agent cannot select a different profile. `developer` determines token scopes;
GitLab project/group membership comes from the operator's `identity_memberships`
(see [Broker-managed memberships](#broker-managed-memberships)) or manual grants. Existing explicit
`clients` entries remain valid and cannot be overwritten by enrollment.

At the first `--gitlab` launch of a new identity, HRX creates a random agent
client credential, enrolls it through `POST /v1/enroll`, then requests the GitLab
service-account credentials through `POST /v1/credentials`. The broker stores
only the agent client credential's digest. HRX stores the client credential here:

```text
~/.config/hrx/
  config.toml
  enrollment.token
  client-tokens/
    laptop/pitput-mayor-2/agent/broker.token
    laptop/another-agent/agent/broker.token
```

The full identity is preserved as nested directories to avoid collisions across
hosts. Directories are mode 0700; token files are 0600. Existing short identities
such as `pitput-dev-h1` can use `client-tokens/pitput-dev-h1/broker.token`, but
new automatic enrollments require three identity components.

A per-agent lock serializes concurrent enrollment. A `.pending` token is retained
if a response is lost; retrying the same launch reuses it. The broker accepts an
identical enrollment retry, but refuses a different token for an existing identity.
Once activated, `broker.token` is reused without another enrollment request.
Deleting an active token is not a rotation mechanism: re-enrollment with a new
secret requires operator reconciliation. Removing a host policy disables further
broker access for its automatically enrolled agents; already-issued GitLab tokens
must be revoked in GitLab if immediate revocation is needed.

The host enrollment credential and agent broker credential are not injected into
the harness. Only that agent's GitLab credential is injected. This separation is
not OS isolation between processes sharing the same user account.

`--dry-run` contacts no broker, enrolls no identity, and writes no client tokens.
Without the plugin, HRX leaves GitLab authentication unchanged. Config errors and
broker errors fail the launch; credentials are never printed in diagnostics.

## Resuming sessions

From the same project directory:

```sh
hrx codex --continue                 # latest HRX-indexed Codex session here
hrx codex --resume SESSION_ID        # exact indexed session
hrx codex --continue --no-yolo       # keep approval prompts this time
```

For newly indexed sessions, HRX restores model route, reasoning, GitLab plugin
and agent identity, and Pitput guidance. It retrieves current credentials from
the broker instead of storing GitLab secrets in session history. Permissions
come from current config/flags, never the old session. Explicit model, reasoning,
and agent identity options can override saved settings.

Older records do not contain GitLab/Pitput settings; repeat `--gitlab`,
`--agent-identity`, and `--pitput` when resuming those. Codex must trust HRX's
SessionStart hook in `/hooks` to create indexed records. `--continue` cannot
recover an unindexed session; use its native session ID with explicit launch
settings. The dashboard displays indexed session IDs.

New interactive terminal launches print a command on exit that resumes that
specific session, including a shell-quoted `cd` to its project directory:

```text
hrx: Session ended. Resume this session:
  cd '/path/to/project' && hrx codex --resume 'SESSION_ID'
```

HRX stays as a supervisor for these launches and preserves exit codes, signals,
and terminal stop/resume behavior. SessionStart supplies the ID through a private
per-launch receipt, which is removed at exit. If no indexed ID was captured,
HRX explains that instead of suggesting another session. One-shot commands,
redirected/non-terminal launches, and detached sessions do not get this hint.
Already-running sessions launched by older HRX versions cannot acquire this
exit handler retroactively. Killing the supervisor with SIGKILL prevents cleanup
and the hint, as with any process exit handler.

## Operator setup

Check the target GitLab version and license against the
[service accounts API](https://docs.gitlab.com/api/service_accounts/).
Instance account provisioning requires an administrator on Self-Managed or
Dedicated. Group provisioning requires the permissions and service-account
availability documented for your GitLab installation. Account creation alone
does not promise access to a private repository: the broker grants only the
project/group memberships listed in `identity_memberships` (described below), and
anything else must be granted by hand.

Keep the broker on a trusted service host, ideally under a separate OS user from
agents. Create a mode-0700 state directory and mode-0600 configuration and secret
files. The GitLab provisioning credential belongs on the broker host only.
Use a distinct, randomly generated client secret (at least 32 characters) for
each allowed agent identity. Clients receive their own secret file; the broker
configuration contains its SHA-256 hex digest.

For example, generate a client credential without printing it:

```sh
umask 077
mkdir -p "$HOME/.config/hrx"
python3 -c 'import secrets; print(secrets.token_urlsafe(48))' \
  > "$HOME/.config/hrx/broker-client-token"
python3 -c 'import hashlib,pathlib; print(hashlib.sha256(pathlib.Path.home().joinpath(".config/hrx/broker-client-token").read_text().strip().encode()).hexdigest())'
```

Place that digest in a private JSON config, adjusting absolute paths:

```json
{
  "gitlab_url": "https://gitlab.example.com",
  "gitlab_token_file": "/srv/hrx-broker/secrets/gitlab-admin-token",
  "state_dir": "/srv/hrx-broker/state",
  "group_id": 123,
  "scopes": ["read_api"],
  "token_days": 30,
  "identity_profiles": {
    "laptop/hrx-mayor/agent": "developer"
  },
  "ssh_hosts": ["gitlab.example.com"],
  "clients": {
    "laptop/hrx-mayor/agent": "REPLACE_WITH_64_CHARACTER_SHA256_DIGEST"
  }
}
```

Omit `group_id` for instance accounts. Configure the minimum token scopes your
agents need. The operator assigns `identity_profiles` to known client identities:
`observer` issues `read_api` + `read_repository`, and `developer` issues `api` +
`write_repository`. Unassigned identities use the configured `scopes` (legacy
default: `api`). Clients cannot select a profile or request scopes. Profiles
control token scopes, not project membership: memberships come only from
`identity_memberships` or manual grants. No administrator or group-wide access
is added by selecting the developer profile.

The GitLab URL must be an HTTPS origin without a subpath. The database remains
bound to that origin and group. Legacy databases migrate their saved token scopes
automatically. Changing an identity's profile or scopes causes the next launch to
issue a replacement token for the same account. Existing tokens are not revoked;
for an urgent permission reduction, also revoke old tokens in GitLab.

`ssh_hosts` lists additional SSH hostnames belonging to this same GitLab instance.
HRX rewrites `git@HOST:...` and `ssh://git@HOST/...` URLs for these aliases and the
canonical GitLab host to HTTPS. Only list aliases you control for that instance.

```sh
gitlab-service-account-broker --config /srv/hrx-broker/config.json --port 8743
```

The service binds only to `127.0.0.1`. Use a supervised process (launchd/systemd)
and either an SSH tunnel or an HTTPS reverse proxy for remote HRX clients.
HRX rejects plain HTTP except literal loopback IPs. Restrict proxy request sizes
and concurrency; the stdlib HTTP server is intended behind such a proxy for
shared deployments. Requests are authenticated with `Authorization: Bearer ...`
and authorized for the exact identity in `clients`. Redirects are never followed.
Possession of an identity's client secret authorizes fetching its GitLab token;
these files are not an isolation boundary between processes running as the same
OS user. Prompt instructions are also not a security sandbox.

## Broker-managed memberships

`identity_memberships` lets the operator declare which GitLab projects and groups
each agent account belongs to. It lives only in the broker's private config;
there is no client-supplied input, and a credential request carrying anything
other than `identity` is still rejected.

```json
"identity_memberships": {
  "laptop/pitput-dev/mayor": [
    {"project": "acme/pitput", "role": "maintainer"}
  ],
  "laptop/research-lab/": [
    {"project": "acme/research-app", "role": "developer"},
    {"group": "acme/labs", "role": "reporter"}
  ],
  "laptop/research-lab/observer": []
}
```

Schema, validated at startup (any violation fails the broker closed):

- A key is either an exact identity or a prefix ending in `/` made of whole
  path components (such as `laptop/` or `laptop/research-lab/`), matching the
  `prefix` convention of `enrollment_clients`.
- A value is a list of at most 100 rules. Each rule has exactly two keys:
  `project` or `group` (a full GitLab path such as `group/sub/project`) and
  `role`, one of `guest`, `reporter`, `developer`, `maintainer`. `owner`, unknown
  keys, leading/trailing slashes, `.`/`..` segments, `.git` suffixes, percent
  encoding, single-segment project paths, and the same target twice in one list
  are rejected.

Precedence: an exact identity key always wins, even when its list is empty (an
empty list is the way to exempt one identity from a prefix). Otherwise the longest
matching prefix wins. Rules are not merged across keys. No match means no
broker-managed memberships.

### Reconciliation

Every successful `POST /v1/credentials`, whether it mints a token or reuses the
cached one, reconciles that account's direct memberships with the provisioning
credential, using `GET`, `POST`, `PUT`, and `DELETE` on
`/projects/:id/members[/:user_id]` and `/groups/:id/members[/:user_id]` with
URL-encoded paths as ids. For each configured target and each previously granted
target:

- No direct membership: the broker adds one with the configured role.
- A membership the broker granted, still exactly as the broker set it: the broker
  updates its role and expiry if they differ from config, or removes it if the
  rule is no longer configured. A config downgrade or rule removal therefore takes
  effect at the identity's next issuance.
- Any other membership (added by a person, or a broker grant whose role or expiry
  someone has since changed): left exactly as it is, and the broker stops tracking
  it. The broker never upgrades, downgrades, extends, or removes it.

The broker records every grant in the `memberships` table of its state database:
target, account id, and the role and expiry it set. Before each write it commits
the intended values as pending, so if GitLab applies a write but the response is
lost, the next reconciliation still recognises the membership as its own. When
GitLab conclusively refuses a write (HTTP 4xx), the pending claim is dropped. If
someone deletes a membership that config still requires, the next issuance adds it
again. Config is the authority for broker-managed targets. Reconciliation is
idempotent: an up-to-date account costs one `GET` per target and no writes.
Inherited memberships are not inspected. GitLab may refuse a direct grant below an
inherited role; that is logged like any other failure.

### Expiry

Each broker membership's `expires_at` is set to the issued credential's
`expires_at` date. Both are UTC dates computed by the broker. A token is unusable
from its expiry date, so the membership never outlives the token it accompanies:
the broker never rounds the date up. Renewal (one day before expiry) moves both
forward together. If an agent stops launching, its memberships lapse with its
last token. GitLab applies membership expiry by date in its own time zone and
removes expired members with a periodic job, so the actual cut-off can differ from
the token's by part of a day.

### Failure handling

Reconciliation is best effort and never blocks issuance. If GitLab errors, times
out, or returns something unexpected, the broker still returns the credential.
It logs a bounded warning to stderr naming the identity, target, and HTTP status
or a fixed reason; it never logs tokens, request bodies, or GitLab response bodies.
The rest of the work is retried on the identity's next issuance. Reconciliation
gets at most 8 seconds of GitLab I/O per credential request (5 seconds per call).
Targets left when that runs out wait for the next issuance, so a slow GitLab
cannot exhaust HRX's 40-second credential request timeout.
Concurrent requests for the same identity skip reconciliation while one is already
running.

Why not fail closed? A failed grant leaves the agent with less access, never more,
so blocking a launch would turn a GitLab hiccup into an outage without making
anything safer. A failed downgrade or removal leaves an existing broker grant in
place, but that grant still carries the expiry of an already issued credential and
lapses with it. For urgent revocation, remove the membership or revoke the token
in GitLab directly, as for tokens.

Group service accounts can only be added to projects and groups that GitLab allows
for that group's service accounts (normally within the owning group hierarchy).
The provisioning credential needs permission to manage members on every
configured target. The credential response is unchanged by this feature: no
membership details are returned to HRX.

## Launch behavior and credentials

The broker API is `POST /v1/credentials` with `{"identity":"..."}`. It returns
`identity`, `username`, `user_id`, `host`, `token`, `expires_at`, `scopes`,
`profile`, and `ssh_hosts`, with
`Cache-Control: no-store`. HRX injects `GITLAB_TOKEN`, `GITLAB_HOST`, and
`GITLAB_API_HOST` in the harness child only, pins HTTPS, disables glab CI auto-login
and debug logging, and removes competing token variables. Parent environment
and shared glab login configuration are not modified. Agents are primed to use
`glab api user` to check attribution, and never to print or persist credentials.

Git authentication is also automatic and noninteractive. HRX installs a child-only
Git credential helper using `GIT_CONFIG_COUNT`; it reads the token from the child
environment and returns it only for HTTPS on the exact configured host. Store and
erase operations are no-ops. Inherited credential helpers are reset so Keychain,
Git Credential Manager, and personal login helpers are not invoked. Terminal and
askpass prompts are disabled, and glab uses `GLAB_NO_PROMPT=true`. Missing, revoked,
or expired credentials fail instead of opening a password dialog. No SSH key
generation or `glab auth login` is needed.

Long sessions outlive the launch token, so HRX refreshes it without relaunching.
Once the launch token is within 6 hours of its expiry date (recorded at launch), the
Git helper asks the broker for the launch identity's current credential on each
request, and a private `glab` shim placed first on the child's `PATH` (`hrx _glab`)
does the same before running the real `glab`. Before that window the broker is not
called, so a launch still issues exactly one credential. A refreshed credential is used only if
it is for the same host and account as the launch, and only in place of the launch
token itself (recognised by a SHA-256 digest HRX records at launch). A
`GITLAB_TOKEN` the caller sets explicitly is always used as given, so a
deliberately different or invalid token still behaves as that token. If the broker
is unavailable, the launch token is used and an expired one fails as before. Refreshed tokens exist only
in the helper's output or the real `glab` process's environment. A shell's own
`GITLAB_TOKEN` keeps the launch value, so direct API calls with it can still expire.

The variables HRX's helper processes read (`HRX_AGENT_IDENTITY`,
`HRX_GITLAB_CREDENTIAL_HOST`/`_USER`, `HRX_GITLAB_LAUNCH_TOKEN_SHA256`/`_EXPIRES`,
`HRX_GLAB_REAL`) are a typed contract defined once in `internal/app/gitlab_env.go`.

### Troubleshooting an expired token

Symptom: `glab` or `git` fails with HTTP 401 and `invalid_token: Token is expired`
in a session that has been running for a day or more.

- **HRX with refresh (this release onward):** `git` and `glab` fetch a fresh token per
  call. If they still fail, the broker is unreachable or refused this identity: check
  broker health and the identity's client token under `~/.config/hrx/client-tokens/`.
  Do not substitute personal credentials.
- **Commands that read `$GITLAB_TOKEN` directly** (for example `curl` with that
  header) still see the launch value. Use `glab api` instead, which goes through the
  refreshing shim.
- **Older HRX builds:** relaunch the session to receive a new token.

Without refresh, a long session's token can expire mid-task, so Git, merge
checks and exports fail until the credential is refreshed by hand.

Configured SSH-to-HTTPS rewrites live in the child environment. Saved repository
remotes and global Git config remain unchanged. Unrelated SSH hosts are not
rewritten and run in SSH batch mode. Git HTTPS requests to other hosts cannot
obtain this token or fall back to a personal credential helper. This can make
private dependencies on other hosts fail; use a separately configured session
for those. Explicit command-line Git overrides remain possible: this is launcher
configuration, not a sandbox against an agent intentionally changing it.

Priming composes with Yesod and Pitput. Tokens are excluded from HRX launch JSON,
command arguments, generated priming files, and session records. The harness and
its subprocesses can read their token from the environment. Do not direct glab
to another host while carrying this environment: glab credentials are ambient,
not a per-host security sandbox. Existing glab
custom headers and local configuration still apply; use a clean glab config for
agent runtimes if personal configuration adds authentication headers.

The broker stores reusable GitLab tokens **unencrypted** in its private SQLite
database. Protect the service account, disk, backups, and state directory as
credentials. Files use 0600 and the directory 0700. No request bodies, tokens, or
upstream error bodies are logged. The broker administrator credential is never
returned to HRX.

## Lifecycle and recovery

Normal requests reuse the saved account and token. At one day before expiration,
the next launch creates a fresh token for the same account. Previous tokens are
left valid until expiry so existing harnesses keep working. Running harnesses
do not refresh automatically. Removing a client from config and restarting
blocks future retrieval but does not revoke already issued GitLab tokens; revoke
them in GitLab when required. Externally revoked tokens are not automatically
replaced before their recorded expiry.

Before any GitLab POST, the broker commits a `pending` state. A crash, upstream
error, or ambiguous timeout leaves that state intact and returns 503 on retry.
Concurrent requests during provisioning also receive 503 and can retry after
the first completes. This deliberately avoids duplicate account or token creation.

To reconcile a pending row, stop the broker, back up its private database, and
inspect GitLab for the stored deterministic username and account ID. Verify that
it is a service account belonging to this broker. Revoke any token from the failed
attempt whose secret was not saved. For a verified existing account, populate
`user_id`, set `token=''`, `expires_at='1970-01-01'`, and `state='ready'` for that
identity; the next request mints only a token. If GitLab conclusively created no
account, remove only that pending row and retry. Never clear pending state without
checking GitLab first. Database loss similarly requires reconciliation rather
than blindly recreating accounts with matching usernames.

## Verification

```sh
make test-broker
```

This runs Go tests with the race detector and Python broker tests. Coverage
includes argument validation, no-op and dry-run behavior, child environment and
redaction, all three harness priming paths, redirect rejection, identity auth,
concurrent provisioning, persistence/restart, renewal, ambiguous failures, and
private storage. An integration test launches the actual standalone broker and
drives it from HRX against an HTTPS GitLab API fixture. When glab is installed,
an additional test runs `glab api user` against a loopback fixture with the child
credentials. No real GitLab account or token is created by these tests.

Git tests invoke the actual HRX executable as a Git credential helper, cover
cross-host and HTTP rejection, suppress inherited helpers and prompts, verify
SSH URL rewrites, and check that saved configuration remains unchanged. Broker
tests cover profile upgrades/downgrades, legacy database migration, token reuse,
and rejection of client-supplied access profiles. Membership tests use an in-memory
and a loopback members API fixture. They cover config validation, precedence,
idempotent grants with credential expiry, renewal, downgrade and removal, human
memberships left untouched, lost responses, refusals, and best-effort issuance
with redacted logs.

Live deployment and provisioning must still be verified against the chosen
GitLab version, license, provisioning credential, and memberships.
