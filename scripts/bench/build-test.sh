#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# < 2 || $# > 3 )); then
  echo 'Usage: build-test.sh <scratch-output-dir> <package> [amd64|arm64]' >&2
  exit 2
fi
output=$(realpath -m "$1")
package=$2
case $package in worker|tui|daemon|transport|session|terminal|release|cli) ;; *) exit 2 ;; esac
arch=${3:-$(go env GOARCH)}
mkdir -p "$output"
python3 "$root/scripts/bench/receipt.py" snapshot --root "$root" --arch "$arch" \
  --package "./internal/$package" --sources "$output/$package.sources.json"
cd "$root"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$output/$package.test" "./internal/$package"
python3 "$root/scripts/bench/receipt.py" create --root "$root" --binary "$output/$package.test" \
  --sources "$output/$package.sources.json" --receipt "$output/$package.receipt.json"
