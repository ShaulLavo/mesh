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
printf '%s\n' "$*" >>"$GH_CALLS"
case " $* " in
  *'/releases/latest '*) printf '%s\n' "${GH_LATEST_TAG:-v0.1.38}" ;;
  *'/check-runs '*)
    if [[ -n ${GH_GREEN_SHAS:-} ]]; then
      commit=$(sed -n 's|.*commits/\([0-9a-f]\{40\}\)/check-runs.*|\1|p' <<<"$*")
      if grep -Fq "$commit" <<<"$GH_GREEN_SHAS"; then printf '1\n'; else printf '0\n'; fi
    else
      printf '%s\n' "${GH_GATE_COUNT:-1}"
    fi ;;
  *'/releases?per_page=100 '*)
    [[ -f $GH_LOG ]] || exit 0
    awk '$1 == "release" && $2 == "create" {for(i=4;i<=NF;i++) if($i == "--target") print $3 "\t" $(i+1)}' "$GH_LOG" ;;
  *' release download '*' --pattern mesh.rb '*)
    [[ -n ${GH_PUBLISHED_CASK:-} ]] || exit 1
    dir=$(awk '{for(i=1;i<=NF;i++) if($i=="--dir") print $(i+1)}' <<<"$*")
    cp "$GH_PUBLISHED_CASK" "$dir/mesh.rb" ;;
  *' release download '*) exit 1 ;;
  *' release view '*" --json assets "*) printf '\n' ;;
  *' release view '*" --json targetCommitish "*)
    awk -v tag="$3" '$1 == "release" && $2 == "create" && $3 == tag {for(i=4;i<=NF;i++) if($i == "--target") print $(i+1)}' "$GH_LOG" ;;
  *' release view '*) [[ -f $GH_LOG ]] && awk -v tag="$3" '$1 == "release" && $2 == "create" && $3 == tag {found=1} END {exit !found}' "$GH_LOG" ;;
  *' release create '*)
    printf '%s\n' "$*" >>"$GH_LOG"
    printf 'https://github.com/ShaulLavo/mesh/releases/tag/untagged-0123\n' ;;
  *) printf 'unexpected gh invocation: %s\n' "$*" >&2; exit 1 ;;
esac
GH
real_grep=$(command -v grep)
cat >"$fake_bin/grep" <<'GREP'
#!/usr/bin/env bash
set -euo pipefail
if [[ $# -eq 2 && $1 == -Ev && $2 == '^Casks/mesh\.rb$' ]]; then
  printf '%s\n' "${GREP_CLASSIFIER_STATUS:-real}" >>"$GREP_CLASSIFIER_LOG"
  if [[ -n ${GREP_CLASSIFIER_STATUS:-} ]]; then
    printf 'fixture classifier failed with status %s\n' "$GREP_CLASSIFIER_STATUS" >&2
    exit "$GREP_CLASSIFIER_STATUS"
  fi
fi
exec "$REAL_GREP" "$@"
GREP
chmod 0755 "$fake_bin/gh" "$fake_bin/grep"

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
    RUNNER_TEMP="$test_root" GH_LOG="$test_root/gh.log" GH_CALLS="$test_root/gh.calls" \
    REAL_GREP="$real_grep" GREP_CLASSIFIER_LOG="$test_root/classifier.log" \
    "$repo_root/scripts/reserve-release.sh" "$@")
}

result=$(GREP_CLASSIFIER_STATUS=2 run_reserve "$base")
grep -Fqx 'action=reconcile' <<<"$result"
grep -Fqx "source=$base" <<<"$result"
[[ ! -e $test_root/classifier.log ]]

result=$(run_reserve "$cask")
grep -Fqx 'action=skip' <<<"$result"
grep -Fqx 'reason=cask-only' <<<"$result"

