#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
python3 "$(dirname -- "${BASH_SOURCE[0]}")/helpers/owner_failure_test.py"
echo 'PASS: owner failure diagnostics preserve quoted captured output'
