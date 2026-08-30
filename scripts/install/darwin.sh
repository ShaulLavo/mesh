#!/bin/sh
set -eu

fail() {
	printf 'MESH_BOOTSTRAP_ERROR=%s\n%s\n' "$1" "$2" >&2
	exit 1
}

if [ "$#" -ne 5 ]; then
	fail service_install "macOS installer requires binary, port, WebSocket path, authorized key, and service asset"
fi

source_binary=$1
daemon_port=$2
websocket_path=$3
authorized_key_b64=$4
service_b64=$5

trap 'rm -f -- "$source_binary"' EXIT HUP INT TERM

[ -n "${HOME:-}" ] || fail service_install "HOME is not set"
command -v launchctl >/dev/null 2>&1 || fail service_install "launchctl is not installed"

remote_uid=$(id -u) || fail service_install "cannot determine the remote user ID"
state_dir=$HOME/.local/state/mesh
binary_dir=$HOME/.local/bin
agent_dir=$HOME/Library/LaunchAgents
binary_path=$binary_dir/mesh
plist_path=$agent_dir/dev.shaulavo.mesh.plist
authorized_keys=$state_dir/authorized_keys
activation_pending=$state_dir/activation.pending

umask 077
mkdir -p "$state_dir" "$binary_dir" "$agent_dir"
chmod 0700 "$state_dir" "$binary_dir" "$agent_dir"

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

if authorized_key=$(printf '%s' "$authorized_key_b64" | base64 -d 2>/dev/null); then
	:
elif authorized_key=$(printf '%s' "$authorized_key_b64" | base64 -D 2>/dev/null); then
	:
else
	fail service_install "cannot decode the adopter public key"
fi
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

plist_tmp=$agent_dir/.dev.shaulavo.mesh.plist.$$
if printf '%s' "$service_b64" | base64 -d >"$plist_tmp" 2>/dev/null; then
	:
elif printf '%s' "$service_b64" | base64 -D >"$plist_tmp" 2>/dev/null; then
	:
else
	fail service_install "cannot decode the launchd service asset"
fi
grep -Fq -- "--tailnet-port=$daemon_port --websocket-path=$websocket_path" "$plist_tmp" ||
	fail service_install "launchd service does not match the requested daemon endpoint"
grep -Fq '<key>AbandonProcessGroup</key>' "$plist_tmp" ||
	fail service_install "launchd service would stop detached session workers"
chmod 0644 "$plist_tmp"
if [ ! -f "$plist_path" ] || ! cmp -s "$plist_tmp" "$plist_path"; then
	mark_activation_pending
	mv -f "$plist_tmp" "$plist_path"
	changed=1
else
	rm -f "$plist_tmp"
fi

gui_domain=gui/$remote_uid
user_domain=user/$remote_uid
gui_service=$gui_domain/dev.shaulavo.mesh
user_service=$user_domain/dev.shaulavo.mesh
if launchctl print "$gui_service" >/dev/null 2>&1; then
	gui_loaded=1
else
	gui_loaded=0
fi
if launchctl print "$user_service" >/dev/null 2>&1; then
	user_loaded=1
else
	user_loaded=0
fi
if launchctl print "$gui_domain" >/dev/null 2>&1; then
	domain=$gui_domain
	service=$gui_service
	loaded=$gui_loaded
	other_service=$user_service
	other_loaded=$user_loaded
elif launchctl print "$user_domain" >/dev/null 2>&1; then
	domain=$user_domain
	service=$user_service
	loaded=$user_loaded
	other_service=$gui_service
	other_loaded=$gui_loaded
else
	fail service_install "launchd has no user domain for UID $remote_uid"
fi
if [ "$other_loaded" -eq 1 ]; then
	mark_activation_pending
	launchctl bootout "$other_service" >/dev/null 2>&1 || fail service_install "launchctl bootout failed for $other_service"
	changed=1
fi
if [ "$loaded" -eq 0 ]; then
	changed=1
fi
if [ "$activation_required" -eq 1 ] && [ "$loaded" -eq 1 ]; then
	launchctl bootout "$service" >/dev/null 2>&1 || fail service_install "launchctl bootout failed for $service"
	loaded=0
fi
if [ "$loaded" -eq 0 ]; then
	launchctl bootstrap "$domain" "$plist_path" >/dev/null 2>&1 || fail service_install "launchctl bootstrap failed for $plist_path"
fi
if [ "$activation_required" -eq 1 ]; then
	launchctl kickstart -k "$service" >/dev/null 2>&1 || fail service_install "launchctl kickstart failed for $service"
fi
launchctl print "$service" >/dev/null 2>&1 || fail service_install "$service is not loaded"
rm -f "$activation_pending" || fail service_install "cannot clear the service activation marker"

if [ "$changed" -eq 1 ]; then
	printf 'MESH_INSTALL_RESULT=configured\n'
else
	printf 'MESH_INSTALL_RESULT=unchanged\n'
fi