printf 'code\n' >>"$checkout/source"
git -C "$checkout" commit -qam code
source_sha=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
assert_classifier_failure() {
  local source=$1 expected_status=$2 status=0
  local tags
  tags=$(git --git-dir="$remote" show-ref --tags) || return 1
  GREP_CLASSIFIER_STATUS=$expected_status run_reserve "$source" \
    >"$test_root/classifier.out" 2>"$test_root/classifier.err" || status=$?
  if [[ $status != "$expected_status" ]]; then
    printf 'FAIL: classifier status %s for %s returned %s:\n' "$expected_status" "$source" "$status" >&2
    cat "$test_root/classifier.out" "$test_root/classifier.err" >&2
    return 1
  fi
  [[ ! -s $test_root/classifier.out ]] || return 1
  grep -Fqx "fixture classifier failed with status $expected_status" "$test_root/classifier.err" || return 1
  grep -Fqx "release reservation: changed-file classifier failed with status $expected_status" "$test_root/classifier.err" || return 1
  [[ $(tail -n 1 "$test_root/classifier.log") == "$expected_status" ]] || return 1
  [[ $(git --git-dir="$remote" show-ref --tags) == "$tags" ]] || return 1
  [[ ! -e $test_root/gh.log ]] || return 1
  printf 'PASS: classifier status %s for %s fails before release creation or tag changes\n' "$expected_status" "$source"
}

classifier_failures=0
for classifier_source in "$cask" "$source_sha"; do
  for classifier_status in 2 127 42; do
    assert_classifier_failure "$classifier_source" "$classifier_status" || classifier_failures=$((classifier_failures + 1))
  done
done
[[ $classifier_failures -eq 0 ]]

result=$(run_reserve "$source_sha")
# The workflow appends this output to GITHUB_OUTPUT, which accepts only
# key=value lines.
if grep -vqE '^[a-z_]+=' <<<"$result"; then
  printf 'FAIL: reservation output is not GITHUB_OUTPUT-safe:\n%s\n' "$result" >&2
  exit 1
fi
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

# The reserve step pipes the script into tee to fill GITHUB_OUTPUT. Without
# pipefail the step takes tee's status, so a reservation that fails after
# pushing its tag leaves the job green with no outputs and every publication
# job skips. Run the step's own command under the shell GitHub would use.
workflow=$repo_root/.github/workflows/release.yml
reserve_step=$(awk '/^      - id: reserve$/ {found=1; next} found && /^      - / {exit} found' "$workflow")
reserve_run=$(sed -n 's/^        run: //p' <<<"$reserve_step")
reserve_shell=$(sed -n 's/^        shell: //p' <<<"$reserve_step")
[[ -n $reserve_run ]] || { echo 'FAIL: release.yml reserve step has no single-line run' >&2; exit 1; }
# GitHub runs `shell: bash` as `bash --noprofile --norc -eo pipefail {0}` and
# a step with no shell as `bash -e {0}`.
case $reserve_shell in
  bash) step_shell=(bash --noprofile --norc -eo pipefail) ;;
  '') step_shell=(bash -e) ;;
  *) printf 'FAIL: unexpected reserve step shell %q\n' "$reserve_shell" >&2; exit 1 ;;
esac
step_root=$test_root/step
mkdir -p "$step_root/scripts"
printf '%s\n' "$reserve_run" >"$step_root/run.sh"
cat >"$step_root/scripts/reserve-release.sh" <<'RESERVE'
#!/usr/bin/env bash
printf 'action=publish\nsource=%s\n' "$1"
exit "$FAKE_RESERVE_STATUS"
RESERVE
chmod 0755 "$step_root/scripts/reserve-release.sh"
run_reserve_step() {
  printf 'existing=kept\n' >"$step_root/github-output"
  (cd "$step_root" && env FAKE_RESERVE_STATUS="$1" GITHUB_OUTPUT="$step_root/github-output" \
    SOURCE_SHA=0123abc EXPLICIT_VERSION= "${step_shell[@]}" run.sh >/dev/null)
}

run_reserve_step 0
[[ $(cat "$step_root/github-output") == $'existing=kept\naction=publish\nsource=0123abc' ]] ||
  { echo 'FAIL: reserve step did not append its output to GITHUB_OUTPUT' >&2; exit 1; }
