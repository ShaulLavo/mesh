#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
python3 -B "$repo_root/scripts/test-verify-runner.py" ResultTests SourceContractTest
echo 'PASS: the runner reports owned results and refuses unsupported hosts before creating state'
