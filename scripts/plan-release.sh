#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo 'usage: plan-release.sh SOURCE-SHA PUBLISHED-SOURCE-SHA PUBLISHED-VERSION' >&2
  exit 2
fi

source_sha=$1
published_source=$2
published_version=$3

git cat-file -e "$source_sha^{commit}" 2>/dev/null || {
  echo "release plan: source $source_sha is not a commit" >&2
  exit 1
}
git cat-file -e "$published_source^{commit}" 2>/dev/null || {
  echo "release plan: published source $published_source is not a commit" >&2
  exit 1
}
if [[ ! $published_version =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "release plan: published version $published_version is not stable semver" >&2
  exit 1
fi

if [[ $source_sha == "$published_source" ]] || git merge-base --is-ancestor "$source_sha" "$published_source"; then
  printf 'action=skip\nsource=%s\nversion=%s\n' "$source_sha" "$published_version"
  exit 0
fi
if ! git merge-base --is-ancestor "$published_source" "$source_sha"; then
  echo "release plan: source $source_sha diverges from published source $published_source" >&2
  exit 1
fi

major=${BASH_REMATCH[1]}
minor=${BASH_REMATCH[2]}
patch=${BASH_REMATCH[3]}
printf 'action=publish\nsource=%s\nversion=v%s.%s.%s\n' "$source_sha" "$major" "$minor" "$((patch + 1))"
