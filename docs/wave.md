# Wave pane labels

New interactive HRX launches automatically label an unnamed local Wave terminal
pane when Wave's authenticated block/client context can be verified. For example:

- `Mayor | Kimi K3 | OpenCode`
- `AWS Infra | Kimi K3 | OpenCode`
- `Dashboard | Kimi K3 | OpenCode`
- `FinAI | Kimi K3 | OpenCode`

The full identity remains in `hrx:wave` block metadata, alongside distinct block,
client, HRX run and (when already known) native session IDs. No credential values,
provider URLs, prompts, boards, or transcripts are copied. Native session IDs that
are discovered later are not retroactively synchronized in this initial adapter.

Disable in your selected HRX configuration (`HRX_CONFIG` or
`~/.config/hrx/config.toml`):

```toml
[wave]
enabled = false
```

The integration requires valid block/client UUIDs, a matching JWT block claim,
a real interactive terminal, a local `wavesrv` ancestor, and successful live
`wsh getmeta` calls. The JWT is authenticated by Wave, not trusted merely because
HRX can decode it. SSH, CI, ordinary terminals, invalid or expired context,
unavailable tools, and unsupported/older command schemas silently skip labeling.
Finding `wsh` on PATH alone does nothing. The entire optional operation has a
1.2-second timeout (plus at most 100ms pipe cleanup per terminated process).
Nothing launches Wave, opens panes, installs hooks, or alters global settings.

Existing explicit titles and existing `hrx:wave` metadata are preserved. HRX
writes only `frame:text` and `hrx:wave`, via JSON patch input. Concurrent local
HRX launches for one block use a nonblocking lock; the second skips labeling.
Labels persist after exit and represent identity, **not current liveness**.
There are no cleanup writes, so later user edits are never overwritten on exit.
Wave does not expose a metadata compare-and-swap operation: a manual title edit
racing the initial read/write is not atomic. A renamed/reused pane is deliberately
left alone. To opt that pane into labeling again, clear the two keys from its
own terminal before starting the new agent:

```sh
wsh setmeta 'frame:text=null' 'hrx:wave=null'
```

This is initial labeling support. HRX agent-list/focus,
explicit launch-in-Wave, cross-tab navigation, live session-binding updates and
attention badges remain future work. Use Wave's own pane navigation and the HRX
dashboard for now. No lifecycle badges are guessed from process activity.

The command contract and `frame:text` header behavior were checked against
Wave v0.14.5 source (`cmd/wsh/cmd/wshcmd-getmeta.go`, `wshcmd-setmeta.go`,
`pkg/wavejwt/wavejwt.go`, and `frontend/app/block/blockframe-header.tsx`) and the
installed v0.14.5 CLI help. Tests use a fake metadata service and real bounded
subprocesses; validation does not change any running user's pane.

## Terminal header key

Wave 0.14.5 terminal panes use `frame:text`, not `frame:title`. The terminal
header hides the view-name section where `frame:title` would render.
`frame:text` supplies visible header text alongside the existing connection
button (for example `operator@laptop`); it does not rename that connection.
This was verified in both the v0.14.5 source and the installed packaged frontend
`app.asar` (`HeaderTextElems` and `BlockFrame_Header`). It replaces the terminal's
default `viewText` elements, which can include restart/exit/command indicators;
use the terminal itself and HRX dashboard for execution status while labeled.
