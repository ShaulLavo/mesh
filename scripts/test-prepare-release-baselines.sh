#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-baseline-test.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT
fixtures=$test_root/fixtures
fake_bin=$test_root/bin
mkdir -p "$fixtures" "$fake_bin"

make_archive() {
  local tag=$1
  local contents=$2
  local directory=$fixtures/$tag
  mkdir -p "$directory/input"
  printf '%s\n' "$contents" >"$directory/input/mesh"
  tar -czf "$directory/mesh_linux_amd64.tar.gz" -C "$directory/input" mesh
}
make_archive v0.1.38 previous
make_archive v0.1.37 older

printf 'manually installed recovery\n' >"$test_root/baseline.bin"
baseline_digest=$(sha256sum "$test_root/baseline.bin" | awk '{print $1}')
baseline_name=baseline_linux_amd64_${baseline_digest}.bin
mkdir -p "$fixtures/v0.1.39"
cp "$test_root/baseline.bin" "$fixtures/v0.1.39/$baseline_name"

cat >"$fake_bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == api ]]; then
  printf '%s\n' v0.1.38 v0.1.37 v0.1.36
  exit 0
fi
[[ $1 == release && $2 == download ]] || exit 1
tag=$3
shift 3
pattern=''
directory=''
while [[ $# -gt 0 ]]; do
  case $1 in
    --pattern) pattern=$2; shift 2 ;;
    --dir) directory=$2; shift 2 ;;
    *) exit 1 ;;
  esac
done
source=$GH_FIXTURES/$tag/$pattern
[[ -f $source ]] || exit 1
cp "$source" "$directory/$pattern"
GH
chmod 0755 "$fake_bin/gh"

output=$test_root/output
list=$test_root/list
env PATH="$fake_bin:$PATH" GH_FIXTURES="$fixtures" GITHUB_REPOSITORY=ShaulLavo/mesh \
  "$repo_root/scripts/prepare-release-baselines.sh" \
  v0.1.39 v0.1.38 linux/amd64 "$baseline_name" "$output" "$list"
[[ $(wc -l <"$list") -eq 3 ]]
grep -Fqx "$output/$baseline_name" "$list"
grep -Fqx "$output/release-v0.1.38/mesh" "$list"
grep -Fqx "$output/release-v0.1.37/mesh" "$list"

bad_name=baseline_linux_amd64_$(printf '0%.0s' {1..64}).bin
cp "$test_root/baseline.bin" "$fixtures/v0.1.39/$bad_name"
if env PATH="$fake_bin:$PATH" GH_FIXTURES="$fixtures" GITHUB_REPOSITORY=ShaulLavo/mesh \
  "$repo_root/scripts/prepare-release-baselines.sh" \
  v0.1.39 v0.1.38 linux/amd64 "$bad_name" "$test_root/bad" "$test_root/bad.list" >/dev/null 2>&1; then
  echo 'baseline preparation accepted a filename with the wrong digest' >&2
  exit 1
fi

echo 'PASS: release baselines are bounded and manually supplied binaries are digest-bound'
