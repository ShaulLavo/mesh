#!/usr/bin/env bash
# Acceptance test for the Mesh contract: killing the client must not kill the
# command. Run from the repo root after `go build -o mesh ./cmd/mesh`.
set -uo pipefail

MESH=${MESH:-$PWD/mesh}
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
trap 'rm -rf "$T"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mkfifo "$T/in1" "$T/in2"

# 1. Start a session running an interactive shell, driven through a fifo.
"$MESH" local -- bash --noprofile --norc < "$T/in1" > "$T/out1" 2>&1 &
CLIENT1=$!
exec 3>"$T/in1"

for _ in $(seq 100); do [ -s "$T/out1" ] && break; sleep 0.05; done

echo "echo \$\$ > $T/pid1" >&3
echo "echo MARKER_ONE" >&3
for _ in $(seq 100); do [ -s "$T/pid1" ] && break; sleep 0.05; done
[ -s "$T/pid1" ] || fail "shell never ran the first command"
PID1=$(cat "$T/pid1")

# 2. Kill the client as violently as possible. This stands in for the laptop
#    lid closing, the terminal being closed, or the wifi vanishing.
kill -9 "$CLIENT1" 2>/dev/null
exec 3>&-
wait "$CLIENT1" 2>/dev/null

sleep 0.3
kill -0 "$PID1" 2>/dev/null || fail "shell $PID1 died with its client"

# 3. The session must still report itself as running.
"$MESH" ls | grep -q running || fail "mesh ls does not show a running session: $("$MESH" ls)"

# 4. Reattach and prove it is the same process, still holding its state.
"$MESH" local -r < "$T/in2" > "$T/out2" 2>&1 &
CLIENT2=$!
exec 4>"$T/in2"
sleep 0.4
echo "echo \$\$ > $T/pid2" >&4
for _ in $(seq 100); do [ -s "$T/pid2" ] && break; sleep 0.05; done
[ -s "$T/pid2" ] || fail "reattached shell never responded"
PID2=$(cat "$T/pid2")

[ "$PID1" = "$PID2" ] || fail "reattached to a different process ($PID1 vs $PID2)"

# 5. Scrollback from before the disconnect must come back on reattach.
grep -q MARKER_ONE "$T/out2" || fail "reattach did not replay output from before the disconnect"

# 6. Clean up: killing the session must actually kill it.
SID=$("$MESH" ls | awk 'NR==2 {print $1}')
"$MESH" kill "$SID" >/dev/null
exec 4>&-
kill -9 "$CLIENT2" 2>/dev/null; wait "$CLIENT2" 2>/dev/null
for _ in $(seq 60); do kill -0 "$PID1" 2>/dev/null || break; sleep 0.05; done
kill -0 "$PID1" 2>/dev/null && fail "mesh kill did not stop the session"

echo "PASS: session $SID survived client death (pid $PID1), replayed scrollback, and died on request"
