#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/work/tmp}/mesh-t25.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$build_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
# The SSH fixture uses the existing loopback-only integration transport.
(cd "$repo_root" && go build -tags mesh_integration -o "$build_root/mesh-ssh" ./cmd/mesh)
python3 "$repo_root/integration/helpers/agent_recovery.py" "$MESH" "$build_root/mesh-ssh"
