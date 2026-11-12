#!/usr/bin/env python3
"""Exercise caller gates without a release, native manager, or downloaded artifact."""

import fnmatch
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
PIN = "d6bc77e2fbee115b4385b1bfa25ee44bebdb6cfe"
WORKFLOWS = ROOT / ".github/workflows"
CI = (WORKFLOWS / "ci.yml").read_text()
RELEASE = (WORKFLOWS / "release.yml").read_text()
PROOFS = (WORKFLOWS / "update-proofs.yml").read_text()
SPEC = importlib.util.spec_from_file_location("ci_gate", ROOT / "scripts/test-helper-recovery-gate.py")
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


def field(source, name):
    match = re.search(rf"^    {re.escape(name)}: (.+)$", source, re.M)
    if not match:
        raise AssertionError(f"missing job field {name}")
    return match.group(1)


def condition_status(condition, **values):
    for context in values:
        condition = condition.replace(context, f'"${{{context.replace(".", "_").replace("-", "_")}}}"')
    environment = dict(os.environ)
    environment.update({key.replace(".", "_").replace("-", "_"): value for key, value in values.items()})
    return GATE.status(f"[[ {condition} ]]", environment)


class InstalledGateWiringTests(unittest.TestCase):
    def assert_consumer(self, source, producer):
        self.assertEqual(field(source, "uses"), f"ShaulLavo/mesh/.github/workflows/installed-catchup.yml@{PIN}")
        for setting in (f"harness_source: {PIN}", "worker_artifact_prefix: update-native",
                        f"worker_artifact_run: ${{{{ fromJSON(needs.{producer}.outputs.worker_artifact_run) }}}}",
                        f"worker_artifact_source: ${{{{ needs.{producer}.outputs.worker_artifact_source }}}}"):
            self.assertIn(setting, source)
        self.assertIn("permissions:\n      contents: read\n      actions: read", source)
        self.assertNotIn("write", source)
        self.assertNotIn("secrets:", source)
        self.assertNotIn("continue-on-error", source)
        self.assertNotIn("always()", source)
        self.assertNotIn("43151893803", source)

    def test_ci_executes_the_real_aggregate_for_all_job_outcomes(self):
        recovery = (WORKFLOWS / "helper-recovery.yml").read_text()
        GATE.check_gate(CI, recovery)
        GATE.check_mutations(CI, recovery)

    def test_normal_proofs_run_once_after_successful_preparation(self):
        installed = GATE.job(CI, "installed-catchup")
        self.assert_consumer(installed, "update-proofs")
        self.assertEqual(field(installed, "needs"), "[changes, update-proofs]")
        self.assertEqual(field(installed, "if"), field(GATE.job(CI, "update-proofs"), "if"))
        self.assertEqual(CI.count(f"uses: ShaulLavo/mesh/.github/workflows/installed-catchup.yml@{PIN}"), 1)
        self.assertNotIn("installed-catchup.yml@", PROOFS)

    def test_update_paths_include_installed_and_release_dependencies(self):
        section = CI.split("id: update_paths\n", 1)[1].split("\n  app-pill:", 1)[0]
        patterns = re.findall(r"^\s+- '([^']+)'$", section, re.M)
        paths = ("internal/update/activate.go", "internal/storage/storage.go", "db/migrations/00017.sql",
                 "internal/worker/main.go", "internal/protocol/wire.go", "internal/daemon/main.go",
                 "scripts/prepare-rollback-context.py", "scripts/fixtures/installed_artifacts.py",
                 "scripts/test-installed-catchup.py", ".github/workflows/installed-catchup.yml")
        for path in paths:
            self.assertTrue(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns), path)

    def test_producer_outputs_bind_source_input_and_current_run_after_all_preparations(self):
        context = GATE.job(PROOFS, "context")
        self.assertEqual(field(context, "needs"), "prepare")
        prepare = GATE.job(PROOFS, "prepare")
        self.assertEqual(re.findall(r"^            key: (.+)$", prepare, re.M),
                         ["linux-amd64", "linux-arm64", "darwin-arm64"])
        self.assertIn("ref: ${{ inputs.source_sha }}", prepare)
        self.assertNotIn("continue-on-error", prepare)
        for output in ("worker_artifact_source", "worker_artifact_run"):
            self.assertIn(f"      {output}:\n", PROOFS.split("\njobs:", 1)[0])
        self.assertIn("value: ${{ jobs.context.outputs.source }}", PROOFS)
        self.assertIn("value: ${{ jobs.context.outputs.run }}", PROOFS)
        export = GATE.step(context, "Export the actual native preparation source and run")
        self.assertIn("SOURCE: ${{ inputs.source_sha }}", export)
        self.assertIn("RUN: ${{ github.run_id }}", export)
        self.assertNotIn("head_sha", PROOFS)
        source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        with tempfile.TemporaryDirectory(prefix="miw-") as directory:
            output = Path(directory) / "outputs"
            environment = dict(os.environ, SOURCE=source, RUN="123456", GITHUB_OUTPUT=str(output))
            self.assertEqual(GATE.status(GATE.run_script(export), environment), 0)
            self.assertEqual(output.read_text(), f"source={source}\nrun=123456\n")
            output.unlink()
            self.assertNotEqual(GATE.status(GATE.run_script(export), dict(environment, SOURCE="0" * 40)), 0)
            self.assertFalse(output.exists())

    def test_publication_uses_the_actual_signed_reserved_draft(self):
        installed = GATE.job(RELEASE, "publication-native")
        self.assert_consumer(installed, "prove-transition")
        self.assertEqual(field(installed, "needs"), "[reserve, prove-transition, publish]")
        self.assertEqual(field(installed, "if"), field(GATE.job(RELEASE, "publish"), "if"))
        for setting in ("publication_only: true", "release_version: ${{ needs.reserve.outputs.version }}",
                        "release_source: ${{ needs.reserve.outputs.source }}", "release_run: ${{ github.run_id }}"):
            self.assertIn(setting, installed)
        publish = GATE.job(RELEASE, "publish")
        self.assertLess(publish.index("name: Upload signed draft assets"), publish.index("name: update-publication-trust"))
        self.assertIn("path: ${{ runner.temp }}/mesh-release-trust", publish)
        self.assertIn("draft: true", GATE.job(RELEASE, "prove-transition"))

    def test_failed_or_skipped_installed_gate_cannot_finalize(self):
        finalize = GATE.job(RELEASE, "finalize")
        self.assertEqual(field(finalize, "needs"), "[reserve, publish, verify-readers, publication-native]")
        self.assertNotIn("always()", finalize)
        condition = field(finalize, "if")
        values = {"needs.reserve.outputs.action": "publish", "github.ref": "refs/heads/main",
                  "needs.verify-readers.result": "success", "needs.publication-native.result": "success"}
        self.assertEqual(condition_status(condition, **values), 0)
        for context in values:
            for rejected in ("failure", "skipped", "cancelled", ""):
                self.assertNotEqual(condition_status(condition, **dict(values, **{context: rejected})), 0,
                                    f"finalize accepted {context}={rejected}")


if __name__ == "__main__":
    unittest.main()
