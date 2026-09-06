#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

packages=(./internal/tunnel ./internal/storage ./internal/edge ./internal/sshd ./internal/daemon ./internal/cli ./internal/identity ./internal/protocol)
go mod tidy -diff
go test -race "${packages[@]}"
go vet "${packages[@]}"
bash integration/reverse_tunnels.sh
printf 'PASS: T18 named reverse tunnels, replay protection and disconnect cleanup\n'
