#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-list-read-only.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$scratch/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/list_read_only.py" "$MESH" "$scratch"
