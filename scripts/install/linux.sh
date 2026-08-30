#!/bin/sh
set -eu

fail() {
	printf 'MESH_BOOTSTRAP_ERROR=%s\n%s\n' "$1" "$2" >&2
	exit 1
}

if [ "$#" -ne 5 ]; then
	fail service_install "linux installer requires binary, port, WebSocket path, authorized key, and service asset"
fi

source_binary=$1
daemon_port=$2
websocket_path=$3
authorized_key_b64=$4
service_b64=$5

trap 'rm -f -- "$source_binary"' EXIT HUP INT TERM

[ -n "${HOME:-}" ] || fail service_install "HOME is not set"
command -v systemctl >/dev/null 2>&1 || fail no_systemd "systemctl is not installed"
command -v loginctl >/dev/null 2>&1 || fail no_systemd "loginctl is not installed"

remote_user=$(id -un) || fail no_systemd "cannot determine the remote user"
remote_uid=$(id -u) || fail no_systemd "cannot determine the remote user ID"
linger=$(loginctl show-user "$remote_user" --property=Linger --value 2>/dev/null) ||
	fail no_systemd "loginctl cannot query user $remote_user"
[ "$linger" = yes ] || fail no_user_lingering "user $remote_user does not linger after logout"

state_dir=$HOME/.local/state/mesh
binary_dir=$HOME/.local/bin
unit_dir=$HOME/.config/systemd/user
binary_path=$binary_dir/mesh
unit_path=$unit_dir/mesh.service
authorized_keys=$state_dir/authorized_keys
activation_pending=$state_dir/activation.pending

umask 077
mkdir -p "$state_dir" "$binary_dir" "$unit_dir"
chmod 0700 "$state_dir" "$binary_dir" "$unit_dir"

if [ -f "$activation_pending" ]; then
	activation_required=1
	changed=1
else
	activation_required=0
	changed=0
fi
mark_activation_pending() {
	if [ "$activation_required" -eq 0 ]; then
		: >"$activation_pending" || fail service_install "cannot mark the service activation pending"
		activation_required=1
	fi
}
if [ ! -f "$binary_path" ] || ! cmp -s "$source_binary" "$binary_path"; then
	binary_tmp=$binary_dir/.mesh.$$
	install -m 0755 "$source_binary" "$binary_tmp" || fail service_install "cannot install $binary_path"
	mark_activation_pending
	mv -f "$binary_tmp" "$binary_path" || fail service_install "cannot publish $binary_path"
	changed=1
fi

authorized_key=$(printf '%s' "$authorized_key_b64" | base64 -d 2>/dev/null) ||
	fail service_install "cannot decode the adopter public key"
auth_tmp=$state_dir/.authorized_keys.$$
if [ -f "$authorized_keys" ]; then
	awk '1' "$authorized_keys" >"$auth_tmp" || fail service_install "cannot read $authorized_keys"
else
	: >"$auth_tmp"
fi
if ! grep -Fqx -- "$authorized_key" "$auth_tmp"; then
	printf '%s\n' "$authorized_key" >>"$auth_tmp"
fi
chmod 0600 "$auth_tmp"
if [ ! -f "$authorized_keys" ] || ! cmp -s "$auth_tmp" "$authorized_keys"; then
	mark_activation_pending
	mv -f "$auth_tmp" "$authorized_keys"
	changed=1
else
	rm -f "$auth_tmp"
fi

unit_tmp=$unit_dir/.mesh.service.$$
printf '%s' "$service_b64" | base64 -d >"$unit_tmp" 2>/dev/null ||
	fail service_install "cannot decode the systemd service asset"
grep -Fqx "ExecStart=%h/.local/bin/mesh daemon --tailnet-port=$daemon_port --websocket-path=$websocket_path" "$unit_tmp" ||
	fail service_install "systemd service does not match the requested daemon endpoint"
grep -Fqx 'KillMode=process' "$unit_tmp" ||
	fail service_install "systemd service would stop detached session workers"
chmod 0644 "$unit_tmp"
if [ ! -f "$unit_path" ] || ! cmp -s "$unit_tmp" "$unit_path"; then
	mark_activation_pending
	mv -f "$unit_tmp" "$unit_path"
	changed=1
else
	rm -f "$unit_tmp"
fi

if [ -d "/run/user/$remote_uid" ]; then
	export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$remote_uid}
	export DBUS_SESSION_BUS_ADDRESS=${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}
fi

if [ "$activation_required" -eq 1 ]; then
	systemctl --user daemon-reload >/dev/null 2>&1 || fail service_install "systemctl --user daemon-reload failed"
fi
if systemctl --user is-enabled --quiet mesh.service >/dev/null 2>&1; then
	was_enabled=1
else
	was_enabled=0
fi
if systemctl --user is-active --quiet mesh.service >/dev/null 2>&1; then
	was_active=1
else
	was_active=0
fi
systemctl --user enable mesh.service >/dev/null 2>&1 || fail service_install "systemctl --user enable mesh.service failed"
if [ "$was_enabled" -eq 0 ] || [ "$was_active" -eq 0 ]; then
	changed=1
fi
if [ "$activation_required" -eq 1 ]; then
	systemctl --user restart mesh.service >/dev/null 2>&1 || fail service_install "systemctl --user restart mesh.service failed"
else
	systemctl --user start mesh.service >/dev/null 2>&1 || fail service_install "systemctl --user start mesh.service failed"
fi
systemctl --user is-active --quiet mesh.service || fail service_install "mesh.service is not active"
rm -f "$activation_pending" || fail service_install "cannot clear the service activation marker"

if [ "$changed" -eq 1 ]; then
	printf 'MESH_INSTALL_RESULT=configured\n'
else
	printf 'MESH_INSTALL_RESULT=unchanged\n'
fi
