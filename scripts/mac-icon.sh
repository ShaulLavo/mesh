#!/usr/bin/env bash
# Renders internal/macapp/assets/mesh.svg into the mesh.icns that Mesh.app embeds.
# Needs rsvg-convert (librsvg); rerun after editing the SVG and commit both files.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
assets="$root/internal/macapp/assets"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
args=()
for size in 16 32 64 128 256 512 1024; do
  rsvg-convert -w "$size" -h "$size" -o "$work/$size.png" "$assets/mesh.svg"
  args+=("$size=$work/$size.png")
done
(cd "$root" && go run ./internal/macapp/cmd/icns -out "$assets/mesh.icns" "${args[@]}")
echo "$assets/mesh.icns"
