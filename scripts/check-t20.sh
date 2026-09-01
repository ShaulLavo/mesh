#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

go test -race -count=1 ./internal/bootstrap ./internal/cli ./cmd/mesh ./scripts/install
go vet ./internal/bootstrap ./internal/cli ./cmd/mesh ./scripts/install
bash integration/bootstrap_installers.sh

unexpected_auth_flags=$(rg -n --glob '*.go' --glob '*.sh' --glob '!**/*_test.go' -- '--auth-key' internal cmd scripts/install |
  rg -v -- '--auth-key=file:/dev/stdin|tailscale-auth-key-file' || true)
if [[ -n $unexpected_auth_flags ]]; then
  printf 'unexpected production auth-key construction:\n%s\n' "$unexpected_auth_flags" >&2
  exit 1
fi

printf 'PASS: T20 provisioning contract\n'
