#!/usr/bin/env bash
# mesh hibernate, the hibernated row in mesh ls, wake on attach by ID, and
# mesh gc, against a real daemon, real workers and a fake Claude provider.
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-hibernation.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$build_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 "$repo_root/integration/helpers/hibernation_cli.py" "$MESH"
