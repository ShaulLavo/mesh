#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

packages=(./internal/serve ./internal/sshfs ./internal/sshd ./internal/daemon)
go mod tidy -diff
python3 scripts/check-ssh-dependencies.py
go test -race "${packages[@]}"
go vet "${packages[@]}"
(cd third_party/wish && go test -race ./scp)
(cd third_party/sftp && go test -race . -run TestRequestFstat)
bash integration/ssh_files.sh
printf 'PASS: T16 read-only SFTP and SCP contract\n'
