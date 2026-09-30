#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh"
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

while IFS='=' read -r name _; do
  case "$name" in
    PATH|HOME|TMPDIR|TERM|LANG|XDG_CONFIG_HOME|XDG_STATE_HOME|XDG_CACHE_HOME|XDG_DATA_HOME|XDG_RUNTIME_DIR|MESH_STATE_DIR|MESH_INTEGRATION_ENTRY|DBUS_SESSION_BUS_ADDRESS|GOCACHE|GOMODCACHE|MESH|MESH_INTEGRATION_BINARY|MESH_CONFIG_DIR|MESH_TEST_ZSH|MESH_SHORT_TMP|PWD|SHLVL|_) ;;
    *) printf 'FAIL: integration environment inherited %s\n' "$name" >&2; exit 1 ;;
  esac
done < <(env)

PYTHONDONTWRITEBYTECODE=1 python3 "$repo_root/integration/helpers/environment_test.py"
echo 'PASS: integration processes inherit only harness-owned variables'
