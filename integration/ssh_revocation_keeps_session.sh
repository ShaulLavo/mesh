#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mesh=${MESH_INTEGRATION_BINARY:-${HOME%/*}/mesh-u3-integration}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$repo_root" && go build -tags mesh_integration -o "$mesh" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/ssh_sessions.py" "$mesh" revocation "$@"
