#!/usr/bin/env bash
# ctrl+c must kill the running command, the way it does in any terminal. That
# needs the child to be a session leader with the PTY slave as its controlling
# terminal, because the line discipline only sends SIGINT to the foreground
# process group of a terminal that has one. Without Setsid and Setctty the
# child has no ctty, ctrl+c is an inert byte, and interactive shells report
# that they cannot set the terminal process group.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
trap 'rm -rf "$T"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# --raw so the 0x03 is passed through rather than intercepted as a detach key.
# The trailing sleep holds stdin open long enough for the byte to be relayed.
( printf '\003'; sleep 5 ) |
  timeout --kill-after=2s 25s "$MESH" local --raw -- \
    sh -c 'sleep 20; echo CHILD_SURVIVED_SIGINT' >"$T/out" 2>"$T/err"
status=$?

case "$status" in
  124 | 137) fail "session did not end after ctrl+c" ;;
esac
grep -q CHILD_SURVIVED_SIGINT "$T/out" && fail "ctrl+c did not reach the child; it ran to completion"
grep -q 'exited' "$T/err" || fail "session.exit was not delivered: $(cat "$T/err")"

# The controlling terminal itself: a child with no ctty reports tty_nr 0 and
# tpgid -1, which is the state that makes every terminal-generated signal a
# no-op. Linux only; the shape of /proc is what makes this assertable.
if [ -r /proc/self/stat ]; then
  timeout 20s "$MESH" local -- sh -c 'sleep 8' >/dev/null 2>&1 &
  client=$!
  sleep 3
  worker=$(pgrep -f "session-worker .*--dir $T/" | head -1)
  [ -n "$worker" ] || fail "no worker found for the ctty check"
  child=$(pgrep -P "$worker" | head -1)
  [ -n "$child" ] || fail "worker has no child process"

  read -r _ _ _ _ pgrp sess tty_nr tpgid _ < <(sed 's/(.*)/x/' "/proc/$child/stat")
  [ "$tty_nr" -ne 0 ] || fail "child has no controlling terminal (tty_nr=0)"
  [ "$tpgid" -eq "$pgrp" ] || fail "child is not the foreground group (tpgid=$tpgid pgrp=$pgrp)"
  [ "$sess" -eq "$child" ] || fail "child is not a session leader (sess=$sess pid=$child)"

  kill -9 "$client" 2>/dev/null
  wait "$client" 2>/dev/null
fi

echo "PASS: terminal signals reach the session child"
