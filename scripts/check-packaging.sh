#!/usr/bin/env bash
# Check the release, installer, service, and CI contracts without credentials.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

fail() {
  echo "packaging contract: $*" >&2
  exit 1
}

contains() {
  local file=$1
  local value=$2
  grep -Fq -- "$value" "$file" || fail "$file does not contain: $value"
}

does_not_contain() {
  local file=$1
  local value=$2
  if grep -Fq -- "$value" "$file"; then
    fail "$file unexpectedly contains: $value"
  fi
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

check_source_contract() {
  contains .goreleaser.yaml 'version: 2'
  contains .goreleaser.yaml 'name_template: "mesh_{{ .Os }}_{{ .Arch }}"'
  contains .goreleaser.yaml '-X github.com/shaul/mesh/internal/bootstrap.releaseVersion={{ .Tag }}'
  contains .goreleaser.yaml 'https://github.com/shaul/mesh/releases/download/{{ .Tag }}/{{ .ArtifactName }}'
  contains .goreleaser.yaml 'owner: shaul'
  contains .goreleaser.yaml 'name: mesh'
  contains .goreleaser.yaml 'directory: Casks'
  does_not_contain .goreleaser.yaml '.Version'

  contains scripts/install/assets/mesh.service 'Restart=on-failure'
  contains scripts/install/assets/mesh.service 'KillMode=process'
  contains scripts/install/assets/mesh.service 'WantedBy=default.target'
  contains scripts/install/assets/dev.shaulavo.mesh.plist '<key>RunAtLoad</key>'
  contains scripts/install/assets/dev.shaulavo.mesh.plist '<key>KeepAlive</key>'
  contains scripts/install/assets/dev.shaulavo.mesh.plist '<key>AbandonProcessGroup</key>'
  does_not_contain scripts/install/assets/dev.shaulavo.mesh.plist '<key>StandardOutPath</key>'
  does_not_contain scripts/install/assets/dev.shaulavo.mesh.plist '<key>StandardErrorPath</key>'
  contains scripts/install/install.go '//go:embed assets/mesh.service'
  contains scripts/install/install.go '//go:embed assets/dev.shaulavo.mesh.plist'
  contains scripts/install/linux.sh 'service_b64=$5'
  contains scripts/install/darwin.sh 'service_b64=$5'
  does_not_contain scripts/install/linux.sh '[Unit]'
  does_not_contain scripts/install/darwin.sh '<!DOCTYPE plist'

  while read -r checksum name; do
    [[ -n $checksum && -n $name ]] || fail 'invalid service checksum manifest'
    local actual
    actual=$(sha256_file "scripts/install/assets/$name")
    [[ $actual == "$checksum" ]] || fail "service checksum for $name is $checksum, want $actual"
  done <scripts/install/assets/checksums.txt

  contains scripts/install.sh 'https://github.com/shaul/mesh'
  contains scripts/install.sh 'https://raw.githubusercontent.com/shaul/mesh/$version/scripts/install/assets'
  contains scripts/install.sh "--proto-redir '=https'"
  contains scripts/install.sh 'gum confirm'
  contains scripts/install.sh 'gum spin'
  contains scripts/install.sh 'verify_checksum "$archive"'
  contains scripts/install.sh 'verify_checksum "$service_asset"'
  contains scripts/install.sh 'MESH_BIN_DIR'
  contains scripts/install.sh 'if [ ! -d "$binary_dir" ]'
  contains scripts/install.sh "'s|@MESH_STDOUT@|\${HOME}/.local/state/mesh/daemon.log|g'"
  does_not_contain scripts/install.sh 'raw.githubusercontent.com/shaul/mesh/master/'

  local archive_verify_line service_verify_line publish_line
  archive_verify_line=$(grep -nF 'verify_checksum "$archive"' scripts/install.sh | cut -d: -f1)
  service_verify_line=$(grep -nF 'verify_checksum "$service_asset"' scripts/install.sh | cut -d: -f1)
  publish_line=$(grep -nF 'install_binary' scripts/install.sh | tail -1 | cut -d: -f1)
  (( archive_verify_line < publish_line && service_verify_line < publish_line )) ||
    fail 'the installer publishes before all fetched artifacts are verified'

  for workflow in .github/workflows/ci.yml .github/workflows/release.yml; do
    contains "$workflow" 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1'
    contains "$workflow" 'actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0'
    if grep -E 'uses: .+@' "$workflow" | grep -Ev '@[0-9a-f]{40}([[:space:]]+# .+)?$'; then
      fail "$workflow contains an action reference that is not a full commit SHA"
    fi
  done
  contains .github/workflows/ci.yml 'go test -race ./...'
  contains .github/workflows/ci.yml './scripts/verify.sh'
  contains .github/workflows/ci.yml 'govulncheck@v1.7.0'
  contains .github/workflows/ci.yml 'golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a # v9.3.0'
  contains .github/workflows/ci.yml 'version: v2.13.2'
  for linter in bodyclose errcheck gosec govet ineffassign staticcheck unused; do
    contains .golangci.yml "    - $linter"
  done
  does_not_contain .golangci.yml '-SA1012'
  does_not_contain .golangci.yml '-SA1019'
  does_not_contain .golangci.yml '-SA2001'
  contains .github/workflows/ci.yml 'goreleaser/goreleaser-action@f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3'
  contains .github/workflows/ci.yml 'version: v2.18.0'
  contains .github/workflows/release.yml 'tags:'
  contains .github/workflows/release.yml 'contents: read'
  contains .github/workflows/release.yml 'needs: validate'
  contains .github/workflows/release.yml 'contents: write'
  contains .github/workflows/release.yml './scripts/check-packaging.sh dist'
  contains .github/workflows/release.yml 'args: release --clean'
  local candidate_check_line publish_release_line
  candidate_check_line=$(grep -nF './scripts/check-packaging.sh dist' .github/workflows/release.yml | cut -d: -f1)
  publish_release_line=$(grep -nF 'Publish GitHub release and Homebrew Cask' .github/workflows/release.yml | cut -d: -f1)
  (( candidate_check_line < publish_release_line )) || fail 'tagged release publishes before its dist contract passes'

  [[ -x integration/reboot_simulation.sh ]] || fail 'reboot_simulation.sh is not executable'
  contains scripts/verify.sh 'integration/*.sh'

  if rg -n 'example\.com|OWNER/REPO|CHANGEME|TODO_URL' \
      .goreleaser.yaml .github scripts/install.sh scripts/install/assets; then
    fail 'packaging contains a placeholder URL or repository'
  fi

  sh -n scripts/install.sh scripts/install/linux.sh scripts/install/darwin.sh
  bash -n scripts/check-packaging.sh scripts/render-demos.sh integration/*.sh
}

check_cross_builds() (
  local build_root
  build_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-cross-builds.XXXXXX")
  trap 'rm -rf -- "$build_root"' EXIT

  local target goos goarch binary metadata
  for target in linux/amd64 linux/arm64 darwin/arm64; do
    goos=${target%/*}
    goarch=${target#*/}
    binary=$build_root/mesh_${goos}_${goarch}
    env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath \
        -ldflags '-X github.com/shaul/mesh/internal/bootstrap.releaseVersion=v0.0.0-contract' \
        -o "$binary" ./cmd/mesh
    metadata=$(go version -m "$binary")
    grep -Fq "GOOS=$goos" <<<"$metadata" || fail "$binary was not built for $goos"
    grep -Fq "GOARCH=$goarch" <<<"$metadata" || fail "$binary was not built for $goarch"
  done
)

