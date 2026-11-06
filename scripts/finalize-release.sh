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
if cmp -s "$cask_file" "$worktree/Casks/mesh.rb"; then
  exit 0
fi
git -C "$worktree" config user.name github-actions
git -C "$worktree" config user.email github-actions@users.noreply.github.com

advance_conflicting_branch() {
  local status=0
  git -C "$worktree" merge-tree --write-tree origin/main HEAD >"$worktree/merge-check" 2>&1 || status=$?
  [[ $status != 0 ]] || return 0
  if [[ $status != 1 ]]; then
    cat "$worktree/merge-check" >&2
    echo "release finalization: could not check $branch against main" >&2
    return 1
  fi
  git -C "$worktree" merge --no-commit --no-ff origin/main || {
    if [[ $(git -C "$worktree" diff --name-only --diff-filter=U) != Casks/mesh.rb ]]; then
      echo "release finalization: $branch conflicts outside the expected Cask" >&2
      return 1
    fi
  }
  cp "$cask_file" "$worktree/Casks/mesh.rb"
  git -C "$worktree" add Casks/mesh.rb
  git -C "$worktree" commit -m "Reconcile Cask for $version with main"
  git -C "$worktree" push origin "HEAD:refs/heads/$branch"
}

branch="release/cask-$version"
git check-ref-format --branch "$branch" >/dev/null
remote_branch=$(git -C "$worktree" ls-remote --heads origin "refs/heads/$branch")
if [[ -n $remote_branch ]]; then
  git -C "$worktree" fetch -q origin "refs/heads/$branch:refs/remotes/origin/$branch"
  changed=$(git -C "$worktree" diff --name-only "origin/main...origin/$branch")
  if [[ $changed != Casks/mesh.rb ]] || ! git -C "$worktree" show "origin/$branch:Casks/mesh.rb" | cmp -s - "$cask_file"; then
    echo "release finalization: existing $branch does not contain only the expected Cask" >&2
    exit 1
  fi
  git -C "$worktree" switch -q --detach "origin/$branch"
  advance_conflicting_branch
else
  git -C "$worktree" switch -q -c "$branch"
  cp "$cask_file" "$worktree/Casks/mesh.rb"
  git -C "$worktree" add Casks/mesh.rb
  git -C "$worktree" commit -m "Brew cask update for mesh version $version"
  git -C "$worktree" push origin "HEAD:refs/heads/$branch"
fi
validated_head=$(git -C "$worktree" rev-parse HEAD)
pr=$(gh pr list --repo "$GITHUB_REPOSITORY" --head "$branch" --base main --state open --json number --jq '.[0].number // empty')
if [[ -z $pr ]]; then
  gh pr create --repo "$GITHUB_REPOSITORY" --head "$branch" --base main \
    --title "Update Cask for $version" \
    --body "## Quick summary

Update the Homebrew Cask to the published Mesh $version release."
  pr=$(gh pr view "$branch" --repo "$GITHUB_REPOSITORY" --json number --jq .number)
fi
[[ -n $pr ]] || { echo "release finalization: no PR found for $branch" >&2; exit 1; }
identity=$(gh pr view "$pr" --repo "$GITHUB_REPOSITORY"   --json headRepository,baseRefName,headRefName,headRefOid,isCrossRepository   --jq '[.headRepository.nameWithOwner, .baseRefName, .headRefName, .headRefOid, .isCrossRepository] | @tsv')
expected=$(printf '%s\tmain\t%s\t%s\tfalse' "$GITHUB_REPOSITORY" "$branch" "$validated_head")
if [[ $identity != "$expected" ]]; then
  echo "release finalization: PR identity or head does not match validated $branch at $validated_head" >&2
  exit 1
fi
auto_merge=$(gh pr view "$pr" --repo "$GITHUB_REPOSITORY" --json autoMergeRequest --jq '.autoMergeRequest != null')
if [[ $auto_merge == false ]]; then
  gh pr merge "$pr" --repo "$GITHUB_REPOSITORY" --auto --squash --match-head-commit "$validated_head"
elif [[ $auto_merge != true ]]; then
  echo "release finalization: unexpected auto-merge state for PR $pr: $auto_merge" >&2
  exit 1
fi
gh workflow run ci.yml --repo "$GITHUB_REPOSITORY" --ref "$branch"
