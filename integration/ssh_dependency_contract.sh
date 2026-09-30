#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh"
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

python3 scripts/check-ssh-dependencies.py
(cd third_party/wish && go test -race ./scp)
(cd third_party/sftp && go test -race . -run TestRequestFstat)
printf 'PASS: SSH dependency patches match pinned upstream and protect read-only file access\n'
