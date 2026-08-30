#!/usr/bin/env bash
# `mesh kill` is a completion barrier. When it prints success, the complete
# process group must already be gone, including a command that ignored SIGHUP.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
CLIENT=""
SESSION_PID=""

cleanup() {
  [ -z "$CLIENT" ] || kill -9 "$CLIENT" 2>/dev/null || true
  [ -z "$SESSION_PID" ] || kill -9 "$SESSION_PID" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mkfifo "$T/in"
"$MESH" local -- /bin/sh -c 'trap "" HUP; echo $$ > "$1"; while :; do sleep 1; done' sh "$T/pid" \
  <"$T/in" >"$T/client.out" 2>"$T/client.err" &
CLIENT=$!
exec 3>"$T/in"
for _ in $(seq 100); do
  [ -s "$T/pid" ] && break
  sleep 0.05
done
[ -s "$T/pid" ] || fail "session did not start: $(cat "$T/client.err")"
SESSION_PID=$(cat "$T/pid")
SID=$("$MESH" ls | awk 'NR == 2 { print $1 }')
[ -n "$SID" ] || fail "session was not listed"

"$MESH" kill "$SID" >"$T/kill.out" || fail "kill command failed"
kill -0 "$SESSION_PID" 2>/dev/null && fail "kill returned while process $SESSION_PID was alive"
grep -q "killed $SID" "$T/kill.out" || fail "kill did not report completion"

exec 3>&-
wait "$CLIENT" 2>/dev/null || true
CLIENT=""
SESSION_PID=""
echo "PASS: kill waited until session $SID was gone"