if run_reserve_step 1; then
  echo 'FAIL: reserve step succeeded although the reservation script failed' >&2
  exit 1
fi

git -C "$checkout" commit -q --allow-empty -m empty
empty_source=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
result=$(GH_LATEST_TAG=v0.1.42 GREP_CLASSIFIER_STATUS=2 run_reserve "$empty_source")
grep -Fqx 'action=publish' <<<"$result"
grep -Fqx "source=$empty_source" <<<"$result"
grep -Fqx 'version=v0.1.43' <<<"$result"

printf 'mixed code\n' >>"$checkout/source"
printf 'mixed cask\n' >>"$checkout/Casks/mesh.rb"
git -C "$checkout" commit -qam mixed
mixed_source=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
result=$(run_reserve "$mixed_source")
grep -Fqx 'action=publish' <<<"$result"
grep -Fqx 'version=v0.1.44' <<<"$result"
[[ $(git --git-dir="$remote" rev-list -n 1 v0.1.44) == "$mixed_source" ]]

# Batched / scheduled auto-selection (empty SOURCE_SHA).
# 1. Already published and Cask matches -> skip (no release jobs).
cp "$checkout/Casks/mesh.rb" "$test_root/matching-cask.rb"
result=$(GH_LATEST_TAG=v0.1.44 GH_PUBLISHED_CASK="$test_root/matching-cask.rb" run_reserve "" "")
grep -Fqx 'action=skip' <<<"$result"
grep -Fqx 'reason=already-published' <<<"$result"

# 2. Already published but Cask differs -> reconcile.
printf 'different cask\n' >"$test_root/stale-cask.rb"
result=$(GH_LATEST_TAG=v0.1.44 GH_PUBLISHED_CASK="$test_root/stale-cask.rb" run_reserve "" "")
grep -Fqx 'action=reconcile' <<<"$result"
grep -Fqx 'version=v0.1.44' <<<"$result"

# 3. Only Casks/mesh.rb changed since last release -> skip cask-only.
printf 'cask-only followup\n' >>"$checkout/Casks/mesh.rb"
git -C "$checkout" commit -qam 'cask-only followup'
git -C "$checkout" push -q origin main
result=$(GH_LATEST_TAG=v0.1.44 run_reserve "" "")
grep -Fqx 'action=skip' <<<"$result"
grep -Fqx 'reason=cask-only' <<<"$result"

# 4. Feature followed by cask-only commit -> NOT skipped; batches feature + cask!
printf 'batched feature\n' >>"$checkout/source"
git -C "$checkout" commit -qam 'batched feature'
batched_feature=$(git -C "$checkout" rev-parse HEAD)
printf 'later cask update\n' >>"$checkout/Casks/mesh.rb"
git -C "$checkout" commit -qam 'later cask update'
batched_head=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main
# None green yet -> clean no-green-commit skip (no red scheduled failure).
result=$(GH_LATEST_TAG=v0.1.44 GH_GATE_COUNT=0 run_reserve "" "")
grep -Fqx 'action=skip' <<<"$result"
grep -Fqx 'reason=no-green-commit' <<<"$result"
# Head untested, older feature green -> selects the older green commit!
result=$(GH_LATEST_TAG=v0.1.44 GH_GREEN_SHAS="$batched_feature" run_reserve "" "")
grep -Fqx 'action=publish' <<<"$result"
grep -Fqx "source=$batched_feature" <<<"$result"
grep -Fqx 'version=v0.1.45' <<<"$result"
# Head green (even though head commit only touched Casks/mesh.rb) -> publishes head!
result=$(GH_LATEST_TAG=v0.1.44 GH_GREEN_SHAS="$batched_head" run_reserve "" "")
grep -Fqx 'action=publish' <<<"$result"
grep -Fqx "source=$batched_head" <<<"$result"
grep -Fqx 'version=v0.1.46' <<<"$result"

echo 'PASS: release reservation rejects classifier errors, requires tested source, preserves retries, advances past failed reservations, and supports batched auto-selection'
