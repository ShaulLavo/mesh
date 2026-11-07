#!/usr/bin/env bash

require_no_boundary_match() {
  local message=$1 status=0
  shift
  grep -rnE "$@" || status=$?
  case "$status" in
    0)
      printf 'FAIL: %s\n' "$message" >&2
      exit 1
      ;;
    1) return 0 ;;
    *)
      printf 'FAIL: private boundary search failed with status %s\n' "$status" >&2
      exit "$status"
      ;;
  esac
}
