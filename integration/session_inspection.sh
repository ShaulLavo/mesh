#!/usr/bin/env bash
# The session picker inspection path is intentionally not a standalone CLI
# command. Run its in-package end-to-end test so the real WebSocket, daemon,
# Unix worker, process observer, and terminal emulator stay covered together.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

go test ./internal/daemon \
  -run '^TestSessionInspectionTraversesWebSocketDaemonAndLiveWorker$' \
  -count=1

echo "PASS: session inspection crossed WebSocket, daemon, worker, and emulator without attaching"
