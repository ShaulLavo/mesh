#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-ssh-sessions.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"
go build -tags mesh_integration -o "$build_root/mesh" ./cmd/mesh
python3 integration/helpers/ssh_sessions.py "$build_root/mesh"