check_demo_commands() (
  local demo_root demo_bin tape line command
  demo_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-demo-check.XXXXXX")
  trap 'rm -rf -- "$demo_root"' EXIT
  demo_bin=$demo_root/mesh
  go build -trimpath -o "$demo_bin" ./cmd/mesh
  for tape in docs/demos/*.tape; do
    local command_count=0
    while IFS= read -r line; do
      [[ $line == 'Type "mesh '*'"' ]] || continue
      command=${line#'Type "mesh '}
      command=${command%'"'}
      read -r -a args <<<"$command"
      "$demo_bin" "${args[@]}" >/dev/null
      ((command_count += 1))
    done <"$tape"
    (( command_count > 0 )) || fail "$tape contains no Mesh command"
  done
)

check_release_dist() {
  local dist=$1
  [[ -d $dist ]] || fail "release directory does not exist: $dist"
  [[ -f $dist/checksums.txt ]] || fail "$dist/checksums.txt is missing"
  [[ -f $dist/homebrew/Casks/mesh.rb ]] || fail "$dist/homebrew/Casks/mesh.rb is missing"

  local expected actual manifest_names archives darwin_checksum cask_checksum
  expected=$'mesh_darwin_arm64.tar.gz\nmesh_linux_amd64.tar.gz\nmesh_linux_arm64.tar.gz'
  archives=("$dist"/mesh_*.tar.gz)
  actual=$(printf '%s\n' "${archives[@]##*/}" | sort)
  [[ $actual == "$expected" ]] || fail "release archives are:\n$actual\nwant:\n$expected"
  manifest_names=$(awk '{print $2}' "$dist/checksums.txt" | sort)
  [[ $manifest_names == "$expected" ]] || fail "checksums.txt names:\n$manifest_names\nwant:\n$expected"
  contains "$dist/homebrew/Casks/mesh.rb" 'https://github.com/shaul/mesh/releases/download/'
  contains "$dist/homebrew/Casks/mesh.rb" 'mesh_darwin_arm64.tar.gz'
  does_not_contain "$dist/homebrew/Casks/mesh.rb" 'mesh_linux_amd64.tar.gz'
  does_not_contain "$dist/homebrew/Casks/mesh.rb" 'mesh_linux_arm64.tar.gz'
  does_not_contain "$dist/homebrew/Casks/mesh.rb" 'example.com'
  [[ $(grep -Fc 'binary "mesh"' "$dist/homebrew/Casks/mesh.rb") -eq 1 ]] ||
    fail "Homebrew Cask must install exactly one mesh binary"
  darwin_checksum=$(awk '$2 == "mesh_darwin_arm64.tar.gz" { print $1 }' "$dist/checksums.txt")
  cask_checksum=$(sed -n 's/^[[:space:]]*sha256 "\([0-9a-fA-F]*\)".*/\1/p' "$dist/homebrew/Casks/mesh.rb")
  [[ ${#cask_checksum} -eq 64 && $cask_checksum == "$darwin_checksum" ]] ||
    fail "Homebrew Cask checksum $cask_checksum does not match Darwin archive checksum $darwin_checksum"

  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$dist" && sha256sum --check --strict checksums.txt)
  else
    (cd "$dist" && shasum -a 256 --check checksums.txt)
  fi

  local archive base target goos goarch unpacked entries metadata archive_mode
  for archive in "${archives[@]}"; do
    base=$(basename "$archive" .tar.gz)
    target=${base#mesh_}
    goos=${target%_*}
    goarch=${target#*_}
    entries=$(tar -tzf "$archive")
    [[ $entries == mesh ]] || fail "$archive does not contain exactly one file named mesh"
    archive_mode=$(tar -tvzf "$archive" | awk 'NR == 1 { print $1 }')
    [[ $archive_mode == -??x* ]] || fail "$archive mesh entry is not owner-executable"
    unpacked=$(mktemp -d "${TMPDIR:-/tmp}/mesh-archive.XXXXXX")
    tar -xzf "$archive" -C "$unpacked"
    [[ -f $unpacked/mesh && ! -L $unpacked/mesh && -x $unpacked/mesh ]] ||
      fail "$archive does not contain an executable regular mesh binary"
    metadata=$(go version -m "$unpacked/mesh")
    grep -Fq "GOOS=$goos" <<<"$metadata" || fail "$archive contains the wrong operating-system binary"
    grep -Fq "GOARCH=$goarch" <<<"$metadata" || fail "$archive contains the wrong architecture binary"
    rm -rf -- "$unpacked"
  done
}

check_source_contract
check_cross_builds
check_demo_commands
if [[ $# -gt 1 ]]; then
  fail 'usage: scripts/check-packaging.sh [dist-directory]'
fi
if [[ $# -eq 1 ]]; then
  check_release_dist "$1"
fi

echo 'PASS: packaging contract'
