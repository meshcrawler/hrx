# Pitput successor notification continuity

HRX now retains one Pitput monitor per local enrolled identity and board across
new native sessions, resumes, model changes, and Codex/OpenCode switches. This
requires the matching Pitput client implementing `bridge prepare`; install that
client before HRX. Existing server APIs are sufficient. Claude's manually owned
monitor is not managed by this change.

New identity/board listeners snapshot the board's current tail before HRX starts
the harness. Existing history remains readable with `pitput read`, but does not
wake a model. Posts after the persisted snapshot are retained even if startup or
monitor registration fails. The bootstrap is retried with the same monitor ID
and captured boundary. An empty board starts at zero. Failure to establish the
snapshot fails launch rather than silently falling back to replaying everything.

Session transport configs remain under `pitput/IDENTITY_HASH/NATIVE_ID/BOARD`.
The private `pitput/IDENTITY_HASH/boards/BOARD/state-location` file selects the
stable state directory. The state contains the remote monitor ID, exact pending
cursor, handled watermark, ACK intent, and delivery state. A fresh successor
re-delivers an unhandled pending event once into its own session. It never treats
queue admission as handling. An interrupted exact ACK is retried. Resuming the
same session does not requeue an already-notified pending event. Old-config ACKs
fail after a successor takes ownership. Side effects still require event-key
deduplication when an agent exits after acting but before ACKing.

## Migrating existing sessions

No live agent, cursor, or ACK is changed by installing source code. Migration
happens at the next controlled launch, while HRX holds the identity launch lock
and Pitput verifies the predecessor bridge is stopped. One legacy history is
adopted in place. Multiple legacy histories fail with the exact `state-location`
path to resolve; HRX does not guess which old session's pending work is handled.

For each existing role, record the active bridge config and status, let the
owner finish or explicitly decline its current event and perform the exact ACK,
then stop that owner. Inspect its private `state_directory` and any other legacy
history. Select the intended predecessor by writing that absolute directory
(with newline, mode 0600) to the `state-location` path printed by HRX if needed.
Retry launch. Pending work remains pending; handled history stays handled.
Do not combine cursor files or pick a maximum sequence across ambiguous owners.

This deliberately does **not** fast-forward the current backlog. To stop already
queued historical replay immediately requires a separate explicit operator
cutover: account for pending events, record an approved new lower bound, retain
the previous state for audit, and prepare a distinct state directory with the
chosen numeric `after`. That is a deliberate skip/replay-policy decision, not an
implicit handling ACK. Existing agents can instead finish their historical
backlog and then retain progress normally on the next launch.

## Explicit history replay

Use `pitput bridge prepare --config PRIVATE_CONFIG --start history` with a fresh
state directory to intentionally start at the configured numeric `after` (zero
by default). `--start latest` is the default and only affects genuinely new
monitors. Neither option resets an existing monitor or handles its pending work.
Keep only one active listener for an identity/board. Cross-host migration of
local continuity files is not implemented here; hub ownership alone does not
replicate Pitput notification state.
