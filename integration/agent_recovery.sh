#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t25.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$build_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
# The SSH fixture uses the existing loopback-only integration transport.
mesh_ssh=${MESH_INTEGRATION_BINARY:-$build_root/mesh-ssh}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$repo_root" && go build -tags mesh_integration -o "$mesh_ssh" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/agent_recovery.py" "$MESH" "$mesh_ssh"
