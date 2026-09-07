#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo 'usage: finalize-release.sh VERSION SOURCE-SHA PREVIOUS-TAG CASK-FILE' >&2
  exit 2
fi
version=$1
source_sha=$(git rev-parse "$2^{commit}")
previous_tag=$3
cask_file=$4
[[ ${GITHUB_REPOSITORY:-} == ShaulLavo/mesh ]] || {
  echo "release finalization: unexpected repository ${GITHUB_REPOSITORY:-<unset>}" >&2
  exit 1
}

latest_tag=$(gh api "repos/$GITHUB_REPOSITORY/releases/latest" --jq .tag_name)
if [[ $latest_tag != "$version" ]]; then
  [[ $latest_tag == "$previous_tag" ]] || {
    latest_source=$(git rev-list -n 1 "$latest_tag")
    if git merge-base --is-ancestor "$source_sha" "$latest_source"; then
      echo "release finalization: $source_sha was superseded by $latest_source; leaving $version as a draft"
      exit 0
    fi
    echo "release finalization: latest release changed from $previous_tag to $latest_tag" >&2
    exit 1
  }
  previous_source=$(git rev-list -n 1 "$previous_tag")
  git merge-base --is-ancestor "$previous_source" "$source_sha" || {
    echo "release finalization: $source_sha no longer advances $previous_source" >&2
    exit 1
  }
  gh release edit "$version" --draft=false --latest
fi

worktree=$(mktemp -d "${RUNNER_TEMP:-/tmp}/mesh-cask.XXXXXX")
trap 'rm -rf -- "$worktree"' EXIT
gh auth setup-git
git clone --quiet --branch main "https://github.com/${GITHUB_REPOSITORY}.git" "$worktree"
mkdir -p "$worktree/Casks"
cp "$cask_file" "$worktree/Casks/mesh.rb"
if git -C "$worktree" diff --quiet -- Casks/mesh.rb; then
  exit 0
fi
git -C "$worktree" config user.name github-actions
git -C "$worktree" config user.email github-actions@users.noreply.github.com
git -C "$worktree" add Casks/mesh.rb
git -C "$worktree" commit -m "Brew cask update for mesh version $version"
git -C "$worktree" pull --rebase origin main
git -C "$worktree" push origin HEAD:main
