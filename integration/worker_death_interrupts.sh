#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mesh=${MESH:-${HOME%/*}/mesh-u3}
if [[ -z ${MESH:-} ]]; then
  (cd "$repo_root" && go build -o "$mesh" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/session_lifecycle.py" worker-death "$mesh" "$@"
