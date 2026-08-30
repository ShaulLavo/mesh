#!/usr/bin/env bash
# The daemon launches workers but does not supervise their lifetime. It still
# has to reap each child that exits while the daemon remains alive.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
DAEMON=""

cleanup() {
  [ -z "$DAEMON" ] || kill -9 "$DAEMON" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

"$MESH" daemon >"$T/daemon.log" 2>&1 &
DAEMON=$!
for _ in $(seq 80); do
  if [ -S "$MESH_STATE_DIR/daemon.sock" ] && timeout --kill-after=1s 1s "$MESH" ls --daemon >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done
[ -S "$MESH_STATE_DIR/daemon.sock" ] || fail "daemon did not start: $(cat "$T/daemon.log")"

"$MESH" local --daemon -- /bin/true >"$T/client.out" 2>"$T/client.err" ||
  fail "short session failed: $(cat "$T/client.err")"

children=""
for _ in $(seq 100); do
  children=$(ps -o pid=,stat=,comm= --ppid "$DAEMON")
  [ -z "$children" ] && break
  sleep 0.02
done
[ -z "$children" ] || fail "daemon retained exited worker: $children"

echo "PASS: daemon reaped its exited worker"
