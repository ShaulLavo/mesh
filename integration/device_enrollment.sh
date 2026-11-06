#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
root=$(mktemp -d)
trap 'status=$?; if (( status == 0 )); then rm -rf -- "$root"; else echo "retained enrollment build: $root" >&2; fi' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$root/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
helpers=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/helpers" && pwd)
python3 "$helpers/enrollment_snapshot_test.py"
python3 "$helpers/native_enrollment_test.py"
python3 "$helpers/native_enrollment.py" "$MESH"
