# Release readiness: what stands between the book and outside readers

The book is written as a hands-on guide for readers who can install hrx. That
requires an open-source release of hrx. This file lists what the release
needs, measured on main at `dbf201f` (2026-11-07). Nothing here has been
decided: licensing and publication are the owner's call.

## Blockers

1. **License.** The repository has no LICENSE file.
2. **Module path.** `go.mod` is `github.com/meshcrawler/hrx`; 139 Go
   files import it. A public module path is needed before anyone can
   `go install` it.
3. **Hard-coded personal infrastructure in the binary.** 59 non-test Go files
   (90 test files) contain internal host names, account names or paths.
   Examples:
   - `internal/app/harnesses.go`: with no `[claude]` table, Claude Code
     defaults to a private gateway URL and an SSH key-fetch command. A public
     build must instead require `[claude.provider]` (as Chapter 2 describes).
   - `internal/app/prime.go`: the agent guide names the owner, private nodes,
     a private dashboard URL and a personal workspace convention.
   - `hrx --help` says "personal harness launcher".
4. **Git history.** 241 of 596 tracked files mention internal names, and so
   does much of the 753-commit history. A public repository should start from
   a cleaned export rather than this history.
5. **The gateway catalog contract.** hrx depends on `/dashboard/models.json`
   and harness capability declarations produced by a private LiteLLM
   repository. Readers need a published schema and a way to produce the file
   from a stock LiteLLM install (book Appendix B promises both).
6. **Private dependencies.** Communication features depend on Pitput (message
   boards) and Yesod (mail and bulletins), both private. Options: release
   them, document a protocol others can implement, or present them as optional
   integrations behind a documented interface. Chapters 5, 9 and 13 depend on
   this choice.
7. **Documentation in the repository.** README.md and docs/ contain internal
   names throughout and mix user docs with plans, evidence and handoffs.

## Not blockers, but worth doing first

- Proxmox backend assumptions (VMID range 3000-3099, tag names, storage names)
  should be configuration, with today's values as defaults.
- `hrx sweep` defaults (`--backup-storage local`) are generic already.
- Screenshots in docs/screenshots show real routes and accounts.

## How the book guards against leaks meanwhile

`book/tools/check.sh scrub` fails on any pattern in `book/scrub-patterns.txt`
inside book text or generated listings. It runs in CI and in the pre-commit
hook. Note that the pattern file itself names internal hosts; replace it with
a CI variable before the repository goes public.
