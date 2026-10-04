#!/bin/sh
set -eu

fail() {
	printf 'MESH_BOOTSTRAP_ERROR=%s\n%s\n' "$1" "$2" >&2
	exit 1
}

if [ "$#" -ne 6 ]; then
	fail service_install "macOS installer requires binary, control port, SSH port, WebSocket path, authorized key, and service asset"
fi

source_binary=$1
daemon_port=$2
ssh_port=$3
websocket_path=$4
authorized_key_b64=$5
service_b64=$6

# An empty source binary means the adopter checked the digest and skipped the
# upload; there is nothing staged to clean up.
if [ -n "$source_binary" ]; then
	trap 'rm -f -- "$source_binary"' EXIT HUP INT TERM
fi

[ -n "${HOME:-}" ] || fail service_install "HOME is not set"
command -v launchctl >/dev/null 2>&1 || fail service_install "launchctl is not installed"

remote_uid=$(id -u) || fail service_install "cannot determine the remote user ID"
state_dir=$HOME/.local/state/mesh
binary_dir=$HOME/.local/bin
agent_dir=$HOME/Library/LaunchAgents
binary_path=$binary_dir/mesh
plist_path=$agent_dir/dev.shaulavo.mesh.plist
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
# A previous failure may have published a plist that launchd has not loaded yet.
reload_required=$activation_required
mark_activation_pending() {
	if [ "$activation_required" -eq 0 ]; then
		: >"$activation_pending" || fail service_install "cannot mark the service activation pending"
		activation_required=1
	fi
}
if [ -z "$source_binary" ]; then
	[ -x "$binary_path" ] ||
		fail service_install "the upload was skipped but $binary_path is missing"
elif [ ! -f "$binary_path" ] || ! cmp -s "$source_binary" "$binary_path"; then
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
approval_result=$(MESH_STATE_DIR="$state_dir" "$binary_path" device approve --allow-root --public-key --json -- "$authorized_key") ||
	fail service_install "cannot approve the adopter device key"
case "$approval_result" in
	'{"changed":true}') mark_activation_pending; changed=1 ;;
	'{"changed":false}') ;;
	*) fail service_install "device approval returned an invalid change result" ;;
esac

plist_tmp=$agent_dir/.dev.shaulavo.mesh.plist.$$
if printf '%s' "$service_b64" | base64 -d >"$plist_tmp" 2>/dev/null; then
	:
elif printf '%s' "$service_b64" | base64 -D >"$plist_tmp" 2>/dev/null; then
	:
else
	fail service_install "cannot decode the launchd service asset"
fi
grep -Fq -- "--tailnet-port=$daemon_port --ssh-port=$ssh_port --websocket-path=$websocket_path" "$plist_tmp" ||
	fail service_install "launchd service does not match the requested daemon endpoint"
grep -Fq '<key>AbandonProcessGroup</key>' "$plist_tmp" ||
	fail service_install "launchd service would stop detached session workers"
chmod 0644 "$plist_tmp"
if [ ! -f "$plist_path" ] || ! cmp -s "$plist_tmp" "$plist_path"; then
	reload_required=1
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
# Teardown visibility and transient bootstrap errors share a five-second backoff budget.
activation_attempts=20
retry_activation() {
	activation_attempts=$((activation_attempts - 1))
	[ "$activation_attempts" -gt 0 ] || fail service_install "$1"
	sleep 0.25
}
bootout_service() {
	if bootout_error=$(launchctl bootout "$1" 2>&1); then
		:
	else
		fail service_install "launchctl bootout failed for $1: $bootout_error"
	fi
	while launchctl print "$1" >/dev/null 2>&1; do
		retry_activation "launchctl bootout timed out for $1"
	done
}
if [ "$other_loaded" -eq 1 ]; then
	mark_activation_pending
	bootout_service "$other_service"
	changed=1
fi
if [ "$loaded" -eq 0 ]; then
	changed=1
fi
# Binary and key changes use kickstart to preserve the registered job and KeepAlive.
if [ "$reload_required" -eq 1 ] && [ "$loaded" -eq 1 ]; then
	bootout_service "$service"
	loaded=0
fi
if [ "$loaded" -eq 0 ]; then
	while ! bootstrap_error=$(launchctl bootstrap "$domain" "$plist_path" 2>&1); do
		retry_activation "launchctl bootstrap failed for $plist_path: $bootstrap_error"
	done
fi
if [ "$activation_required" -eq 1 ]; then
	if kickstart_error=$(launchctl kickstart -k "$service" 2>&1); then
		:
	else
		fail service_install "launchctl kickstart failed for $service: $kickstart_error"
	fi
fi
launchctl print "$service" >/dev/null 2>&1 || fail service_install "$service is not loaded"
rm -f "$activation_pending" || fail service_install "cannot clear the service activation marker"

if [ "$changed" -eq 1 ]; then
	printf 'MESH_INSTALL_RESULT=configured\n'
else
	printf 'MESH_INSTALL_RESULT=unchanged\n'
fi
