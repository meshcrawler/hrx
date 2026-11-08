#!/usr/bin/env bash
# Regenerate every listing the book includes from a fresh build of hrx.
# Usage: book/tools/gen-listings.sh [OUTPUT_DIR]   (default book/generated)
# Only offline commands are used: no gateway, credentials or network.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
out="${1:-$root/book/generated}"
mkdir -p "$out"

bin="$(mktemp -d)/hrx"
(cd "$root" && go build -trimpath -o "$bin" ./cmd/hrx)

# Run with an empty home so no local configuration leaks into a listing.
sandbox="$(mktemp -d)"
run() { HOME="$sandbox" XDG_CONFIG_HOME="$sandbox/.config" XDG_CACHE_HOME="$sandbox/.cache" \
  CODEX_HOME="$sandbox/.codex" "$bin" "$@" 2>/dev/null; }

run --help > "$out/hrx-help.txt"

rm -rf "$sandbox" "$(dirname "$bin")"
