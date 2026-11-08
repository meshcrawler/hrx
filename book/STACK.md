# Book stack: how to make another book in this style

This is a recipe for an agent (or a person) who wants to produce a technical
book that looks and builds like *Running Agent Teams with hrx*: a practical,
O'Reilly-style guide written in Markdown, rendered to a typeset PDF, a
multi-page website and an EPUB from one source. Copy `book/` into the new
repository and change the parts marked **change** below.

## Toolchain

| Piece | Choice | Why |
|---|---|---|
| Source format | Pandoc Markdown, one file per chapter | Plain text, diffable, agents write it well |
| Converter | pandoc 3.6+ (CI pins `pandoc/extra:3.9` by digest) | One source, every format |
| PDF engine | XeLaTeX, `scrbook` (KOMA-Script) class | Real typesetting; XeLaTeX avoids LuaLaTeX's slow system-font scan on macOS |
| Web | pandoc `chunkedhtml`, one page per chapter, `--chunk-template='%i.html'` | Clean URLs from chapter ids; built-in navigation and `sitemap.json` |
| EPUB | pandoc EPUB 3, `--split-level=1`, same CSS as the web edition | Free once the HTML styling exists |
| Extensions | One Lua filter, `filters/callouts.lua` | Callouts, sidebars, part pages and generated-listing includes for all formats |
| Code highlighting | pandoc `--syntax-highlighting=tango` | Readable in print and on screen |
| Checks | `tools/check.sh` (bash) | Deterministic: chapter list, scrub for private names, freshness of generated listings |
| CI | GitLab: `book-check` (blocking) and `book-build` (artifacts) | See `.gitlab-ci.yml` |

## A shared brand system

hrx's cover, mark and palette aren't meant to stand alone — they're the first
member of a small family across a suite of tools (hrx, Yesod, Pitput, and
whatever comes after). The idea, shared across all of them:

- **One palette family**, each tool picking one named palette rather than
  inventing colors from scratch. The source of truth for the palette options
  lives outside this repo, in the suite's own private branding workspace —
  copy the hex values you need into `latex/preamble.tex` and `web/book.css`,
  don't depend on that source existing at build time.
- **One mark grammar** (flat geometric blocks, one idea, drawn from the
  product's name if it has one worth drawing from, otherwise from what the
  product does) — see "Cover and brand mark" below.
- **One cover construction** (full-bleed pencil-sketch illustration over a
  solid color band) with **a different, specific scene per product** — never
  the same illustration reskinned.

If you're setting up a new book in this suite, look at an existing sibling's
`book/brand/README.md` before designing a new one from scratch.

## Typography and color

Fonts all ship with TeX Live and are loaded **by file name**, so the build
never depends on what fonts the machine has installed:

| Role | PDF | Web (CSS stack) |
|---|---|---|
| Body | TeX Gyre Pagella (Palatino design) | Iowan Old Style, Palatino, Georgia, serif |
| Headings, tables, callout labels | TeX Gyre Heros (Helvetica design), bold | system-ui sans-serif |
| Code | Source Code Pro at 0.84 scale | ui-monospace, SF Mono, Menlo, Consolas |

