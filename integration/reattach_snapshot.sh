#!/usr/bin/env bash
# Reattaching to a full-screen program must repaint its current screen without
# replaying raw output from the screen that preceded it.
set -uo pipefail

MESH=${MESH:-$PWD/mesh}
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
C1=""
C2=""
SID=""

cleanup() {
  [ -z "$SID" ] || "$MESH" kill "$SID" >/dev/null 2>&1 || true
  [ -z "$C1" ] || kill -9 "$C1" 2>/dev/null || true
  [ -z "$C2" ] || kill -9 "$C2" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

wait_for_text() {
  local text=$1
  local file=$2
  for _ in $(seq 100); do
    grep -Fq "$text" "$file" 2>/dev/null && return 0
    sleep 0.05
  done
  return 1
}

mkfifo "$T/in1" "$T/in2"

"$MESH" local -- sh -c '
  printf "RAW_HISTORY_MARKER\r\n"
  printf "\033[?1049h\033[2J\033[3;4H\033[1;32mCURRENT_SCREEN\033[0m"
  IFS= read -r _
  printf "\033[?1049l"
' <"$T/in1" >"$T/out1" 2>&1 &
C1=$!
exec 3>"$T/in1"

wait_for_text CURRENT_SCREEN "$T/out1" || fail "full-screen command did not paint"
printf '\035' >&3
for _ in $(seq 100); do
  kill -0 "$C1" 2>/dev/null || break
  sleep 0.05
done
kill -0 "$C1" 2>/dev/null && fail "first client did not detach"
wait "$C1" 2>/dev/null
C1=""
exec 3>&-

SID=$("$MESH" ls | awk 'NR == 2 { print $1 }')
[ -n "$SID" ] || fail "session was not listed after detach"

"$MESH" attach "$SID" <"$T/in2" >"$T/out2" 2>&1 &
C2=$!
exec 4>"$T/in2"

wait_for_text CURRENT_SCREEN "$T/out2" || fail "reattach did not repaint the current screen"
grep -Fq RAW_HISTORY_MARKER "$T/out2" && fail "reattach replayed raw history instead of a snapshot"
grep -Fq $'\033[?1049h' "$T/out2" || fail "snapshot did not restore the alternate screen"

printf 'exit\n' >&4
exec 4>&-
for _ in $(seq 100); do
  kill -0 "$C2" 2>/dev/null || break
  sleep 0.05
done
kill -0 "$C2" 2>/dev/null && fail "session did not exit after input"
wait "$C2" 2>/dev/null
C2=""

echo "PASS: reattach restored the rendered alternate screen without raw history"
