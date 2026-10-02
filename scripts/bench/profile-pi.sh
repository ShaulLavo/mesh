#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# != 2 )); then
  echo 'Usage: profile-pi.sh <arm64-build-dir-with-test-binaries> <local-profile-output-dir>' >&2
  exit 2
fi
build=$(realpath "$1")
output=$(realpath -m "$2")
python3 "$root/scripts/bench/receipt.py" verify --binary "$build/mesh" --receipt "$build/receipt.json" --cross-target > /dev/null
remote=$(ssh -o BatchMode=yes -o ConnectTimeout=10 pi 'mktemp -d /tmp/mesh-m5-XXXXXX')
[[ $remote == /tmp/mesh-m5-* ]] || exit 1
cleanup() {
  ssh -o BatchMode=yes pi "python3 '$remote/remote_cleanup.py' '$remote'" || return 1
  ssh -o BatchMode=yes pi "rm -rf -- '$remote'"
}
trap cleanup EXIT
scp -q "$build/mesh" "$build/receipt.json" "$build"/*.test "$build"/*.receipt.json \
  "$root/scripts/bench/receipt.py" "$root/scripts/bench/remote_cleanup.py" \
  "$root/scripts/bench/process_diagnostics.py" "pi:$remote/"
# Short serial runs preserve headroom for the Pi's TV/dashboard workload.
ssh -o BatchMode=yes pi "timeout --signal=TERM --kill-after=10s 90s bash -s '$remote'" <<'REMOTE'
set -euo pipefail
cd "$1"
export TMPDIR="$PWD"
python3 receipt.py verify --binary mesh --receipt receipt.json > verified-receipt.json
for package in worker tui daemon transport session terminal release cli; do
  python3 receipt.py verify --binary "$package.test" --receipt "$package.receipt.json" > "$package.verified-receipt.json"
  "./$package.test" -test.run '^$' -test.bench . -test.benchtime=200ms \
    -test.count=1 -test.benchmem -test.cpuprofile "$package.cpu.pprof" \
    -test.memprofile "$package.heap.pprof" > "$package.bench.txt"
done
GODEBUG=inittrace=1 ./mesh --version > startup.stdout.txt 2> startup.inittrace.txt
uname -a > host.txt
REMOTE
mkdir -p "$output"
scp -q "pi:$remote/*.pprof" "pi:$remote/*.txt" "pi:$remote/*verified-receipt.json" "$output/"
for package in worker tui daemon transport session terminal release cli; do
  for sample in cpu alloc_space inuse_space; do
    profile="$output/$package.heap.pprof"
    [[ $sample != cpu ]] || profile="$output/$package.cpu.pprof"
    go tool pprof -top -sample_index "$sample" "$build/$package.test" "$profile" \
      > "$output/$package.$sample.txt"
  done
done
