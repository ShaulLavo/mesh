#!/usr/bin/env bash

update_proof_cleanup() {
  local status=$? root=$1
  if [[ -e $root/proof/cleanup-pending ]]; then
    printf 'Fixture cleanup unconfirmed. Retained %s\n' "$root" >&2
    if (( status == 0 )); then status=1; fi
  else
    rm -rf -- "$root" || status=1
  fi
  exit "$status"
}
