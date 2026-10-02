#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/session_cleanup.sh" || exit 1
set -euo pipefail

: "${MESH:?dashboard requires the verification binary}"
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
DAEMON=""
cleanup() {
  local status=$?
  if [[ -n $DAEMON ]]; then kill "$DAEMON" 2>/dev/null || true; wait "$DAEMON" 2>/dev/null || true; fi
  finish_fixture_cleanup "$MESH_STATE_DIR" "$T" "$status"
}
trap cleanup EXIT
"$MESH" daemon --subscriber-limit 1 --unix-connection-limit 8 >"$T/daemon.log" 2>&1 &
DAEMON=$!
go build -o "$T/screen" ./integration/helpers/dashboard-screen
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/dashboard.py" "$MESH" "$MESH_STATE_DIR/daemon.sock" --screen "$T/screen" "$@"
