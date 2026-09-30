#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

while IFS='=' read -r name _; do
  case "$name" in
    PATH|HOME|TMPDIR|TERM|LANG|XDG_RUNTIME_DIR|DBUS_SESSION_BUS_ADDRESS|GOCACHE|GOMODCACHE|MESH|MESH_INTEGRATION_BINARY|MESH_CONFIG_DIR|PWD|SHLVL|_) ;;
    *) printf 'FAIL: integration environment inherited %s\n' "$name" >&2; exit 1 ;;
  esac
done < <(env)

PYTHONDONTWRITEBYTECODE=1 python3 "$repo_root/integration/helpers/environment_test.py"
echo 'PASS: integration processes inherit only harness-owned variables'
