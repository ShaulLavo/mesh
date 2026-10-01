#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/session_cleanup.sh" || exit 1
set -euo pipefail

T=$(mktemp -d "${TMPDIR:-/tmp}/mesh-client-signal.XXXXXX")
export MESH_STATE_DIR="$T/state" MESH_CONFIG_DIR="$T/config"
CLIENT1='' CLIENT2='' SID=''

cleanup() {
  for client in "$CLIENT1" "$CLIENT2"; do
    if [[ -n $client ]]; then
      kill -TERM "$client" 2>/dev/null || true
      wait "$client" 2>/dev/null || true
    fi
  done
  if [[ -z $SID && -n ${MESH:-} && -x $MESH ]]; then
    SID=$("$MESH" ls 2>/dev/null | awk 'NR==2 {print $1}') || true
  fi
  if [[ -n $SID ]]; then "$MESH" kill "$SID" >/dev/null 2>&1 || true; fi
  wait_for_fixture_workers "$MESH_STATE_DIR" || return 1
  rm -rf -- "$T"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

if [[ -z ${MESH:-} ]]; then
  MESH="$T/mesh"
  go build -o "$MESH" ./cmd/mesh
fi

mkfifo "$T/in1" "$T/in2"
"$MESH" local -- bash --noprofile --norc <"$T/in1" >"$T/out1" 2>&1 &
CLIENT1=$!
exec 3>"$T/in1"
printf 'echo $$ > %q\necho SIGNAL_MARKER\n' "$T/pid1" >&3
for _ in $(seq 100); do [[ -s $T/pid1 ]] && break; sleep 0.05; done
[[ -s $T/pid1 ]] || fail "shell never started"
PID1=$(cat "$T/pid1")
SID=$("$MESH" ls | awk 'NR==2 {print $1}')
[[ -n $SID ]] || fail "session was not listed"

kill -TERM "$CLIENT1"
for _ in $(seq 100); do kill -0 "$CLIENT1" 2>/dev/null || break; sleep 0.05; done
if kill -0 "$CLIENT1" 2>/dev/null; then
  kill -KILL "$CLIENT1" 2>/dev/null || true
  fail "SIGTERM did not cancel the blocked client attachment"
fi
wait "$CLIENT1" 2>/dev/null || true
CLIENT1=
exec 3>&-
kill -0 "$PID1" 2>/dev/null || fail "SIGTERM to the client killed its command"

"$MESH" attach "$SID" <"$T/in2" >"$T/out2" 2>&1 &
CLIENT2=$!
exec 4>"$T/in2"
printf 'echo $$ > %q\n' "$T/pid2" >&4
for _ in $(seq 100); do [[ -s $T/pid2 ]] && break; sleep 0.05; done
[[ -s $T/pid2 ]] || fail "reattached shell never responded"
[[ $(cat "$T/pid2") == "$PID1" ]] || fail "reattachment started a different command"
for _ in $(seq 100); do grep -q SIGNAL_MARKER "$T/out2" && break; sleep 0.05; done
grep -q SIGNAL_MARKER "$T/out2" || fail "reattachment lost the pre-signal output"
printf '\035' >&4
for _ in $(seq 100); do kill -0 "$CLIENT2" 2>/dev/null || break; sleep 0.05; done
kill -0 "$CLIENT2" 2>/dev/null && fail "reattached client did not detach"
wait "$CLIENT2"
CLIENT2=
exec 4>&-

echo "PASS: SIGTERM cancelled the CLI; session $SID survived and reattached with pid $PID1"
