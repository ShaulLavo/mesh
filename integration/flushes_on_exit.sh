#!/usr/bin/env bash
# A worker must flush accepted terminal output and session.exit before its
# process ends, even when the client consumes output slowly.
set -uo pipefail

MESH=${MESH:-$PWD/mesh}
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
trap 'rm -rf "$T"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

slow_copy() {
  local chunk=""
  while IFS= read -r -N 4096 chunk; do
    printf '%s' "$chunk"
    sleep .002
  done
  [ -z "$chunk" ] || printf '%s' "$chunk"
  return 0
}

"$MESH" local -- sh -c \
  'head -c 131072 /dev/zero | tr "\000" x; printf FINAL_MARKER; exit 7' \
  2>"$T/stderr" |
  slow_copy >"$T/stdout"
STATUS=("${PIPESTATUS[@]}")

[ "${STATUS[0]}" -eq 7 ] || fail "client exit status was ${STATUS[0]}, want 7: $(cat "$T/stderr")"
[ "${STATUS[1]}" -eq 0 ] || fail "slow output reader failed with ${STATUS[1]}"
[ "$(wc -c <"$T/stdout")" -eq 131084 ] || fail "terminal output was truncated"
[ "$(tail -c 12 "$T/stdout")" = FINAL_MARKER ] || fail "final output marker is missing"
grep -q 'session .* exited (7)' "$T/stderr" || fail "session.exit was not delivered: $(cat "$T/stderr")"

echo "PASS: queued terminal output and exit status flushed before worker shutdown"
