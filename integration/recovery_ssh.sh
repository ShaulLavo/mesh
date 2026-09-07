#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-recovery-ssh.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"
mesh=${MESH_INTEGRATION_BINARY:-$build_root/mesh}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  go build -tags mesh_integration -o "$mesh" ./cmd/mesh
fi
python3 integration/helpers/recovery_transactions.py ssh "$mesh"
