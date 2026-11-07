#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

# This fixture starts no daemon and opens no network listener.
task_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-skill-install.XXXXXX")
trap 'rm -rf -- "$task_root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$task_root/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/skill_install.py" "$MESH" "$task_root"
