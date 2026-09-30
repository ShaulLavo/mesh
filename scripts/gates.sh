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
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mesh-gates.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
if [[ -z $report_dir ]]; then
  report_dir=$scratch/reports
fi
mkdir -p "$report_dir"
report_dir=$(cd "$report_dir" && pwd)

require_version() {
  local tool=$1 pattern=$2
  if ! "$tool" --version | grep -Eq "$pattern"; then
    echo "gates: $tool version mismatch; see README.md" >&2
    exit 2
  fi
}

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
packages=(./...)
if (( fast )); then
  packages=()
  while IFS= read -r -d '' file; do
    [[ $file != third_party/* && $file == *.go && -f $file ]] || continue
    dirname -- "$file" >>"$scratch/scope"
    if [[ -n $(gofmt -l "$file") ]]; then
      echo "gofmt: FAIL ($file)" >&2
      exit 1
    fi
  done < <(git diff --cached --name-only --diff-filter=ACMR -z)
  if [[ ! -s $scratch/scope ]]; then
    echo 'gates: PASS (no staged Go packages)'
    exit 0
  fi
  sort -u "$scratch/scope" -o "$scratch/scope"
  while IFS= read -r directory; do
    packages+=("./$directory")
  done <"$scratch/scope"
fi
if (( ! fast )); then
  find cmd internal scripts integration -type f -name '*.go' -print0 |
    xargs -0 gofmt -l >"$scratch/unformatted"
  if [[ -s $scratch/unformatted ]]; then
    echo 'gofmt: FAIL' >&2
    cat "$scratch/unformatted" >&2
    exit 1
  fi
fi
echo 'gofmt: PASS'
go vet "${packages[@]}"
echo 'vet: PASS'
require_version golangci-lint 'version 2\.13\.2([[:space:]]|$)'
status=0
golangci-lint run --output.json.path="$report_dir/golangci.json" --output.text.path="$report_dir/golangci.txt" \
  "${packages[@]}" || status=$?
if (( status > 1 )) || [[ ! -s $report_dir/golangci.json ]]; then
  echo "golangci: ERROR (tool exit $status)" >&2
  exit 2
fi
if (( fast )); then
  printf '[]\n' | tee "$report_dir/deadcode.json" "$report_dir/shellcheck.json" >"$report_dir/ruff.json"
  python3 .gates/check.py --reports "$report_dir" --scope "$scratch/scope"
  exit
fi
run_report deadcode "$report_dir/deadcode.json" \
  go run golang.org/x/tools/cmd/deadcode@v0.49.0 -json -filter '^github\.com/shaul/mesh/' ./cmd/...
require_version shellcheck '^version: 0\.11\.0$'
shell_files=()
while IFS= read -r -d '' file; do
  shell_files+=("$file")
done < <(find scripts integration -type f -name '*.sh' -print0)
run_report shellcheck "$report_dir/shellcheck.json" shellcheck --format=json "${shell_files[@]}"
require_version ruff '^ruff 0\.16\.9$'
run_report ruff "$report_dir/ruff.json" ruff check --isolated --select E4,E7,E9,F \
  --output-format=json scripts integration .gates
python3 -m unittest discover -s .gates -p 'test_*.py'
echo 'baseline tests: PASS'

unexpected_auth_flags=$(grep -RnE --include='*.go' --include='*.sh' --exclude='*_test.go' -- '--auth-key' internal cmd scripts/install |
  grep -vE -- '--auth-key=file:/dev/stdin|tailscale-auth-key-file' || true)
if [[ -n $unexpected_auth_flags ]]; then
  printf 'bootstrap auth-key contract: FAIL\n%s\n' "$unexpected_auth_flags" >&2
  exit 1
fi
echo 'bootstrap auth-key contract: PASS'
if go list -deps ./cmd/mesh | grep -Eq '^github\.com/charmbracelet/(bubbletea|lipgloss)$'; then
  echo 'terminal dependencies: FAIL (legacy terminal initialization)' >&2
  exit 1
fi
echo 'terminal dependencies: PASS'
arguments=()
if (( update )); then
  arguments+=(--update-baseline)
fi
python3 .gates/check.py --reports "$report_dir" "${arguments[@]}"
