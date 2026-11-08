# The hrx book

*Running Agent Teams with hrx*, a practical guide for outside readers. Source is
pandoc Markdown in `src/`; one build produces a PDF (LaTeX), a multi-page web
edition and an EPUB.

- Outline and chapter status: [OUTLINE.md](OUTLINE.md)
- What an open-source release needs: [RELEASE-READINESS.md](RELEASE-READINESS.md)

## Build

```sh
make book            # PDF, web and EPUB into book/out/
make book-pdf        # or book-html, book-epub
make book-check      # scrub, freshness and chapter-list checks
make book-hooks      # install the pre-commit hook
```

Requirements: pandoc 3.6 or later. The PDF also needs XeLaTeX with
`tcolorbox`, `titlesec`, `fvextra`, `tex-gyre` and `sourcecodepro`; on a basic
TeX Live install, `tlmgr --usermode install` those. CI uses the pinned
`pandoc/extra` image, so a clean build never depends on a local TeX setup.

## Writing

- One file per chapter in `src/`, listed in order in `chapters.txt`.
- Give each chapter an explicit id: `# Title {#id}`. Link with `[Chapter 2](#id)`.
- Parts: `# Part II: Working Every Day {.part .unnumbered}` in its own file.
- Callouts: `::: note`, `::: tip`, `::: warning`, and `::: sidebar` whose first
  heading is the sidebar title. `filters/callouts.lua` renders them for every
  format.
- hrx output is never pasted. Add the command to `tools/gen-listings.sh` and
  include the result: ```` ```{.text include="generated/NAME.txt"} ````.
- Update `sources.yaml` when a chapter starts documenting new code.

### Placeholders

The book is for outside readers. Never use real infrastructure; the scrub
check enforces this.

| Use | For |
|---|---|
| `https://gateway.example.com/v1` | The model gateway |
| `acme` | A provider account on the gateway (`or-acme/...`, `tfy-acme/...`, `fw-acme-...`) |
| `laptop/docs/agent` | An agent identity |
| `team-board` | A shared message board |
| `pve1`, `pve2` | Proxmox nodes |
| `https://example.com/hrx.git` | The repository |

## Checks

| Check | What fails it | Where it runs |
|---|---|---|
| `chapters` | `chapters.txt` names a missing file | pre-commit, CI |
| `scrub` | Book text or a listing matches `scrub-patterns.txt` | pre-commit, CI |
| `fresh` | A generated listing differs from what current hrx prints | pre-commit (when CLI code changes), CI |
| build | pandoc or LaTeX fails on any format | CI |

The checks are deterministic and blocking. A model-based review that suggests
which chapters a merge request has made stale is planned as an advisory,
non-blocking CI job driven by `sources.yaml`. It belongs in CI, not in the
pre-commit hook: most commits here come from agents, and a hook that calls a
model on every commit would be slow, costly, need gateway credentials on every
machine, and could block a commit on a non-deterministic answer.
