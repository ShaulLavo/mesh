#!/usr/bin/env python3
"""Exercise path classification and Git's view of the tested PR."""
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parent
CLASSIFIER = runpy.run_path(str(ROOT / "ci-changes.py"))
CHECKS_FOR = CLASSIFIER["checks_for"]
CLASSIFY = CLASSIFIER["classify"]


class ClassifyTests(unittest.TestCase):
    def test_selects_checks_for_the_actual_ci_inputs(self):
        cases = {
            "README.md": (),
            "docs/guides/usage.md": (),
            "docs/reference/cli.md": ("code",),
            "docs/demos/status.tape": ("code",),
            "web/app-pill/pnpm-lock.yaml": ("code", "browser"),
            "internal/apppill/assets/pill.js": ("code", "browser", "packaging"),
            "internal/apps/render.go": ("code", "browser", "packaging"),
            "internal/htmlrender/page.go": ("code", "browser", "packaging"),
            "scripts/install/assets/mesh.service": ("code", "packaging", "update_proofs"),
            "scripts/install/assets/dev.shaulavo.mesh.plist": ("code", "packaging", "update_proofs"),
            "scripts/fixtures/fixture_tls.py": ("code", "packaging", "update_proofs"),
            "internal/protocol/frame.go": ("code", "packaging", "update_proofs"),
            "Casks/mesh.rb": ("code", "packaging", "update_proofs"),
            "go.mod": CLASSIFIER["ALL"],
            ".github/actions/apt-packages/install.sh": CLASSIFIER["ALL"],
            ".github/workflows/installed-catchup.yml": CLASSIFIER["ALL"],
            "unfamiliar/input.bin": CLASSIFIER["ALL"],
        }
        for path, selected in cases.items():
            with self.subTest(path=path):
                self.assertEqual(CHECKS_FOR(path), set(selected))

    def test_main_checks_source_and_dispatch_checks_snapshot(self):
        self.assertEqual(CLASSIFY("push", "refs/heads/main", "", ""), {"code", "browser"})
        self.assertEqual(CLASSIFY("workflow_dispatch", "refs/heads/automation/cask-updates", "", ""),
                         {"code", "browser", "packaging"})

    def test_invalid_or_missing_pr_paths_fail(self):
        with self.assertRaisesRegex(ValueError, "must be commit SHAs"):
            CLASSIFY("pull_request", "", "", "")
        with patch.object(subprocess, "check_output", return_value=b""), self.assertRaisesRegex(ValueError, "no changed paths"):
            CLASSIFY("pull_request", "", "a" * 40, "b" * 40)
        with self.assertRaisesRegex(ValueError, "unexpected CI event"):
            CLASSIFY("pull_request_target", "", "", "")

    def test_real_pr_diff_includes_deletions_and_both_sides_of_a_rename(self):
        with tempfile.TemporaryDirectory(prefix="mesh-ci-changes-") as name:
            self.check_real_pr_diff(Path(name))

    def check_real_pr_diff(self, repo):
        env = dict(os.environ, GIT_AUTHOR_NAME="Mesh test", GIT_AUTHOR_EMAIL="mesh@example.invalid",
                   GIT_COMMITTER_NAME="Mesh test", GIT_COMMITTER_EMAIL="mesh@example.invalid")

        def git(*args):
            return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True).strip()

        git("init", "-q", "-b", "main")
        (repo / "docs").mkdir()
        (repo / "web/app-pill").mkdir(parents=True)
        source = repo / "web/app-pill/old\nname.txt"
        source.write_text("original content\n")
        (repo / "go.mod").write_text("module mesh.test\n")
        git("add", ".")
        git("commit", "-qm", "base")
        base = git("rev-parse", "HEAD")
        source.rename(repo / "docs/renamed.md")
        (repo / "go.mod").unlink()
        git("add", "-A")
        git("commit", "-qm", "head")
        head = git("rev-parse", "HEAD")
        result = subprocess.check_output(["python3", str(ROOT / "ci-changes.py"), "pull_request", "", base, head],
                                         cwd=repo, text=True)
        self.assertEqual(dict(line.split("=") for line in result.splitlines()),
                         {name: "true" for name in CLASSIFIER["ALL"]})
        paths = subprocess.check_output(["git", "diff", "--name-only", "-z", "--no-renames", base, head], cwd=repo)
        self.assertEqual(set(paths.split(b"\0")[:-1]), {b"docs/renamed.md", b"go.mod", b"web/app-pill/old\nname.txt"})


if __name__ == "__main__":
    unittest.main()
