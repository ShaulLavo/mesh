#!/usr/bin/env bash
# Section 11 gateway benchmark in the integration isolation. Usage:
#   scripts/bench/mcp_gateway.sh MESH_BINARY OUTPUT.json
source "$(dirname -- "${BASH_SOURCE[0]}")/../../integration/helpers/isolate.sh" || exit 1
set -euo pipefail
python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/mcp_gateway.py" "$1" "$2"
