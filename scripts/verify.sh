#!/usr/bin/env bash
# Build each Mesh variant once, then run integrations with bounded concurrency.
# The controller owns every job and its state; this wrapper creates nothing.
set -uo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd) || exit 1
exec python3 -B "$repo_root/scripts/verify_runner.py" "$repo_root" "$BASH"
