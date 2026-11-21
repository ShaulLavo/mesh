#!/usr/bin/env python3
"""Exercise caller gates without a release, native manager, or downloaded artifact."""

import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
PIN = "512d714bb8e7a6a3a70f48dd1c4ecb134aabd6e7"
WORKFLOWS = ROOT / ".github/workflows"
CI = (WORKFLOWS / "ci.yml").read_text()
RELEASE = (WORKFLOWS / "release.yml").read_text()
PROOFS = (WORKFLOWS / "update-proofs.yml").read_text()
INSTALLED_PR = "github.event_name == 'pull_request' && needs.changes.outputs.installed == 'true'"
PROOF_STEP = "Exercise signed updates and preserved sessions with the native manager"
CLEANUP_STEP = "Verify native services and listeners stop after fixture errors"
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
    def test_manager_harness_runs_with_the_actual_workflow_command(self):
        workflow = (WORKFLOWS / "installed-catchup.yml").read_text()
        job = GATE.job(workflow, "ordered-manager-domains")
        command = next(line.strip() for line in job.splitlines() if 'installed_artifacts.py harness ' in line)
        source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        with tempfile.TemporaryDirectory(prefix="harness-cli-") as temporary:
            output = Path(temporary) / "ordered-managers"
            output.mkdir()
            environment = dict(os.environ, GITHUB_WORKSPACE=str(ROOT), RUNNER_TEMP=temporary, MESH_HARNESS_SOURCE=source,
                               PYTHONDONTWRITEBYTECODE="1")
            environment.pop("PYTHONPATH", None)
            result = subprocess.run(["bash", "-c", command], cwd=ROOT, env=environment, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            observed = json.loads((output / "harness-source.json").read_text())
            self.assertEqual((observed["actual"], observed["expected"]), (source, source))

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

    def test_pr_proofs_share_native_runner_and_setup_after_classification(self):
        native = GATE.job(CI, "native")
        self.assertNotIn("\n  installed:\n", CI)
        self.assertEqual(field(native, "if"), "needs.changes.outputs.code == 'true'")
        self.assertEqual(field(native, "needs"), "changes")
        self.assertEqual(native.count("uses: actions/checkout@"), 1)
        self.assertIn("persist-credentials: false", native)
        self.assertIn("\npermissions:\n  contents: read\n", CI)
        plain_setup = GATE.step(native, "Set up Go for native fixtures")
        self.assertIn("if: github.event_name != 'pull_request' || needs.changes.outputs.installed != 'true'", plain_setup)
        self.assertIn("uses: actions/setup-go@", plain_setup)
        installed_setup = GATE.step(native, "Set up Go for native and installed fixtures")
        self.assertIn(f"if: {INSTALLED_PR}\n", installed_setup)
        self.assertIn("uses: ./.github/actions/installed-go", installed_setup)
        proof = GATE.step(native, PROOF_STEP)
        self.assertIn(f"if: {INSTALLED_PR}\n", proof)
        self.assertIn("INSTALLED_CATCHUP_PLATFORM: ${{ matrix.platform }}", proof)
        self.assertIn("MESH_HARNESS_SOURCE: ${{ github.sha }}", proof)
        self.assertIn("scripts/prove-installed-catchup.py", proof)
        self.assertIn("--native-preflight --pull-request", proof)
        cleanup = GATE.step(native, CLEANUP_STEP)
        self.assertIn(f"if: {INSTALLED_PR} && matrix.platform != 'linux/arm64'\n", cleanup)
        self.assertIn("MESH_TEST_NATIVE_MANAGER: '1'", cleanup)
        ordered = ("id: installed_go", "id: bridge", "id: installed_proof", "id: installed_cleanup", "id: result")
        positions = [native.index(marker) for marker in ordered]
        self.assertEqual(positions, sorted(positions))
        for name, artifact in (("Upload native fixture evidence", "native-fixtures"), ("Upload installed fixture evidence", "installed-pr")):
            upload = GATE.step(native, name)
            self.assertIn(f"name: {artifact}-${{{{ matrix.runner }}}}", upload)
            self.assertIn("always()", upload)
        self.assertIn(f"always() && {INSTALLED_PR}", GATE.step(native, "Upload installed fixture evidence"))
        for setting in ("continue-on-error", "secrets.", "download-artifact", "actions: write", "contents: write"):
            self.assertNotIn(setting, native)
        self.assertNotIn("installed-catchup.yml@", CI)
        self.assertNotIn("installed-catchup.yml@", PROOFS)
        catchup = (WORKFLOWS / "installed-catchup.yml").read_text()
        self.assertNotIn("  pull_request:", catchup.split("\npermissions:", 1)[0])

    def test_actual_native_cleanup_shell_requires_both_selected_cases_to_pass(self):
        cleanup = GATE.step(GATE.job(CI, "native"), CLEANUP_STEP)
        stub = """python3() {
          test "$#" -eq 4 && test "$1" = -B && test "$2" = scripts/test-installed-catchup.py || return 73
          test "$3" = NativeServiceTest.test_exception_after_launch_stops_real_service || return 73
          test "$4" = InstalledCleanupTest.test_terminal_failure_still_stops_actual_listener_and_both_units || return 73
          printf 'Ran %s tests in 0.001s\\n\\n%s\\n' "$CASES" "$VERDICT"
          return "$EXIT"
        }
        """
        command = stub + GATE.run_script(cleanup)
        with tempfile.TemporaryDirectory(prefix="installed-pr-cleanup-") as temporary:
            (Path(temporary) / "installed-pr").mkdir()
            environment = dict(os.environ, RUNNER_TEMP=temporary, CASES="2", VERDICT="OK", EXIT="0")
            self.assertEqual(GATE.status(command, environment), 0)
            for failure in ({"CASES": "1"}, {"VERDICT": "OK (skipped=1)"}, {"VERDICT": ""}, {"EXIT": "1"}):
                self.assertNotEqual(GATE.status(command, environment | failure), 0, failure)

    def test_update_paths_include_installed_and_release_dependencies(self):
        classifier = (ROOT / "scripts/ci-changes.py").read_text()
        classify = {"__name__": "ci_changes"}
        exec(compile(classifier, "ci-changes.py", "exec"), classify)  # noqa: S102 - the repository classifier under test
        paths = ("internal/update/activate.go", "internal/storage/storage.go", "db/migrations/00017.sql",
                 "internal/worker/main.go", "internal/protocol/wire.go", "internal/daemon/main.go",
                 "scripts/prepare-rollback-context.py", "scripts/fixtures/installed_artifacts.py",
                 "scripts/test-installed-catchup.py", ".github/workflows/installed-catchup.yml")
        for path in paths:
            self.assertIn("installed", classify["checks_for"](path), path)

    def test_producer_outputs_bind_source_input_and_current_run_after_all_preparations(self):
        context = GATE.job(PROOFS, "context")
        self.assertEqual(field(context, "needs"), "prepare")
        prepare = GATE.job(PROOFS, "prepare")
        self.assertEqual(re.findall(r"^            key: (.+)$", prepare, re.M),
                         ["linux-amd64", "linux-arm64", "darwin-arm64"])
        self.assertIn("ref: ${{ inputs.source_sha }}", prepare)
        self.assert_only_cache_save_is_optional(prepare)
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

    def assert_only_cache_save_is_optional(self, prepare):
        sections = re.split(r"^      - ", prepare, flags=re.M)
        self.assertNotIn("continue-on-error", sections[0])
        for step in sections[1:]:
            if step.startswith("name: Cache this exact native preparation for parallel proof shards\n"):
                self.assertIn("uses: actions/cache/save@caa296126883cff596d87d8935842f9db880ef25", step)
                self.assertIn("continue-on-error: true", step)
                continue
            self.assertNotIn("continue-on-error", step)

    def test_native_build_upload_and_job_failures_cannot_be_optional(self):
        prepare = GATE.job(PROOFS, "prepare")
        for marker in ("    needs: policy", "      - name: Build the native candidate and startup-fault fixture once",
                       "      - uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1"):
            changed = prepare.replace(marker, marker + "\n        continue-on-error: true", 1)
            with self.assertRaises(AssertionError):
                self.assert_only_cache_save_is_optional(changed)
        changed = prepare.replace("uses: actions/cache/save@caa296126883cff596d87d8935842f9db880ef25",
                                  "uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a")
        with self.assertRaises(AssertionError):
            self.assert_only_cache_save_is_optional(changed)

    def test_publication_uses_the_actual_signed_reserved_draft(self):
        installed = GATE.job(RELEASE, "publication-native")
        self.assert_consumer(installed, "prove-transition")
        self.assertEqual(field(installed, "needs"), "[reserve, prove-transition, publish]")
        self.assertEqual(field(installed, "if"), field(GATE.job(RELEASE, "publish"), "if"))
        for setting in ("publication_only: true", "release_version: ${{ needs.reserve.outputs.version }}",
                        "release_source: ${{ needs.reserve.outputs.source }}", "release_run: ${{ fromJSON(github.run_id) }}"):
            self.assertIn(setting, installed)
        harness = (WORKFLOWS / "installed-catchup.yml").read_text()
        self.assertIn("      release_run:\n        type: number\n", harness)
        publish = GATE.job(RELEASE, "publish")
        self.assertLess(publish.index("name: Upload signed draft assets"), publish.index("name: update-publication-trust"))
        self.assertIn("path: ${{ runner.temp }}/mesh-release-trust", publish)
        self.assertIn("draft: true", GATE.job(RELEASE, "prove-transition"))

    def test_failed_or_skipped_installed_gate_cannot_finalize(self):
        finalize = GATE.job(RELEASE, "finalize")
        self.assertEqual(field(finalize, "needs"), "[reserve, publish, verify-readers, publication-native]")
        condition = next(line for line in finalize.splitlines() if line.startswith("    if:"))
        self.assertNotIn("always()", condition)
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
