#!/usr/bin/env bash

isolation_helper=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/isolation.py
isolation_entry=$(cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")
if [[ ${MESH_INTEGRATION_ENTRY:-} == "$isolation_entry" ]]; then
  python3 "$isolation_helper" --check || exit "$?"
else
  python3 "$isolation_helper" "$BASH" "$isolation_entry" "$@"
  exit "$?"
fi
