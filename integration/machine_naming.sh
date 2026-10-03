#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 "$repo_root/integration/helpers/test_machine_naming.py"
scratch=$(mktemp -d "${MESH_SHORT_TMP:-${TMPDIR:-/tmp}}/mesh-name.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$scratch/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
(cd "$repo_root" && go build -o "$scratch/control-client" ./integration/helpers/control-client)
TMPDIR="$scratch" python3 "$repo_root/integration/helpers/machine_naming.py" "$MESH" --control-client "$scratch/control-client"
