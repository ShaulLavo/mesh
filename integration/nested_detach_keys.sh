#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="$repo_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
exec python3 "$repo_root/integration/helpers/terminal_window.py" nested-detach "$MESH"
