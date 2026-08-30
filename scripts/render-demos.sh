#!/usr/bin/env bash
# Rebuild every terminal demo with the pinned VHS release.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
vhs_version=v0.11.0
demo_bin=$repo_root/dist/demo/mesh
cd "$repo_root"

mkdir -p "$(dirname -- "$demo_bin")"
tapes=("$repo_root"/docs/demos/*.tape)
if [[ ! -e ${tapes[0]} ]]; then
  echo 'render demos: no tape files found' >&2
  exit 1
fi

if command -v vhs >/dev/null 2>&1; then
  installed_version=$(vhs --version)
  if [[ $installed_version != *"$vhs_version"* ]]; then
    echo "render demos: VHS $vhs_version is required; found $installed_version" >&2
    exit 1
  fi
  go build -trimpath -o "$demo_bin" ./cmd/mesh
  for tape in "${tapes[@]}"; do
    PATH="$(dirname -- "$demo_bin"):$PATH" vhs "$tape"
  done
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "render demos: install VHS $vhs_version or Docker" >&2
  exit 1
fi

case $(uname -m) in
  x86_64 | amd64) container_arch=amd64 ;;
  aarch64 | arm64) container_arch=arm64 ;;
  *) echo "render demos: unsupported container architecture $(uname -m)" >&2; exit 1 ;;
esac
env CGO_ENABLED=0 GOOS=linux GOARCH="$container_arch" \
  go build -trimpath -o "$demo_bin" ./cmd/mesh
for tape in "${tapes[@]}"; do
  relative_tape=${tape#"$repo_root"/}
  docker run --rm --init \
    -v "$repo_root:/vhs" \
    -w /vhs \
    -e PATH=/vhs/dist/demo:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    "ghcr.io/charmbracelet/vhs:$vhs_version" "$relative_tape"
done
