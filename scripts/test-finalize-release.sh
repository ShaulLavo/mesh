#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-finalize-test.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT
remote=$test_root/remote.git
checkout=$test_root/checkout
mkdir -p "$test_root/bin" "$checkout"
git init -q --bare "$remote"
git init -q -b main "$checkout"
git -C "$checkout" config user.name test
git -C "$checkout" config user.email test@example.invalid
git -C "$checkout" remote add origin "$remote"
mkdir "$checkout/Casks"
echo old >"$checkout/Casks/mesh.rb"
git -C "$checkout" add .
git -C "$checkout" commit -qm base
git -C "$checkout" tag v0.1.1
echo source >"$checkout/source"
git -C "$checkout" add source
git -C "$checkout" commit -qm source
source_sha=$(git -C "$checkout" rev-parse HEAD)
git -C "$checkout" push -q origin main --tags
git --git-dir="$remote" symbolic-ref HEAD refs/heads/main
git config --file "$test_root/gitconfig" url."file://$remote".insteadOf https://github.com/ShaulLavo/mesh.git
cat >"$remote/hooks/pre-receive" <<'HOOK'
#!/usr/bin/env bash
while read -r old new ref; do
  [[ $ref != refs/heads/main ]] || { echo 'default branch push rejected' >&2; exit 1; }
done
HOOK
chmod +x "$remote/hooks/pre-receive"
cat >"$test_root/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$GH_LOG"
case "$1 $2" in
  'api repos/ShaulLavo/mesh/releases/latest') echo "${LATEST_TAG:-v0.1.1}" ;;
  'release edit'|'auth setup-git') ;;
  'pr list')
    [[ ${FAIL_LIST:-0} == 0 ]] || { echo 'list API failed' >&2; exit 1; }
    [[ ! -f $PR_CREATED ]] || echo 17 ;;
  'pr create') touch "$PR_CREATED"; echo 'https://example.invalid/pr/17' ;;
  'pr view')
    if [[ $* == *headRefOid* ]]; then
      head=$(git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/release/cask-v0.1.2)
      printf '%s\tmain\trelease/cask-v0.1.2\t%s\tfalse\n' "${PR_REPOSITORY:-ShaulLavo/mesh}" "$head"
    elif [[ $* == *autoMergeRequest* ]]; then
      [[ ! -f $PR_CREATED.auto ]] && echo false || echo true
    else
      echo 17
    fi
    if [[ -n ${RACE_HEAD:-} ]]; then git --git-dir="$FIXTURE_REMOTE" update-ref refs/heads/release/cask-v0.1.2 "$RACE_HEAD"; fi ;;
  'pr merge')
    if [[ $* == *--match-head-commit* && ${*: -1} != $(git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/release/cask-v0.1.2) ]]; then
      echo 'PR head changed after validation' >&2; exit 1
    fi
    [[ ${FAIL_MERGE:-0} == 0 ]] || { echo 'merge API failed' >&2; exit 1; }; touch "$PR_CREATED.auto" ;;
  'workflow run') [[ ${FAIL_DISPATCH:-0} == 0 ]] || { echo 'dispatch API failed' >&2; exit 1; } ;;
  *) echo "unexpected gh: $*" >&2; exit 1 ;;
