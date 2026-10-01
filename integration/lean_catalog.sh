#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-lean.XXXXXX")
  trap 'rm -rf -- "$build_root"' EXIT
  MESH="$build_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/lean_catalog.py" "$MESH" "$@"
