#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

: "${MESH:?name_observation requires the verification binary}"
root=$(mktemp -d)
trap 'rm -rf -- "$root"' EXIT
go build -o "$root/screen" ./integration/helpers/dashboard-screen
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/name_observation.py" "$MESH" --screen "$root/screen" "$@"
