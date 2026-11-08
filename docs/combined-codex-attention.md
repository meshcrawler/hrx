# Combined Codex attention preview

Use `hrx codex --pitput --pitput-board BOARD --pitput-combined-preview` against
a compatible service with `combined_attention_preview = true` explicitly enabled.
The accepted evidence comes from disposable Linux infrastructure; deployment is
a separate operational action. The default launcher and saved policies keep
existing behavior. See the [server-side filtering guide](server-side-filtering.md)
for launch and handling examples.
This opt-in selects one identity consumer for one board, DMs and typed asks;
DM-disabled and multiple-board requests fail explicitly. OpenCode and interactive
Claude now have separate backend parity;
the controller details below describe Codex.

The existing coverage controller remains the sole native receive owner.
`pitput/attention/admit` on its private auxiliary ingress validates exact current
runtime/session binding, stable combined delivery ID and immutable payload hash.
The normal router/fence owns `thread/queue/add`. Exact attempt replay returns its persisted outcome; only an explicit next
attempt after definite non-admission advances. Attempt numbers use the existing
Admission numeric range, with no arbitrary lifetime retry count. Preparation and actual response
classification are appended to the existing coverage journal. A replay returns
historical admission with the original admission binding while separately
validating the current runtime. A prepared/ambiguous operation is never blindly
resubmitted. Queue admission is neither source read, handling nor ask resolution.

Pitput's existing once-per-session compact protocol and admission publication
outbox handle presentation, exact read, explicit ACK and reporting. Connection
nonce/socket rotation does not rename retained references. Unsupported attach or
reset states preserve work and fail explicitly. Old clients or services cannot
silently fall back from the opted-in combined protocol.

The manual Linux `pitput-combined-launch` job requires
`HRX_PITPUT_COMBINED=1`, the existing digest-pinned `HRX_CI_IMAGE`, and a full
`HRX_PITPUT_SOURCE_SHA`. It builds/validates that wheel, starts disposable
PostgreSQL/Pitput and scripted loopback Responses, exercises the production
launcher/controller, and requires same-native-session resume with real pending
work, installed complete read/reply/ACK helpers, privacy denial, separate reply
and unresolved ask state, known original report attribution, native post-tool
completion and unforced owned cleanup. No paid inference is used. Pure/mock
contracts and Python lifecycle fixtures do not replace this proof.
