#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# != 2 )); then
  echo 'Usage: profile.sh <scratch-profile-build-dir> <profile-output-dir>' >&2
  exit 2
fi
build=$(realpath "$1")
output=$(realpath -m "$2")
mkdir -p "$output"
cd "$root"
for package in worker tui daemon transport session terminal release cli; do
  go test -run '^$' -bench . -benchtime=1s -count=3 -benchmem \
    -cpuprofile "$output/$package.cpu.pprof" -memprofile "$output/$package.heap.pprof" \
    -o "$output/$package.test" "./internal/$package" > "$output/$package.bench.txt"
  for sample in cpu alloc_space inuse_space; do
    profile="$output/$package.heap.pprof"
    [[ $sample != cpu ]] || profile="$output/$package.cpu.pprof"
    go tool pprof -top -sample_index "$sample" "$output/$package.test" "$profile" \
      > "$output/$package.$sample.txt"
  done
done
runtime="$output/runtime-$(date -u +%Y%m%dT%H%M%SZ)"
python3 scripts/bench/run.py --binary "$build/mesh" --commit "$(cat "$build/commit.txt")" \
  --go-version "$(cat "$build/go-version.txt")" --scratch-parent "$build" \
  --sessions 1 --idle-seconds 1 --settle-seconds 0 --repeats 1 --throughput-bytes 16777216 \
  --profile-dir "$runtime" --output "$output/instrumented.json"
for profile in "$runtime"/*.pprof; do
  go tool pprof -top "$build/mesh" "$profile" > "$profile.top.txt"
done
GODEBUG=inittrace=1 "$build/mesh" --version > "$output/startup.stdout.txt" 2> "$output/startup.inittrace.txt"
