#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
evidence=${1:?usage: prove-hooks.sh EVIDENCE_DIR}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-hooks-proof.XXXXXX")
cleanup() {
  git -C "$scratch/repo" worktree remove --force "$scratch/worktree" >/dev/null 2>&1 || true
  rm -rf -- "$scratch"
}
trap cleanup EXIT
mkdir "$scratch/repo"
git -C "$repo_root" archive HEAD | tar -x -C "$scratch/repo"
git -C "$scratch/repo" init -q -b main
git -C "$scratch/repo" config user.name 'Hook fixture'
git -C "$scratch/repo" config user.email 'hook-fixture@example.invalid'
git -C "$scratch/repo" add .
git -C "$scratch/repo" -c core.hooksPath=/dev/null commit -qm 'Fixture source'
git -C "$scratch/repo" worktree add -qb hook-proof "$scratch/worktree"
cp "$(command -v lefthook)" "$scratch/fixture-lefthook"
(cd "$scratch/repo" && "$scratch/fixture-lefthook" install)
cd "$scratch/worktree"
common=$(git rev-parse --git-common-dir)
[[ -x $common/hooks/pre-commit && -x $common/hooks/pre-push ]]
grep -q 'exit 1' "$common/hooks/pre-commit"
grep -q 'run "pre-push"' "$common/hooks/pre-push"
printf 'PASS: shared hooks in %s\n' "$common"

attempt() {
  local name=$1 expected=$2 message=$3 status=0 before after
  before=$(git rev-parse HEAD)
  TIMEFORMAT='pre-commit elapsed %3R s'
  { time git commit -m "$name" -m 'Co-Authored-By: Claude Code <noreply@anthropic.com>'; } >"$evidence/$name.txt" 2>&1 || status=$?
  cat "$evidence/$name.txt"
  [[ $status == "$expected" ]]
  grep -Fq "$message" "$evidence/$name.txt"
  after=$(git rev-parse HEAD)
  if (( expected )); then
    [[ $before == "$after" ]]
  else
    [[ $before != "$after" ]]
  fi
  printf 'PASS: %s (exit %s)\n' "$name" "$status"
}

cat >internal/session/hook_g115.go <<'GO'
package session

func HookG115(n uint64) uint8 { return uint8(n) }
GO
git add internal/session/hook_g115.go
# A safe unstaged copy must not conceal the unsafe index version.
printf 'package session\n\nfunc HookG115(n uint64) uint64 { return n }\n' >internal/session/hook_g115.go
attempt staged-g115 1 'G115: integer overflow conversion'
rm internal/session/hook_g115.go
git add -u internal/session/hook_g115.go

printf '\n// Hook timing fixture.\n' >>internal/session/ring.go
git add internal/session/ring.go
attempt normal-change 0 'golangci: PASS'

cat >scripts/hook-shell.sh <<'SH'
#!/usr/bin/env bash
value="fixture value"
echo $value
SH
git add scripts/hook-shell.sh
attempt staged-shellcheck 1 '[SC2086]'
rm scripts/hook-shell.sh
git add -u scripts/hook-shell.sh

printf 'package session\nfunc HookFormatting( ) { }\n' >internal/session/hook_format.go
git add internal/session/hook_format.go
attempt staged-formatting 1 'gofmt: FAIL'
rm internal/session/hook_format.go
git add -u internal/session/hook_format.go

mkdir "$scratch/limited-tools"
for tool in bash sh git dirname cat grep uname tr sed go gofmt golangci-lint; do
  ln -s "$(command -v "$tool")" "$scratch/limited-tools/$tool"
done
printf '\n// Missing-tool fixture.\n' >>internal/session/ring.go
git add internal/session/ring.go
PATH="$scratch/limited-tools" attempt missing-shellcheck 1 'Run mise install, then lefthook install'
rm "$scratch/fixture-lefthook"
PATH="$scratch/limited-tools" attempt missing-lefthook 1 "Can't find lefthook in PATH"
grep -Fq 'Make sure lefthook is available' "$evidence/missing-lefthook.txt"
printf 'PASS: pre-push is wired to full gates; full execution is covered separately by CI\n'