Palette (defined in `latex/preamble.tex` and as CSS variables in
`web/book.css`, with dark-mode values in the CSS). hrx uses **Cobalt &
Citrus**, one of a small family of shared palettes (see "A shared brand
system" below); pick a different one, or your own, for a new book:

| Token | Light | Use |
|---|---|---|
| ink (`hrxink`) | `#132548` | Cover band background, body text on the band |
| signal (`hrxaccent`/`hrxsignal`) | `#2454D9` | Chapter and part labels, links, note rule |
| citrus (`hrxcitrus`/`hrxcitrustext`) | `#D5ED42` bg / `#445108` text | Tip rule, small emphasis, cover accent rule |
| panel (`hrxnote`/`hrxwarn`/`hrxside`) | `#E2E8F7` | Note/warning/sidebar backgrounds |
| muted (`hrxmuted`, `hrxline`) | `#52617B` / `#BAC6DC` | Sidebar rule, secondary text |

**Change** the palette first; it carries most of a book's identity. Keep all
the color *uses* wired through a small set of named tokens (`hrxaccent`,
`hrxnote`/`hrxnoteframe`, `hrxtip`/`hrxtipframe`, `hrxwarn`/`hrxwarnframe`,
`hrxside`/`hrxsideframe`, plus the cover's own `hrxpaper`/`hrxink`/`hrxcitrus`)
so that swapping a palette is a find-and-replace of hex values, not a
restructure — that is how the Cobalt & Citrus pass over this book's original
rust-and-cream palette stayed a small diff.

## Page layout (PDF)

- US letter, 1 in margins, one-sided, no paragraph indent, 4 pt paragraph space.
- Chapters: small uppercase "CHAPTER N" label in the accent color above a large
  bold sans title. Parts: centered "PART I" label and title on their own page.
- Running heads: sans, small; chapter title on even pages, section title on
  odd pages, page number on the outside.
- Callouts: `tcolorbox`, breakable across pages, left rule only, small bold
  sans label. Sidebars use top and bottom rules instead.
- Code: shaded, wrapped with `fvextra` so long commands never run off the page.

## Writing conventions (the "O'Reilly style" part)

These matter more than the fonts.

- Second person, practical voice; every chapter is built around tasks the
  reader does, in order, with commands they can run.
- Each chapter opens with one paragraph saying what the reader will be able to
  do by the end, and closes with a **Summary** that leads into the next chapter.
- Part pages carry two or three sentences describing the part.
- The Preface has: Who This Book Is For, What You Need, How This Book Is
  Organized, Conventions Used in This Book, Using the Examples.
- Callouts are short. Note = background you can skip; Tip = a better way;
  Warning = money, credentials or data at risk. A sidebar is a longer aside
  with its own title, used about once per chapter.
- Tables for anything compared along the same columns (statuses, flags, symptoms).
- Figures are numbered per chapter ("Figure 1-1") in an italic line under the figure.
- Don't copy any *publisher's* trade dress (no O'Reilly animal engravings, no
  other publisher's cover grid or colophon) and use only the structure and
  the voice from outside sources. The book's own cover, mark and palette
  below are a different thing — build each new book its own.

## Cover and brand mark

hrx's cover (`book/brand/cover-art.png`, wired in by the `\maketitle`
override in `latex/preamble.tex`) is the first instance of a pattern meant to
repeat across a whole suite of books/tools, not a one-off. The construction:

- **One full-bleed illustration** across the top ~65% of a 16:9 frame, a
  pencil-sketch / graphite scene (hand-drawn cross-hatching, architectural
  line work, grayscale on off-white, *not* photorealistic, *not* color).
  The bottom ~35% is left nearly blank on purpose in the illustration itself.
- **One solid color band** composited under it (the palette's `ink` token),
  carrying: a small mark, the product wordmark (lowercase, bold sans, e.g.
  `hrx`), a one-line subtitle, a one-line "deck" sentence, and a build-time
  edition stamp (date + short git revision, written to a gitignored
  `latex/version.tex` by `tools/build.sh` and `\input` by the preamble —
  see `\BookBuildDate`/`\BookRevision` there).
- The illustration's **scene is specific to the product**, not reused or
  lightly reskinned from a sibling book. hrx's is an imagined 19th-century
  communications workshop: three distinct antique rigs, each cabled into one
  shared patch panel — a literal picture of "three harnesses, one gateway."
  When you make a new book's cover, find the equivalent literal picture for
  *that* product before reaching for a generic office/factory/control-room
  scene.
- To generate a new scene: describe the pencil-sketch style precisely
  (monochrome graphite, cross-hatching, light from upper left, 16:9, no text
  or logos in the image, illustration confined to the top ~65%), describe
  the product-specific scene in concrete physical objects (not abstractions
  like "AI" or "cloud"), and generate a few alternates to choose from before
  committing — see `book/brand/README.md` for how hrx's alternates were
  judged and which one won.

**The mark** (`book/brand/marks/*.svg`, `*.pdf`) is flat, 2-3 color geometric
blocks — no gradients, no literal pictograms, one idea. Draw the idea from
whichever of these the product actually has:
  1. the product's *name itself*, if it's a word with its own meaning or
     spelling worth drawing from;
  2. failing that, what the product *does*, mechanically — hrx's mark
     (`convergence.svg`) has no name-based source to draw on, so it draws
     three tapering rails funneling into one bar: literally the "many
     harnesses, one gateway" mechanism.
Sketch 2-3 candidates, render them to PNG at a small size (favicon/avatar
scale) before picking one — an idea that's clear as a paragraph of
reasoning often isn't legible as a 40px mark, and the reverse also happens.
Keep the rejected candidates in `book/brand/marks/` with a line in
`book/brand/README.md` saying why they lost; that record is what keeps the
next book's design pass from re-litigating the same choice.

## Markdown idioms the filter understands

```markdown
# Chapter Title {#chapter-id}              explicit id; link with [Chapter 2](#chapter-id)
# Part II: Working Every Day {.part .unnumbered}   in its own file; LaTeX drops "Part II: "
# Preface {.unnumbered}

::: warning
Text.
:::

::: sidebar
### Sidebar title
Text.
:::

```{.text include="generated/hrx-help.txt"}
```                                          replaced by the file's contents at build time
```

## Files to copy

```text
book/
  metadata.yaml        title, subtitle, author, rights            (change)
  chapters.txt         build order                                 (change)
  src/                 chapters                                    (change)
  brand/               cover art, mark svg+pdf, README of choices  (change)
  filters/callouts.lua callouts, parts, includes
  latex/preamble.tex   fonts, colors, headings, running heads, callout boxes
  web/book.css         web and EPUB styling, light and dark
  tools/build.sh       pdf | html | epub | all  ->  book/out/
  tools/check.sh       chapters | scrub | fresh | all
  tools/gen-listings.sh  regenerates generated/ from the real program  (change)
  tools/pre-commit     runs the fast checks; install with make book-hooks
  scrub-patterns.txt   names that must never appear                 (change)
  sources.yaml         chapter -> code paths, for the docs review   (change)
```

## Setting up a machine

```sh
brew install pandoc                      # or your package manager
# Basic TeX Live (e.g. BasicTeX) plus, without root:
tlmgr init-usertree
tlmgr --usermode install tcolorbox pgf environ tikzfill pdfcol listingsutf8 \
  trimspaces framed fvextra fancyvrb titlesec tex-gyre sourcecodepro \
  footnotehyper csquotes etoolbox xstring catchfile lineno
make book
```

## Gotchas already hit

- LuaLaTeX can sit for 10+ minutes on its first run on macOS while it indexes
  every system font. Use XeLaTeX, and load fonts by file name.
- `--from=markdown+attributes` fails: `attributes` is not a Markdown
  extension. Plain `--from=markdown` already includes fenced divs and attributes.
- Unnumbered chapters give `chunkedhtml` file names starting with `-`; set
  `--chunk-template='%i.html'` so files are named after chapter ids.
- `: caption` under a code block is not a caption in pandoc (that syntax is for
  tables); write the figure caption as an italic paragraph.
- KOMA-Script warns that `titlesec` is incompatible. The warning is harmless
  for this layout, but don't add KOMA heading commands on top of titlesec.
- In zsh, a bare `===` or `=====` word is treated as `=command` expansion and
  fails; quote separators in shell scripts.
