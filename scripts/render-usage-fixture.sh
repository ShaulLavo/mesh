#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "usage: $0 OUTPUT_DIRECTORY" >&2
  exit 2
fi
if ! command -v rsvg-convert >/dev/null 2>&1; then
  echo 'usage fixture: skipped PNG rendering; rsvg-convert is unavailable' >&2
  exit 0
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p -- "$1"
output=$(cd -- "$1" && pwd)
cd -- "$repo_root"
go test ./internal/tui -run '^TestDashboardUsageEvidence$' -count=1 -args -usage-evidence-dir "$output"
for svg in "$output"/*.svg; do
  rsvg-convert "$svg" -o "${svg%.svg}.png"
done
printf 'usage fixture: actual renderer evidence written to %s\n' "$output"
