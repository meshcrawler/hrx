# Frozen case 5: owned admission crash boundaries

Status: implemented source and pure checks; native acceptance NOT_RUN. The real
matrix must pass before any recovery or milestone completion is claimed.

The existing admission-binary wrapper crashes before calling the real owner or
after an actual admitted reply. The ACK proxy separately loses transport before
commit, after successful commit and after an ACK response. These probes remain
required and do not stand in for the owner's journal/submit gap.

The test supplies the narrowly typed `RuntimeOptions.AttentionTestHook` through an
unexported HRX runtime field. Normal launches supply nil. There is no public flag,
environment fault mode, global hook, extra receive owner or synthetic native
response. The callback emits only phase, thread/delivery/digest, attempt and owned
response ordinal, then waits for cancellation when the actual runtime stops.

The prepared callback runs after durable `prepared_no_write_v1` and before durable
`submitting`. Every native queue submission follows its durable submitting stage.
Production replay can therefore classify only that versioned prepared state,
without a later submitting stage, as not-sent. The callback itself grants no
recovery authority. The explicit next attempt must handle once after resume.
Legacy prepared, submitting, uncertain and no-match states cannot use that proof.

The queue-response callback runs after the actual owned queue/add response was
consumed and before admitted-journal commit. The fixture pauses its scripted
provider before any tool handling, stops the real runtime, then resumes the same
native session. Recovery must find positive exact queue/history client ID and
payload evidence through the sole owner. Queue/add is not idempotent in pinned
Codex0.163.0; an ambiguous request is never resent. Positive recovery retains the
original admission binding and identifies the actual query witness separately as
`recovery_source`, `recovery_binding` and `recovery_ordinal`. Replays authenticate
the current runtime while preserving that historical witness and report digest.

Each boundary requires eventual one handling effect, not merely source retention
or recovered admission. If an interrupted native turn remains inert, the fixed
case fails. No-match, malformed/incomplete history, duplicate IDs, mismatched
payload, unsupported recovery or missing fixtures also prevent acceptance.
