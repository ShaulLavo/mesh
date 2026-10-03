#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo 'usage: prove-helper-recovery.sh OUTPUT-DIRECTORY' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$1"
output=$(cd -- "$1" && pwd)
# Native Unix sockets need a short path on Darwin's hosted runner.
root=$(mktemp -d "${MESH_SHORT_TMP:-${TMPDIR:-/tmp}}/mesh-helper-proof.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
mkdir -p "$root/build"
(cd "$repo_root" && go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.1.162' -o "$root/build/fixed-helper" ./cmd/mesh)
git -C "$repo_root" rev-parse HEAD > "$output/candidate-source.txt"
printf '%s\n' 'The fixed helper is an unpublished PR-source fixture. Historical v149/v151/v159 inputs are actual public artifacts.' > "$output/artifact-boundaries.txt"
status=0
python3 "$repo_root/scripts/prove-helper-recovery.py" "$root/proof" "$root/build/fixed-helper" > "$output/proof.log" 2>&1 || status=$?
if [[ -d $root/proof ]]; then
  while IFS= read -r path; do
    relative=${path#"$root/proof/"}
    mkdir -p "$output/$(dirname -- "$relative")"
    cp "$path" "$output/$relative"
  done < <(find "$root/proof" -type f \( -name '*.json' -o -name '*.jsonl' -o -name '*.log' \) ! -name 'tailscale.json')
fi
cat "$output/proof.log"
exit "$status"
