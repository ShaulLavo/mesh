#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
MESH="${MESH_INTEGRATION_BINARY:-${MESH:-}}"
if [[ -z ${MESH:-} ]]; then
  MESH="${HOME%/*}/mesh"
  go build -tags mesh_integration -o "$MESH" ./cmd/mesh
fi
python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/pipe_execution.py" "$MESH"
