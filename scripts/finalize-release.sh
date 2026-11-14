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
  python3 "$(dirname -- "${BASH_SOURCE[0]}")/verify-worker-publication.py" "$version" "$source_sha" "${MESH_INSTALLED_COMPLETION:?missing post-sign installed completion}"
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

branch=automation/cask-updates
remote_branch=$(git -C "$worktree" ls-remote --heads origin "refs/heads/$branch")
old=
if [[ -n $remote_branch ]]; then
  git -C "$worktree" fetch -q origin "refs/heads/$branch:refs/remotes/origin/$branch"
  old=$(git -C "$worktree" rev-parse "origin/$branch")
  changed=$(git -C "$worktree" diff --name-only "origin/main...$old")
  if [[ $changed != Casks/mesh.rb ]]; then
    echo "release finalization: existing $branch contains changes outside the Cask" >&2
    exit 1
  fi
fi
cp "$cask_file" "$worktree/Casks/mesh.rb"
git -C "$worktree" add Casks/mesh.rb
if [[ -n $old && $(git -C "$worktree" write-tree) == $(git -C "$worktree" rev-parse "$old^{tree}") ]] &&
  git -C "$worktree" merge-base --is-ancestor origin/main "$old"; then
  git -C "$worktree" reset -q --hard "$old"
else
  git -C "$worktree" commit -m "Brew cask update for mesh version $version"
  git -C "$worktree" push --force-with-lease="refs/heads/$branch:$old" origin "HEAD:refs/heads/$branch"
fi
validated_head=$(git -C "$worktree" rev-parse HEAD)
pr=$(gh pr list --repo "$GITHUB_REPOSITORY" --head "$branch" --base main --state open --json number --jq '.[0].number // empty')
title="Update Cask for $version"
body="## Quick summary

Update the Homebrew Cask to the published Mesh $version release. This standing PR receives newer releases while CI is pending."
if [[ -z $pr ]]; then
  pr=$(gh pr create --repo "$GITHUB_REPOSITORY" --head "$branch" --base main \
    --title "$title" --body "$body")
else
  gh pr edit "$pr" --repo "$GITHUB_REPOSITORY" --title "$title" --body "$body"
fi
[[ -n $pr ]] || { echo "release finalization: no PR found for $branch" >&2; exit 1; }
identity=$(gh pr view "$pr" --repo "$GITHUB_REPOSITORY"   --json headRepository,baseRefName,headRefName,headRefOid,isCrossRepository   --jq '[.headRepository.nameWithOwner, .baseRefName, .headRefName, .headRefOid, .isCrossRepository] | @tsv')
expected=$(printf '%s\tmain\t%s\t%s\tfalse' "$GITHUB_REPOSITORY" "$branch" "$validated_head")
if [[ $identity != "$expected" ]]; then
  echo "release finalization: PR identity or head does not match validated $branch at $validated_head" >&2
  exit 1
fi
draft=$(gh pr view "$pr" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft)
case $draft in
  false) ;;
  true)
    echo "release finalization: Cask PR $pr is a draft; mark it ready for review in GitHub in $GITHUB_REPOSITORY" >&2
    exit 1 ;;
  *)
    echo "release finalization: unexpected draft state for PR $pr: $draft" >&2
    exit 1 ;;
esac
run=$(gh run list --repo "$GITHUB_REPOSITORY" --workflow ci.yml --branch "$branch" --commit "$validated_head" \
  --event workflow_dispatch --limit 1 --json status,conclusion --jq '.[0] // empty')
if [[ -z $run ]] || jq -e '.status == "completed" and .conclusion != "success"' <<< "$run" >/dev/null; then
  gh api --method POST "repos/$GITHUB_REPOSITORY/statuses/$validated_head" \
    -f context="Complete CI gate" -f state=pending -f description="Waiting for dispatched CI"
  gh workflow run ci.yml --repo "$GITHUB_REPOSITORY" --ref "$branch"
fi
auto_merge=$(gh pr view "$pr" --repo "$GITHUB_REPOSITORY" --json autoMergeRequest --jq '.autoMergeRequest != null')
if [[ $auto_merge == false ]]; then
  gh pr merge "$pr" --repo "$GITHUB_REPOSITORY" --auto --squash --match-head-commit "$validated_head"
elif [[ $auto_merge != true ]]; then
  echo "release finalization: unexpected auto-merge state for PR $pr: $auto_merge" >&2
  exit 1
fi
echo "release finalization: Cask PR $pr waits for dispatched CI before auto-merge."
