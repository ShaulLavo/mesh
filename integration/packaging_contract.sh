#!/usr/bin/env bash
set -euo pipefail

if [[ -z ${MESH:-} ]]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo 'FAIL: build' >&2; exit 1; }
fi

./scripts/check-packaging.sh

run_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-packaging.XXXXXX")
cleanup() {
  rm -rf -- "$run_root"
}
trap cleanup EXIT

case $(uname -m) in
  x86_64 | amd64) host_arch=amd64 ;;
  aarch64 | arm64) host_arch=arm64 ;;
  *) echo "FAIL: unsupported integration architecture $(uname -m)" >&2; exit 1 ;;
esac

release_dir=$run_root/release
payload_dir=$run_root/payload
mkdir -p "$release_dir" "$payload_dir"
cp "$MESH" "$payload_dir/mesh"
archive=mesh_linux_${host_arch}.tar.gz
tar -czf "$release_dir/$archive" -C "$payload_dir" mesh
darwin_archive=mesh_darwin_arm64.tar.gz
cp "$release_dir/$archive" "$release_dir/$darwin_archive"
if command -v sha256sum >/dev/null 2>&1; then
  archive_checksum=$(sha256sum "$release_dir/$archive" | awk '{print $1}')
  darwin_checksum=$(sha256sum "$release_dir/$darwin_archive" | awk '{print $1}')
else
  archive_checksum=$(shasum -a 256 "$release_dir/$archive" | awk '{print $1}')
  darwin_checksum=$(shasum -a 256 "$release_dir/$darwin_archive" | awk '{print $1}')
fi
printf '%s  %s\n%s  %s\n' \
  "$archive_checksum" "$archive" "$darwin_checksum" "$darwin_archive" >"$release_dir/checksums.txt"

fake_bin=$run_root/fake-bin
mkdir -p "$fake_bin"
cat >"$fake_bin/loginctl" <<'EOF'
#!/bin/sh
case "$1" in
  show-user) printf 'yes\n' ;;
  enable-linger) exit 0 ;;
  *) exit 1 ;;
esac
EOF
cat >"$fake_bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_SYSTEMCTL_LOG"
case "$*" in
  *restart*)
    if [ -n "${FAKE_SYSTEMCTL_FAIL_FILE:-}" ] && [ -f "$FAKE_SYSTEMCTL_FAIL_FILE" ]; then
      rm -f "$FAKE_SYSTEMCTL_FAIL_FILE"
      exit 1
    fi
    ;;
  *is-active*) exit 0 ;;
  *) exit 0 ;;
esac
EOF
chmod 0755 "$fake_bin/loginctl" "$fake_bin/systemctl"

install_home=$run_root/home
install_bin=$install_home/custom-bin
mkdir -p "$install_home" "$install_bin"
chmod 0755 "$install_bin"
systemctl_log=$run_root/systemctl.log
install_env=(
  HOME="$install_home"
  PATH="$fake_bin:$PATH"
  FAKE_SYSTEMCTL_LOG="$systemctl_log"
  MESH_VERSION=v0.0.0-contract
  MESH_INSTALL_YES=1
  MESH_RELEASE_DIR="$release_dir"
  MESH_SERVICE_ASSET_DIR="$PWD/scripts/install/assets"
  MESH_BIN_DIR="$install_bin"
)

first=$(env "${install_env[@]}" sh scripts/install.sh)
grep -Fq 'installed at' <<<"$first"
cmp -s "$MESH" "$install_bin/mesh"
test "$(stat -c '%a' "$install_bin")" = 755
unit=$install_home/.config/systemd/user/mesh.service
grep -Fqx "ExecStart=$install_bin/mesh daemon --tailnet-port=7337 --websocket-path=/mesh" "$unit"
grep -Fqx 'KillMode=process' "$unit"

: >"$systemctl_log"
second=$(env "${install_env[@]}" sh scripts/install.sh)
grep -Fq 'unchanged at' <<<"$second"
if grep -Fq 'restart mesh.service' "$systemctl_log"; then
  echo 'FAIL: unchanged installer rerun restarted the daemon' >&2
  exit 1
fi

retry_home=$run_root/retry-home
retry_bin=$retry_home/custom-bin
retry_log=$run_root/retry-systemctl.log
retry_failure=$run_root/fail-systemctl-once
mkdir -p "$retry_home" "$retry_bin"
touch "$retry_failure"
retry_env=(
  HOME="$retry_home"
  PATH="$fake_bin:$PATH"
  FAKE_SYSTEMCTL_LOG="$retry_log"
  FAKE_SYSTEMCTL_FAIL_FILE="$retry_failure"
  MESH_VERSION=v0.0.0-contract
  MESH_INSTALL_YES=1
  MESH_RELEASE_DIR="$release_dir"
  MESH_SERVICE_ASSET_DIR="$PWD/scripts/install/assets"
  MESH_BIN_DIR="$retry_bin"
)
if retry_failure_output=$(env "${retry_env[@]}" sh scripts/install.sh 2>&1); then
  echo 'FAIL: installer ignored the simulated systemd restart failure' >&2
  exit 1
fi
grep -Fq 'restart mesh.service' "$retry_log"
test -f "$retry_home/.local/state/mesh/activation.pending"
cmp -s "$MESH" "$retry_bin/mesh"

: >"$retry_log"
retry_success=$(env "${retry_env[@]}" sh scripts/install.sh)
grep -Fq 'installed at' <<<"$retry_success"
grep -Fq 'daemon-reload' "$retry_log"
grep -Fq 'restart mesh.service' "$retry_log"
test ! -e "$retry_home/.local/state/mesh/activation.pending"

