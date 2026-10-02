#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$root/scripts/bench" -p test_run.py
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$root/scripts/bench" -p test_process_diagnostics.py
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-m5-test-XXXXXX")
cleanup() {
  [[ -z $(find "$scratch" -name "mesh-m5-*" -type d -mindepth 1 -print -quit) ]] || return 1
  rm -rf -- "$scratch"
}
trap cleanup EXIT
"$root/scripts/bench/build.sh" "$scratch/build" "$(go env GOARCH)" smoke
python3 "$root/scripts/bench/test_receipt.py" --binary "$scratch/build/mesh" --receipt "$scratch/build/receipt.json" --scratch-parent "$scratch"
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$root/scripts/bench" -p test_profile.py
python3 "$root/scripts/bench/test_faults.py" --binary "$MESH" --scratch-parent "$scratch"
python3 "$root/scripts/bench/run.py" --binary "$scratch/build/mesh" --receipt "$scratch/build/receipt.json" --scratch-parent "$scratch" --sessions 0 2 \
  --idle-seconds 1 --settle-seconds 0 --repeats 1 --output "$scratch/result.json"
python3 - "$scratch/result.json" <<'PY'
import json
from pathlib import Path
import sys
result = json.loads(Path(sys.argv[1]).read_text())
assert [case['sessions'] for case in result['cases']] == [0, 2]
assert result['cases'][1]['idle']['worker_rss_median_bytes'][0] > 0
assert result['cases'][1]['throughput']['received_bytes'] == 1048576 + 14
assert result['cases'][1]['attach']['median']['first_paint_ms'] > 0
assert result['cases'][0]['version']['median']['cpu_ms'] > 0
assert not list(Path(sys.argv[1]).parent.glob('mesh-m5-*'))
PY
