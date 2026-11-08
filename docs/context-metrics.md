# Native context and compaction observations

## Launch-time model limits

For gateway routes that borrow another model's Codex instruction/tool template,
HRX requires a positive gateway `max_input_tokens` value. Unknown input capacity
does not become a 32K default and does not inherit the borrowed model's capacity:
launch (including dry-run and resume) fails with an actionable error. Unrelated
routes with unknown input capacity are omitted from the generated catalog with
a warning; they do not prevent a known-capacity route from launching.

The cached model keeps input allowance, total `context_length`, and output limit
separate. Codex's generated history window uses the input allowance, capped by
the total capacity when declared. For example, 1,050,000 total / 922,000 input /
128,000 output produces a 922,000-token history window, not a claim that the
model's total capacity is 922,000. Native exact-match templates keep their own
Codex metadata; these checks apply to borrowed templates.

After correcting gateway metadata, relaunch the harness to load a newly generated
catalog. Existing processes may retain their old limits. The observed effective
window can be smaller because Codex reserves headroom. Launch metadata does not
establish provider pricing, caching discounts, or actual long-context acceptance.

The dashboard's Context column is a **latest native estimate**, not cumulative
billing usage. It shows `~N%` only when the same native observation supplies a
positive effective limit; otherwise it shows estimated tokens with an unknown
limit. Hover for exact counts, source/version and the observation timestamp.
Readings older than two minutes or from an unavailable host are marked stale.
This is not an exact measure of unsent prompts, queued tools or in-flight output.

Compactions shows **N observed**, never an asserted lifetime total. Local session
records may have been pruned, imported or rewritten. Unknown is distinct from
zero observed events. Missing/unsupported native metadata does not affect agent
launches, presence or the other dashboard columns.

## Verified sources

| Harness | Evidence / version | Context estimate | Completed compaction |
| --- | --- | --- | --- |
| Codex | Local structured rollout records, codex-cli 0.163.0 | `event_msg/token_count.payload.info.last_token_usage.total_tokens`; effective denominator from that record's `model_context_window`. Lifetime totals are never displayed as context. | Persisted `compacted` record, deduplicated by `window_id`, then `compaction_response_id`, then event timestamp for older shapes. |
| Claude Code | Local structured project JSONL, Claude 2.1.306 | Latest assistant `message.usage.input_tokens + cache_read_input_tokens + cache_creation_input_tokens`. After compaction, `compactMetadata.postTokens` if present. No verified effective limit in these records, so no percentage. | `type: system`, `subtype: compact_boundary`, deduplicated by `uuid`. Requests/pre-compaction events do not count. |
| OpenCode | Installed v2.1.2 investigated | **Unsupported** in this adapter. | **Unsupported**. A `part.type=compaction` record is a request, not proof of successful completion. |

Codex's official [app-server documentation](https://developers.openai.com/codex/app-server/)
documents token usage updates and context-compaction items. This implementation
reads the persisted native metadata rather than connecting another client to a
live server. The parsed local shapes above were checked against installed native
records; parser tests contain synthetic examples of those shapes with canary
content, not copies of real transcripts. These schemas are not a promise of
compatibility with every future harness version.

OpenCode follow-up: establish a version-pinned read-only session API binding with
successful `session.compacted` evidence and a verified effective context limit.
Its local SQLite records contain assistant usage and compaction requests, but
inspecting those alone does not establish the current version's successful
completion semantics. Do not turn requests, failed summaries or token drops into
completed compactions. No adapter is enabled by inference from another version.

## Collection, privacy and identity

New launches privately capture the actual `CODEX_HOME` or `CLAUDE_CONFIG_DIR`
(defaulting to the respective directory under launch `HOME`). Only these storage
roots are retained, not arbitrary environment variables. Existing monitor files
without a source binding remain unknown until the agent is relaunched. Legacy
process recovery can capture these roots from the same-user process environment
when it already has a trusted HRX launch/session association. No hooks are added
to existing sessions.

Only the bound native session's uniquely named file under `sessions` or `projects`
is read. Codex must also match `session_meta.id`; every Claude event must match
`sessionId`, and Claude sidechains/subagent directories are excluded.
Codex history events predating the native session metadata timestamp are excluded
so a fork does not count inherited parent compactions as its own. Duplicate
file matches are unavailable rather than guessed. Archived/missing files and
remote-only sessions without local native records remain unknown. Separate HRX
run files and host-local state isolate hosts, installations and sessions.