: >"$retry_log"
retry_unchanged=$(env "${retry_env[@]}" sh scripts/install.sh)
grep -Fq 'unchanged at' <<<"$retry_unchanged"
if grep -Fq 'restart mesh.service' "$retry_log"; then
  echo 'FAIL: recovered installer rerun restarted the daemon again' >&2
  exit 1
fi

bad_release=$run_root/bad-release
bad_home=$run_root/bad-home
mkdir -p "$bad_release" "$bad_home"
cp "$release_dir/checksums.txt" "$bad_release/checksums.txt"
cp "$release_dir/$archive" "$bad_release/$archive"
printf 'corrupt' >>"$bad_release/$archive"
if bad_output=$(env HOME="$bad_home" PATH="$fake_bin:$PATH" \
    MESH_VERSION=v0.0.0-contract MESH_INSTALL_YES=1 MESH_SKIP_SERVICE=1 \
    MESH_RELEASE_DIR="$bad_release" sh scripts/install.sh 2>&1); then
  echo 'FAIL: installer accepted a corrupt release archive' >&2
  exit 1
fi
grep -Fq 'checksum mismatch' <<<"$bad_output"
test ! -e "$bad_home/.local/bin/mesh"

bad_assets=$run_root/bad-assets
bad_asset_home=$run_root/bad-asset-home
mkdir -p "$bad_assets" "$bad_asset_home"
cp scripts/install/assets/* "$bad_assets/"
printf 'corrupt' >>"$bad_assets/mesh.service"
if bad_asset_output=$(env HOME="$bad_asset_home" PATH="$fake_bin:$PATH" \
    FAKE_SYSTEMCTL_LOG="$systemctl_log" MESH_VERSION=v0.0.0-contract MESH_INSTALL_YES=1 \
    MESH_RELEASE_DIR="$release_dir" MESH_SERVICE_ASSET_DIR="$bad_assets" \
    sh scripts/install.sh 2>&1); then
  echo 'FAIL: installer accepted a corrupt service asset' >&2
  exit 1
fi
grep -Fq 'checksum mismatch' <<<"$bad_asset_output"
test ! -e "$bad_asset_home/.local/bin/mesh"

darwin_bin=$run_root/darwin-bin
hostile_home=$run_root/'darwin&home'
launch_state=$run_root/launchd-state
launch_log=$run_root/launchctl.log
mkdir -p "$darwin_bin" "$hostile_home"
cat >"$fake_bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
  -s) printf 'Darwin\n' ;;
  -m) printf 'arm64\n' ;;
  *) exit 1 ;;
esac
EOF
cat >"$fake_bin/launchctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LAUNCH_LOG"
state_file() {
  case "$1" in
    gui/*) printf '%s.gui\n' "$FAKE_LAUNCH_STATE" ;;
    user/*) printf '%s.user\n' "$FAKE_LAUNCH_STATE" ;;
    *) exit 1 ;;
  esac
}
case "$1" in
  print)
    case "$2" in
      gui/*/dev.shaulavo.mesh|user/*/dev.shaulavo.mesh)
        test -f "$(state_file "$2")"
        ;;
      gui/*) test "${FAKE_GUI_AVAILABLE:-1}" = 1 ;;
      user/*) exit 0 ;;
    esac
    ;;
  bootstrap) : >"$(state_file "$2")" ;;
  bootout) rm -f "$(state_file "$2")" ;;
  *) exit 0 ;;
esac
EOF
chmod 0755 "$fake_bin/uname" "$fake_bin/launchctl"
darwin_env=(
  HOME="$hostile_home"
  PATH="$fake_bin:$PATH"
  FAKE_LAUNCH_STATE="$launch_state"
  FAKE_LAUNCH_LOG="$launch_log"
  MESH_VERSION=v0.0.0-contract
  MESH_INSTALL_YES=1
  MESH_RELEASE_DIR="$release_dir"
  MESH_SERVICE_ASSET_DIR="$PWD/scripts/install/assets"
  MESH_BIN_DIR="$darwin_bin"
)
env "${darwin_env[@]}" FAKE_GUI_AVAILABLE=0 \
  sh scripts/install.sh >/dev/null
test -f "$launch_state.user"
test ! -e "$launch_state.gui"

: >"$launch_log"
env "${darwin_env[@]}" FAKE_GUI_AVAILABLE=1 \
  sh scripts/install.sh >/dev/null
test -f "$launch_state.gui"
test ! -e "$launch_state.user"
grep -Fq 'bootout user/' "$launch_log"
grep -Fq 'bootstrap gui/' "$launch_log"

: >"$launch_log"
darwin_unchanged=$(env "${darwin_env[@]}" FAKE_GUI_AVAILABLE=1 \
  sh scripts/install.sh)
grep -Fq 'unchanged at' <<<"$darwin_unchanged"
if grep -Eq 'bootout|bootstrap' "$launch_log"; then
  echo 'FAIL: unchanged launchd rerun reloaded the daemon' >&2
  exit 1
fi
plist=$hostile_home/Library/LaunchAgents/dev.shaulavo.mesh.plist
grep -Fq '${HOME}/.local/state/mesh/daemon.log' "$plist"
if grep -Fq 'darwin&home' "$plist"; then
  echo 'FAIL: hostile HOME was injected into the launchd plist' >&2
  exit 1
fi

echo 'PASS: release and service checksums gate atomic installation'
