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
  'api --method')
    head=$(cat "$PR_CREATED.validated")
    [[ $* == "api --method POST repos/ShaulLavo/mesh/statuses/$head -f context=Complete CI gate -f state=pending -f description=Waiting for dispatched CI" ]]
    [[ ${FAIL_STATUS:-0} == 0 ]] || { echo 'status API failed' >&2; exit 1; } ;;
  'api repos/ShaulLavo/mesh/releases/latest') echo "${LATEST_TAG:-v0.1.1}" ;;
  'release edit'|'auth setup-git') ;;
  'pr list')
    [[ ${FAIL_LIST:-0} == 0 ]] || { echo 'list API failed' >&2; exit 1; }
    [[ ! -f $PR_CREATED ]] || echo 17 ;;
  'pr create') touch "$PR_CREATED"; echo 17 ;;
  'pr edit') [[ $* == *"--title Update Cask for ${CASK_VERSION:-v0.1.2}"* ]] ;;
  'pr view')
    if [[ $* == *headRefOid* ]]; then
      head=$(git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/automation/cask-updates)
      printf '%s\n' "$head" > "$PR_CREATED.validated"
      printf '%s\tmain\tautomation/cask-updates\t%s\tfalse\n' "${PR_REPOSITORY:-ShaulLavo/mesh}" "$head"
    elif [[ $* == *isDraft* ]]; then
      [[ ${FAIL_DRAFT:-0} == 0 ]] || { echo 'draft API failed' >&2; exit 1; }
      echo "${PR_DRAFT:-false}"
    elif [[ $* == *autoMergeRequest* ]]; then
      [[ ! -f $PR_CREATED.auto ]] && echo false || echo true
    else
      echo 17
    fi
    if [[ -n ${RACE_HEAD:-} ]]; then git --git-dir="$FIXTURE_REMOTE" update-ref refs/heads/automation/cask-updates "$RACE_HEAD"; fi ;;
  'pr merge')
    if [[ $* == *--match-head-commit* && ${*: -1} != $(git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/automation/cask-updates) ]]; then
      echo 'PR head changed after validation' >&2; exit 1
    fi
    [[ ${FAIL_MERGE:-0} == 0 ]] || { echo 'merge API failed' >&2; exit 1; }; touch "$PR_CREATED.auto" ;;
  'run list')
    head=$(git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/automation/cask-updates)
    if [[ -f $PR_CREATED.ci && $(cat "$PR_CREATED.ci") == "$head" ]]; then
      echo '{"status":"completed","conclusion":"success"}'
    fi ;;
  'workflow run')
    [[ ${FAIL_DISPATCH:-0} == 0 ]] || { echo 'dispatch API failed' >&2; exit 1; }
    git --git-dir="$FIXTURE_REMOTE" rev-parse refs/heads/automation/cask-updates > "$PR_CREATED.ci" ;;
  *) echo "unexpected gh: $*" >&2; exit 1 ;;
esac
GH
chmod +x "$test_root/bin/gh"
cat >"$test_root/bin/python3" <<'PYTHON'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == */verify-worker-publication.py ]]; then
  printf '%s\n' 'worker evidence gate' >> "$GH_LOG"
  [[ ${FAIL_WORKER_PROOF:-0} == 0 ]]
  exit "$?"
