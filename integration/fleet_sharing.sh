#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-fleet-sharing-bin.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$scratch/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/fleet_sharing.py" "$MESH"
