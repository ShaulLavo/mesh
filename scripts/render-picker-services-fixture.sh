#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "usage: $0 OUTPUT_DIRECTORY" >&2
  exit 2
fi
if ! command -v rsvg-convert >/dev/null 2>&1; then
  echo 'picker fixture: skipped PNG rendering; rsvg-convert is unavailable' >&2
  exit 0
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p -- "$1"
output=$(cd -- "$1" && pwd)
cd -- "$repo_root"
go test ./internal/tui -run '^TestFleetServiceEvidence$' -count=1 -args -usage-evidence-dir "$output"
for text in "$output"/picker-services-*.txt; do
  awk '{sub(/[ \t]+$/, ""); print}' "$text" >"${text}.tmp"
  mv -- "${text}.tmp" "$text"
done
for svg in "$output"/picker-services-*.svg; do
  rsvg-convert "$svg" -o "${svg%.svg}.png"
done
printf 'picker fixture: synthetic production-renderer evidence written to %s\n' "$output"
