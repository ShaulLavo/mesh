#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
if [[ -z ${MESH:-} ]]; then
  MESH="${HOME%/*}/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/durable_output.py" "$MESH"
