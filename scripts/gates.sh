#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"
fast=0
update=0
report_dir=
while (( $# )); do
  case "$1" in
    --fast) fast=1 ;;
    --update-baseline) update=1 ;;
    --report-dir)
      [[ $# -ge 2 ]] || { echo 'gates: --report-dir needs a path' >&2; exit 2; }
      report_dir=$2
      shift
      ;;
    *) echo "usage: $0 [--fast] [--update-baseline] [--report-dir DIR]" >&2; exit 2 ;;
  esac
  shift
done
if (( fast && update )); then
  echo 'gates: --update-baseline requires a full scan' >&2
  exit 2
fi

install_hint='Run mise install, then lefthook install from the repository.'
require_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'gates: missing %s. %s\n' "$1" "$install_hint" >&2
    exit 2
  fi
}
require_version() {
  local tool=$1 pattern=$2
  require_tool "$tool"
  if ! "$tool" --version | grep -Eq "$pattern"; then
    printf 'gates: %s version mismatch. %s\n' "$tool" "$install_hint" >&2
    exit 2
  fi
}
require_tool go
require_tool gofmt
require_version golangci-lint 'version 2\.13\.2([[:space:]]|$)'
require_version shellcheck '^version: 0\.11\.0$'

scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-gates.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z $report_dir ]]; then
  report_dir=$scratch/reports
fi
mkdir -p "$report_dir"
report_dir=$(cd "$report_dir" && pwd)

run_report() {
  local name=$1 destination=$2 status=0
  shift 2
  "$@" >"$destination" || status=$?
  if (( status > 1 )) || [[ ! -s $destination ]]; then
    echo "$name: ERROR (tool exit $status, report $destination)" >&2
    exit 2
  fi
}

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
packages=(./... ./.gates)
shell_files=()
arguments=()
if (( fast )); then
  git diff --cached --name-only --no-renames --diff-filter=ACMRD -z >"$scratch/staged"
  # Inspect the index, not unstaged edits, without stashing or modifying the worktree.
  git checkout-index --all --prefix="$scratch/index/"
  cd "$scratch/index"
  packages=()
  arguments=(--partial)
  while IFS= read -r -d '' file; do
    [[ $file != third_party/* ]] || continue
    if [[ $file == *.sh && -f $file ]]; then
      shell_files+=("$file")
      arguments+=(--shell-file "$file")
    fi
    [[ $file == *.go ]] || continue
    directory=$(dirname -- "$file")
    [[ -d $directory ]] || continue
    go_files=("$directory"/*.go)
    [[ -f ${go_files[0]} ]] || continue
    if [[ -f $file ]]; then
      formatted=$(gofmt -l "$file")
      if [[ -n $formatted ]]; then
        echo "gofmt: FAIL ($file)" >&2
        exit 1
      fi
    fi
    seen=0
    for package in ${packages[@]+"${packages[@]}"}; do
      [[ $package != "./$directory" ]] || seen=1
    done
    if (( ! seen )); then
      packages+=("./$directory")
      arguments+=(--package "$directory")
    fi
  done <"$scratch/staged"
  if (( ${#packages[@]} == 0 && ${#shell_files[@]} == 0 )); then
    echo 'gates: PASS (no staged Go or shell files)'
    exit 0
  fi
else
  find cmd internal scripts integration .gates -type f -name '*.go' -print0 |
    xargs -0 gofmt -l >"$scratch/unformatted"
  if [[ -s $scratch/unformatted ]]; then
    echo 'gofmt: FAIL' >&2
    cat "$scratch/unformatted" >&2
    exit 1
  fi
  while IFS= read -r -d '' file; do
    shell_files+=("$file")
  done < <(find scripts integration -type f -name '*.sh' -print0)
fi
echo 'gofmt: PASS'
if (( ${#packages[@]} )); then
  go vet "${packages[@]}"
  echo 'vet: PASS'
  status=0
  golangci-lint run --output.json.path="$report_dir/golangci.json" --output.text.path="$report_dir/golangci.txt" \
    "${packages[@]}" || status=$?
  if (( status > 1 )) || [[ ! -s $report_dir/golangci.json ]]; then
    echo "golangci: ERROR (tool exit $status)" >&2
    exit 2
  fi
else
  printf '{"Issues":[]}\n' >"$report_dir/golangci.json"
fi
if (( ${#shell_files[@]} )); then
  run_report shellcheck "$report_dir/shellcheck.json" shellcheck --format=json "${shell_files[@]}"
else
  printf '[]\n' >"$report_dir/shellcheck.json"
fi
if (( fast )); then
  printf '[]\n' >"$report_dir/deadcode.json"
  printf '[]\n' >"$report_dir/ruff.json"
  go run ./.gates --reports "$report_dir" ${arguments[@]+"${arguments[@]}"}
  exit
fi
run_report deadcode "$report_dir/deadcode.json" \
  go run golang.org/x/tools/cmd/deadcode@v0.49.0 -json -filter '^github\.com/shaul/mesh/' ./cmd/...
require_version ruff '^ruff 0\.16\.9$'
run_report ruff "$report_dir/ruff.json" ruff check --isolated --select E4,E7,E9,F \
  --output-format=json scripts integration .gates
go test ./.gates
echo 'baseline tests: PASS'

unexpected_auth_flags=$(grep -RnE --include='*.go' --include='*.sh' --exclude='*_test.go' -- '--auth-key' internal cmd scripts/install |
  grep -vE -- '--auth-key=file:/dev/stdin|tailscale-auth-key-file' || true)
if [[ -n $unexpected_auth_flags ]]; then
  printf 'bootstrap auth-key contract: FAIL\n%s\n' "$unexpected_auth_flags" >&2
  exit 1
fi
echo 'bootstrap auth-key contract: PASS'
dependencies=$(go list -deps ./cmd/mesh)
if grep -Eq '^github\.com/charmbracelet/(bubbletea|lipgloss)$' <<<"$dependencies"; then
  echo 'terminal dependencies: FAIL (legacy terminal initialization)' >&2
  exit 1
fi
echo 'terminal dependencies: PASS'
if (( update )); then
  arguments+=(--update-baseline)
fi
go run ./.gates --reports "$report_dir" ${arguments[@]+"${arguments[@]}"}
