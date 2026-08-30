#!/usr/bin/env bash
# Reading recent output is a one-shot control request. It must not steal the
# terminal from the client that is already attached.
set -uo pipefail

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
"$MESH" local -- bash --noprofile --norc <"$T/in" >"$T/client.out" 2>"$T/client.err" &
CLIENT=$!
exec 3>"$T/in"
for _ in $(seq 100); do
  SID=$(sed -n 's/^session \([0-9A-Z][0-9A-Z][0-9A-Z][0-9A-Z]\)$/\1/p' "$T/client.err" | head -n1)
  [ -n "$SID" ] && break
  sleep 0.05
done
[ -n "$SID" ] || fail "session did not start: $(cat "$T/client.err")"

echo 'echo BEFORE_LOGS; echo $$ > '"$T/pid-before" >&3
for _ in $(seq 100); do
  grep -q BEFORE_LOGS "$T/client.out" 2>/dev/null && [ -s "$T/pid-before" ] && break
  sleep 0.05
done
[ -s "$T/pid-before" ] || fail "attached shell did not answer before logs"

"$MESH" logs --tail 4096 "$SID" >"$T/logs.out" || fail "logs command failed"
grep -q BEFORE_LOGS "$T/logs.out" || fail "recent terminal output was missing: $(cat "$T/logs.out")"
kill -0 "$CLIENT" 2>/dev/null || fail "logs stole or closed the active attachment"

echo 'echo AFTER_LOGS; echo $$ > '"$T/pid-after" >&3
for _ in $(seq 100); do
  grep -q AFTER_LOGS "$T/client.out" 2>/dev/null && [ -s "$T/pid-after" ] && break
  sleep 0.05
done
[ -s "$T/pid-after" ] || fail "attached shell did not answer after logs"
[ "$(cat "$T/pid-before")" = "$(cat "$T/pid-after")" ] || fail "logs reached a different shell"

"$MESH" kill "$SID" >/dev/null || fail "could not clean up session"
exec 3>&-
wait "$CLIENT" 2>/dev/null || true
CLIENT=""
for _ in $(seq 100); do
  [ ! -S "$MESH_STATE_DIR/s/$SID/sock" ] && break
  sleep 0.02
done
printf 'old diagnostic\nEXITED_LOG\n' >>"$MESH_STATE_DIR/s/$SID/worker.log"
"$MESH" logs --tail 11 "$SID" >"$T/exited-logs.out" || fail "exited logs command failed"
[ "$(cat "$T/exited-logs.out")" = EXITED_LOG ] || fail "exited log tail was not bounded correctly"
SID=""

echo "PASS: live logs do not attach, and exited logs use the bounded durable tail"
