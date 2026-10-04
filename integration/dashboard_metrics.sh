#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

: "${MESH:?dashboard_metrics requires the verification binary}"
root=$(mktemp -d)
trap 'rm -rf -- "$root"' EXIT
go build -o "$root/screen" ./integration/helpers/dashboard-screen
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/dashboard_metrics.py" "$MESH" --screen "$root/screen" "$@"
