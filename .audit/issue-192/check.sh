#!/usr/bin/env bash
set -euo pipefail
export PATH=$HOME/.local/share/mise/shims:$PATH
task_scratch=$(mktemp -d /work/tmp/i192-XXXX)
trap 'rm -rf -- "$task_scratch"' EXIT
export TMPDIR="$task_scratch"
go mod tidy -diff > .audit/issue-192/tidy.txt 2>&1
go vet ./... > .audit/issue-192/vet.txt 2>&1
go test -race ./... > .audit/issue-192/race.txt 2>&1
./scripts/verify.sh > .audit/issue-192/integration.txt 2>&1
./scripts/gates.sh > .audit/issue-192/gates.txt 2>&1
go test -race ./internal/machinename ./internal/cli -run '^Test(ClaimCacheBatch|AuthenticatedNameCacheLock)' -count=1 -v > .audit/issue-192/green.txt 2>&1
