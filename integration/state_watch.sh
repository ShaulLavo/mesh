#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/session_cleanup.sh" || exit 1
set -euo pipefail

: "${MESH:?state_watch requires the verification binary}"
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
DAEMON=""
cleanup() {
  local status=$?
  if [[ -n $DAEMON ]]; then kill "$DAEMON" 2>/dev/null || true; wait "$DAEMON" 2>/dev/null || true; fi
  finish_fixture_cleanup "$MESH_STATE_DIR" "$T" "$status"
}
trap cleanup EXIT
mkdir -p "$HOME/watch-site"
printf 'watch fixture\n' > "$HOME/watch-site/index.html"
"$MESH" daemon --subscriber-limit 1 --unix-connection-limit 8 >"$T/daemon.log" 2>&1 &
DAEMON=$!
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/state_watch.py" "$MESH_STATE_DIR/daemon.sock" "$HOME/watch-site" --seconds "${MESH_WATCH_SECONDS:-10}"
