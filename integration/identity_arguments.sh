#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-id-args.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$scratch/mesh"
  go build -o "$MESH" ./cmd/mesh
fi

for id in \
  O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik \
  -kg0FH9uaQw2k-_2EzYEZAPNiuKhTzGzxAc1hWkjlWU \
  --SIk3MI1hDVozJz3MTfWKGmr_C7ylJNQV3c8JZ8zFI; do
  "$MESH" device approve "$id" --allow-root >"$scratch/approve"
  command grep -q '^approve SHA256:' "$scratch/approve"
  "$MESH" device revoke "$id" >"$scratch/revoke"
  command grep -q '^revoke SHA256:' "$scratch/revoke"
  if "$MESH" device revoke "$id" --typo >"$scratch/typo" 2>&1; then
    echo 'FAIL: unknown option was accepted' >&2
    exit 1
  fi
  command grep -q 'unknown flag: --typo' "$scratch/typo"
done

generated=$("$MESH" device identity --json | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])')
"$MESH" device approve "$generated" --allow-root >"$scratch/approve"
"$MESH" device revoke "$generated" >"$scratch/revoke"
echo 'PASS: ordinary, leading-dash, double-dash and generated identity operands'
