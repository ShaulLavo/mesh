#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="$repo_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
  export MESH
fi
python3 "$repo_root/integration/helpers/isolation_test.py"
echo 'PASS: integration entry points refuse live paths and leave decoy hosts unread'
