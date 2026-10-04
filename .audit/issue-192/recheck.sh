#!/usr/bin/env bash
set -euo pipefail
export PATH=$HOME/.local/share/mise/shims:$PATH
task_scratch=$(mktemp -d /work/tmp/i192-XXXX)
trap 'rm -rf -- "$task_scratch"' EXIT
export TMPDIR="$task_scratch"
go test -race ./internal/machinename ./internal/cli > .audit/issue-192/affected-race.txt 2>&1
./scripts/gates.sh > .audit/issue-192/gates.txt 2>&1
go test -race ./internal/machinename ./internal/cli -run '^Test(ClaimCacheBatch|AuthenticatedNameCacheLock)' -count=1 -v > .audit/issue-192/green.txt 2>&1
