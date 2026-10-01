#!/usr/bin/env bash

export PYTHONDONTWRITEBYTECODE=1

isolation_helper=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/isolation.py || exit 1
isolation_entry=$(cd -- "$(dirname -- "$0")" && pwd)/${0##*/} || exit 1
if [[ ${MESH_INTEGRATION_ENTRY:-} == "$isolation_entry" ]]; then
  python3 "$isolation_helper" --check || exit "$?"
else
  exec python3 "$isolation_helper" "$BASH" "$isolation_entry" "$@" || exit "$?"
fi
