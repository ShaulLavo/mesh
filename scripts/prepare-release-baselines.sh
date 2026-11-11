#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 6 ]]; then
  echo 'usage: prepare-release-baselines.sh CANDIDATE-VERSION PREVIOUS-TAG OS/ARCH FROZEN-ASSETS OUTPUT-DIR OUTPUT-LIST' >&2
  exit 2
fi
[[ ${GITHUB_REPOSITORY:-} == ShaulLavo/mesh ]] || {
  echo "release baselines: unexpected repository ${GITHUB_REPOSITORY:-<unset>}" >&2
  exit 1
}
[[ -n ${MESH_RELEASE_SELECTION:-} && -f $MESH_RELEASE_SELECTION ]] || {
  echo 'release baselines: MESH_RELEASE_SELECTION must name the verified publisher selection' >&2
  exit 1
}
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
exec python3 "$repo_root/scripts/prepare-release-baselines.py" "$@"
