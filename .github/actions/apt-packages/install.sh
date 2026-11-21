#!/usr/bin/env bash
# Install Ubuntu packages from a cached copy of the apt lists and downloaded
# .deb files when one is present, with no network access at all, and fetch
# from the archive only when the cache is missing or does not cover the
# request. Usage: install.sh CACHE_DIR [APT_GET_INSTALL_ARGUMENT...]
set -euo pipefail

dir=$1
shift
lists="$dir/lists"
archives="$dir/archives"
output=${GITHUB_OUTPUT:-/dev/null}
# apt reads and writes only the cache directory, never the system lists.
apt_dirs=(-o "Dir::State::Lists=$lists" -o "Dir::Cache::Archives=$archives")
# --no-download makes apt refuse to fetch anything; the dead proxy makes any
# request that slips through fail fast instead of reaching the archive.
offline=(--no-download -o Acquire::http::Proxy=http://127.0.0.1:9 -o Acquire::https::Proxy=http://127.0.0.1:9 -o Acquire::Retries=0)

as_root() {
  if [ "$(id -u)" = 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

install_offline() {
  [ -d "$lists" ] && [ -d "$archives" ] || return 1
  echo "apt: installing from the cache in $dir with no network access"
  as_root apt-get install -y "${offline[@]}" "${apt_dirs[@]}" "$@"
}

if install_offline "$@"; then
  echo "apt: installed from the cache, no network used"
  echo "source=cache" >> "$output"
  exit 0
fi

echo "apt: the cache is missing or does not cover this request; using the archive"
as_root rm -rf "$dir"
as_root mkdir -p "$lists/partial" "$archives/partial"
as_root apt-get update "${apt_dirs[@]}"
as_root apt-get install -y -o APT::Keep-Downloaded-Packages=true "${apt_dirs[@]}" "$@"
# actions/cache runs as the runner user and must read every file it saves.
as_root chown -R "$(id -u):$(id -g)" "$dir"
echo "source=network" >> "$output"
