#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail

python3 -B "$(dirname -- "${BASH_SOURCE[0]}")/helpers/session_scope_test.py"
echo 'PASS: scope observations reject unavailable systemd queries'
