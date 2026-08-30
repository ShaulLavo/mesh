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
exit 0
EOF
cat >"$fake_bin/loginctl" <<'EOF'
#!/bin/sh
printf 'yes\n'
EOF
cat >"$fake_bin/launchctl" <<'EOF'
#!/bin/sh
case "$1" in
  print)
    case "$2" in
      gui/*/dev.shaulavo.mesh|user/*/dev.shaulavo.mesh)
        test -f "$FAKE_LAUNCH_STATE"
        ;;
      gui/*|user/*)
        exit 0
        ;;
    esac
    ;;
  bootstrap)
    : >"$FAKE_LAUNCH_STATE"
    ;;
  bootout)
    rm -f "$FAKE_LAUNCH_STATE"
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

linux_home=$run_root/linux-home
mkdir -p "$linux_home"
linux_source=$run_root/mesh-linux-first
cp "$MESH" "$linux_source"
linux_first=$(env HOME="$linux_home" PATH="$fake_bin:$PATH" \
  sh scripts/install/linux.sh "$linux_source" 7337 /mesh "$authorized_key_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$linux_first"

linux_source=$run_root/mesh-linux-second
cp "$MESH" "$linux_source"
linux_second=$(env HOME="$linux_home" PATH="$fake_bin:$PATH" \
  sh scripts/install/linux.sh "$linux_source" 7337 /mesh "$authorized_key_b64")
grep -Fxq 'MESH_INSTALL_RESULT=unchanged' <<<"$linux_second"
cmp -s "$MESH" "$linux_home/.local/bin/mesh"
grep -Fqx 'ExecStart=%h/.local/bin/mesh daemon --tailnet-port=7337 --websocket-path=/mesh' \
  "$linux_home/.config/systemd/user/mesh.service"
test "$(grep -Fxc "$authorized_key" "$linux_home/.local/state/mesh/authorized_keys")" -eq 1
test "$(stat -c '%a' "$linux_home/.local/state/mesh/authorized_keys")" = 600

no_systemd_bin=$run_root/no-systemd-bin
mkdir -p "$no_systemd_bin"
ln -s "$(command -v rm)" "$no_systemd_bin/rm"
no_systemd_source=$run_root/mesh-no-systemd
cp "$MESH" "$no_systemd_source"
if no_systemd_output=$(env HOME="$run_root/no-systemd-home" PATH="$no_systemd_bin" \
  /bin/sh scripts/install/linux.sh "$no_systemd_source" 7337 /mesh "$authorized_key_b64" 2>&1); then
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
  sh scripts/install/linux.sh "$no_linger_source" 7337 /mesh "$authorized_key_b64" 2>&1); then
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
  sh scripts/install/darwin.sh "$darwin_source" 7337 /mesh "$authorized_key_b64")
grep -Fxq 'MESH_INSTALL_RESULT=configured' <<<"$darwin_first"

darwin_source=$run_root/mesh-darwin-second
cp "$MESH" "$darwin_source"
darwin_second=$(env HOME="$darwin_home" PATH="$fake_bin:$PATH" FAKE_LAUNCH_STATE="$launch_state" \
  sh scripts/install/darwin.sh "$darwin_source" 7337 /mesh "$authorized_key_b64")
grep -Fxq 'MESH_INSTALL_RESULT=unchanged' <<<"$darwin_second"
cmp -s "$MESH" "$darwin_home/.local/bin/mesh"
grep -Fq '<string>--tailnet-port=7337</string>' \
  "$darwin_home/Library/LaunchAgents/dev.shaulavo.mesh.plist"
test "$(grep -Fxc "$authorized_key" "$darwin_home/.local/state/mesh/authorized_keys")" -eq 1
test "$(stat -c '%a' "$darwin_home/.local/state/mesh/authorized_keys")" = 600

echo "PASS: systemd and launchd installers converge on rerun"
