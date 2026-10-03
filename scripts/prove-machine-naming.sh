#!/usr/bin/env bash
set -euo pipefail
if [[ $# -ne 1 ]]; then
  echo 'usage: prove-machine-naming.sh OUTPUT-DIRECTORY' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform=$(go env GOHOSTOS)/$(go env GOHOSTARCH)
if [[ -n ${NAMING_PROOF_PLATFORM:-} && $platform != "$NAMING_PROOF_PLATFORM" ]]; then
  echo 'FAIL: machine naming proof requires the requested native platform' >&2
  exit 1
fi
mkdir -p "$1"
output=$(cd -- "$1" && pwd)
python3 "$repo_root/integration/helpers/test_machine_naming.py" > "$output/marker-tests.txt" 2>&1
root=$(mktemp -d "${MESH_SHORT_TMP:-/tmp}/mesh-naming.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
mkdir -p "$root/baseline"
# This authenticated source baseline is not a published release artifact.
baseline=0d18a898ce377e9afe73f2a83f94f8b014196873
git -C "$repo_root" archive "$baseline" | tar -x -C "$root/baseline"
(cd "$root/baseline" && go build -trimpath -o "$root/baseline-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -o "$root/candidate-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -o "$root/control-client" ./integration/helpers/control-client)
printf 'nativePlatform=%s\nbaselineSource=%s\ncandidateSource=%s\npublishedReleaseTransition=false\nreaderCutoverComplete=false\n' \
  "$platform" "$baseline" "$(git -C "$repo_root" rev-parse HEAD)" > "$output/source.txt"
TMPDIR="$root" python3 "$repo_root/integration/helpers/machine_naming.py" "$root/candidate-mesh" \
  --control-client "$root/control-client" --baseline "$root/baseline-mesh" --evidence "$output" > "$output/proof.txt" 2>&1 || {
    cat "$output/proof.txt" >&2
    exit 1
  }
cat "$output/proof.txt"
