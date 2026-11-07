#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
mesh_test=${MESH_INTEGRATION_BINARY:-${HOME%/*}/mesh-execution-fault}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  go build -tags mesh_integration -o "$mesh_test" ./cmd/mesh
fi
python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/durable_finalization.py" "$mesh_test"
