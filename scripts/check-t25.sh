#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
build_root=$(mktemp -d "${TMPDIR:-/work/tmp}/mesh-t25.XXXXXX")
trap 'rm -rf -- "$build_root"' EXIT
cd "$repo_root"
# Legacy terminal libraries query the PTY during package init, before helpers
# can enforce their hook timeout or produce a plain error.
dependencies=$(go list -deps ./cmd/mesh)
if rg -q '^github\.com/charmbracelet/(bubbletea|lipgloss)$' <<<"$dependencies"; then
  printf 'FAIL: legacy terminal initialization in the Mesh executable\n' >&2
  exit 1
fi
packages=(./internal/agentresume ./internal/recovery ./internal/worker ./internal/protocol ./internal/daemon ./internal/cli ./internal/tui ./internal/sshd)
env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID go test -race "${packages[@]}"
go vet "${packages[@]}"
go build -o "$build_root/mesh" ./cmd/mesh
for scenario in agent_recovery recovery_races recovery_remote_survival recovery_ssh recovery_history; do
  env -u MESH_DEPTH -u MESH_SESSION_ID -u MESH_HOST_ID \
    MESH="$build_root/mesh" bash "integration/$scenario.sh"
done
for target in linux/amd64 linux/arm64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -o "$build_root/mesh-${target//\//-}" ./cmd/mesh
done
printf 'PASS: T25 conversation recovery contract (fake providers; native compatibility is recorded separately)\n'
