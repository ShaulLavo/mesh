#!/bin/sh
# Install a checksum-verified Mesh release and its per-user daemon service.
set -eu

repository_url=https://github.com/shaul/mesh

fail() {
	printf 'mesh installer: %s\n' "$1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
		return
	fi
	fail "sha256sum or shasum is required"
}

verify_checksum() {
	file=$1
	want=$2
	label=$3
	got=$(sha256_file "$file")
	[ "$got" = "$want" ] || fail "$label checksum mismatch: got $got, want $want"
}

download() {
	url=$1
	destination=$2
	title=$3
	if [ "${MESH_INSTALL_YES:-0}" = 1 ]; then
		curl -fL --retry 3 --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$destination" "$url"
	else
		gum spin --title "$title" -- \
			curl -fL --retry 3 --proto '=https' --proto-redir '=https' --tlsv1.2 -o "$destination" "$url"
	fi
}

detect_target() {
	case $(uname -s) in
		Linux) target_os=linux ;;
		Darwin) target_os=darwin ;;
		*) fail "only Linux and macOS are supported" ;;
	esac
	case $(uname -m) in
		x86_64 | amd64) target_arch=amd64 ;;
		aarch64 | arm64) target_arch=arm64 ;;
		*) fail "unsupported architecture $(uname -m)" ;;
	esac
	if [ "$target_os/$target_arch" = darwin/amd64 ]; then
		fail "macOS releases require Apple silicon (arm64)"
	fi
}

resolve_version() {
	if [ -n "${MESH_VERSION:-}" ]; then
		version=$MESH_VERSION
	else
		latest_url=$(curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o /dev/null \
			-w '%{url_effective}' "$repository_url/releases/latest") ||
			fail "cannot resolve the latest Mesh release"
		version=${latest_url##*/}
	fi
	case $version in
		*[!A-Za-z0-9._-]* | '') fail "invalid release version $version" ;;
	esac
}

copy_or_download_release() {
	if [ -n "${MESH_RELEASE_DIR:-}" ]; then
		cp "$MESH_RELEASE_DIR/checksums.txt" "$checksums"
		cp "$MESH_RELEASE_DIR/$archive_name" "$archive"
		return
	fi
	release_url=$repository_url/releases/download/$version
	download "$release_url/checksums.txt" "$checksums" "Downloading Mesh checksums"
	download "$release_url/$archive_name" "$archive" "Downloading $archive_name"
}

fetch_service_assets() {
	asset_name=$1
	asset_destination=$2
	manifest_destination=$3
	if [ -n "${MESH_SERVICE_ASSET_DIR:-}" ]; then
		cp "$MESH_SERVICE_ASSET_DIR/$asset_name" "$asset_destination"
		cp "$MESH_SERVICE_ASSET_DIR/checksums.txt" "$manifest_destination"
		return
	fi
	asset_root=https://raw.githubusercontent.com/shaul/mesh/$version/scripts/install/assets
	download "$asset_root/checksums.txt" "$manifest_destination" "Downloading service checksums"
	download "$asset_root/$asset_name" "$asset_destination" "Downloading $asset_name"
}

render_service() {
	asset=$1
	destination=$2
	if [ "$target_os" = linux ]; then
		sed \
			-e "s|@MESH_BINARY@|$binary_path|g" \
			-e 's|@MESH_PORT@|7337|g' \
			-e 's|@MESH_SSH_PORT@|2222|g' \
			-e 's|@MESH_WEBSOCKET_PATH@|/mesh|g' \
			"$asset" >"$destination"
	else
		sed \
			-e "s|@MESH_BINARY@|$binary_path|g" \
			-e 's|@MESH_PORT@|7337|g' \
			-e 's|@MESH_SSH_PORT@|2222|g' \
			-e 's|@MESH_WEBSOCKET_PATH@|/mesh|g' \
			-e 's|@MESH_STDOUT@|${HOME}/.local/state/mesh/daemon.log|g' \
			-e 's|@MESH_STDERR@|${HOME}/.local/state/mesh/daemon.err.log|g' \
			"$asset" >"$destination"
	fi
	if grep -q '@MESH_' "$destination"; then
		fail "service asset contains an unresolved template token"
	fi
}

