#!/usr/bin/env bash
# Render the book. Usage: book/tools/build.sh [pdf|html|epub|all] (default all).
# Output goes to book/out/. Needs pandoc 3.6+; the PDF also needs xelatex with
# tcolorbox, titlesec, fvextra, tex-gyre and sourcecodepro.
set -euo pipefail

cd "$(dirname "$0")/.."
target="${1:-all}"
mkdir -p out

chapters=()
while IFS= read -r line; do
  [[ -z "$line" || "$line" == \#* ]] && continue
  chapters+=("src/$line")
done < chapters.txt

common=(
  --from=markdown
  --metadata-file=metadata.yaml
  --lua-filter=filters/callouts.lua
  --resource-path=.:src
  --toc
)

build_pdf() {
  printf '%% generated at build time; do not commit\n\\renewcommand{\\BookBuildDate}{%s}\n\\renewcommand{\\BookRevision}{%s}\n' \
    "$(date -u +'%B %e, %Y' | tr -s ' ')" \
    "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    > latex/version.tex
  pandoc "${common[@]}" "${chapters[@]}" \
    --pdf-engine=xelatex \
    --top-level-division=chapter --number-sections \
    -V documentclass=scrbook -V classoption=oneside -V papersize=letter \
    -V geometry:margin=1in -V colorlinks=true -V linkcolor=hrxaccent -V urlcolor=hrxaccent \
    --syntax-highlighting=tango \
    --include-in-header=latex/preamble.tex \
    -o out/hrx-book.pdf
  echo "out/hrx-book.pdf"
}

build_html() {
  rm -rf out/web
  pandoc "${common[@]}" "${chapters[@]}" \
    --number-sections --to=chunkedhtml --split-level=1 --chunk-template='%i.html' \
    --css=book.css --syntax-highlighting=tango \
    -o out/web
  cp web/book.css out/web/
  echo "out/web/index.html"
}

build_epub() {
  pandoc "${common[@]}" "${chapters[@]}" \
    --split-level=1 --css=web/book.css \
    -o out/hrx-book.epub
  echo "out/hrx-book.epub"
}

case "$target" in
  pdf) build_pdf ;;
  html) build_html ;;
  epub) build_epub ;;
  all) build_html; build_epub; build_pdf ;;
  *) echo "usage: $0 [pdf|html|epub|all]" >&2; exit 2 ;;
esac
