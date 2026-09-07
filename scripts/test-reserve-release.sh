#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-release-reserve-test.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT

remote=$test_root/remote.git
checkout=$test_root/checkout
fake_bin=$test_root/bin
mkdir -p "$fake_bin"
git init -q --bare "$remote"
git init -q "$checkout"
git -C "$checkout" config user.name test
git -C "$checkout" config user.email test@example.invalid
git -C "$checkout" remote add origin "$remote"
mkdir -p "$checkout/scripts"
cp "$repo_root/scripts/plan-release.sh" "$checkout/scripts/plan-release.sh"

cat >"$fake_bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *'/releases/latest '*) printf '%s\n' "${GH_LATEST_TAG:-v0.1.38}" ;;
  *'/check-runs '*) printf '%s\n' "${GH_GATE_COUNT:-1}" ;;
  *'/releases?per_page=100 '*)
    [[ -f $GH_LOG ]] || exit 0
    awk '$1 == "release" && $2 == "create" {for(i=4;i<=NF;i++) if($i == "--target") print $3 "\t" $(i+1)}' "$GH_LOG" ;;
  *' release download '*) exit 1 ;;
  *' release view '*" --json assets "*) printf '\n' ;;
  *' release view '*" --json targetCommitish "*)
    awk -v tag="$3" '$1 == "release" && $2 == "create" && $3 == tag {for(i=4;i<=NF;i++) if($i == "--target") print $(i+1)}' "$GH_LOG" ;;
  *' release view '*) [[ -f $GH_LOG ]] && awk -v tag="$3" '$1 == "release" && $2 == "create" && $3 == tag {found=1} END {exit !found}' "$GH_LOG" ;;
  *' release create '*) printf '%s\n' "$*" >>"$GH_LOG" ;;
  *) printf 'unexpected gh invocation: %s\n' "$*" >&2; exit 1 ;;
esac
GH
chmod 0755 "$fake_bin/gh"

printf 'base\n' >"$checkout/source"
git -C "$checkout" add source
git -C "$checkout" commit -qm base
base=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" tag v0.1.38 "$base"
git -C "$checkout" branch -M main
git -C "$checkout" push -q origin main --tags

mkdir -p "$checkout/Casks"
printf 'cask\n' >"$checkout/Casks/mesh.rb"
git -C "$checkout" add Casks/mesh.rb
git -C "$checkout" commit -qm cask
cask=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main

run_reserve() {
  (cd "$checkout" && env PATH="$fake_bin:$PATH" GITHUB_REPOSITORY=ShaulLavo/mesh \
    RUNNER_TEMP="$test_root" GH_LOG="$test_root/gh.log" "$repo_root/scripts/reserve-release.sh" "$@")
}

result=$(run_reserve "$cask")
grep -Fqx 'action=skip' <<<"$result"
grep -Fqx 'reason=cask-only' <<<"$result"

printf 'code\n' >>"$checkout/source"
git -C "$checkout" commit -qam code
source_sha=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
result=$(run_reserve "$source_sha")
grep -Fqx 'action=publish' <<<"$result"
grep -Fqx 'version=v0.1.39' <<<"$result"
[[ $(git --git-dir="$remote" rev-list -n 1 v0.1.39) == "$source_sha" ]]
grep -Fq 'release create v0.1.39' "$test_root/gh.log"

result=$(GH_LATEST_TAG=v0.1.39 run_reserve "$source_sha")
grep -Fqx 'action=reconcile' <<<"$result"
grep -Fqx 'version=v0.1.39' <<<"$result"

if run_reserve "$source_sha" v0.1.37 >/dev/null 2>&1; then
  echo 'reserve accepted an explicit version older than the published release' >&2
  exit 1
fi

printf 'untested\n' >>"$checkout/source"
git -C "$checkout" commit -qam untested
untested=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
if GH_GATE_COUNT=0 run_reserve "$untested" >/dev/null 2>&1; then
  echo 'reserve accepted a source without the complete CI gate' >&2
  exit 1
fi

result=$(run_reserve "$untested")
grep -Fqx 'version=v0.1.40' <<<"$result"
[[ $(git --git-dir="$remote" rev-list -n 1 v0.1.39) == "$source_sha" ]]
[[ $(git --git-dir="$remote" rev-list -n 1 v0.1.40) == "$untested" ]]
result=$(run_reserve "$untested")
grep -Fqx 'version=v0.1.40' <<<"$result"
[[ $(grep -c 'release create v0.1.40 ' "$test_root/gh.log") -eq 1 ]]
if run_reserve "$untested" v0.1.39 >/dev/null 2>&1; then
  echo 'reserve reused another source explicit reservation' >&2
  exit 1
fi

printf 'draft-only source\n' >>"$checkout/source"
git -C "$checkout" commit -qam draft-only
draft_source=$(git -C "$checkout" rev-parse HEAD)
printf 'release create v0.1.41 --draft --target %s\n' "$draft_source" >>"$test_root/gh.log"
printf 'newer source\n' >>"$checkout/source"
git -C "$checkout" commit -qam newer
newer_source=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
result=$(run_reserve "$newer_source")
grep -Fqx 'version=v0.1.42' <<<"$result"
result=$(run_reserve "$draft_source")
grep -Fqx 'version=v0.1.41' <<<"$result"

echo 'PASS: release reservation requires tested source, preserves retries, and advances past failed reservations'
