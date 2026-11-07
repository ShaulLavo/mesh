#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  scratch="$XDG_CACHE_HOME/configuration-delivery-bin"
  mkdir -m 700 "$scratch"
  MESH="$scratch/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
screen="$XDG_CACHE_HOME/configuration-screen"
(cd "$repo_root" && go build -o "$screen" ./integration/helpers/dashboard-screen)
exec python3 "$repo_root/integration/helpers/configuration_delivery.py" "$MESH" "$screen"
