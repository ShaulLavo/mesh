#!/usr/bin/env bash
# Bare mesh rejects redirected input instead of waiting for picker keystrokes.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
export MESH_CONFIG_DIR="$T/config"

cleanup() {
  rm -rf "$T"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

timeout --kill-after=1s 2s "$MESH" </dev/null >"$T/stdout" 2>"$T/stderr"
status=$?
[ "$status" -ne 0 ] || fail "bare mesh accepted redirected input"
[ "$status" -ne 124 ] || fail "bare mesh hung without a terminal"
grep -qi 'interactive picker needs a terminal' "$T/stderr" ||
  fail "non-terminal error did not explain the host and session forms: $(cat "$T/stderr")"

echo "PASS: bare mesh returns promptly when no terminal is available"
