#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-release-plan.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT

git -C "$test_root" init -q
git -C "$test_root" config user.name test
git -C "$test_root" config user.email test@example.invalid
printf 'base\n' >"$test_root/state"
git -C "$test_root" add state
git -C "$test_root" commit -qm base
base=$(git -C "$test_root" rev-parse HEAD)
printf 'ancestor\n' >>"$test_root/state"
git -C "$test_root" commit -qam ancestor
ancestor=$(git -C "$test_root" rev-parse HEAD)
printf 'descendant\n' >>"$test_root/state"
git -C "$test_root" commit -qam descendant
descendant=$(git -C "$test_root" rev-parse HEAD)

plan=$(cd "$test_root" && "$repo_root/scripts/plan-release.sh" "$descendant" "$base" v0.1.38)
grep -Fqx 'action=publish' <<<"$plan"
grep -Fqx 'version=v0.1.39' <<<"$plan"

plan=$(cd "$test_root" && "$repo_root/scripts/plan-release.sh" "$descendant" "$descendant" v0.1.39)
grep -Fqx 'action=skip' <<<"$plan"
grep -Fqx 'version=v0.1.39' <<<"$plan"

# Simulate B publishing before an older A workflow finishes. A must become a
# successful skip, and must never allocate v0.1.40 for older source.
plan=$(cd "$test_root" && "$repo_root/scripts/plan-release.sh" "$ancestor" "$descendant" v0.1.39)
grep -Fqx 'action=skip' <<<"$plan"
if grep -Fq 'v0.1.40' <<<"$plan"; then
  echo 'release plan advanced after a delayed ancestor' >&2
  exit 1
fi

git -C "$test_root" switch -qc divergent "$base"
printf 'divergent\n' >>"$test_root/state"
git -C "$test_root" commit -qam divergent
divergent=$(git -C "$test_root" rev-parse HEAD)
if (cd "$test_root" && "$repo_root/scripts/plan-release.sh" "$divergent" "$descendant" v0.1.39 >/dev/null 2>&1); then
  echo 'release plan accepted divergent history' >&2
  exit 1
fi

echo 'PASS: release planning advances source history monotonically'
