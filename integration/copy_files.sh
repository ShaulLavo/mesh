#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-copy-build.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
mesh=${MESH:-$build_root/mesh}
if [[ -z ${MESH:-} ]]; then
  (cd "$repo_root" && go build -o "$mesh" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/copy_files.py" "$mesh"
