#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo 'usage: prove-device-auth-cutover.sh OUTPUT-DIRECTORY' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$1"
output=$(cd -- "$1" && pwd)
# Native Unix sockets need a short path on Darwin's hosted runner.
root=$(mktemp -d "${MESH_SHORT_TMP:-/tmp}/mesh-auth-cutover.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
baseline=0cc5acd25e47a445c41e6052c5b7919fadb54883
mkdir -p "$root/old" "$root/build"
git -C "$repo_root" archive "$baseline" | tar -xf - -C "$root/old"
(cd "$root/old" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.150' -o "$root/build/old-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.151' -o "$root/build/new-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.152' -o "$root/build/fleet-mesh" ./cmd/mesh)
(cd "$repo_root" && go build -trimpath -o "$root/build/control-client" ./integration/helpers/control-client)
printf '%s\n' "$baseline" > "$output/old-source.txt"
# The external providers run only fixture-owned processes; no installed Mesh or services.
python3 "$repo_root/scripts/prove-device-auth-cutover.py" "$root/proof" \
  "$root/build/old-mesh" "$root/build/new-mesh" "$root/build/fleet-mesh" "$baseline" "$root/build/control-client" >"$output/proof.log" 2>&1 || {
  mkdir -p "$output/failed-fixture"
  if [[ -d $root/proof ]]; then
    find "$root/proof" -type f \( -name '*.log' -o -name 'events.jsonl' -o -name 'result.json' \) -exec cp {} "$output/failed-fixture/" \;
  fi
  cat "$output/proof.log" >&2
  exit 1
}
cp "$root/proof/result.json" "$root/proof/events.jsonl" "$output/"
cat "$output/proof.log"
