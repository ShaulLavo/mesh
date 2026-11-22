#!/usr/bin/env bash
set -euo pipefail

if [[ $# -gt 2 ]]; then
  echo 'usage: reserve-release.sh [SOURCE-SHA] [EXPLICIT-VERSION]' >&2
  exit 2
fi
[[ ${GITHUB_REPOSITORY:-} == ShaulLavo/mesh ]] || {
  echo "release reservation: unexpected repository ${GITHUB_REPOSITORY:-<unset>}" >&2
  exit 1
}

requested_source=${1:-}
explicit_version=${2:-}
workflow_sha=${WORKFLOW_SHA:-}
if [[ -n $workflow_sha ]]; then
  [[ $workflow_sha =~ ^[0-9a-f]{40}$ ]] || {
    echo "release reservation: WORKFLOW_SHA must be an exact lowercase commit SHA" >&2
    exit 1
  }
fi
git fetch --force origin main 'refs/tags/*:refs/tags/*'

temporary=$(mktemp -d "${RUNNER_TEMP:-/tmp}/mesh-release-reserve.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT

is_main_first_parent() {
  local commit=$1
  git rev-list --first-parent origin/main >"$temporary/first-parent.txt" || return 1
  grep -Fxq "$commit" "$temporary/first-parent.txt"
}

effective_gate_sha() {
  local current=$1 published_source=$2
  local parent commit_changes
  while [[ $current != "$published_source" ]]; do
    parent=$(git rev-parse "${current}^1" 2>/dev/null) || break
    commit_changes=$(git diff --name-only --no-renames "$parent..$current") || return 1
    if [[ -z $commit_changes ]] || classify_non_cask_changes "$commit_changes"; then
      printf '%s\n' "$current"
      return 0
    fi
    current=$parent
  done
  printf '%s\n' "$current"
}

classify_non_cask_changes() {
  local changed=$1
  if [[ -z $changed ]]; then
    return 0
  fi
  local classifier_status=0
  grep -Ev '^Casks/mesh\.rb$' <<<"$changed" >/dev/null || classifier_status=$?
  case "$classifier_status" in
    0) return 0 ;;
    1) return 1 ;;
    *)
      printf 'release reservation: changed-file classifier failed with status %s\n' "$classifier_status" >&2
      exit "$classifier_status"
      ;;
  esac
}

# Sets GATE_STATUS to "green" or "missing"; exits non-zero on API errors.
check_green_gate() {
  local source_sha=$1
  local suite_ids suite_json gate_count
  suite_ids=$(gh api --paginate \
    -H 'Accept: application/vnd.github+json' \
    "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs?head_sha=$source_sha&branch=main&event=push&per_page=100" \
    --jq ".workflow_runs[] | select(.head_sha == \"$source_sha\" and .head_branch == \"main\" and .event == \"push\" and .status == \"completed\" and .conclusion == \"success\") | .check_suite_id")
  if [[ -z $suite_ids ]]; then
    GATE_STATUS=missing
    return 0
  fi
  suite_json=$(python3 -c 'import json,sys; ids=[int(line) for line in sys.stdin.read().splitlines() if line.strip()]; print(json.dumps(ids))' <<<"$suite_ids")
  gate_count=$(gh api --paginate \
    -H 'Accept: application/vnd.github+json' \
    "repos/$GITHUB_REPOSITORY/commits/$source_sha/check-runs?check_name=Complete%20CI%20gate&per_page=100" \
    --jq '[.check_runs[] | select(.name == "Complete CI gate" and .status == "completed" and .conclusion == "success" and .app.slug == "github-actions" and (([.check_suite.id // -1] - '"$suite_json"') | length == 0))] | length' |
    awk '{sum += $1} END {print sum + 0}')
  [[ $gate_count =~ ^[0-9]+$ ]] || {
    echo "release reservation: invalid check-runs count for $source_sha: $gate_count" >&2
    exit 1
  }
  if (( gate_count > 0 )); then
    GATE_STATUS=green
  else
    GATE_STATUS=missing
  fi
}

maybe_reconcile_cask() {
  local latest_tag=$1 published_source=$2 explicit_version=$3
  if [[ -n $explicit_version && $explicit_version != "$latest_tag" ]]; then
    return 1
  fi
  rm -f -- "$temporary/mesh.rb"
  gh release download "$latest_tag" --pattern mesh.rb --dir "$temporary" >/dev/null || {
    echo "release reservation: failed to download mesh.rb for $latest_tag while checking Cask state" >&2
    exit 1
  }
  if ! git show "origin/main:Casks/mesh.rb" 2>/dev/null | cmp -s - "$temporary/mesh.rb"; then
    printf 'action=reconcile\nsource=%s\nversion=%s\nprevious_tag=%s\nprevious_source=%s\n' \
      "$published_source" "$latest_tag" "$latest_tag" "$published_source"
    return 0
  fi
  return 1
}

