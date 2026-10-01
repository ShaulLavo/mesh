#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 -B "$repo_root/integration/helpers/session_cleanup_test.py"

if [[ -z ${MESH:-} ]]; then
  test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-cleanup.XXXXXX")
  trap 'rm -rf -- "$test_root"' EXIT
  MESH="$test_root/mesh"
  go build -o "$MESH" "$repo_root/cmd/mesh"
  export MESH
fi
for ((attempt = 1; attempt <= 30; attempt++)); do
  bash "$repo_root/integration/client_signal_detaches.sh"
done
echo 'PASS: shell fixture cleanup waits for worker final writes in 30 client-signal runs'