esac
GH
chmod +x "$test_root/bin/gh"
echo new >"$test_root/cask"
run_finalize() {
  (cd "$checkout" && env PATH="$test_root/bin:$PATH" GIT_CONFIG_GLOBAL="$test_root/gitconfig" \
    GITHUB_REPOSITORY=ShaulLavo/mesh RUNNER_TEMP="$test_root" GH_LOG="$test_root/gh.log" \
    PR_CREATED="$test_root/pr" FIXTURE_REMOTE="$remote" "$repo_root/scripts/finalize-release.sh" v0.1.2 "$source_sha" v0.1.1 "$test_root/cask")
}
run_finalize
branch=release/cask-v0.1.2
head=$(git --git-dir="$remote" rev-parse "refs/heads/$branch")
[[ $(git --git-dir="$remote" rev-parse refs/heads/main) == "$source_sha" ]]
grep -Fq 'pr merge 17 --repo ShaulLavo/mesh --auto --squash' "$test_root/gh.log"
grep -Fq "workflow run ci.yml --repo ShaulLavo/mesh --ref $branch" "$test_root/gh.log"
run_finalize
[[ $(git --git-dir="$remote" rev-parse "refs/heads/$branch") == "$head" ]]
[[ $(grep -c '^pr create ' "$test_root/gh.log") == 1 ]]
check_foreign_pr() {
  if PR_REPOSITORY=foreign/mesh run_finalize >"$test_root/error" 2>&1; then
    echo 'foreign PR was accepted' >&2; exit 1
  fi
  grep -Fq 'PR identity or head does not match' "$test_root/error"
}
check_moved_head() {
  git -C "$checkout" switch -qc racer
  echo unrelated >"$checkout/unrelated"
  git -C "$checkout" add unrelated
  git -C "$checkout" commit -qm 'concurrent unrelated change'
  git -C "$checkout" push -q origin HEAD:refs/heads/racer
  race_head=$(git -C "$checkout" rev-parse HEAD)
  git -C "$checkout" switch -q main
  rm -f "$test_root/pr.auto"
  if RACE_HEAD="$race_head" run_finalize >"$test_root/error" 2>&1; then
    echo 'moved PR head was accepted' >&2; exit 1
  fi
  grep -Fq 'PR head changed after validation' "$test_root/error"
  git --git-dir="$remote" update-ref "refs/heads/$branch" "$head"
}
check_conflicting_main() {
  echo 'older pending Cask merged' >"$checkout/Casks/mesh.rb"
  git -C "$checkout" commit -qam 'older Cask on main'
  git -C "$checkout" push -q origin HEAD:refs/heads/older-cask
  new_main=$(git -C "$checkout" rev-parse HEAD)
  git --git-dir="$remote" update-ref refs/heads/main "$new_main"
  run_finalize
  new_head=$(git --git-dir="$remote" rev-parse "refs/heads/$branch")
  if ! git --git-dir="$remote" merge-base --is-ancestor "$new_main" "$new_head"; then
    echo 'conflicting Cask branch was not advanced' >&2; exit 1
  fi
  [[ $(git --git-dir="$remote" diff --name-only "$new_main" "$new_head") == Casks/mesh.rb ]]
  git --git-dir="$remote" show "$new_head:Casks/mesh.rb" | cmp -s - "$test_root/cask"
  grep -Fq -- "--match-head-commit $new_head" "$test_root/gh.log"
}
case ${1:-all} in
  foreign) check_foreign_pr; exit ;;
  moved) check_moved_head; exit ;;
  conflict) check_conflicting_main; exit ;;
  all) check_foreign_pr; check_moved_head; check_conflicting_main ;;
  *) echo 'unknown control' >&2; exit 2 ;;
esac
if FAIL_LIST=1 run_finalize >"$test_root/error" 2>&1; then echo 'list failure was swallowed' >&2; exit 1; fi
grep -Fq 'list API failed' "$test_root/error"
if FAIL_DISPATCH=1 run_finalize >"$test_root/error" 2>&1; then echo 'dispatch failure was swallowed' >&2; exit 1; fi
grep -Fq 'dispatch API failed' "$test_root/error"
rm "$test_root/pr.auto"
if FAIL_MERGE=1 run_finalize >"$test_root/error" 2>&1; then echo 'merge failure was swallowed' >&2; exit 1; fi
grep -Fq 'merge API failed' "$test_root/error"
echo different >"$test_root/cask"
if run_finalize >"$test_root/error" 2>&1; then echo 'mismatched branch was reused' >&2; exit 1; fi
grep -Fq 'does not contain only the expected Cask' "$test_root/error"
cp "$checkout/Casks/mesh.rb" "$test_root/cask"
before=$(wc -l <"$test_root/gh.log")
run_finalize
if tail -n "+$((before + 1))" "$test_root/gh.log" | grep -qE '^(pr|workflow) '; then
  echo 'unchanged cask opened PR or dispatched CI' >&2; exit 1
fi
echo later >>"$checkout/source"
git -C "$checkout" commit -qam later
git -C "$checkout" tag v0.1.3
before=$(wc -l <"$test_root/gh.log")
LATEST_TAG=v0.1.3 run_finalize
[[ $(wc -l <"$test_root/gh.log") -eq $((before + 1)) ]]
echo 'PASS: Cask PR handoff and retry contract'
