#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  scratch="$XDG_CACHE_HOME/fleet-profile-bin"
  mkdir -m 700 "$scratch"
  MESH="$scratch/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
exec python3 "$repo_root/integration/helpers/fleet_profile.py" "$MESH"
