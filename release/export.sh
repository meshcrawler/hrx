#!/usr/bin/env bash
# Build the public HRX export: a fresh repository with a single commit, made
# from the current HEAD of this (private) repository.
#
#   release/export.sh --out DIR --module github.com/OWNER/hrx \
#     --author "Name <email>" [--scrub FILE] [--skip-tests] \
#     [--base git@github.com:OWNER/hrx.git] [--message "Release v0.2.0"]
#
# Steps: archive HEAD, drop release/exclude.txt paths, rewrite the Go module
# path, refuse on any scrub-pattern match, require a LICENSE, build, vet and run
# pure unit tests, scan for secrets with gitleaks, then commit once. With
# --base, the commit goes on top of the existing public history; without it,
# the export is a fresh repository.
# Nothing is pushed. Publishing is a separate, deliberate step.
set -euo pipefail

root="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
out="" module="" author="" skip_tests=0 base="" message=""
scrub="${HRX_RELEASE_SCRUB:-$HOME/.config/hrx/release-scrub.txt}"
private_module="github.com/meshcrawler/hrx"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    --module) module="$2"; shift 2 ;;
    --author) author="$2"; shift 2 ;;
    --scrub) scrub="$2"; shift 2 ;;
    --skip-tests) skip_tests=1; shift ;;
    --base) base="$2"; shift 2 ;;
    --message) message="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$out" && -n "$module" && -n "$author" ]] || {
  echo "usage: $0 --out DIR --module MODULE_PATH --author 'Name <email>' [--scrub FILE] [--skip-tests] [--base PUBLIC_GIT_URL] [--message MSG]" >&2; exit 2; }
[[ "$author" =~ ^[^\<]+\ \<[^\>]+@[^\>]+\>$ ]] || { echo "--author must look like 'Name <email>'" >&2; exit 2; }
[[ -f "$scrub" ]] || { echo "scrub pattern file not found: $scrub" >&2; exit 2; }
[[ ! -e "$out" ]] || { echo "refusing to overwrite existing $out" >&2; exit 2; }
if [[ -n "$(git -C "$root" status --porcelain)" ]]; then
  echo "working tree is not clean; the export is made from HEAD, so commit first" >&2; exit 2
fi
command -v gitleaks >/dev/null || { echo "gitleaks is required (brew install gitleaks)" >&2; exit 2; }

step() { printf '\n== %s\n' "$*"; }

step "archive HEAD $(git -C "$root" rev-parse --short HEAD)"
mkdir -p "$out"
git -C "$root" archive HEAD | tar -x -C "$out"

step "drop private paths"
while IFS= read -r path; do
  [[ -z "$path" || "$path" == \#* ]] && continue
  rm -rf "${out:?}/$path"
  echo "  - $path"
done < "$root/release/exclude.txt"

step "rewrite module path to $module"
grep -rlF "$private_module" "$out" | while IFS= read -r file; do
  perl -pi -e "s#\Q$private_module\E#$module#g" "$file"
done

step "scrub"
patterns="$(grep -vE '^\s*(#|$)' "$scrub")"
if hits="$(grep -rniIE -f <(printf '%s\n' "$patterns") "$out")"; then
  echo "$hits" | sed "s|$out/||" | head -50 >&2
  echo "scrub: $(echo "$hits" | wc -l | tr -d ' ') matches; fix them in the private repository and export again" >&2
  exit 1
fi
# Binary files (images) cannot be grepped meaningfully; list them for review.
find "$out" -type f \( -name '*.png' -o -name '*.jpg' -o -name '*.gif' -o -name '*.pdf' \) | sed "s|$out/|  review image: |"
echo "scrub: ok"

step "license"
[[ -f "$out/LICENSE" ]] || { echo "no LICENSE at the repository root; add one before exporting" >&2; exit 1; }
head -1 "$out/LICENSE"

if [[ $skip_tests == 0 ]]; then
  step "build, vet and pure unit tests"
  (cd "$out" && go build ./... && go vet ./... && go test ./internal/catalog ./internal/backend ./internal/monitor ./internal/hub)
fi

step "secret scan"
gitleaks dir --no-banner --redact "$out"

if [[ -n "$base" ]]; then
  # Incremental sync: reuse the public repository's history and add this
  # export as one new commit on top of it. Files deleted privately disappear.
  step "sync onto $base"
  clone="$(mktemp -d)"
  git clone -q "$base" "$clone/public"
  mv "$clone/public/.git" "$out/.git"
  rm -rf "$clone"
  if ! git -C "$out" rev-parse -q --verify HEAD >/dev/null; then
    git -C "$out" checkout -q -b main 2>/dev/null || true
  fi
else
  step "fresh repository"
  git -C "$out" init -q -b main
fi
git -C "$out" add -A
if git -C "$out" rev-parse -q --verify HEAD >/dev/null && git -C "$out" diff --cached --quiet; then
  echo "no changes since the last public release"; exit 0
fi
if [[ -z "$message" ]]; then
  message="Release from $(git -C "$root" rev-parse --short HEAD)"
  git -C "$out" rev-parse -q --verify HEAD >/dev/null || message="Initial public release of hrx"
fi
GIT_AUTHOR_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  git -C "$out" -c user.name="${author% <*}" -c user.email="$(sed -E 's/.*<([^>]+)>.*/\1/' <<<"$author")" \
  commit -q -m "$message"
git -C "$out" log --format='%h %an <%ae> %s' -3
echo
echo "Export ready in $out (not pushed). Review it, then push it to the public remote yourself."
