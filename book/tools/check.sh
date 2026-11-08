#!/usr/bin/env bash
# Deterministic book checks, run by CI and by the pre-commit hook.
#   scrub   fail if book text contains infrastructure names (book/scrub-patterns.txt)
#   fresh   fail if a generated listing no longer matches the current hrx
#   chapters fail if chapters.txt names a missing file
# Usage: book/tools/check.sh [scrub|fresh|chapters|all]   (default all)
set -euo pipefail

book="$(cd "$(dirname "$0")/.." && pwd)"
what="${1:-all}"
status=0

check_scrub() {
  local patterns="$book/scrub-patterns.txt"
  local hits
  hits="$(grep -rniE -f <(grep -vE '^\s*(#|$)' "$patterns") \
    "$book/src" "$book/generated" "$book/OUTLINE.md" || true)"
  if [[ -n "$hits" ]]; then
    echo "scrub: internal names found in book text (use the placeholders in book/README.md):" >&2
    echo "$hits" | sed "s|$book/|book/|" >&2
    status=1
  else
    echo "scrub: ok"
  fi
}

check_fresh() {
  local tmp
  tmp="$(mktemp -d)"
  "$book/tools/gen-listings.sh" "$tmp"
  if diff -ru "$book/generated" "$tmp" > "$tmp.diff"; then
    echo "fresh: ok"
  else
    echo "fresh: generated listings are stale; run book/tools/gen-listings.sh and review the prose that uses them:" >&2
    sed "s|$tmp|current|g" "$tmp.diff" >&2
    status=1
  fi
  rm -rf "$tmp" "$tmp.diff"
}

check_chapters() {
  local missing=0
  while IFS= read -r line; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ -f "$book/src/$line" ]] || { echo "chapters: missing src/$line" >&2; missing=1; }
  done < "$book/chapters.txt"
  [[ $missing == 0 ]] && echo "chapters: ok" || status=1
}

case "$what" in
  scrub) check_scrub ;;
  fresh) check_fresh ;;
  chapters) check_chapters ;;
  all) check_chapters; check_scrub; check_fresh ;;
  *) echo "usage: $0 [scrub|fresh|chapters|all]" >&2; exit 2 ;;
esac
exit $status
