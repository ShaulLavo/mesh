#!/usr/bin/env bash
# Each session runs in its own systemd scope, so systemd-oomd can kill one
# runaway session without taking every other session and the daemon with it.
set -uo pipefail

if [ "$(uname -s)" != Linux ] || ! busctl --user status >/dev/null 2>&1; then
  echo "PASS: no systemd user manager; sessions keep their launcher's cgroup"
  exit 0
fi
if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
CLIENT=""
SID=""

cleanup() {
  [ -z "$SID" ] || "$MESH" kill "$SID" >/dev/null 2>&1 || true
  [ -z "$CLIENT" ] || kill -9 "$CLIENT" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mkfifo "$T/in"
"$MESH" local -- /bin/sh -c 'cat /proc/self/cgroup > "$1.tmp"; mv "$1.tmp" "$1"; while :; do sleep 1; done' sh "$T/cgroup" \
  <"$T/in" >"$T/client.out" 2>"$T/client.err" &
CLIENT=$!
exec 3>"$T/in"
for _ in $(seq 100); do
  [ -s "$T/cgroup" ] && break
  sleep 0.05
done
[ -s "$T/cgroup" ] || fail "session did not start: $(cat "$T/client.err")"
# The state directory holds exactly this session; `mesh ls` would also list
# the adopted hosts of whoever runs the script.
for _ in $(seq 100); do
  [ -S "$(echo "$MESH_STATE_DIR"/s/*/sock)" ] && break
  sleep 0.05
done
SID=$(basename "$(dirname "$(echo "$MESH_STATE_DIR"/s/*/sock)")")
[ -S "$MESH_STATE_DIR/s/$SID/sock" ] || fail "session socket did not appear"

grep -q "^0::.*/mesh-session-$SID\.scope\$" "$T/cgroup" || fail "command cgroup is $(cat "$T/cgroup"), want mesh-session-$SID.scope"

"$MESH" kill "$SID" >/dev/null || fail "kill failed"
for _ in $(seq 100); do
  systemctl --user is-active --quiet "mesh-session-$SID.scope" || break
  sleep 0.05
done
systemctl --user is-active --quiet "mesh-session-$SID.scope" && fail "scope outlived the session"
SID=""
exec 3>&-
wait "$CLIENT" 2>/dev/null || true
CLIENT=""
echo "PASS: session command ran in its own scope"