mark_activation_pending() {
	if [ "$manage_service" -eq 1 ] && [ "$activation_required" -eq 0 ]; then
		: >"$activation_pending" || fail "cannot mark the service activation pending"
		activation_required=1
	fi
}

clear_activation_pending() {
	rm -f "$activation_pending" || fail "cannot clear the service activation marker"
}

install_binary() {
	if [ ! -d "$binary_dir" ]; then
		mkdir -p "$binary_dir"
		if [ -z "${MESH_BIN_DIR:-}" ]; then
			chmod 0700 "$binary_dir"
		fi
	fi
	if [ -f "$binary_path" ] && cmp -s "$extracted_binary" "$binary_path"; then
		binary_changed=0
		return
	fi
	mark_activation_pending
	binary_tmp=$binary_dir/.mesh.$$
	install -m 0755 "$extracted_binary" "$binary_tmp"
	mv -f "$binary_tmp" "$binary_path"
	binary_changed=1
}

install_systemd_service() {
	need systemctl
	need loginctl
	user=$(id -un)
	linger=$(loginctl show-user "$user" --property=Linger --value 2>/dev/null || true)
	if [ "$linger" != yes ]; then
		loginctl enable-linger "$user" >/dev/null 2>&1 ||
			fail "cannot enable user lingering; run: loginctl enable-linger $user"
	fi
	unit_dir=$HOME/.config/systemd/user
	unit_path=$unit_dir/mesh.service
	mkdir -p "$unit_dir"
	chmod 0700 "$unit_dir"
	if [ ! -f "$unit_path" ] || ! cmp -s "$rendered_service" "$unit_path"; then
		mark_activation_pending
		unit_tmp=$unit_dir/.mesh.service.$$
		install -m 0644 "$rendered_service" "$unit_tmp"
		mv -f "$unit_tmp" "$unit_path"
	fi
	if [ "$activation_required" -eq 1 ]; then
		systemctl --user daemon-reload >/dev/null
	fi
	systemctl --user enable mesh.service >/dev/null
	if systemctl --user is-active --quiet mesh.service; then
		if [ "$activation_required" -eq 1 ]; then
			systemctl --user restart mesh.service >/dev/null
		fi
	else
		systemctl --user start mesh.service >/dev/null
	fi
	systemctl --user is-active --quiet mesh.service || fail "mesh.service did not start"
	clear_activation_pending
}

install_launchd_service() {
	need launchctl
	uid=$(id -u)
	agent_dir=$HOME/Library/LaunchAgents
	plist_path=$agent_dir/dev.shaulavo.mesh.plist
	mkdir -p "$HOME/.local/state/mesh" "$agent_dir"
	chmod 0700 "$HOME/.local/state/mesh" "$agent_dir"
	if [ ! -f "$plist_path" ] || ! cmp -s "$rendered_service" "$plist_path"; then
		mark_activation_pending
		plist_tmp=$agent_dir/.dev.shaulavo.mesh.plist.$$
		install -m 0644 "$rendered_service" "$plist_tmp"
		mv -f "$plist_tmp" "$plist_path"
	fi
	gui_domain=gui/$uid
	user_domain=user/$uid
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
		fail "launchd has no user domain for UID $uid"
	fi
	if [ "$other_loaded" -eq 1 ]; then
		mark_activation_pending
		launchctl bootout "$other_service" >/dev/null
	fi
	if [ "$loaded" -eq 1 ] && [ "$activation_required" -eq 1 ]; then
		launchctl bootout "$service" >/dev/null
		loaded=0
	fi
	if [ "$loaded" -eq 0 ]; then
		launchctl bootstrap "$domain" "$plist_path" >/dev/null
	fi
	launchctl print "$service" >/dev/null 2>&1 || fail "$service did not start"
	clear_activation_pending
}

[ -n "${HOME:-}" ] || fail "HOME is not set"
need awk
need cmp
need curl
need grep
need install
need sed
need tar
if [ "${MESH_INSTALL_YES:-0}" != 1 ]; then
	need gum
	case $(gum --version 2>/dev/null) in
		*v2.* | *" 2."*) ;;
		*) fail "Gum v2 is required (https://github.com/charmbracelet/gum)" ;;
	esac
	if ! gum confirm "Install Mesh and start its user daemon?" </dev/tty; then
		fail "cancelled"
	fi
