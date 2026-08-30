#!/usr/bin/env bash
set -euo pipefail

if [[ -z ${MESH:-} ]]; then
  MESH=$PWD/mesh
  go build -o "$MESH" ./cmd/mesh || { echo "FAIL: build" >&2; exit 1; }
fi

run_root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-bootstrap-installers.XXXXXX")
cleanup() {
  rm -rf -- "$run_root"
}
trap cleanup EXIT

fake_bin=$run_root/bin
mkdir -p "$fake_bin"
cat >"$fake_bin/systemctl" <<'EOF'
#!/bin/sh
if [ -n "${FAKE_SYSTEMCTL_LOG:-}" ]; then
  printf '%s\n' "$*" >>"$FAKE_SYSTEMCTL_LOG"
fi
case "$*" in
  *restart*)
    if [ -n "${FAKE_SYSTEMCTL_FAIL_FILE:-}" ] && [ ! -f "$FAKE_SYSTEMCTL_FAIL_FILE" ]; then
      : >"$FAKE_SYSTEMCTL_FAIL_FILE"
      exit 1
    fi
    ;;
esac
exit 0
EOF
cat >"$fake_bin/loginctl" <<'EOF'
#!/bin/sh
printf 'yes\n'
EOF
cat >"$fake_bin/launchctl" <<'EOF'
#!/bin/sh
if [ -n "${FAKE_LAUNCH_LOG:-}" ]; then
  printf '%s\n' "$*" >>"$FAKE_LAUNCH_LOG"
fi
if [ "${FAKE_LAUNCH_FAIL_ON:-}" = "$1" ] && [ ! -f "${FAKE_LAUNCH_FAIL_FILE:-/dev/null}" ]; then
  : >"$FAKE_LAUNCH_FAIL_FILE"
  exit 1
