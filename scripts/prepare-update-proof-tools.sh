#!/usr/bin/env bash
set -euo pipefail
if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo 'usage: prepare-update-proof-tools.sh OUTPUT VERSION [FINAL-ARCHIVE]' >&2
  exit 2
fi
output=$(python3 -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve())' "$1")
version=$2
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$output"
python3 "$repo_root/scripts/fixtures/noindex_paths.py" "$output"
cd "$repo_root"
source_commit=$(git rev-parse HEAD)
if [[ -n $(git status --porcelain) ]]; then
  echo 'native proof preparation requires a clean checkout' >&2
  exit 1
fi
if [[ $# == 3 ]]; then
  tar -xzf "$3" -C "$output" mesh
  mv "$output/mesh" "$output/candidate"
else
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid= -X github.com/shaul/mesh/internal/release.Version=$version" -o "$output/candidate" ./cmd/mesh
fi
chmod 0755 "$output/candidate"
python3 scripts/fixtures/release_transition.py startup-overlay "$repo_root" "$output/startup-overlay"
CGO_ENABLED=0 go build -trimpath -overlay "$output/startup-overlay/overlay.json" \
  -ldflags "-s -w -buildid= -X github.com/shaul/mesh/internal/release.Version=$version" \
  -o "$output/startup-fault-candidate" ./cmd/mesh
go build -trimpath -o "$output/boundary" ./internal/storage/cmd/boundary
go test -c -o "$output/worker-probe" ./internal/updatebootstrap
python3 scripts/fixtures/release_transition.py startup-identity "$output/candidate" "$output/candidate" "$output/startup-fault-candidate"
python3 scripts/record-update-preparation.py "$output" "$source_commit"
