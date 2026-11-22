#!/usr/bin/env bash
set -euo pipefail

output=$1
shift
test "$(git rev-parse HEAD)" = "$MESH_RELEASE_TOOL_SOURCE"
test -z "$(git status --porcelain)"
for target in "$@"; do
  system=${target%/*}
  architecture=${target#*/}
  directory="$output/$system-$architecture"
  mkdir -p "$directory"
  GOOS="$system" GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -o "$directory/mesh-release-sign" ./cmd/mesh-release-sign
  GOOS="$system" GOARCH="$architecture" CGO_ENABLED=0 go build -trimpath -o "$directory/mesh-release-routes" ./cmd/mesh-release-routes
  PYTHONPATH=scripts python3 -m fixtures.release_tool record "$directory" "$MESH_RELEASE_TOOL_SOURCE" "$GITHUB_RUN_ID" source "$target"
done
