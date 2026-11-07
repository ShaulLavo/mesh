#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# The isolation owner registered this binary/state root before starting us.
mesh=${MESH:-${HOME%/*}/mesh-u3}
if [[ -z ${MESH:-} ]]; then
  (cd "$repo_root" && go build -o "$mesh" ./cmd/mesh)
fi
python3 -B "$repo_root/integration/helpers/session_lifecycle_test.py"
python3 "$repo_root/integration/helpers/session_lifecycle.py" daemon-restart "$mesh" "$@"
