#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/update-status-test.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
mkdir "$scratch/bin"
awk '
  /^      - name: Report the real CI verdict on the tested commit$/ { found=1 }
  found && /^        run: \|$/ { body=1; next }
  body && /^          / { sub(/^          /, ""); print; next }
  body { exit }
' "$root/.github/workflows/ci.yml" > "$scratch/report.sh"
test -s "$scratch/report.sh"
cat > "$scratch/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ ${FAIL_API:-0} == 0 ]] || exit 1
printf '%s\n' "$*" >> "$STATUS_LOG"
MOCK
chmod +x "$scratch/bin/gh"
export PATH="$scratch/bin:$PATH" GH_TOKEN=fixture GH_REPO=ShaulLavo/fixture HEAD=tested-head RUN_URL=https://example.invalid/run STATUS_LOG="$scratch/statuses"
run_report() {
  : > "$STATUS_LOG"
  bash -euo pipefail "$scratch/report.sh"
  test "$(grep -c '^api --method POST repos/ShaulLavo/fixture/statuses/tested-head ' "$STATUS_LOG")" = 1
}
for result in success failure cancelled skipped unknown ''; do
  export RESULT="$result"
  run_report
  state=failure
  if [[ "$result" == success ]]; then state=success; fi
  grep -Fq -- '-f context=Complete CI gate -f state='"$state"' -f target_url=https://example.invalid/run' "$STATUS_LOG"
done
if FAIL_API=1 bash -euo pipefail "$scratch/report.sh"; then
  echo 'Status API failure was ignored' >&2; exit 1
fi
echo 'PASS: required statuses report the real tested results and API errors fail closed'
