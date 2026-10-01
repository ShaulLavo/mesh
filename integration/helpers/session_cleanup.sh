#!/usr/bin/env bash

wait_for_fixture_workers() {
  local state=$1 socket deadline=$((SECONDS + 10))
  for socket in "$state"/s/*/sock; do
    while [[ -e $socket ]]; do
      if (( SECONDS >= deadline )); then
        printf 'FAIL: fixture worker %s did not finish writing session data\n' "$socket" >&2
        return 1
      fi
      # Kill acknowledges the command exit before the worker's final disk writes.
      MESH_STATE_DIR="$state" "$MESH" ls >/dev/null 2>&1 || true
    done
  done
}
