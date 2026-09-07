#!/usr/bin/env bash
set -euo pipefail

repository=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
check_root=${MESH_UPDATE_CHECK_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/mesh-update-check.XXXXXX")}
mkdir -p "$check_root"
check_root=$(cd -- "$check_root" && pwd)
cd -- "$repository"

go test -race \
  ./internal/release/... ./internal/update/... ./internal/updateinstall/... \
  ./internal/updatebootstrap/... ./internal/updategate/... \
  ./internal/updatenotice/... ./internal/daemon/... ./internal/cli/... \
  ./internal/tui/... ./internal/worker/... >"$check_root/race.log" 2>&1

go build -trimpath -ldflags '-X github.com/shaul/mesh/internal/release.Version=v0.0.0' \
  -o "$check_root/mesh" ./cmd/mesh
"$check_root/mesh" version --json >"$check_root/version.json"
python3 - "$check_root" <<'PY'
import hashlib
import json
import pathlib
import sys

directory = pathlib.Path(sys.argv[1])
build = json.loads((directory / "version.json").read_text())
assert build["version"] == "v0.0.0", build
assert build["digest"] == hashlib.sha256((directory / "mesh").read_bytes()).hexdigest(), build
assert build["stateVersion"] > 0 and build["workerProtocol"] > 0 and build["updateProtocol"] == 1, build
PY

"$check_root/mesh" update --help >"$check_root/help.txt"
for option in --all --local --host --fleet --version --yes --json --check; do
  grep -Fq -- "$option" "$check_root/help.txt"
done
for action in status retry cancel trust; do
  "$check_root/mesh" update "$action" --help >/dev/null
done

mkdir -p "$check_root/config" "$check_root/state"
if env MESH_CONFIG_DIR="$check_root/config" MESH_STATE_DIR="$check_root/state" \
  "$check_root/mesh" update --yes --json >"$check_root/unscoped.json" 2>"$check_root/unscoped.error"; then
  echo 'update acceptance: an unattended operation inferred an unreviewed fleet' >&2
  exit 1
fi
[[ ! -s $check_root/unscoped.json ]] || {
  echo 'update acceptance: rejected JSON invocation polluted stdout' >&2
  exit 1
}

scripts/test-plan-release.sh >"$check_root/release-plan.log" 2>&1
scripts/test-reserve-release.sh >"$check_root/release-reservation.log" 2>&1
scripts/test-prepare-release-baselines.sh >"$check_root/release-baselines.log" 2>&1
printf 'Update transaction, fleet, authentication, notice, CLI, and release checks passed. Evidence: %s\n' "$check_root"
printf '%s\n' 'These checks exercise subprocess crashes. VM reboot and native service-manager evidence is recorded separately.'
