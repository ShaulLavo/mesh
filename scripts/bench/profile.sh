#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# != 2 )); then
  echo 'Usage: profile.sh <scratch-profile-build-dir> <profile-output-dir>' >&2
  exit 2
fi
build=$(realpath "$1")
output=$(realpath -m "$2")
python3 "$root/scripts/bench/receipt.py" verify --binary "$build/mesh" --receipt "$build/receipt.json" > /dev/null
mkdir -p "$output"
cd "$root"
for package in worker tui daemon transport session terminal release cli; do
  scripts/bench/build-test.sh "$output" "$package"
  python3 scripts/bench/receipt.py verify --binary "$output/$package.test" --receipt "$output/$package.receipt.json" > /dev/null
  "$output/$package.test" -test.run '^$' -test.bench . -test.benchtime=1s -test.count=3 -test.benchmem \
    -test.cpuprofile "$output/$package.cpu.pprof" -test.memprofile "$output/$package.heap.pprof" \
    > "$output/$package.bench.txt"
  for sample in cpu alloc_space inuse_space; do
    profile="$output/$package.heap.pprof"
    [[ $sample != cpu ]] || profile="$output/$package.cpu.pprof"
    go tool pprof -top -sample_index "$sample" "$output/$package.test" "$profile" \
      > "$output/$package.$sample.txt"
  done
done
runtime="$output/runtime-$(date -u +%Y%m%dT%H%M%SZ)"
python3 scripts/bench/run.py --binary "$build/mesh" --receipt "$build/receipt.json" --scratch-parent "$build" \
  --sessions 1 --idle-seconds 1 --settle-seconds 0 --repeats 1 --throughput-bytes 16777216 \
  --profile-dir "$runtime" --output "$output/instrumented.json"
for profile in "$runtime"/*.pprof; do
  go tool pprof -top "$build/mesh" "$profile" > "$profile.top.txt"
done
GODEBUG=inittrace=1 "$build/mesh" --version > "$output/startup.stdout.txt" 2> "$output/startup.inittrace.txt"
