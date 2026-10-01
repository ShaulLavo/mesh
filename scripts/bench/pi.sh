#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# != 2 )); then
  echo 'Usage: pi.sh <local-arm64-build-dir> <local-results-dir>' >&2
  exit 2
fi
build=$(realpath "$1")
results=$(realpath -m "$2")
remote=$(ssh -o BatchMode=yes -o ConnectTimeout=10 pi 'mktemp -d /tmp/mesh-m5-XXXXXX')
[[ $remote == /tmp/mesh-m5-* ]] || exit 1
cleanup() {
  # Never broad-pkill: run.py owns its daemon and session lifecycle cleanup.
  ssh -o BatchMode=yes pi "python3 '$remote/remote_cleanup.py' '$remote'" || return 1
  ssh -o BatchMode=yes pi "rm -rf -- '$remote'"
}
trap cleanup EXIT
scp -q "$build/mesh" "$build/commit.txt" "$build/go-version.txt" \
  "$root/scripts/bench/run.py" "$root/scripts/bench/workload.py" \
  "$root/scripts/bench/remote_cleanup.py" "pi:$remote/"
commit=$(cat "$build/commit.txt")
go_version=$(cat "$build/go-version.txt")
# Timeout wraps the Python coordinator, which handles TERM and cleans up. The
# independent EXIT cleanup also checks /proc by exact scratch executable path.
ssh -o BatchMode=yes pi "timeout --signal=TERM --kill-after=15s 300s python3 '$remote/run.py' --binary '$remote/mesh' --commit '$commit' --go-version '$go_version' --scratch-parent '$remote' --idle-seconds 30 --output '$remote/pi.json'"
mkdir -p "$results"
scp -q "pi:$remote/pi.json" "pi:$remote/pi.md" "$results/"
