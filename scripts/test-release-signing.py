#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
WORKFLOW = (ROOT / ".github/workflows/release.yml").read_text()


def step_run(name):
    lines = WORKFLOW.splitlines()
    start = lines.index(f"      - name: {name}")
    block = []
    reading = False
    for line in lines[start + 1:]:
        if line.startswith("      - "):
            break
        if reading:
            block.append(line[10:])
        if line == "        run: |":
            reading = True
    if not block:
        raise AssertionError(f"missing executable workflow step {name}")
    return "\n".join(block)


VERIFY = step_run("Verify original bundle and candidate verifier")
PUBLISH = step_run("Upload and publish after ancestry recheck")


class ReleaseSigningTests(unittest.TestCase):
    def run_publication(self, fail):
        with tempfile.TemporaryDirectory(prefix="mesh-signing-") as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            (root / "scripts").mkdir()
            (root / "dist").mkdir()
            for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh_linux_amd64.tar.gz",
                         "mesh_linux_arm64.tar.gz", "mesh_darwin_arm64.tar.gz"):
                (root / "dist" / name).write_text("fixture")
            fake = """#!/usr/bin/env bash
set -euo pipefail
printf '%s %s\\n' "${0##*/}" "$*" >> "$CALLS"
if [[ ${0##*/} == go && $1 == build ]]; then cp "$0" "$3"; chmod +x "$3"; exit 0; fi
if [[ ${0##*/} == gh && $1 == attestation && ${FAIL:-} == official ]]; then exit 1; fi
if [[ $1 == refresh && ${FAIL:-} == trust ]]; then exit 1; fi
if [[ $1 == refresh && ${FAIL:-} == resume && ! -f $RESUMED ]]; then touch "$RESUMED"; exit 75; fi
if [[ $1 == wrap && ${FAIL:-} == wrap ]]; then exit 1; fi
if [[ $1 == verify && ${FAIL:-} == candidate ]]; then exit 1; fi
if [[ $1 == verify-metadata && ${FAIL:-} == metadata ]]; then exit 1; fi
"""
            for name in ("gh", "go"):
                path = root / "bin" / name
                path.write_text(fake)
                path.chmod(0o700)
            finalizer = root / "scripts/finalize-release.sh"
            finalizer.write_text('#!/usr/bin/env bash\nset -euo pipefail\ntouch "$PUBLISHED"\n')
            finalizer.chmod(0o700)
            env = dict(os.environ, PATH=f"{root / 'bin'}:{os.environ['PATH']}",
                       CALLS=str(root / "calls"), PUBLISHED=str(root / "published"),
                       BUNDLE=str(root / "bundle"), FAIL=fail,
                       RUNNER_TEMP=str(root), MESH_RELEASE_TRUST_CACHE=str(root / "trust"),
                       RESUMED=str(root / "resumed"),
                       VERSION="v0.1.1", SOURCE="a" * 40, PREVIOUS_TAG="v0.1.0")
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", VERIFY + "\n" + PUBLISH],
                                    cwd=root, env=env, capture_output=True, text=True, check=False)
            published = (root / "published").exists()
            calls = (root / "calls").read_text()
            return result, published, calls

    def test_success_verifies_every_subject_before_publishing(self):
        result, published, calls = self.run_publication("")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(published)
        self.assertEqual(calls.count("gh attestation verify"), 6)
        self.assertLess(calls.index("mesh-release-sign verify"),
                        calls.index("gh release upload"))

    def test_official_or_candidate_failure_keeps_draft_unpublished(self):
        for failure in ("official", "trust", "wrap", "candidate", "metadata"):
            with self.subTest(failure=failure):
                result, published, calls = self.run_publication(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(published)
                self.assertNotIn("gh release upload", calls)

    def test_budget_status_resumes_only_the_authenticated_refresh(self):
        result, published, calls = self.run_publication("resume")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(published)
        self.assertEqual(calls.count("mesh-release-sign refresh"), 2)

    def test_action_failure_cannot_be_ignored(self):
        signing = WORKFLOW[WORKFLOW.index("      - name: Attest final manifest and archives"):]
        signing = signing[:signing.index("      - name: Upload and publish after ancestry recheck")]
        self.assertIn("actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6", signing)
        self.assertNotIn("continue-on-error", signing)
        self.assertNotIn("always()", signing)

    def test_signing_permissions_are_confined_to_trusted_publication(self):
        for text in (WORKFLOW, (ROOT / ".github/workflows/ci.yml").read_text()):
            publish = text.index("\n  publish:\n")
            earlier = text[:publish]
            guarded = text[publish:]
            for grant in ("id-token: write", "attestations: write", "artifact-metadata: write"):
                self.assertNotIn(grant, earlier)
                self.assertIn(grant, guarded)
        caller = (ROOT / ".github/workflows/ci.yml").read_text()
        self.assertIn("if: github.event_name == 'push' && github.ref == 'refs/heads/main'", caller[caller.index("\n  publish:\n"):])


if __name__ == "__main__":
    unittest.main()
