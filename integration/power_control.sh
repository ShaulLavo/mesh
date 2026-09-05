#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
state_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-power.XXXXXX")
daemon_pid=
cleanup() {
  if [[ -n "$daemon_pid" ]]; then
    kill "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  rm -rf -- "$state_root"
}
trap cleanup EXIT
cd "$repo_root"
mesh_binary=${MESH:-"$repo_root/mesh"}
export MESH_STATE_DIR="$state_root/state"
export MESH_CONFIG_DIR="$state_root/config"
mkdir -p "$MESH_CONFIG_DIR"

"$mesh_binary" daemon --tailnet-port=0 >"$state_root/daemon.log" 2>&1 &
daemon_pid=$!
for attempt in {1..100}; do
  [[ -S "$MESH_STATE_DIR/daemon.sock" ]] && break
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    cat "$state_root/daemon.log" >&2
    exit 1
  fi
  sleep 0.02
done
if [[ ! -S "$MESH_STATE_DIR/daemon.sock" ]]; then
  cat "$state_root/daemon.log" >&2
  exit 1
fi
"$mesh_binary" wake deny >"$state_root/permission.log"
rg -q 'Wake permission disabled' "$state_root/permission.log"
"$mesh_binary" ls >"$state_root/list.log"
kill "$daemon_pid"
wait "$daemon_pid"
daemon_pid=

go test ./internal/daemon -run '^TestWake(CrossesWebSocketAndHonorsTargetRevocation|PermissionUsesRealLocalDaemonSocket|LocalConsentIsAdvertisedWithoutSending)$' -count=1
go test ./internal/inhibit -count=1
printf 'PASS: wake controls crossed real daemon and WebSocket boundaries\n'