fi
exec /usr/bin/python3 "$@"
PYTHON
chmod +x "$test_root/bin/python3"
echo new >"$test_root/cask"
run_finalize() {
  (cd "$checkout" && env PATH="$test_root/bin:$PATH" GIT_CONFIG_GLOBAL="$test_root/gitconfig" \
    GITHUB_REPOSITORY=ShaulLavo/mesh RUNNER_TEMP="$test_root" GH_LOG="$test_root/gh.log" \
    MESH_INSTALLED_COMPLETION="$test_root/installed-completion" PR_CREATED="$test_root/pr" FIXTURE_REMOTE="$remote" "$repo_root/scripts/finalize-release.sh" "${CASK_VERSION:-v0.1.2}" "$source_sha" v0.1.1 "$test_root/cask")
}
run_finalize >"$test_root/output"
cat "$test_root/output"
branch=automation/cask-updates
head=$(git --git-dir="$remote" rev-parse "refs/heads/$branch")
[[ $(git --git-dir="$remote" rev-parse refs/heads/main) == "$source_sha" ]]
grep -Fq 'pr merge 17 --repo ShaulLavo/mesh --auto --squash' "$test_root/gh.log"
grep -Fq 'waits for dispatched CI before auto-merge' "$test_root/output"
run_finalize
[[ $(git --git-dir="$remote" rev-parse "refs/heads/$branch") == "$head" ]]
[[ $(grep -c '^pr create ' "$test_root/gh.log") == 1 ]]
[[ $(grep -c '^pr merge ' "$test_root/gh.log") == 1 ]]
[[ $(grep -c '^workflow run ' "$test_root/gh.log") == 1 ]]
check_draft_pr() {
  local before
  before=$(grep -c '^pr merge ' "$test_root/gh.log")
  if PR_DRAFT=true run_finalize >"$test_root/error" 2>&1; then
    echo 'draft PR was accepted' >&2; exit 1
  fi
  grep -Fq 'mark it ready for review in GitHub' "$test_root/error"
  if PR_DRAFT=unknown run_finalize >"$test_root/error" 2>&1; then
    echo 'unknown draft state was accepted' >&2; exit 1
  fi
  grep -Fq 'unexpected draft state' "$test_root/error"
  if FAIL_DRAFT=1 run_finalize >"$test_root/error" 2>&1; then
    echo 'draft API failure was swallowed' >&2; exit 1
  fi
  grep -Fq 'draft API failed' "$test_root/error"
  [[ $(grep -c '^pr merge ' "$test_root/gh.log") == "$before" ]]
}
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
  draft) check_draft_pr; exit ;;
  foreign) check_foreign_pr; exit ;;
  moved) check_moved_head; exit ;;
  conflict) check_conflicting_main; exit ;;
  all) check_draft_pr; check_foreign_pr; check_moved_head; check_conflicting_main ;;
  *) echo 'unknown control' >&2; exit 2 ;;
esac
for failure in FAIL_STATUS FAIL_DISPATCH; do
  rm -f "$test_root/pr.ci"
  merges=$(grep -c '^pr merge ' "$test_root/gh.log")
  if (export "$failure=1"; run_finalize >"$test_root/error" 2>&1); then
    echo "$failure was ignored" >&2; exit 1
  fi
  [[ $(grep -c '^pr merge ' "$test_root/gh.log") == "$merges" ]]
done
if FAIL_LIST=1 run_finalize >"$test_root/error" 2>&1; then echo 'list failure was swallowed' >&2; exit 1; fi
grep -Fq 'list API failed' "$test_root/error"
rm "$test_root/pr.auto"
if FAIL_MERGE=1 run_finalize >"$test_root/error" 2>&1; then echo 'merge failure was swallowed' >&2; exit 1; fi
grep -Fq 'merge API failed' "$test_root/error"
echo different >"$test_root/cask"
CASK_VERSION=v0.1.3 run_finalize
[[ $(grep -c '^pr create ' "$test_root/gh.log") == 1 ]]
grep -Fq -- '--title Update Cask for v0.1.3' "$test_root/gh.log"
git --git-dir="$remote" show "refs/heads/$branch:Casks/mesh.rb" | cmp -s - "$test_root/cask"
[[ $(git --git-dir="$remote" for-each-ref --format='%(refname)' 'refs/heads/automation/cask*') == "refs/heads/$branch" ]]
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
[[ $(grep -c '^workflow run ' "$test_root/gh.log") -ge 3 ]]
echo 'PASS: Cask PR handoff and retry contract'

bash "$repo_root/scripts/test-report-update-status.sh"