The read-only scanner saves its byte offset and hashed successful-event IDs in
the existing private monitor run record. Each run derives an absolute observed
count from its native session history: resume never adds the predecessor run's
count, and restart/replay never increments an already observed event. A resumed
run must receive a native context observation after its launch time before
displaying context or a denominator. A model-change event or completed compaction
invalidates the preceding context estimate until a new valid reading arrives.
Codex status/rate-limit notifications can repeat the preceding inference's token
usage. A private hash of latest/lifetime token totals distinguishes those repeats
from new inference observations; it neither exports cumulative tokens nor treats
them as context occupancy. Repeated reports preserve the original reading age
and cannot restore pre-compaction context. A changed limit without a new
inference invalidates the reading until native usage advances.
Completed runs ignore native events after their recorded exit time. Runs whose
exit was inferred without an exact timestamp preserve their last observations.
After reaching the end of an explicitly completed run's records, its metrics
freeze so a successor resume cannot alter them through later malformed records.

Work is bounded to 4 MiB per run and 16 MiB per monitor snapshot, with a 500 ms
collection deadline checked between records. A single native record is accepted
only up to 2 MiB; larger records are skipped with bounded progress and mark the
history partial. Discovery examines at most 20,000 directory entries, refuses a
partial/ambiguous search, and retries missing files at most every 30 seconds.
There is a 1,024 distinct compaction-event cap per run; reaching it yields a lower
bound marked partial. Very large histories may take multiple refreshes to catch
up. Native source files are opened as regular files, read-only, with final-link
following disabled; symlink traversal/escape is rejected.

The scanner assumes native append-only records. Replacement, shrinkage, or a
changed initial prefix resets its cursor and marks history partial. An in-place
edit of already scanned middle records which preserves file identity, prefix and
size cannot be reconstructed; this is another reason counts are observed-only.
Incomplete final lines are retried after append; malformed records are skipped,
invalidate the prior context estimate, and make coverage partial. The two-minute
stale threshold is display freshness, not evidence that an idle session changed.
Native timestamps and resume cutoffs require reasonably synchronized local clocks.

Only allowlisted counts, availability, timestamps, source/version and session ID
enter snapshot APIs or the hub. Source paths, offsets, hashes, transcript content,
prompts, tool payloads and credentials stay off those APIs. JSONL must be read
locally to extract structured metadata, but no transcript is copied or logged.
The collector never starts inference, requests compaction, modifies native
storage, connects to a paid model endpoint, or changes a live harness session.

## Compatibility and rollout

The optional hub is integrated into main. The dashboard uses relative asset/API paths and
bookmarkable status filters, so it can be served under a subpath such as `/hrx/`.
Standalone use still requires no hub. The service templates are documentation;
merging them does not install or restart a deployed service.

`metrics` is an optional field in version-1 monitor records. New dashboards/hubs
accept old records with no metrics, and older dashboard frontends ignore the new
field. Hub storage already persists allowlisted observation JSON, so no database
migration is required. **Upgrade a hub server before sending it new collector
records**: an old hub uses strict request decoding and rejects unknown fields.
The upgraded hub validates availability, source, session association and numeric
bounds. Pooled feeds preserve per-host metadata and freshness.

To upgrade an existing dashboard deployment:

1. Build a candidate without replacing the stable launcher. If using the hub,
   upgrade/verify its API first against a disposable source.
2. Upgrade the host collector and dashboard viewer using their existing service
   configuration, transport and subpath. Preserve
   `?status=running`, system theme, authentication and restart policies.
3. Newly launched/resumed agents capture the storage binding. Existing agents
   without a binding remain Unknown and can finish normally; do not restart them
   merely to populate metrics.
4. Verify a known Codex reading, Claude tokens without a fabricated percentage,
   OpenCode Unknown, source outage freshness, and that credentials/paths/content
   are absent from API responses. Roll back viewer/collector binaries if needed;
   native sessions and their files are untouched. Remove new collectors before
   rolling an optional hub back to a decoder that predates `metrics`.

Private run records now allow up to 256 KiB to hold the bounded compaction dedup
state. Old launchers had a 16 KiB limit: rolling back across this feature may leave
large new run records unreadable to the old monitor. Retain the compatible
collector or archive those completed monitor records before such a rollback;
this does not affect native harness session files or their resume capability.

## Validation

`go test -race ./internal/monitor ./internal/hub` checks native-shape parsing,
restart/resume, duplicate completion events, current versus cumulative usage,
model changes, missing limits, malformed/truncated/oversized/rotated files,
sidechain/session isolation, and collector-to-HTTP serialization/redaction.
`python3 tests/integration/hub_store.py` runs disposable PostgreSQL round trips
with mixed old/new observations and idempotent replay. The existing Playwright
dashboard test additionally checks context percentages/unknown/stale/observed
labels and live compaction updates, preserving sorting, filters, compact layout,
system themes, mobile overflow and text escaping. All use isolated fixtures;
no paid inference or production-session mutation is required.