fi

detect_target
resolve_version
binary_dir=${MESH_BIN_DIR:-$HOME/.local/bin}
case $binary_dir in
	/*) ;;
	*) fail "binary directory must be an absolute path" ;;
esac
case $binary_dir in
	*[!A-Za-z0-9_./-]*) fail "binary directory contains a character unsupported by service files" ;;
esac
binary_path=$binary_dir/mesh
archive_name=mesh_${target_os}_${target_arch}.tar.gz
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/mesh-install.XXXXXX")
cleanup() {
	rm -rf -- "$work_dir"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
archive=$work_dir/$archive_name
checksums=$work_dir/checksums.txt
copy_or_download_release

checksum_count=$(awk -v file="$archive_name" '$2 == file { count++ } END { print count + 0 }' "$checksums")
[ "$checksum_count" -eq 1 ] || fail "checksums.txt does not name exactly one $archive_name"
archive_checksum=$(awk -v file="$archive_name" '$2 == file { print $1 }' "$checksums")
[ "${#archive_checksum}" -eq 64 ] || fail "checksums.txt contains an invalid checksum for $archive_name"
case $archive_checksum in
	*[!0-9a-fA-F]* | '')
		fail "checksums.txt contains an invalid checksum for $archive_name"
		;;
esac
verify_checksum "$archive" "$archive_checksum" "$archive_name"

archive_entries=$(tar -tzf "$archive") || fail "cannot read $archive_name"
[ "$archive_entries" = mesh ] || fail "$archive_name must contain exactly one file named mesh"
tar -xzf "$archive" -C "$work_dir"
extracted_binary=$work_dir/mesh
[ -f "$extracted_binary" ] && [ ! -L "$extracted_binary" ] || fail "$archive_name does not contain a regular mesh binary"

if [ "${MESH_SKIP_SERVICE:-0}" != 1 ]; then
	if [ "$target_os" = linux ]; then
		service_asset_name=mesh.service
	else
		service_asset_name=dev.shaulavo.mesh.plist
	fi
	service_asset=$work_dir/$service_asset_name
	service_checksums=$work_dir/service-checksums.txt
	fetch_service_assets "$service_asset_name" "$service_asset" "$service_checksums"
	service_checksum_count=$(awk -v file="$service_asset_name" '$2 == file { count++ } END { print count + 0 }' "$service_checksums")
	[ "$service_checksum_count" -eq 1 ] || fail "service checksums do not name exactly one $service_asset_name"
	service_asset_checksum=$(awk -v file="$service_asset_name" '$2 == file { print $1 }' "$service_checksums")
	[ "${#service_asset_checksum}" -eq 64 ] || fail "service checksums contain an invalid checksum for $service_asset_name"
	case $service_asset_checksum in
		*[!0-9a-fA-F]* | '') fail "service checksums contain an invalid checksum for $service_asset_name" ;;
	esac
	verify_checksum "$service_asset" "$service_asset_checksum" "$service_asset_name"
	rendered_service=$work_dir/rendered-service
	render_service "$service_asset" "$rendered_service"
fi

activation_required=0
manage_service=0
if [ "${MESH_SKIP_SERVICE:-0}" != 1 ]; then
	manage_service=1
	state_dir=$HOME/.local/state/mesh
	activation_pending=$state_dir/activation.pending
	mkdir -p "$state_dir"
	chmod 0700 "$state_dir"
	if [ -f "$activation_pending" ]; then
		activation_required=1
	fi
else
	activation_pending=
fi

install_binary
if [ "${MESH_SKIP_SERVICE:-0}" != 1 ]; then
	if [ "$target_os" = linux ]; then
		install_systemd_service
	else
		install_launchd_service
	fi
fi

if [ "$binary_changed" -eq 1 ] || [ "$activation_required" -eq 1 ]; then
	result=installed
else
	result=unchanged
fi
printf 'Mesh %s (%s/%s) %s at %s\n' "$version" "$target_os" "$target_arch" "$result" "$binary_path"