fi
case "$1" in
  print)
    case "$2" in
      gui/*/dev.shaulavo.mesh) test -f "$FAKE_LAUNCH_STATE.gui" ;;
      user/*/dev.shaulavo.mesh) test -f "$FAKE_LAUNCH_STATE.user" ;;
      gui/*) test "${FAKE_GUI_AVAILABLE:-1}" = 1 ;;
      user/*) exit 0 ;;
    esac
    ;;
  bootstrap)
    case "$2" in
      gui/*) : >"$FAKE_LAUNCH_STATE.gui" ;;
      user/*) : >"$FAKE_LAUNCH_STATE.user" ;;
      *) exit 1 ;;
    esac
    ;;
  bootout)
    case "$2" in
      gui/*) rm -f "$FAKE_LAUNCH_STATE.gui" ;;
      user/*) rm -f "$FAKE_LAUNCH_STATE.user" ;;
      *) exit 1 ;;
    esac
    ;;
  kickstart)
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod 0755 "$fake_bin/systemctl" "$fake_bin/loginctl" "$fake_bin/launchctl"

authorized_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMeshBootstrapInstallerFixture adopter'
authorized_key_b64=$(printf '%s' "$authorized_key" | base64 | tr -d '\n')
linux_service_b64=$(sed \
  -e 's|@MESH_BINARY@|%h/.local/bin/mesh|g' \
  -e 's|@MESH_PORT@|7337|g' \
  -e 's|@MESH_SSH_PORT@|2222|g' \
  -e 's|@MESH_WEBSOCKET_PATH@|/mesh|g' \
  scripts/install/assets/mesh.service | base64 | tr -d '\n')
darwin_service_b64=$(sed \
  -e 's|@MESH_BINARY@|${HOME}/.local/bin/mesh|g' \
  -e 's|@MESH_PORT@|7337|g' \
  -e 's|@MESH_SSH_PORT@|2222|g' \
  -e 's|@MESH_WEBSOCKET_PATH@|/mesh|g' \
  -e 's|@MESH_STDOUT@|${HOME}/.local/state/mesh/daemon.log|g' \
  -e 's|@MESH_STDERR@|${HOME}/.local/state/mesh/daemon.err.log|g' \
  scripts/install/assets/dev.shaulavo.mesh.plist | base64 | tr -d '\n')

linux_home=$run_root/linux-home
mkdir -p "$linux_home"
linux_source=$run_root/mesh-linux-first
cp "$MESH" "$linux_source"
linux_first=$(env HOME="$linux_home" PATH="$fake_bin:$PATH" \
  sh scripts/install/linux.sh "$linux_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$linux_first"

linux_source=$run_root/mesh-linux-second
cp "$MESH" "$linux_source"
linux_second=$(env HOME="$linux_home" PATH="$fake_bin:$PATH" \
  sh scripts/install/linux.sh "$linux_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=unchanged' <<<"$linux_second"
cmp -s "$MESH" "$linux_home/.local/bin/mesh"
grep -Fqx 'ExecStart=%h/.local/bin/mesh daemon --tailnet-port=7337 --ssh-port=2222 --websocket-path=/mesh' \
  "$linux_home/.config/systemd/user/mesh.service"
grep -Fqx 'KillMode=process' "$linux_home/.config/systemd/user/mesh.service"
test "$(grep -Fxc "$authorized_key" "$linux_home/.local/state/mesh/authorized_keys")" -eq 1
test "$(stat -c '%a' "$linux_home/.local/state/mesh/authorized_keys")" = 600

linux_retry_home=$run_root/linux-retry-home
linux_fail_file=$run_root/linux-restart-failed
mkdir -p "$linux_retry_home"
linux_source=$run_root/mesh-linux-retry-first
cp "$MESH" "$linux_source"
if linux_retry_output=$(env HOME="$linux_retry_home" PATH="$fake_bin:$PATH" \
    FAKE_SYSTEMCTL_FAIL_FILE="$linux_fail_file" \
    sh scripts/install/linux.sh "$linux_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64" 2>&1); then
  echo 'FAIL: Linux installer ignored a failed daemon restart' >&2
  exit 1
fi
grep -Fq 'MESH_BOOTSTRAP_ERROR=service_install' <<<"$linux_retry_output"
test -f "$linux_retry_home/.local/state/mesh/activation.pending"

linux_source=$run_root/mesh-linux-retry-second
cp "$MESH" "$linux_source"
linux_retry=$(env HOME="$linux_retry_home" PATH="$fake_bin:$PATH" \
  FAKE_SYSTEMCTL_FAIL_FILE="$linux_fail_file" \
  sh scripts/install/linux.sh "$linux_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$linux_retry"
test ! -e "$linux_retry_home/.local/state/mesh/activation.pending"

linux_source=$run_root/mesh-linux-retry-third
cp "$MESH" "$linux_source"
linux_retry=$(env HOME="$linux_retry_home" PATH="$fake_bin:$PATH" \
  FAKE_SYSTEMCTL_FAIL_FILE="$linux_fail_file" \
  sh scripts/install/linux.sh "$linux_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=unchanged' <<<"$linux_retry"

no_systemd_bin=$run_root/no-systemd-bin
mkdir -p "$no_systemd_bin"
ln -s "$(command -v rm)" "$no_systemd_bin/rm"
no_systemd_source=$run_root/mesh-no-systemd
cp "$MESH" "$no_systemd_source"
if no_systemd_output=$(env HOME="$run_root/no-systemd-home" PATH="$no_systemd_bin" \
  /bin/sh scripts/install/linux.sh "$no_systemd_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64" 2>&1); then
  echo "FAIL: Linux installer accepted a host without systemd" >&2
  exit 1
fi
grep -Fxq 'MESH_BOOTSTRAP_ERROR=no_systemd' <<<"$no_systemd_output"

no_linger_bin=$run_root/no-linger-bin
mkdir -p "$no_linger_bin"
cp "$fake_bin/systemctl" "$no_linger_bin/systemctl"
cat >"$no_linger_bin/loginctl" <<'EOF'
#!/bin/sh
printf 'no\n'
EOF
chmod 0755 "$no_linger_bin/loginctl"
no_linger_source=$run_root/mesh-no-linger
cp "$MESH" "$no_linger_source"
if no_linger_output=$(env HOME="$run_root/no-linger-home" PATH="$no_linger_bin:$PATH" \
  sh scripts/install/linux.sh "$no_linger_source" 7337 2222 /mesh "$authorized_key_b64" "$linux_service_b64" 2>&1); then
  echo "FAIL: Linux installer accepted a user without lingering" >&2
  exit 1
fi
grep -Fxq 'MESH_BOOTSTRAP_ERROR=no_user_lingering' <<<"$no_linger_output"

darwin_home=$run_root/darwin-home
mkdir -p "$darwin_home"
launch_state=$run_root/launchd-loaded
darwin_source=$run_root/mesh-darwin-first
cp "$MESH" "$darwin_source"
darwin_first=$(env HOME="$darwin_home" PATH="$fake_bin:$PATH" FAKE_LAUNCH_STATE="$launch_state" \
  sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$darwin_first"

darwin_source=$run_root/mesh-darwin-second
cp "$MESH" "$darwin_source"
darwin_second=$(env HOME="$darwin_home" PATH="$fake_bin:$PATH" FAKE_LAUNCH_STATE="$launch_state" \
  sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=unchanged' <<<"$darwin_second"
cmp -s "$MESH" "$darwin_home/.local/bin/mesh"
grep -Fq -- '--tailnet-port=7337 --ssh-port=2222' \
  "$darwin_home/Library/LaunchAgents/dev.shaulavo.mesh.plist"
grep -Fq '<key>AbandonProcessGroup</key>' \
  "$darwin_home/Library/LaunchAgents/dev.shaulavo.mesh.plist"
test "$(grep -Fxc "$authorized_key" "$darwin_home/.local/state/mesh/authorized_keys")" -eq 1
test "$(stat -c '%a' "$darwin_home/.local/state/mesh/authorized_keys")" = 600

darwin_retry_home=$run_root/darwin-retry-home
darwin_retry_state=$run_root/darwin-retry-state
darwin_fail_file=$run_root/darwin-bootout-failed
mkdir -p "$darwin_retry_home"
: >"$darwin_retry_state.gui"
darwin_source=$run_root/mesh-darwin-retry-first
cp "$MESH" "$darwin_source"
if darwin_retry_output=$(env HOME="$darwin_retry_home" PATH="$fake_bin:$PATH" \
    FAKE_LAUNCH_STATE="$darwin_retry_state" FAKE_LAUNCH_FAIL_ON=bootout \
    FAKE_LAUNCH_FAIL_FILE="$darwin_fail_file" \
    sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64" 2>&1); then
  echo 'FAIL: macOS installer ignored a failed launchd replacement' >&2
  exit 1
fi
grep -Fq 'MESH_BOOTSTRAP_ERROR=service_install' <<<"$darwin_retry_output"
test -f "$darwin_retry_home/.local/state/mesh/activation.pending"

darwin_source=$run_root/mesh-darwin-retry-second
cp "$MESH" "$darwin_source"
darwin_retry=$(env HOME="$darwin_retry_home" PATH="$fake_bin:$PATH" \
  FAKE_LAUNCH_STATE="$darwin_retry_state" FAKE_LAUNCH_FAIL_ON=bootout \
  FAKE_LAUNCH_FAIL_FILE="$darwin_fail_file" \
  sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$darwin_retry"
test ! -e "$darwin_retry_home/.local/state/mesh/activation.pending"

transition_home=$run_root/darwin-transition-home
transition_state=$run_root/darwin-transition-state
mkdir -p "$transition_home"
darwin_source=$run_root/mesh-darwin-transition-user
cp "$MESH" "$darwin_source"
env HOME="$transition_home" PATH="$fake_bin:$PATH" FAKE_LAUNCH_STATE="$transition_state" \
  FAKE_GUI_AVAILABLE=0 \
  sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64" >/dev/null
test -f "$transition_state.user"
test ! -e "$transition_state.gui"

darwin_source=$run_root/mesh-darwin-transition-gui
cp "$MESH" "$darwin_source"
transition_result=$(env HOME="$transition_home" PATH="$fake_bin:$PATH" \
  FAKE_LAUNCH_STATE="$transition_state" FAKE_GUI_AVAILABLE=1 \
  sh scripts/install/darwin.sh "$darwin_source" 7337 2222 /mesh "$authorized_key_b64" "$darwin_service_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$transition_result"
test -f "$transition_state.gui"
test ! -e "$transition_state.user"

echo "PASS: systemd and launchd installers converge across retries and domain changes"
