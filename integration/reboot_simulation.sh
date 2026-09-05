#!/usr/bin/env bash
# A running record from another boot is historical evidence, not a process the
# new daemon may resurrect.
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
CLIENT=""
SID=""
SHELL_PID=""
WORKER_PID=""

cleanup() {
  [ -z "$CLIENT" ] || kill -9 "$CLIENT" 2>/dev/null || true
  [ -z "$DAEMON1" ] || kill -9 "$DAEMON1" 2>/dev/null || true
  [ -z "$DAEMON2" ] || kill -9 "$DAEMON2" 2>/dev/null || true
  [ -z "$WORKER_PID" ] || kill -9 "$WORKER_PID" 2>/dev/null || true
  [ -z "$SHELL_PID" ] || kill -9 "$SHELL_PID" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

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

mkfifo "$T/in"
"$MESH" daemon >"$T/daemon1.log" 2>&1 &
DAEMON1=$!
wait_for_daemon "$DAEMON1" || fail "first daemon did not start: $(cat "$T/daemon1.log")"

"$MESH" local --daemon -- bash --noprofile --norc <"$T/in" >"$T/out" 2>"$T/client.err" &
CLIENT=$!
exec 3>"$T/in"
for _ in $(seq 120); do
  SID=$(sed -n 's/^new session \([0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]\)$/\1/p' "$T/client.err" | head -n1)
  [ -n "$SID" ] && break
  sleep 0.05
done
[ -n "$SID" ] || fail "client did not create a session: $(cat "$T/client.err")"
echo "echo \$\$ > $T/shell_pid" >&3
for _ in $(seq 120); do [ -s "$T/shell_pid" ] && break; sleep 0.05; done
[ -s "$T/shell_pid" ] || fail "session shell did not start"
SHELL_PID=$(cat "$T/shell_pid")
SESSION_PID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["pid"])' "$MESH_STATE_DIR/s/$SID/meta.json") || fail "could not read session PID"
WORKER_PID=$(python3 - "$SESSION_PID" <<'PY'
import sys

with open(f"/proc/{sys.argv[1]}/stat", encoding="utf-8") as status:
    print(status.read().split()[3])
PY
) || fail "could not identify session worker"

kill -9 "$DAEMON1" 2>/dev/null
wait "$DAEMON1" 2>/dev/null
DAEMON1=""
for _ in $(seq 120); do
  kill -0 "$CLIENT" 2>/dev/null || break
  sleep 0.05
done
kill -0 "$CLIENT" 2>/dev/null && fail "attached client did not observe daemon death"
wait "$CLIENT" 2>/dev/null
CLIENT=""
exec 3>&-

python3 - "$MESH_STATE_DIR/s/$SID/meta.json" <<'PY' || fail "could not prepare previous-boot metadata"
import json
import os
import sys
import time

path = sys.argv[1]
deadline = time.monotonic() + 4
while True:
    with open(path, encoding="utf-8") as source:
        metadata = json.load(source)
    if metadata.get("state") == "detached":
        break
    if time.monotonic() >= deadline:
        raise RuntimeError("worker did not publish detached metadata after daemon death")
    time.sleep(0.01)
metadata["bootId"] = "simulated-previous-boot"
temporary = path + ".reboot"
with open(temporary, "w", encoding="utf-8") as destination:
    json.dump(metadata, destination)
    destination.flush()
    os.fsync(destination.fileno())
os.replace(temporary, path)
PY

"$MESH" daemon >"$T/daemon2.log" 2>&1 &
DAEMON2=$!
wait_for_daemon "$DAEMON2" || fail "replacement daemon did not start: $(cat "$T/daemon2.log")"
for _ in $(seq 120); do
  [ "$(database_state)" = interrupted ] && break
  sleep 0.05
done
[ "$(database_state)" = interrupted ] || fail "old-boot session was not recorded as interrupted"
kill -0 "$WORKER_PID" 2>/dev/null || fail "boot mismatch test worker died before classification"
kill -0 "$SESSION_PID" 2>/dev/null || fail "boot mismatch test session died before classification"
"$MESH" ls --daemon | grep -q "$SID.*interrupted" || fail "daemon did not report $SID as interrupted"
if timeout --kill-after=1s 3s "$MESH" attach --daemon "$SID" </dev/null >"$T/resurrect.out" 2>&1; then
  fail "daemon attached to interrupted session $SID"
fi
grep -q interrupted "$T/resurrect.out" || fail "daemon rejected interrupted session without explaining its state"

echo "PASS: simulated reboot classified reachable session $SID as interrupted and refused resurrection"