if [[ -n $requested_source ]]; then
  [[ $requested_source =~ ^[0-9a-f]{40}$ ]] || {
    echo "release reservation: source must be an exact lowercase commit SHA" >&2
    exit 1
  }
  source_sha=$(git rev-parse "$requested_source^{commit}")
  [[ $source_sha == "$requested_source" ]] || {
    echo "release reservation: source $requested_source did not resolve exactly" >&2
    exit 1
  }
  is_main_first_parent "$source_sha" || {
    echo "release reservation: $source_sha is not on origin/main first-parent history" >&2
    exit 1
  }
else
  # Auto-select targets the running workflow revision (github.sha on main) so
  # attestation provenance, installed harness pin, and built archives always
  # refer to the exact same commit.
  if [[ -n $workflow_sha ]]; then
    source_sha=$(git rev-parse "$workflow_sha^{commit}")
  else
    source_sha=$(git rev-parse HEAD)
  fi
  is_main_first_parent "$source_sha" || {
    echo "release reservation: workflow commit $source_sha is not on origin/main first-parent history" >&2
    exit 1
  }
fi

latest_tag=$(gh api "repos/$GITHUB_REPOSITORY/releases/latest" --jq .tag_name)
if gh release download "$latest_tag" --pattern mesh-release.json --dir "$temporary" >/dev/null 2>&1; then
  published_source=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "$temporary/mesh-release.json")
else
  published_source=$(git rev-list -n 1 "$latest_tag")
fi

plan=$(scripts/plan-release.sh "$source_sha" "$published_source" "$latest_tag")
action=$(sed -n 's/^action=//p' <<<"$plan")
if [[ $action == skip ]]; then
  if [[ -z $requested_source ]]; then
    if maybe_reconcile_cask "$latest_tag" "$published_source" "$explicit_version"; then
      exit 0
    fi
    printf 'action=skip\nsource=%s\nreason=already-published\nprevious_tag=%s\nprevious_source=%s\n' \
      "$source_sha" "$latest_tag" "$published_source"
    exit 0
  fi
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

changed=$(git diff --name-only --no-renames "$published_source..$source_sha")
if ! classify_non_cask_changes "$changed"; then
  if [[ -z $requested_source ]] && maybe_reconcile_cask "$latest_tag" "$published_source" "$explicit_version"; then
    exit 0
  fi
  printf 'action=skip\nsource=%s\nreason=cask-only\nprevious_tag=%s\nprevious_source=%s\n' \
    "$source_sha" "$latest_tag" "$published_source"
  exit 0
fi

# Publishing must match the running workflow commit so OIDC attestation
# provenance and the workflow's pinned installed harness stay bound to the
# exact source being built and finalized.
if [[ -n $workflow_sha && $source_sha != "$workflow_sha" ]]; then
  echo "release reservation: publish source $source_sha does not match workflow commit $workflow_sha" >&2
  exit 1
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

GATE_STATUS=missing
check_green_gate "$source_sha"
if [[ $GATE_STATUS != green ]]; then
  gate_sha=$(effective_gate_sha "$source_sha" "$published_source")
  if [[ $gate_sha != "$source_sha" && $gate_sha != "$published_source" ]]; then
    trailing_changes=$(git diff --name-only --no-renames "$gate_sha..$source_sha")
    if ! classify_non_cask_changes "$trailing_changes"; then
      check_green_gate "$gate_sha"
    fi
  fi
fi
if [[ $GATE_STATUS != green ]]; then
  if [[ -z $requested_source ]]; then
    if maybe_reconcile_cask "$latest_tag" "$published_source" "$explicit_version"; then
      exit 0
    fi
    printf 'action=skip\nsource=%s\nreason=no-green-commit\nprevious_tag=%s\nprevious_source=%s\n' \
      "$source_sha" "$latest_tag" "$published_source"
    exit 0
  fi
  echo "release reservation: $source_sha has no successful main CI Complete CI gate run" >&2
  exit 1
fi

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
  # gh prints the draft's URL on stdout, which the workflow appends to
  # GITHUB_OUTPUT; a bare URL there fails the step after the draft exists.
  gh release create "$version" --draft --verify-tag --target "$source_sha" --title "Mesh $version" --notes "Automated release reserved for source $source_sha." >&2
fi
baseline_assets=$(gh release view "$version" --json assets --jq \
  '[.assets[].name | select(test("^baseline_(linux_amd64|linux_arm64|darwin_arm64)_[0-9a-f]{64}\\.bin$"))] | sort | join(",")')
printf 'action=publish\nsource=%s\nversion=%s\nprevious_tag=%s\nprevious_source=%s\nbaseline_assets=%s\n' \
  "$source_sha" "$version" "$latest_tag" "$published_source" "$baseline_assets"
