#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$root/scripts/bench" -p test_readiness.py -v
