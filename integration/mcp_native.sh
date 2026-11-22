#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
MESH="${MESH_INTEGRATION_BINARY:-${MESH:-}}"
if [[ -z ${MESH:-} ]]; then
  MESH="${HOME%/*}/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/mcp_native.py" "$MESH"
