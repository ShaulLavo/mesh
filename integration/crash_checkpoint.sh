#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="${HOME%/*}/mesh-checkpoint"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 -B "$repo_root/integration/helpers/crash_checkpoint.py" "$MESH"
