#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-t22.XXXXXX")

cleanup() {
  rm -rf -- "$build_root"
}
trap cleanup EXIT

cd "$repo_root"

packages=(
  ./internal/protocol
  ./internal/terminal
  ./internal/inspection
  ./internal/worker
  ./internal/daemon
  ./internal/cli
  ./internal/tui
)

env -u MESH_DEPTH go test -race -count=1 "${packages[@]}"
env -u MESH_DEPTH go vet "${packages[@]}"

for target in linux/amd64 darwin/arm64; do
  goos=${target%/*}
  goarch=${target#*/}
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -o "$build_root/mesh-$goos-$goarch" ./cmd/mesh
done

printf 'PASS: T22 live session inspector\n'
