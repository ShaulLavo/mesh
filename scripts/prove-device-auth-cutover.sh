#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo 'usage: prove-device-auth-cutover.sh OUTPUT-DIRECTORY' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 "$repo_root/scripts/test-device-auth-cutover.py"
mkdir -p "$1"
output=$(cd -- "$1" && pwd)
# Native Unix sockets need a short path on Darwin's hosted runner.
root=$(mktemp -d "${MESH_SHORT_TMP:-/tmp}/mesh-auth-cutover.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
mkdir -p "$root/build"
(cd "$repo_root" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.160' -o "$root/build/candidate-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.161' -o "$root/build/fleet-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -o "$root/build/control-client" ./integration/helpers/control-client)
git -C "$repo_root" rev-parse HEAD > "$output/candidate-source.txt"
printf '%s\n' 'Both candidate patches are source-built PR fixtures; historical binaries are public artifacts.' > "$output/artifact-boundaries.txt"
preserve_published_evidence() {
  [[ -d $root/proof/published ]] || return 0
  while IFS= read -r path; do
    relative=${path#"$root/proof/"}
    mkdir -p "$output/$(dirname -- "$relative")"
    cp "$path" "$output/$relative"
  done < <(find "$root/proof/published" -type f -name '*.json')
}
# The external providers run only fixture-owned processes; no installed Mesh or services.
python3 "$repo_root/scripts/prove-device-auth-cutover.py" "$root/proof" \
  "$root/build/candidate-mesh" "$root/build/fleet-mesh" "$root/build/control-client" >"$output/proof.log" 2>&1 || {
  mkdir -p "$output/failed-fixture"
  if [[ -d $root/proof ]]; then
    find "$root/proof" -type f \( -name '*.log' -o -name 'events.jsonl' -o -name 'result.json' -o -name 'verified.json' \) -exec cp {} "$output/failed-fixture/" \;
  fi
  preserve_published_evidence
  cat "$output/proof.log" >&2
  exit 1
}
preserve_published_evidence
cp "$root/proof/result.json" "$root/proof/events.jsonl" "$root/proof/published/verified.json" "$output/"
cat "$output/proof.log"
