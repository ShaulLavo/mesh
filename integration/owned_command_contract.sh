#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/owned_command_test.py"
echo 'PASS: owned commands join every descendant before deleting state'
