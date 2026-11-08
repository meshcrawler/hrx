# Public release process

The public hrx repository is a cleaned export of this private one, with fresh
history. This repository keeps everything, including the material listed in
`exclude.txt`.

```sh
release/export.sh --out /tmp/hrx-public --module github.com/OWNER/hrx \
  --author "Name <email>"
```

The script refuses to run on a dirty tree, drops `exclude.txt` paths, rewrites
the Go module path, fails on any match from the private scrub list
(`~/.config/hrx/release-scrub.txt`, deliberately outside this repository),
requires a LICENSE, builds, vets and runs pure unit tests, scans with gitleaks,
and makes one commit. It never pushes.

Code, tests and public docs use the placeholders in `PLACEHOLDERS.md`.
Settings that used to be hard-coded are now configuration:

| Setting | Config |
|---|---|
| Claude Code gateway | `[claude]`, `[claude.provider]` (required) |
| Compact catalog account labels | `[catalog.labels]` |
| Agent workspace convention | `[agents] workspace_root` (opt-in) |
| Wave terminal role labels | `[wave.role_labels]` |

Live CI lanes take private hosts from CI/CD variables: `HRX_CI_GATEWAY`,
`HRX_COMPAT_INVENTORY` (file), `HRX_CI_GITLAB_HOST`, `HRX_PITPUT_PROJECT_PATH`.

Known follow-up: `internal/catalog/declared.go` declares the
`claude-teams/claude-opus-5-5` route for Claude Code because the gateway does
not yet; remove it once the gateway publishes that declaration.
