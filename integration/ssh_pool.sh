#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-ssh-pool.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"
mesh=${MESH_INTEGRATION_BINARY:-$build_root/mesh}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  go build -tags mesh_integration -o "$mesh" ./cmd/mesh
fi
python3 -B integration/helpers/ssh_pool.py "$mesh"
