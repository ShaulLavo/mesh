#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

if [ -z "${MESH:-}" ]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh
fi
T=$(mktemp -d)
export MESH_STATE_DIR="$T/state"
export MESH_CONFIG_DIR="$T/config"
trap 'rm -rf "$T"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# Flag parsing can fail before Cobra reaches the privacy switch.
if "$MESH" --private-secret-flag --privacy ls >"$T/stdout" 2>"$T/stderr"; then
  fail "invalid flag succeeded"
fi
if grep -q 'private-secret-flag' "$T/stderr"; then
  fail "parser error revealed the original invalid flag"
fi
grep -q 'privacy=false' "$T/stderr" || fail "parser error lost privacy guidance"

for mode in live previous; do
  command=("$MESH" logs 7K3D)
  if [[ $mode == previous ]]; then command+=(--previous); fi
  MESH_PRIVACY=1 "${command[@]}" >"$T/stdout" 2>"$T/stderr"
  grep -qx 'Terminal output hidden by privacy mode' "$T/stdout" || fail "logs were not withheld"
done

if MESH_PRIVACY=1 "$MESH" --privacy=false logs 7K3D >"$T/stdout" 2>"$T/stderr"; then
  fail "override pretended a nonexistent session existed"
fi
if grep -q 'Terminal output hidden by privacy mode' "$T/stdout"; then
  fail "explicit false did not override the environment"
fi

echo "PASS: privacy covers early errors and logs while explicit false restores normal behavior"
