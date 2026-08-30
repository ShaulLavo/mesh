#!/usr/bin/env bash
# A session program may ask the terminal questions: DSR cursor position, device
# attributes, DECRQM. The attached client's real terminal answers those; Mesh's
# shadow emulator must never try to, because nothing reads its replies and the
# worker holds its mutex across the write. A regression here wedges the worker
# permanently: no output reaches the ring, kill never completes, and meta.json
# stays "running" forever. That is what running nvim or tmux used to do.
set -uo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
trap 'rm -rf "$T"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Each query is one the emulator answers. Any of them blocking is the defect.
for query in '\033[6n' '\033[?6n' '\033[c' '\033[>c' '\033[5n'; do
  # A wedged client sits in raw-mode relay and ignores SIGTERM, so this needs
  # --kill-after or a regression hangs the run instead of failing it.
  timeout --kill-after=2s 15s "$MESH" local -- \
    sh -c "printf '${query}'; printf MARKER_AFTER_QUERY; exit 4" \
    >"$T/out" 2>"$T/err"
  status=$?

  # 124 is the SIGTERM timeout, 137 the SIGKILL that follows it.
  case "$status" in
    124 | 137) fail "session wedged on query ${query}" ;;
  esac
  [ "$status" -eq 4 ] || fail "exit status was $status for ${query}, want 4: $(cat "$T/err")"
  grep -q MARKER_AFTER_QUERY "$T/out" || fail "output after ${query} never reached the client"
  grep -q 'exited (4)' "$T/err" || fail "session.exit was not delivered for ${query}: $(cat "$T/err")"
done

# The worker must actually be gone, not merely detached, and its recorded state
# must say so. A wedged worker leaves meta.json reading "running" forever.
sleep 1
for meta in "$T"/state/s/*/meta.json; do
  [ -e "$meta" ] || continue
  ! grep -q '"state": *"running"' "$meta" || fail "a session is still marked running: $meta"
done

# Scope this to our own state directory. verify.sh runs every script
# concurrently against one shared binary, so matching on the binary name alone
# would pick up other scripts' healthy workers.
! pgrep -af "session-worker .*--dir $T/" >/dev/null 2>&1 || fail "a session worker outlived its session"

echo "PASS: terminal queries do not wedge the worker"
