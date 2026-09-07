#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 6 ]]; then
  echo 'usage: prepare-release-baselines.sh CANDIDATE-VERSION PREVIOUS-TAG OS/ARCH FROZEN-ASSETS OUTPUT-DIR OUTPUT-LIST' >&2
  exit 2
fi
candidate_version=$1
previous_tag=$2
platform=$3
frozen_assets=$4
output_dir=$5
output_list=$6
[[ ${GITHUB_REPOSITORY:-} == ShaulLavo/mesh ]] || {
  echo "release baselines: unexpected repository ${GITHUB_REPOSITORY:-<unset>}" >&2
  exit 1
}
case $platform in
  linux/amd64) platform_key=linux_amd64; archive=mesh_linux_amd64.tar.gz ;;
  linux/arm64) platform_key=linux_arm64; archive=mesh_linux_arm64.tar.gz ;;
  darwin/arm64) platform_key=darwin_arm64; archive=mesh_darwin_arm64.tar.gz ;;
  *) echo "release baselines: unsupported platform $platform" >&2; exit 1 ;;
esac

mkdir -p "$output_dir"
: >"$output_list"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

download_release() {
  local tag=$1
  local required=$2
  local label=${tag//[^A-Za-z0-9_.-]/_}
  local directory=$output_dir/release-$label
  mkdir -p "$directory"
  if ! gh release download "$tag" --pattern "$archive" --dir "$directory"; then
    if [[ $required == true ]]; then
      echo "release baselines: retained release $tag has no $archive" >&2
      return 1
    fi
    echo "::notice::Release $tag has no $archive for $platform; builds with that exact digest remain outside the supported update window." >&2
    return 0
  fi
  tar -xzf "$directory/$archive" -C "$directory" mesh
  [[ -f $directory/mesh && ! -L $directory/mesh ]] || {
    echo "release baselines: $tag archive did not contain a regular mesh binary" >&2
    return 1
  }
  chmod 0755 "$directory/mesh"
  printf '%s\n' "$directory/mesh" >>"$output_list"
}

# The immediately previous public release is mandatory. The next seven most
# recent stable releases form a bounded skew window when their platform archive
# exists. Missing older archives are reported and excluded from the exact
# digest allowlist rather than receiving fabricated compatibility claims.
download_release "$previous_tag" true
considered=1
while IFS= read -r tag; do
  [[ $considered -lt 8 ]] || break
  [[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || continue
  [[ $tag != "$candidate_version" && $tag != "$previous_tag" ]] || continue
  download_release "$tag" false
  considered=$((considered + 1))
done < <(gh api --paginate "repos/$GITHUB_REPOSITORY/releases?per_page=100" \
  --jq '.[] | select(.draft == false and .prerelease == false) | .tag_name')

IFS=',' read -r -a assets <<<"$frozen_assets"
for name in "${assets[@]}"; do
  [[ -n $name ]] || continue
  [[ $name =~ ^baseline_${platform_key}_([0-9a-f]{64})\.bin$ ]] || continue
  expected=${BASH_REMATCH[1]}
  path=$output_dir/$name
  gh release download "$candidate_version" --pattern "$name" --dir "$output_dir"
  actual=$(sha256_file "$path")
  [[ $actual == "$expected" ]] || {
    echo "release baselines: $name hashes to $actual, not its frozen name $expected" >&2
    exit 1
  }
  chmod 0755 "$path"
  printf '%s\n' "$path" >>"$output_list"
done

sort -u -o "$output_list" "$output_list"
