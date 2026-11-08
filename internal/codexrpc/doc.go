/*
Package codexrpc is the independent receiver/demultiplexer foundation for a future
Codex lifecycle observer. It is dormant: nothing in internal/app imports it.
It has no dialer, native process launcher, subscription, resume, replay, handling
ACK, journal, lifecycle reducer, or production activation switch.

# Source basis

The integration reviewed here starts at HRX main
0216cabae1519b2cf4ae05926db28399ff659b02. Its
internal/app/pitput_runtime.go:219-250 discovery RPC discards unmatched frames;
253-271 initializes the private connection; 287-323 reads loaded threads;
476-528 binds and starts bridges; 529-545 then watches processes without
continuously draining that RPC connection. New cannot simply wrap an existing
partly consumed connection and declare its past interval covered.

The pinned native source is official rust-v0.163.0, commit
83086188ae0614bd882818ff1aa23f3194962012:

  - app-server/src/request_processors/thread_processor.rs:1500-1607 subscribes
    the ordinary root's creating connection; 1652-1669 broadcasts thread/started.
  - app-server/src/request_processors/thread_lifecycle.rs:323-387 emits detailed
    events to per-thread subscribers. A global notification is not subscription.
  - app-server/src/thread_state.rs:62-68 and thread_lifecycle.rs:846-969 provide
    an ordered warm-resume join/snapshot, with actual side effects, not passive
    subscription or historical lifecycle-notification replay.
  - app-server-protocol/src/protocol/v2/item.rs:430-432,1343-1349,1421-1427
    exposes compaction identity and lifecycle timestamps, but no cause or replay
    cursor. common.rs:2078-2095 adds emittedAtMs, not a coverage watermark.
  - app-server/src/thread_state.rs:557-674 supports additive subscribers and
    per-connection removal; queue connections do not inherently steal another
    connection's bytes. tui/src/session_queue_commands.rs:27-127 enqueues work,
    without establishing observer coverage.
  - app-server/src/transport.rs:140-177 can disconnect a slow reader on outbound
    queue overflow. Silently ignoring events is not a valid recovery strategy.

These are version-matched source contracts, not a native binary-build or
production acceptance attestation. The earlier monitoring audit is Pitput
/b/hrx-dev/monitoring-roadmap-20/1242; the assignment is /1270.

# Ownership and implementable host interface

Profiles owns fresh native creation, initialization, root selection, baseline,
and the gate that admits the first UI or queued work. Monitoring owns stream
draining, loss evidence, reconnect classification and the eventual lifecycle
reducer. Lead retains Darwin process ownership. This foundation chooses none of
their contracts.

The proposed profiles-to-monitoring handoff must supply:

  - a private endpoint/native-home/version binding and exact native thread and
    session identities;
  - an exclusively owned, continuously drained Wire and a fresh connection
    generation (not a placeholder monitor ID);
  - an explicit admission mode and its evidence: fresh-creator subscription
    before any admitted work, or a separately approved ordered warm-rejoin
    snapshot contract with its preceding UNKNOWN interval;
  - the owner of admission stop, durable invalidation, reconnect authorization
    and final shutdown. No implicit transfer back to legacy ReadJSON is allowed.

Client.Call and Notify are the sole write path after handoff; Client.Next is the
sole incoming event path. Reply forwards a caller-selected response to a received
server request, never automatically approving a permission or dynamic tool.
It refuses requests from a predecessor Client, even if public generation text is
changed. The host must bound and authorize its server-request work; receiving a
request is not permission to grant it.

New only starts the local reader. Its transport_open state is deliberately NOT
OBSERVING. Message.Ordinal and Response.Ordinal share local arrival order, so a
controller can locate an RPC reply relative to buffered events. Neither is a
native sequence, subscription receipt, replay cursor or missing-boundary detector.
The foundation does not pair compaction items or count completed lifecycles.

Loss and coverage controller (not activated in this change)

The future host controller must durably start UNKNOWN, with a fresh generation.
It may enter OBSERVING only after profiles' approved subscription/baseline
evidence and admission gate have been checked. An open socket, thread/read,
global thread/started, queue acceptance or Pitput handling ACK is insufficient.

Read/write failure, malformed or unowned response, unresolved-RPC cancellation,
overflow, liveness timeout, shutdown and generation replacement invalidate
coverage. Done and the terminal Snapshot provide an out-of-band failure fence;
the buffered prefix is discarded with a visible count, not replayed as current
evidence. A message already returned by Next still requires the host's
generation/coverage fence at semantic commit. A transport Snapshot by itself
cannot implement that durable transaction.

Before activation the controller needs one serialized generation/journal owner:
loss must close semantic admission before further lifecycle results can commit,
and persist UNKNOWN plus the observed-prefix checkpoint/reason. A failed journal
write must surface as unverified invalidation, never a durable OBSERVING claim.
Retrying invalidation does not restore coverage. This durable controller and its
atomic commit fence are an explicit integration dependency, not supplied here.

Never match an item/completed after a gap to a predecessor's item/started and
claim a covered pair. Preserve an explicit UNKNOWN interval; partial/unpaired
history remains partial. Public cause is unknown unless separate authoritative
native evidence supplies it. Handling ACK must not complete a lifecycle.

Reconnect is a new Wire and generation supplied by the authorized connection
owner, not an automatic retry inside Client. The old generation stays terminal.
The native API has no demonstrated side-effect-free subscription/replay cursor
that repairs a gap. If the profiles contract cannot authorize a new subscription
and a baseline, reconnect can restore transport only: coverage stays UNKNOWN.
Warm resume must never be hidden as a passive resubscribe or untouched fresh
session. No historical event is synthesized to bridge an uncovered interval.

# Boundedness, liveness and shutdown

Default limits are 4 MiB per frame, 128 buffered messages and 8 MiB of buffered
payload/metadata bytes. These are explicit fail-closed transport limits, not
evidence that every native workload fits them. Wire must enforce frame bounds
before unbounded allocation; Client checks again and preserves original JSON.
The package retains at most one control RPC and no unbounded request/tombstone
map. Concurrent Call returns ErrBusy. Notification/server-request buffering never
blocks the reader waiting on a control response; overflow terminates the stream.
Remote error bodies are returned privately, while error classifications contain
no remote text. The package has no payload logger.

The separate /1285 change supplies a DORMANT NewWebSocketWire adapter using the
existing Gorilla v1.5.3 dependency. It accepts an already-owned *websocket.Conn;
there is no dialer, native creation, initialization, thread subscription, resume
or automatic reconnect. Invalid configuration leaves the socket caller-owned.
Successful construction transfers all socket I/O/handler/deadline ownership.
Immediately hand it to New; no competing or partly consumed legacy reader is
permitted. Profiles still owns creation/admission and the approved handoff.

Only Wire.Read invokes NextReader, so control handlers run on that same reader.
Before payload allocation it sets Gorilla's frame/message read limit, and uses
a limit+one sentinel reader to bound decompressed output too. It accepts text
only and rejects invalid UTF-8. Defaults are 4 MiB per message, 45s read timeout,
10s data-write timeout, 5s control-write timeout, 15s ping interval and 10s pong
timeout. Positive options are configurable; the message cap is at most 64 MiB,
control timeout cannot exceed pong timeout, and read timeout must exceed
ping interval plus pong timeout. Each Read's requested byte limit can tighten
the configured cap. Raw errors/control payloads are never logged or returned.

One worker sends nonce/sequence-correlated WebSocket pings, never JSON/model
RPC keepalives. Only a matching, nonduplicate pong received before the exact
challenge deadline renews the read deadline and satisfies the ping. Arbitrary
application traffic, peer pings, unsolicited/stale/late pongs cannot hide missing
pongs. Quiet application traffic remains healthy while control liveness works.
The worker's independent pong deadline also closes a half-open/non-drained
connection; it does not depend on a blocked NextReader returning first.
One bounded writer token serializes adapter-owned data and ping/pong/close
writes (including time spent waiting for that token). Gorilla itself additionally
serializes library-generated protocol-error close frames with its concurrent-safe
WriteControl primitive. Read-side deadline/handler setters never run from the
heartbeat or data-writer goroutine. Cancellation callbacks close the owned socket
and are joined before the operation receipt ends; no stale successful-operation
callback can later invalidate a successor call.

Pinned native control-frame support is source-backed, not assumed from global
notifications: official rust-v0.163.0/83086188,
app-server-transport/src/transport/websocket.rs:172-214 runs independent inbound
and outbound tasks; 249-285 maps both Axum and Tungstenite ping payloads and pong
construction; 290-328 serializes control/data writes; 331-389 explicitly echoes
Ping(payload) via the bounded control queue and closes if that queue is full.
The Unix control socket uses that same run_websocket_connection at
transport/unix_socket.rs:165-211. Its native source test
transport/unix_socket_tests.rs:185-194 asserts the exact "check" ping/pong echo.
Source supports the protocol; finite latency under actual load is NOT guaranteed
by these lines. A delayed/full native queue must cause UNKNOWN, not a passing
coverage claim. CI-only socket tests exercise real Gorilla framing, compressed
size limits, quiet pongs, half-open peers with app traffic and cancellation.
A separately required pinned GitLab-native test launches ONLY a disposable
app-server/control socket and verifies repeated matching pongs across quiet read
deadlines and joined cleanup without initialization, thread creation or a model
request. That narrow acceptance does not prove subscription, Darwin runtime,
native reconnect/admission, loaded-server latency or production coverage.

First loss is terminal. WebSocketWire.Stop reports actual reader, writers,
heartbeat and socket-Close receipts with the retained fixed cause. Close error,
expired deadline or any unjoined operation remains ErrShutdownUnverified; no
replacement connection or generation is fabricated. A malicious/broken socket
implementation that ignores cancellation/Close cannot be certified by Stop.
The production controller and approved admission/handoff remain required.

Shutdown order: profiles stops new work/admission; controller fences and
durably invalidates coverage; Client.Stop closes its owned transport and verifies
reader, in-flight writer and close completion; only then may the existing HRX
process cleanup proceed. Stop reports ErrShutdownUnverified for a context
deadline, unjoined worker or close error. It never claims a clean successor or
recovered coverage. A broken adapter that ignores Close/cancellation may outlive
Stop; the failed receipt is retained, not relabeled success or fixed by starting
a second consumer.

Queue coexistence needs one reader of this socket, NOT an exclusive server
client. UI and codex queue may use other connections. Their work must still obey
profiles' admission and exact thread binding. A queue receipt is not native
completion, and queue shutdown must not remove this receiver's ownership.

Exact integration changes deferred until those interfaces are accepted:

  - replace pitputRPC.call's direct ReadJSON with Call/Notify on the stream owner;
  - arrange reader ownership before initialize and native creation/admission;
  - replace post-discovery read abandonment with continuous Next and the fenced
    controller/reducer, not an additional parallel reader;
  - change startPitputRuntime cleanup to invalidate/join the receiver before
    removing its native server/socket; retain all failure receipts;
  - keep session metrics/primer decisions gated by explicit covered generation,
    not the existing transcript counts or bridge ACK watermark.

# Acceptance gates

The in-memory tests here establish ordered notifications during RPC, server
requests, remote errors, queue-connection separation, mid-turn EOF/pending RPC
failure, fresh-generation reconnect refusal, byte/count overflow, malformed/
duplicate/trailing frames, cancellation without retry, serialized writes and
honest shutdown/deadline/close-error receipts. They make no native subscription,
actual compaction or durable-journal claim.

Before production activation require deterministic controller/adapter tests for
all those cases plus native/control keepalive half-open timeout, durable
invalidation failure/retry, stale generation racing semantic commit, missed start
with later completion, reconnect subscription refusal and incomplete snapshots,
and shutdown with events/RPC pending. Pinned GitLab native acceptance must then
prove creator subscription and pre-first-work baseline, queue/UI coexistence,
real event framing, mid-turn disconnect/rejoin gap labels, finite read/write/
liveness limits and cleanup. Darwin/native/DB/PTY/heavy work stays on GitLab;
paid model semantics need separate authorization. Nothing here activates a
running agent, replays peer events or takes over profiles/lead work.
*/
package codexrpc
