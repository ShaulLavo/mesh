#!/usr/bin/env bash
set -euo pipefail

mode=$1
spec=$2
root=$3
release=$(jq -er '.release.version' "$spec")
source_sha=$(jq -er '.release.source' "$spec")
writer=$(jq -er '.run' "$spec")
export PYTHONDONTWRITEBYTECODE=1 GOTOOLCHAIN=local

mkdir "$root"
if [[ $mode == prepare ]]; then
  exec python3 scripts/qualify-publication.py "$spec" prepare "$root" "$MESH_HARNESS_SOURCE"
fi

run_readers() {
  local platform=$1 output=$2 key=${1//\//-}
  python3 scripts/run-original-update-readers.py "$root/packets/$key" "$platform" \
    "$root/baselines" "$root/trust" "$output/readers" \
    --publication-platform-plan "$PUBLICATION_PLAN" --publication-plan-sha256 "$PLAN_SHA"
}
if [[ $mode == readers ]]; then
  python3 scripts/qualify-publication.py "$spec" reader-inputs "$root" "$INSTALLED_CATCHUP_PLATFORM"
  run_readers "$INSTALLED_CATCHUP_PLATFORM" "$RUNNER_TEMP/qualification-evidence"
  exit
fi

export MESH_RELEASE_TOOL_SOURCE="$MESH_HARNESS_SOURCE" MESH_RELEASE_TOOL_RUN="$GITHUB_RUN_ID" MESH_RELEASE_TOOL_ROLE=harness
export MESH_RELEASE_TOOL="$root/harness.noindex/mesh-release-sign" MESH_RELEASE_TRUST_CACHE="$root/trust"
test "$(git rev-parse HEAD)" = "$MESH_HARNESS_SOURCE"
test -z "$(git status --porcelain)"
mkdir "$root/harness.noindex"
go build -trimpath -o "$MESH_RELEASE_TOOL" ./cmd/mesh-release-sign

if [[ $mode == verify ]]; then
  PYTHONPATH=scripts python3 -m fixtures.release_tool record "$root/harness.noindex" "$MESH_HARNESS_SOURCE" "$GITHUB_RUN_ID" harness linux/amd64
  python3 scripts/qualify-publication.py "$spec" verify-inputs "$root"
  mkdir "$root/completions"
  attempts=$(python3 scripts/qualify-publication.py "$spec" evidence "$root/completions" "$EVIDENCE_HANDOFF")
  exec python3 scripts/verify-worker-publication.py "$release" "$source_sha" "$root/completions" "$root/draft/$release" "$writer" \
    --qualification --platform-plan "$PUBLICATION_PLAN" --plan-sha256 "$PLAN_SHA" --harness "$MESH_HARNESS_SOURCE" --attempts "$attempts"
fi

test "$mode" = native
platform=$INSTALLED_CATCHUP_PLATFORM
python3 scripts/qualify-publication.py "$spec" native-inputs "$root" "$platform"
go build -trimpath -o "$root/harness.noindex/native-helper" ./scripts/fixtures/native-helper
go test -c -tags mesh_installed_fixture -o "$root/harness.noindex/publication-reader" ./internal/update
PYTHONPATH=scripts python3 -m fixtures.release_tool record "$root/harness.noindex" "$MESH_HARNESS_SOURCE" "$GITHUB_RUN_ID" harness "$platform"

export MESH_PUBLICATION_DRAFT="$root/draft/$release" MESH_PUBLICATION_RUN="$writer"
export MESH_PUBLICATION_TRUST_CACHE="$MESH_RELEASE_TRUST_CACHE" MESH_PREPARATION_SOURCE="$source_sha"
input="$root/native.noindex"
PYTHONPATH=scripts python3 -m fixtures.installed_artifacts native "$input" "$source_sha" "$platform"
chmod +x "$input/tools/worker-probe"
output="$RUNNER_TEMP/qualification-evidence"
plan=(--publication-platform-plan "$PUBLICATION_PLAN" --publication-plan-sha256 "$PLAN_SHA")
python3 scripts/prove-installed-catchup.py "$output" --native-preflight \
  --native-helper "$root/harness.noindex/native-helper" --publication-version "$release" --publication-source "$source_sha" \
  --worker-registry "$input/retained/workers.json" --worker-probe "$input/tools/worker-probe" "${plan[@]}"
run_readers "$platform" "$output"
python3 scripts/publication-jobs.py record-platform "$output" "$platform" "$PUBLICATION_PLAN" "$PLAN_SHA"
