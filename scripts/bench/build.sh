#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
if (( $# < 1 || $# > 3 )); then
  echo 'Usage: build.sh <scratch-output-dir> [amd64|arm64] [profile]' >&2
  exit 2
fi
output=$(realpath "$1")
arch=${2:-$(go env GOARCH)}
mkdir -p "$output"
version=$(git -C "$root" describe --tags --abbrev=0)
args=()
if [[ ${3:-} == profile ]]; then
  # Overlay main only in this build. Production sources and init order stay put.
  python3 - "$root" "$output" <<'PY'
import json
from pathlib import Path
import sys
root, output = map(Path, sys.argv[1:])
main = root / 'cmd/mesh/main.go'
source = main.read_text()
needle = 'func main() {'
if source.count(needle) != 1:
    raise SystemExit('profiling overlay requires exactly one main function')
replacement = output / 'profile-main.go'
replacement.write_text(source.replace(needle, needle + '\n finish := benchProfile()\n defer finish()'))
overlay = {'Replace': {str(main): str(replacement),
    str(root / 'cmd/mesh/bench_profile.go'): str(root / 'scripts/bench/profile-main.go.txt')}}
(output / 'overlay.json').write_text(json.dumps(overlay))
PY
  args=(-overlay "$output/overlay.json")
fi
profile=()
[[ ${3:-} != profile ]] || profile=(--profile)
python3 "$root/scripts/bench/receipt.py" snapshot --root "$root" --arch "$arch" "${profile[@]}" --sources "$output/sources.json"
cd "$root"
# Pin the normal release version so --version exercises the shipped CLI path.
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build "${args[@]}" \
  -ldflags="-X github.com/shaul/mesh/internal/release.Version=$version" \
  -o "$output/mesh" ./cmd/mesh
python3 "$root/scripts/bench/receipt.py" create --root "$root" --binary "$output/mesh" \
  --sources "$output/sources.json" --receipt "$output/receipt.json"
