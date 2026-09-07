#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo 'usage: reserve-release.sh SOURCE-SHA [EXPLICIT-VERSION]' >&2
  exit 2
fi
[[ ${GITHUB_REPOSITORY:-} == ShaulLavo/mesh ]] || {
  echo "release reservation: unexpected repository ${GITHUB_REPOSITORY:-<unset>}" >&2
  exit 1
}

requested_source=$1
[[ $requested_source =~ ^[0-9a-f]{40}$ ]] || {
  echo "release reservation: source must be an exact lowercase commit SHA" >&2
  exit 1
}
source_sha=$(git rev-parse "$requested_source^{commit}")
[[ $source_sha == "$requested_source" ]] || {
  echo "release reservation: source $requested_source did not resolve exactly" >&2
  exit 1
}
explicit_version=${2:-}
git fetch --force origin main 'refs/tags/*:refs/tags/*'
git merge-base --is-ancestor "$source_sha" origin/main || {
  echo "release reservation: $source_sha is not on origin/main" >&2
  exit 1
}

changed=$(git diff-tree --no-commit-id --name-only -r "$source_sha")
if [[ -n $changed ]] && ! grep -Ev '^Casks/mesh\.rb$' <<<"$changed" >/dev/null; then
  printf 'action=skip\nsource=%s\nreason=cask-only\n' "$source_sha"
  exit 0
fi

latest_tag=$(gh api "repos/$GITHUB_REPOSITORY/releases/latest" --jq .tag_name)
temporary=$(mktemp -d "${RUNNER_TEMP:-/tmp}/mesh-release-reserve.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
if gh release download "$latest_tag" --pattern mesh-release.json --dir "$temporary" >/dev/null 2>&1; then
  published_source=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$temporary/mesh-release.json")
else
  published_source=$(git rev-list -n 1 "$latest_tag")
fi

plan=$(scripts/plan-release.sh "$source_sha" "$published_source" "$latest_tag")
action=$(sed -n 's/^action=//p' <<<"$plan")
if [[ $action == skip ]]; then
  if [[ $source_sha == "$published_source" ]]; then
    [[ -z $explicit_version || $explicit_version == "$latest_tag" ]] || {
      echo "release reservation: published source $source_sha is $latest_tag, not requested $explicit_version" >&2
      exit 1
    }
    printf 'action=reconcile\nsource=%s\nversion=%s\nprevious_tag=%s\nprevious_source=%s\n' \
      "$source_sha" "$latest_tag" "$latest_tag" "$published_source"
    exit 0
  fi
  printf '%s\nprevious_tag=%s\nprevious_source=%s\n' "$plan" "$latest_tag" "$published_source"
  exit 0
fi
version=$(sed -n 's/^version=//p' <<<"$plan")
if [[ -n $explicit_version ]]; then
  [[ $explicit_version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "release reservation: explicit version $explicit_version is invalid" >&2
    exit 1
  }
  explicit_parts=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
  [[ $latest_tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "release reservation: latest version $latest_tag is invalid" >&2
    exit 1
  }
  latest_parts=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
  greater=false
  for index in 0 1 2; do
    if (( 10#${explicit_parts[$index]} > 10#${latest_parts[$index]} )); then
      greater=true
      break
    fi
    if (( 10#${explicit_parts[$index]} < 10#${latest_parts[$index]} )); then
      break
    fi
  done
  [[ $greater == true ]] || {
    echo "release reservation: explicit version $explicit_version does not advance $latest_tag" >&2
    exit 1
  }
  version=$explicit_version
fi

gate_count=$(gh api \
  -H 'Accept: application/vnd.github+json' \
  "repos/$GITHUB_REPOSITORY/commits/$source_sha/check-runs" \
  --jq '[.check_runs[] | select(.name == "Complete CI gate" and .status == "completed" and .conclusion == "success" and .app.slug == "github-actions")] | length')
[[ $gate_count =~ ^[0-9]+$ && $gate_count -gt 0 ]] || {
  echo "release reservation: $source_sha has no successful Complete CI gate check" >&2
  exit 1
}

if [[ -z $explicit_version ]]; then
  gh api --paginate "repos/$GITHUB_REPOSITORY/releases?per_page=100" \
    --jq '.[] | select(.draft == true) | [.tag_name, .target_commitish] | @tsv' >"$temporary/drafts.tsv"
  git for-each-ref --format='%(refname:short)%09%(*objectname)%09%(objectname)' refs/tags >"$temporary/tags.tsv"
  version=$(python3 - "$source_sha" "$latest_tag" "$temporary" <<'PY'
import pathlib
import re
import sys

source, latest, directory = sys.argv[1:]
stable = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")

def parts(tag):
    match = stable.fullmatch(tag)
    return tuple(map(int, match.groups())) if match else None

baseline = parts(latest)
if baseline is None:
    raise SystemExit("release reservation: latest version is not stable semver")
reservations = {}
for line in (pathlib.Path(directory) / "drafts.tsv").read_text().splitlines():
    tag, target = line.split("\t", 1)
    if parts(tag) is not None:
        reservations[parts(tag)] = target
for line in (pathlib.Path(directory) / "tags.tsv").read_text().splitlines():
    tag, peeled, direct = line.split("\t")
    if parts(tag) is not None:
        reservations[parts(tag)] = peeled or direct
owned = [tag for tag, target in reservations.items() if tag > baseline and target == source]
candidate = min(owned) if owned else (baseline[0], baseline[1], baseline[2] + 1)
while candidate in reservations and reservations[candidate] != source:
    candidate = (candidate[0], candidate[1], candidate[2] + 1)
print("v%d.%d.%d" % candidate)
PY
)
fi

tag_exists=false
if git show-ref --verify --quiet "refs/tags/$version"; then
  tag_exists=true
  reserved_source=$(git rev-list -n 1 "$version")
  [[ $reserved_source == "$source_sha" ]] || {
    echo "release reservation: $version already names $reserved_source, not $source_sha" >&2
    exit 1
  }
fi

release_exists=false
if gh release view "$version" >/dev/null 2>&1; then
  release_exists=true
  if [[ $tag_exists == false ]]; then
    draft_target=$(gh release view "$version" --json targetCommitish --jq .targetCommitish)
    [[ $draft_target == "$source_sha" ]] || {
      echo "release reservation: existing $version draft targets $draft_target, not $source_sha" >&2
      exit 1
    }
  fi
fi

if [[ $tag_exists == false ]]; then
  git config user.name 'github-actions[bot]'
  git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
  git tag -a "$version" "$source_sha" -m "Mesh release reservation $version for $source_sha"
  git push origin "refs/tags/$version"
fi

if [[ $release_exists == false ]]; then
  gh release create "$version" --draft --verify-tag --target "$source_sha" --title "Mesh $version" --notes "Automated release reserved for source $source_sha."
fi
baseline_assets=$(gh release view "$version" --json assets --jq \
  '[.assets[].name | select(test("^baseline_(linux_amd64|linux_arm64|darwin_arm64)_[0-9a-f]{64}\\.bin$"))] | sort | join(",")')
printf 'action=publish\nsource=%s\nversion=%s\nprevious_tag=%s\nprevious_source=%s\nbaseline_assets=%s\n' \
  "$source_sha" "$version" "$latest_tag" "$published_source" "$baseline_assets"
