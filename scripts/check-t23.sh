#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t23.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"

packages=(./internal/protocol ./internal/worker ./internal/transport ./internal/daemon ./internal/cli ./internal/tui)
env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID go test -race "${packages[@]}"
go vet "${packages[@]}"
go build -o "$build_root/mesh" ./cmd/mesh

for scenario in nested_detach_keys nested_resize window_death_keeps_remote window_entry window_relaunch; do
  env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID \
    MESH="$build_root/mesh" bash "integration/$scenario.sh"
done

for target in linux/amd64 linux/arm64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} \
    go build -o "$build_root/mesh-${target//\//-}" ./cmd/mesh
done

printf 'PASS: T23 terminal entry, nesting, and recovery\n'
