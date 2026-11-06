#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-agent-reads.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$root/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
go build -o "$root/fixture" ./integration/helpers/agent-host-fixture
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/agent_reads.py" "$MESH" "$root/fixture" "$root"
