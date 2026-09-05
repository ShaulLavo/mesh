#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t17.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"

packages=(./internal/cli ./internal/tui ./internal/sshd ./internal/daemon ./cmd/mesh)
env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID go test -race "${packages[@]}"
go vet "${packages[@]}"
for scenario in ssh_front_door ssh_sessions; do
  env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID \
    bash "integration/$scenario.sh"
done

for target in linux/amd64 linux/arm64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} \
    go build -o "$build_root/mesh-${target//\//-}" ./cmd/mesh
done

printf 'PASS: T17 SSH session lifecycle and terminal handoff\n'
