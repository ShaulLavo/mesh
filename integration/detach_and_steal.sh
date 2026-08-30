#!/usr/bin/env bash
# Covers the two client-side behaviours we committed to: ctrl+] detaches
# without killing anything, and a second attach steals the session from the
# first (single attacher, steal on attach).
set -uo pipefail

# Build unless the caller pointed at a binary of their own. A stale ./mesh
# fails these scripts for reasons that have nothing to do with the code.
if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
trap 'rm -rf "$T"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mkfifo "$T/in1" "$T/in2"

"$MESH" local -- bash --noprofile --norc < "$T/in1" > "$T/out1" 2>&1 &
C1=$!
exec 3>"$T/in1"
for _ in $(seq 100); do [ -s "$T/out1" ] && break; sleep 0.05; done
echo "echo \$\$ > $T/pid1" >&3
for _ in $(seq 100); do [ -s "$T/pid1" ] && break; sleep 0.05; done
PID1=$(cat "$T/pid1") || fail "shell never started"

# ctrl+] is 0x1d. Sending it must return the client cleanly, not kill the shell.
printf '\035' >&3
for _ in $(seq 100); do kill -0 "$C1" 2>/dev/null || break; sleep 0.05; done
kill -0 "$C1" 2>/dev/null && fail "ctrl+] did not detach the client"
grep -q "detached" "$T/out1" || fail "client did not report detaching: $(cat "$T/out1")"
kill -0 "$PID1" 2>/dev/null || fail "ctrl+] killed the shell"
exec 3>&-

# Reattach twice: the second attach must evict the first.
SID=$("$MESH" ls | awk 'NR==2 {print $1}')
"$MESH" attach "$SID" < /dev/null > "$T/out_a" 2>&1 &
CA=$!
sleep 0.4
"$MESH" attach "$SID" < "$T/in2" > "$T/out_b" 2>&1 &
CB=$!
exec 4>"$T/in2"
sleep 0.5

kill -0 "$CA" 2>/dev/null && fail "first client was not evicted by the second"

# And the stealing client has a working session.
echo "echo \$\$ > $T/pid2" >&4
for _ in $(seq 100); do [ -s "$T/pid2" ] && break; sleep 0.05; done
[ "$(cat "$T/pid2")" = "$PID1" ] || fail "stealing client got a different process"

"$MESH" kill "$SID" >/dev/null
exec 4>&-
kill -9 "$CB" 2>/dev/null; wait "$CB" 2>/dev/null
echo "PASS: ctrl+] detaches cleanly and a second attach steals session $SID"
