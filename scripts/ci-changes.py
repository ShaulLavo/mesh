#!/usr/bin/env python3
"""Select the CI checks from the paths changed by a pull request."""
import fnmatch
import os
import re
import subprocess
import sys


CORE = frozenset({"code"})
BROWSER = CORE | {"browser"}
PACKAGING = CORE | {"packaging"}
UPDATE = PACKAGING | {"installed"}
ALL = UPDATE | BROWSER

# First match wins. Paths without a rule require the full PR suite.
PATH_CHECKS = (
    (("README.md", "AGENTS.md", "CLAUDE.md", "SECURITY.md", "LICENSE"), frozenset()),
    (("docs/reference/cli.md", "docs/demos/*"), CORE),
    (("docs/*.md", "docs/assets/*"), frozenset()),
    (("web/app-pill/*",), BROWSER),
    (("go.mod", "go.sum", "mise.toml", "db/*", ".github/*", "third_party/*"), ALL),
    # The browser proof builds its renderer from Go source and embeds pill assets.
    (("internal/apps/*", "internal/apppill/*", "internal/htmlrender/*", "internal/serve/*"), PACKAGING | BROWSER),
    (("cmd/*", "internal/cli/*", "internal/release*/*", "internal/update*/*", "internal/storage/*",
      "internal/worker/*", "internal/session/*", "internal/protocol/*", "internal/daemon/*",
      "internal/paths/*", "internal/identity/*", "internal/transport*/*", "internal/sshd/*",
      "internal/machinename/*", "internal/testenv/*", "internal/skills/*",
      "integration/*", "scripts/*", ".goreleaser.yaml", "Casks/*"), UPDATE),
    ((".golangci.yml", ".gates/*", "lefthook.yml"), CORE),
)


def checks_for(path):
    for patterns, checks in PATH_CHECKS:
        if any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns):
            return checks
    return ALL


def changed_paths(base, head):
    for commit in (base, head):
        if not re.fullmatch(r"[0-9a-f]{40}", commit):
            raise ValueError("PR base and head must be commit SHAs")
    # Include both ends of a rename and deleted paths. Git's NUL output keeps
    # unusual filenames literal and has no API limit on the number of paths.
    diff = subprocess.check_output(
        ["git", "diff", "--name-only", "-z", "--no-renames", f"{base}...{head}", "--"],
    )
    return list(map(os.fsdecode, diff.split(b"\0")[:-1]))


def classify(event, ref, base, head):
    if event == "push" and ref == "refs/heads/main":
        return BROWSER
    if event == "workflow_dispatch":
        return BROWSER | PACKAGING
    if event != "pull_request":
        raise ValueError(f"unexpected CI event: {event}")
    paths = changed_paths(base, head)
    if not paths:
        raise ValueError("PR path classification has no changed paths")
    return frozenset().union(*(checks_for(path) for path in paths))


if __name__ == "__main__":
    selected = classify(*sys.argv[1:])
    for check in sorted(ALL):
        print(f"{check}={'true' if check in selected else 'false'}")
