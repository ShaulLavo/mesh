#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t19.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"

packages=(./internal/wake ./internal/wakeclient ./internal/inhibit ./internal/protocol ./internal/transport ./internal/cli ./internal/daemon ./internal/edge ./internal/bootstrap ./cmd/mesh)
env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID go test -race "${packages[@]}"
go vet "${packages[@]}"
go build -o "$build_root/mesh" ./cmd/mesh
MESH="$build_root/mesh" bash integration/power_control.sh

for target in linux/amd64 linux/arm64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} \
    go build -o "$build_root/mesh-${target//\//-}" ./cmd/mesh
done

printf 'PASS: T19 wake permission, sender selection, recovery, and inhibition\n'
