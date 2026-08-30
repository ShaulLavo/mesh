#!/bin/sh
set -eu

fail() {
	printf 'MESH_BOOTSTRAP_ERROR=%s\n%s\n' "$1" "$2" >&2
	exit 1
}

if [ "$#" -ne 4 ]; then
	fail service_install "macOS installer requires binary, port, WebSocket path, and authorized key"
fi

source_binary=$1
daemon_port=$2
websocket_path=$3
authorized_key_b64=$4

trap 'rm -f -- "$source_binary"' EXIT HUP INT TERM

[ -n "${HOME:-}" ] || fail service_install "HOME is not set"
command -v launchctl >/dev/null 2>&1 || fail service_install "launchctl is not installed"
if printf '%s' "$HOME" | grep '[&<>]' >/dev/null 2>&1; then
	fail service_install "HOME contains a character that cannot be written to the launchd plist"
fi

remote_uid=$(id -u) || fail service_install "cannot determine the remote user ID"
state_dir=$HOME/.local/state/mesh
binary_dir=$HOME/.local/bin
agent_dir=$HOME/Library/LaunchAgents
binary_path=$binary_dir/mesh
plist_path=$agent_dir/dev.shaulavo.mesh.plist
authorized_keys=$state_dir/authorized_keys
stdout_path=$state_dir/daemon.log
stderr_path=$state_dir/daemon.err.log

umask 077
mkdir -p "$state_dir" "$binary_dir" "$agent_dir"
chmod 0700 "$state_dir" "$binary_dir" "$agent_dir"

changed=0
if [ ! -f "$binary_path" ] || ! cmp -s "$source_binary" "$binary_path"; then
	binary_tmp=$binary_dir/.mesh.$$
	install -m 0755 "$source_binary" "$binary_tmp" || fail service_install "cannot install $binary_path"
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
	mv -f "$auth_tmp" "$authorized_keys"
	changed=1
else
	rm -f "$auth_tmp"
fi

plist_tmp=$agent_dir/.dev.shaulavo.mesh.plist.$$
cat >"$plist_tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.shaulavo.mesh</string>
	<key>ProgramArguments</key>
	<array>
		<string>$binary_path</string>
		<string>daemon</string>
		<string>--tailnet-port=$daemon_port</string>
		<string>--websocket-path=$websocket_path</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$stdout_path</string>
	<key>StandardErrorPath</key>
	<string>$stderr_path</string>
</dict>
</plist>
EOF
chmod 0644 "$plist_tmp"
if [ ! -f "$plist_path" ] || ! cmp -s "$plist_tmp" "$plist_path"; then
	mv -f "$plist_tmp" "$plist_path"
	changed=1
else
	rm -f "$plist_tmp"
fi

if launchctl print "gui/$remote_uid" >/dev/null 2>&1; then
	domain=gui/$remote_uid
elif launchctl print "user/$remote_uid" >/dev/null 2>&1; then
	domain=user/$remote_uid
else
	fail service_install "launchd has no user domain for UID $remote_uid"
fi
service=$domain/dev.shaulavo.mesh

if launchctl print "$service" >/dev/null 2>&1; then
	loaded=1
else
	loaded=0
	changed=1
fi
if [ "$changed" -eq 1 ] && [ "$loaded" -eq 1 ]; then
	launchctl bootout "$service" >/dev/null 2>&1 || fail service_install "launchctl bootout failed for $service"
	loaded=0
fi
if [ "$loaded" -eq 0 ]; then
	launchctl bootstrap "$domain" "$plist_path" >/dev/null 2>&1 || fail service_install "launchctl bootstrap failed for $plist_path"
fi
if [ "$changed" -eq 1 ]; then
	launchctl kickstart -k "$service" >/dev/null 2>&1 || fail service_install "launchctl kickstart failed for $service"
fi
launchctl print "$service" >/dev/null 2>&1 || fail service_install "$service is not loaded"

if [ "$changed" -eq 1 ]; then
	printf 'MESH_INSTALL_RESULT=configured\n'
else
	printf 'MESH_INSTALL_RESULT=unchanged\n'
fi
