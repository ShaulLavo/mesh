#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-scoped-access.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$root/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/scoped_access.py" "$MESH" "$root"
