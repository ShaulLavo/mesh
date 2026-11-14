#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo 'usage: prove-installed-catchup.sh OUTPUT-DIRECTORY' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export PYTHONDONTWRITEBYTECODE=1
python3 "$repo_root/scripts/prove-installed-catchup.py" "$1" --native-preflight
