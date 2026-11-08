# hrx brand

The first brand assets for hrx, built as a sibling in the same visual system
as Yesod and Pitput — one letterform-study grammar, one shared palette
family, each tool with its own mark and its own illustrated world.

## Palette: Cobalt & Citrus

hrx uses the Cobalt & Citrus palette: cool porcelain paper, deep navy ink, an
electric-blue signal color, and a citrus accent reserved for small emphasis.
Token values live in `book/latex/preamble.tex` (`hrxpaper`, `hrxink`,
`hrxsignal`, `hrxcitrus`, `hrxcitrustext`, `hrxpanel`, `hrxmuted`, `hrxline`),
matching the `cobalt` entry in the suite's shared `palettes.json`.

## Mark: Convergence

`marks/convergence.svg` — three harness rails taper and funnel into one wide
gateway bar. Unlike Yesod's letterform studies (drawn from יסוד's own
spelling), hrx has no Hebrew-word basis, so the mark is drawn from what hrx
*does*: route several harnesses (Codex, Claude Code, OpenCode) through one
model gateway. `marks/convergence-white.pdf` is the on-dark variant used on
the book cover, next to the lowercase "hrx" wordmark.

`marks/socket.svg` is a rejected alternate (a launcher body with three pins)
— stronger as a standalone avatar, but less specific to what hrx does. Kept
for reference.

## Cover: `cover-art.png`

A pencil-sketch illustration, same register as Yesod's factory-floor cover
but its own scene and its own world: an imagined late-19th-century shtetl
communications workshop. An operator works a wooden console; three distinct
antique rigs (telegraph key, hand-cranked telephone cabinet, an
electromechanical dial instrument — standing in for the three harnesses)
each run a cable bundle down into one central manual patch panel. Generated
via an AI image model from a written scene description; two more generic
"tech control room" alternates were generated first and rejected in favor
of this one, for being specific to what hrx actually does rather than
reading as a stock illustration.

The illustration fills the top ~65% of a 16:9 frame; the bottom is left
nearly blank on purpose, so `book/latex/preamble.tex`'s `\maketitle` override
can composite a solid Cobalt & Citrus band under it with the wordmark, mark,
subtitle and edition stamp — same construction as Yesod's title page.
