#!/usr/bin/env bash
# A daemon is disposable: restarting it must neither replace nor interrupt a
# worker that it discovered and served before the restart.
set -uo pipefail

# Build unless the caller pointed at a binary of their own. A stale ./mesh
# fails these scripts for reasons that have nothing to do with the code.
if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
DAEMON1=""
DAEMON2=""
CLIENT1=""
CLIENT2=""
SID=""
SHELL_PID=""

cleanup() {
  [ -z "$SID" ] || "$MESH" kill "$SID" >/dev/null 2>&1 || true
  [ -z "$CLIENT1" ] || kill -9 "$CLIENT1" 2>/dev/null || true
  [ -z "$CLIENT2" ] || kill -9 "$CLIENT2" 2>/dev/null || true
  [ -z "$DAEMON1" ] || kill -9 "$DAEMON1" 2>/dev/null || true
  [ -z "$DAEMON2" ] || kill -9 "$DAEMON2" 2>/dev/null || true
  [ -z "$SHELL_PID" ] || kill -9 "$SHELL_PID" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

wait_for_file() {
  local file=$1
  for _ in $(seq 120); do
    [ -s "$file" ] && return 0
    sleep 0.05
  done
  return 1
}

wait_for_daemon() {
  local pid=$1
  for _ in $(seq 40); do
    kill -0 "$pid" 2>/dev/null || return 1
    if [ -S "$MESH_STATE_DIR/daemon.sock" ] && timeout --kill-after=1s 1s "$MESH" ls --daemon >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.05
  done
  return 1
}

database_state() {
  python3 - "$MESH_STATE_DIR/mesh.db" "$SID" <<'PY'
import sqlite3
import sys

with sqlite3.connect(sys.argv[1]) as database:
    row = database.execute("SELECT state FROM sessions WHERE id = ?", (sys.argv[2],)).fetchone()
print("" if row is None else row[0])
PY
}

mkfifo "$T/in1" "$T/in2"
"$MESH" daemon >"$T/daemon1.log" 2>&1 &
DAEMON1=$!
wait_for_daemon "$DAEMON1" || fail "first daemon did not start: $(cat "$T/daemon1.log")"

"$MESH" local --daemon -- bash --noprofile --norc <"$T/in1" >"$T/out1" 2>"$T/client1.err" &
CLIENT1=$!
exec 3>"$T/in1"
for _ in $(seq 120); do
  SID=$(sed -n 's/^new session \([0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]\)$/\1/p' "$T/client1.err" | head -n1)
  [ -n "$SID" ] && break
  sleep 0.05
done
[ -n "$SID" ] || fail "client did not create through daemon: $(cat "$T/client1.err")"

echo "echo \$\$ > $T/shell_pid; echo BEFORE_RESTART" >&3
wait_for_file "$T/shell_pid" || fail "session shell did not answer before restart"
SHELL_PID=$(cat "$T/shell_pid")
SESSION_PID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$MESH_STATE_DIR/s/$SID/meta.json") || fail "could not read session PID"
[ "$(database_state)" = running ] || fail "daemon did not publish running session $SID"

kill -9 "$DAEMON1" 2>/dev/null
wait "$DAEMON1" 2>/dev/null
DAEMON1=""
for _ in $(seq 120); do
  kill -0 "$CLIENT1" 2>/dev/null || break
  sleep 0.05
done
kill -0 "$CLIENT1" 2>/dev/null && fail "attached client did not observe daemon death"
wait "$CLIENT1" 2>/dev/null
CLIENT1=""
exec 3>&-

# A lifecycle kill is delayed by five seconds. Wait beyond that boundary so a
# stray delayed action cannot make this test pass transiently.
sleep 5.2
kill -0 "$SESSION_PID" 2>/dev/null || fail "session process $SESSION_PID died with daemon"
kill -0 "$SHELL_PID" 2>/dev/null || fail "session process $SHELL_PID died with daemon"

"$MESH" daemon >"$T/daemon2.log" 2>&1 &
DAEMON2=$!
wait_for_daemon "$DAEMON2" || fail "replacement daemon did not start: $(cat "$T/daemon2.log")"
for _ in $(seq 120); do
  [ "$(database_state)" = running ] && break
  sleep 0.05
done
[ "$(database_state)" = running ] || fail "replacement daemon did not rediscover $SID as running"
SESSION_AFTER=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$MESH_STATE_DIR/s/$SID/meta.json")
[ "$SESSION_AFTER" = "$SESSION_PID" ] || fail "session PID changed across daemon restart ($SESSION_PID to $SESSION_AFTER)"

"$MESH" attach --daemon "$SID" <"$T/in2" >"$T/out2" 2>"$T/client2.err" &
CLIENT2=$!
exec 4>"$T/in2"
echo "echo \$\$ > $T/shell_pid_after; echo AFTER_RESTART" >&4
wait_for_file "$T/shell_pid_after" || fail "reattached session did not answer: $(cat "$T/client2.err")"
[ "$(cat "$T/shell_pid_after")" = "$SHELL_PID" ] || fail "reattach reached a different process"
for _ in $(seq 120); do
  grep -q AFTER_RESTART "$T/out2" 2>/dev/null && break
  sleep 0.05
done
grep -q AFTER_RESTART "$T/out2" || fail "reattach did not relay output"

printf '\035' >&4
exec 4>&-
for _ in $(seq 120); do
  kill -0 "$CLIENT2" 2>/dev/null || break
  sleep 0.05
done
kill -0 "$CLIENT2" 2>/dev/null && fail "reattached client did not detach"
wait "$CLIENT2" 2>/dev/null
CLIENT2=""

echo "PASS: daemon restart preserved session $SID and process $SESSION_PID"
