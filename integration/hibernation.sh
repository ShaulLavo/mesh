#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t27.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$build_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/hibernation.py" "$MESH"
