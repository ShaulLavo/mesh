#!/usr/bin/env python3
"""Offline hosted publication scheduling, artifact intake and failure controls."""
import ast
import contextlib
import copy
import hashlib
import importlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import unittest
from unittest.mock import patch
import warnings
import zipfile

from fixtures import installed_publication, release_worker_completion as completion

ROOT = Path(__file__).resolve().parent.parent
WORKFLOW = (ROOT / ".github/workflows/installed-catchup.yml").read_text()
controls = importlib.import_module("test-publication-jobs")
corpus = importlib.import_module("test-bundled-release")
wiring = importlib.import_module("test-installed-workflow")
depth = importlib.import_module("test-installed-catchup").maximum_depth


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), ROOT / "scripts" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


workflow = load("publication-workflow")
jobs = load("publication-jobs")


def cli(arguments):
    output = io.StringIO()
    with patch.object(sys, "argv", ["publication-jobs.py"] + arguments), contextlib.redirect_stdout(output):
        jobs.main()
    return output.getvalue()


def cli_inventory(directory, platform, harness):
    return json.loads(cli(["inventory", str(directory), platform, harness]))


class PublicationWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.fixture = controls.PublicationJobsTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.configure(staged=True)
        self.root, self.directory = self.fixture.root, self.fixture.directory
        self.harness = controls.controls.SOURCE
        patcher = patch.object(workflow, "inventory", side_effect=cli_inventory)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_matrix_runs_exact_complete_cli_inventory_for_each_platform(self):
        actual = workflow.matrix(self.directory, self.harness)["include"]
        self.assertEqual(len({row["artifact"] for row in actual}), len(actual))
        for platform, runner in workflow.RUNNERS.items():
            selected = [row for row in actual if row["platform"] == platform]
            inventory = cli_inventory(self.directory, platform, self.harness)
            self.assertEqual([row["task"] for row in selected], [task["id"] for task in inventory["tasks"]])
            self.assertEqual({row["runner"] for row in selected}, {runner})
            self.assertEqual(sum(row["task"].startswith("published-chain-rb:") for row in selected), 1)
        self.assertEqual(actual, workflow.matrix(self.directory, self.harness)["include"])

    def test_empty_or_oversized_matrix_fails_without_truncation(self):
        for tasks in ([], [{"id": str(number)} for number in range(86)]):
            with patch.object(workflow, "inventory", return_value={"tasks": tasks}), self.assertRaisesRegex(ValueError, "matrix limit"):
                workflow.matrix(self.directory, self.harness)

    def test_actual_inventory_subprocess_is_offline_and_requires_exact_harness(self):
        directory = self.root / "first-updater"
        shutil.copytree(self.directory, directory)
        (directory / "mesh-retention.json").write_text("[]")
        scripts = corpus.legacy_corpus_source(self.root / "source") / "scripts"
        result = subprocess.run(["python3", str(scripts / "publication-jobs.py"), "inventory", str(directory),
            "linux/amd64", self.harness], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        actual = json.loads(result.stdout)
        self.assertTrue(actual["tasks"])
        self.assertIsNone(actual["routeContract"]["first"])
        self.assertEqual(actual["harness"], self.harness)
        result = subprocess.run(["python3", str(scripts / "publication-jobs.py"), "inventory", str(directory),
            "linux/amd64", "main"], capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("harness must be an exact source SHA", result.stderr)

    def emit_archive(self, platform, row, number):
        source = self.root / platform.replace("/", "-")
        output = self.root / (platform.replace("/", "-") + "-job-" + str(number))
        output.mkdir()
        inventory = cli_inventory(self.directory, platform, self.harness)
        task = completion.select_task(inventory, row["task"])
        for name in ("real-draft.json", "harness-source.json", "first-published-updater.json"):
            shutil.copyfile(source / name, output / name)
        if "case" in task:
            shutil.copyfile(source / task["case"], output / task["case"])
        else:
            relative = Path("groups") / task["scenario"] / str(task["group"]["index"])
            shutil.copytree(source / relative, output / relative)
        completion.record_task(output, inventory, task, self.fixture.context, self.fixture.fixture.selection)
        archive = self.root / (row["artifact"] + ".zip")
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as package:
            for path in output.rglob("*.json"):
                package.write(path, "publication-proof/" + str(path.relative_to(output)))
        return {"id": number + 1, "name": row["artifact"], "size_in_bytes": archive.stat().st_size,
                "digest": "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest(), "expired": False,
                "workflow_run": {"id": 42}, "created_at": "2026-11-19T10:00:00Z"}, archive

    def test_same_run_pinned_child_archives_feed_real_cli_aggregate_for_all_platforms(self):
        outputs = self.root / "complete"
        outputs.mkdir()
        for platform in workflow.RUNNERS:
            self.roundtrip(platform, outputs)
        completion.validate(outputs, self.fixture.context, self.harness, self.fixture.fixture.selection)

    def test_verified_signed_snapshot_feeds_complete_scheduler_and_aggregate(self):
        snapshot = self.snapshot_fixture()
        with patch.dict(os.environ, GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted", GITHUB_REF="refs/heads/main"), \
                patch.object(workflow.draft_intake.release_tool, "materialize", side_effect=self.materialize_control), \
                patch.object(workflow.draft_intake.release_worker_index, "validate_index"), \
                patch.object(workflow.draft_intake.release_documents, "selection_projection", return_value=[]):
            workflow.draft("v0.1.99", self.harness, self.root / "verified-documents", snapshot, 42)
        self.directory = self.root / "verified-documents"
        self.fixture.context = completion.release_context(self.directory)
        self.fixture.fixture.release = self.fixture.context
        for platform in controls.controls.index.PLATFORMS:
            shutil.rmtree(self.root / (platform["os"] + "-" + platform["arch"]))
            self.fixture.fixture.write_completion(platform)
            self.fixture.fixture.install_platform_routes(platform, self.fixture.context["routeContract"])
        outputs = self.root / "verified-complete"
        outputs.mkdir()
        for platform in workflow.RUNNERS:
            self.roundtrip(platform, outputs)
        completion.validate(outputs, self.fixture.context, self.harness, self.fixture.fixture.selection)

    def snapshot_fixture(self):
        snapshot = self.root / "signed-snapshot"
        self.control_logical = self.root / "signed-logical"
        shutil.copytree(self.directory, self.control_logical)
        snapshot.mkdir()
        manifest = (self.control_logical / "mesh-release.json").read_bytes()
        (self.control_logical / "mesh-update.json").write_text(json.dumps({"version": "v0.1.99", "offline": "no signature acceptance",
            "manifest": hashlib.sha256(manifest).hexdigest()}))
        for name in ("mesh-update-root.json", "mesh-workers.json"):
            (self.control_logical / name).write_text('{"offline":"complete intake control"}')
        for name in workflow.draft_intake.release_documents.PHYSICAL + ("mesh.rb",):
            source = self.control_logical / name
            (snapshot / name).write_bytes(source.read_bytes() if source.exists() else b"offline physical control")
        checksums = []
        for name in workflow.draft_intake.ARCHIVES.values():
            data = ("offline archive control " + name).encode()
            (snapshot / name).write_bytes(data)
            checksums.append(hashlib.sha256(data).hexdigest() + "  " + name + "\n")
        (snapshot / "checksums.txt").write_text("".join(checksums))
        files = sorted(path for path in snapshot.iterdir() if path.is_file())
        assets = [{"name": path.name, "id": number + 1, "state": "uploaded", "size": path.stat().st_size,
                   "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()} for number, path in enumerate(files)]
        release = {"id": 1, "draft": True, "tag_name": "v0.1.99", "target_commitish": self.harness,
            "repository": "ShaulLavo/mesh", "url": "https://api.github.com/repos/ShaulLavo/mesh/releases/1",
            "html_url": "https://github.com/ShaulLavo/mesh/releases/tag/v0.1.99", "intake_run": 42, "frozen_baselines": [], "assets": assets}
        (snapshot / "draft.json").write_text(json.dumps(release))
        return snapshot

    def materialize_control(self, _snapshot, _release, destination, _inventory):
        shutil.copytree(self.control_logical, destination)

    def roundtrip(self, platform, outputs):
        emitted = [self.emit_archive(platform, row, number) for number, row in enumerate(workflow.rows(self.directory, platform, self.harness))]
        artifacts = [item for item, _archive in emitted]
        locations = {item["id"]: archive.as_uri() for item, archive in emitted}
        destination = self.root / (platform.replace("/", "-") + "-children.noindex")
        with patch.object(workflow.installed_artifacts, "pages", return_value=artifacts) as pages, \
                patch.object(workflow.installed_storage, "archive_url", side_effect=lambda _repo, ident: locations[ident]):
            workflow.fetch("ShaulLavo/mesh", 42, self.directory, platform, self.harness, destination)
        pages.assert_called_once_with("repos/ShaulLavo/mesh/actions/runs/42/artifacts", "artifacts")
        output = outputs / platform.replace("/", "-")
        cli(["aggregate", str(self.directory), platform, self.harness, "--inputs", str(destination), "--output", str(output)])
        receipt = next(destination.rglob("publication-task.json"))
        receipt.unlink()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            cli(["aggregate", str(self.directory), platform, self.harness, "--inputs", str(destination), "--output", str(output) + "-missing"])

    def test_missing_duplicate_expired_and_extra_children_fail_before_transfer(self):
        rows = workflow.rows(self.directory, "linux/amd64", self.harness)
        artifacts = [{"id": number + 1, "name": row["artifact"], "size_in_bytes": 10, "digest": "sha256:" + "a" * 64,
                      "expired": False, "workflow_run": {"id": 42}, "created_at": "2026-11-19T10:00:00Z"}
                     for number, row in enumerate(rows)]
        expired = copy.deepcopy(artifacts)
        expired[0]["expired"] = True
        extra = artifacts + [artifacts[0] | {"name": artifacts[0]["name"] + "extra"}]
        tied = artifacts + [artifacts[0] | {"id": 99}]
        other_run = artifacts[:-1] + [artifacts[-1] | {"workflow_run": {"id": 43}}]
        for selected in (artifacts[:-1], tied, expired, extra, other_run):
            with patch.object(workflow.installed_artifacts, "pages", return_value=selected), \
                    patch.object(workflow, "fetch_child") as transfer, self.assertRaisesRegex(ValueError, "exact complete same-run"):
                workflow.fetch("repo", 42, self.directory, "linux/amd64", self.harness, self.root / "rejected")
            transfer.assert_not_called()
        self.assertFalse((self.root / "rejected").exists())

    def test_a_failed_jobs_rerun_transfers_each_childs_newest_upload(self):
        rows = workflow.rows(self.directory, "linux/amd64", self.harness)
        artifacts = [{"id": number + 1, "name": row["artifact"], "size_in_bytes": 10, "digest": "sha256:" + "a" * 64,
                      "expired": False, "workflow_run": {"id": 42}, "created_at": "2026-11-19T10:00:00Z"}
                     for number, row in enumerate(rows)]
        rerun = artifacts[0] | {"id": 99, "created_at": "2026-11-19T11:00:00Z"}
        with patch.object(workflow.installed_artifacts, "pages", return_value=artifacts + [rerun]), \
                patch.object(workflow, "fetch_child") as transfer:
            workflow.fetch("repo", 42, self.directory, "linux/amd64", self.harness, self.root / "rerun")
        transferred = {call.args[1]["id"] for call in transfer.call_args_list}
        self.assertEqual(transferred, {99} | {item["id"] for item in artifacts[1:]})

    def test_archive_size_digest_and_storage_failures_propagate_and_remove_transfer_scratch(self):
        row = workflow.rows(self.directory, "linux/amd64", self.harness)[0]
        item, archive = self.emit_archive("linux/amd64", row, 0)
        pin = workflow.artifact_pin(item)
        with patch.object(workflow.installed_storage, "archive_url", return_value=archive.as_uri()):
            with self.assertRaisesRegex(RuntimeError, "pinned size or SHA-256"):
                workflow.fetch_child("repo", pin | {"digest": "sha256:" + "f" * 64}, self.root / "changed")
            with self.assertRaisesRegex(RuntimeError, "exceeds its pinned size"):
                workflow.fetch_child("repo", pin | {"size": 1}, self.root / "overlong")
        with patch.object(workflow.shutil, "disk_usage", return_value=type("Disk", (), {"free": 1})()), \
                patch.object(workflow.installed_storage, "archive_url") as locate:
            with self.assertRaisesRegex(RuntimeError, "acquisition lacks"):
                workflow.fetch_child("repo", pin, self.root / "no-space")
        locate.assert_not_called()
        self.assertEqual(list(self.root.glob("publication-child-*")), [])
        with self.assertRaisesRegex(ValueError, "acquisition bound"):
            workflow.artifact_pin(item | {"size_in_bytes": workflow.MAX_ARCHIVE + 1})

    def test_zip_rejects_traversal_duplicates_symlinks_and_expansion_before_extraction(self):
        for label in ("../outside", "/absolute", "back\\slash", "duplicate", "symlink", "large"):
            self.reject_zip(label)

    def reject_zip(self, label):
        archive = self.root / "bad.zip"
        with zipfile.ZipFile(archive, "w") as package:
            info = zipfile.ZipInfo(label)
            info.external_attr = (stat.S_IFLNK | 0o777) << 16 if label == "symlink" else (stat.S_IFREG | 0o644) << 16
            package.writestr(info, b"outside")
            if label == "duplicate":
                self.write_duplicate(package)
        destination = self.root / "bad-extraction"
        with patch.object(workflow, "MAX_UNPACKED", 1 if label == "large" else workflow.MAX_UNPACKED):
            with self.assertRaises(ValueError):
                workflow.extract(archive, destination)
        self.assertFalse(destination.exists())

    def write_duplicate(self, package):
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", UserWarning)
            package.writestr("duplicate", b"second")

    def test_cleanup_requires_actual_native_root_teardown_after_launch(self):
        control, proof = self.root / "publication-control", self.root / "publication-proof"
        control.mkdir()
        proof.mkdir()
        (control / "fixture-launch.json").write_text('{"started":true}')
        native = self.root / "native-root"
        native.mkdir()
        inputs = self.root / "native-tools.noindex"
        inputs.mkdir()
        for value in (None, {"absent": False, "root": str(native)}, {"absent": True, "root": str(native)}):
            if value is not None:
                (proof / "scratch-cleanup.json").write_text(json.dumps(value))
            with self.assertRaisesRegex(RuntimeError, "ownership unconfirmed"):
                workflow.cleanup(self.root)
            self.assertTrue(inputs.exists())
        native.rmdir()
        workflow.cleanup(self.root)
        self.assertFalse(inputs.exists())
        self.assertTrue(json.loads((control / "tools-cleanup-decision.json").read_text())["inputsMayBeRemoved"])

    def test_prelaunch_failure_can_remove_owned_tools(self):
        for name in ("native-worker-input.noindex", "native-tools.noindex", "publication-trust", "publication-draft-input"):
            (self.root / name).mkdir()
        workflow.cleanup(self.root)
        self.assertFalse((self.root / "native-tools.noindex").exists())

    def test_workflow_dependencies_fail_closed_and_keep_finalizer_and_five_pins_unchanged(self):
        native = wiring.job(WORKFLOW, "publication-native")
        aggregate = wiring.job(WORKFLOW, "publication-aggregate")
        self.assertIn("needs: [publication-plan, publication-tools]", native)
        self.assertIn("matrix: ${{ fromJSON(needs.publication-plan.outputs.tasks) }}", native)
        self.assertIn("needs: [publication-plan, publication-native, publication-tools]", aggregate)
        for job in (wiring.job(WORKFLOW, "publication-plan"), native, aggregate):
            self.assertIn("github.ref == 'refs/heads/main'", job)
            self.assertNotIn("continue-on-error", job)
            self.assertNotRegex(job, r"^    if: always\(\)")
            self.assertIn("ref: ${{ inputs.harness_source || github.event.pull_request.head.sha || github.sha }}", job)
            self.assertIn("name: release-signed-draft", job)
            self.assertIn("run-id: ${{ inputs.release_run || github.run_id }}", job)
            self.assertIn("path: ${{ runner.temp }}/publication-draft-input/${{ inputs.release_version }}", job)
            self.assertNotIn(": write", job)
        self.assertIn("timeout-minutes: 30", native)
        self.assertIn("--worker-registry", native)
        self.assertIn("installed_artifacts.py native", native)
        self.assertNotIn("actions/setup-go", native)
        self.assertNotIn("go build", native)
        self.assertNotIn("go test", native)
        self.assertIn("Acquire the exact same-run reviewed H tool artifact", native)
        self.assertIn("--publication-task", native)
        self.assertIn("PUBLICATION_TASK: ${{ matrix.task }}", native)
        self.assertIn("MESH_PUBLICATION_DRAFT: ${{ runner.temp }}/publication-draft-input/${{ inputs.release_version }}", native)
        self.assertIn("MESH_PUBLICATION_RUN: ${{ inputs.release_run || github.run_id }}", native)
        self.assertIn("name: ${{ matrix.artifact }}", native)
        self.assertIn("name: installed-publication-${{ matrix.key }}", aggregate)
        self.assertIn("publication-trust", aggregate)
        pin = importlib.import_module("test-installed-gate-wiring").PIN
        for name in ("release", "ci"):
            caller = (ROOT / ".github/workflows" / (name + ".yml")).read_text()
            self.assertEqual(re.findall(r"uses: ShaulLavo/mesh/.github/workflows/installed-catchup.yml@([0-9a-f]{40})", caller), [pin])
            self.assertIn("harness_source: " + pin, caller)

    def test_actual_native_shell_forwards_one_complete_task_and_propagates_failure(self):
        command = wiring.step(wiring.job(WORKFLOW, "publication-native"), "Exercise the actual signed draft through native services")
        (self.root / "publication-control").mkdir()
        environment = os.environ | {"RUNNER_TEMP": str(self.root), "HAS_BASELINE": "false", "RELEASE_VERSION": "v0.1.99",
            "RELEASE_SOURCE": self.harness, "PUBLICATION_TASK": "published-chain-rb:0"}
        for status in (0, 19):
            wrappers = 'go() { :; }; python3() { printf "%s\\n" "$@"; return ' + str(status) + '; };\n'
            result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", wrappers + command], env=environment,
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, status, result.stderr)
            args = result.stdout.splitlines()
            self.assertEqual(args[args.index("--publication-task") + 1], "published-chain-rb:0")
            self.assertIn("--native-preflight", args)
            self.assertIn("--native-helper", args)
            self.assertNotIn("--clock-mode", args)
            self.assertNotIn("--installed-adoption", args)

    def test_aggregate_shell_stops_after_draft_or_fetch_or_aggregate_failure(self):
        command = wiring.step(wiring.job(WORKFLOW, "publication-aggregate"), "Acquire exact same-run child evidence and aggregate every task once")
        environment = os.environ | {"RUNNER_TEMP": str(self.root), "GITHUB_REPOSITORY": "ShaulLavo/mesh", "GITHUB_RUN_ID": "42",
            "RELEASE_VERSION": "v0.1.99", "RELEASE_SOURCE": self.harness, "RELEASE_RUN": "42", "PLATFORM": "darwin/arm64", "MESH_HARNESS_SOURCE": self.harness}
        for failure, count in (("draft", 1), ("fetch", 2), ("aggregate", 3), ("none", 3)):
            wrapper = 'python3() { printf "%s\\n" "$2"; [[ $2 != "' + failure + '" ]]; };\n'
            result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", wrapper + command], env=environment,
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(len(result.stdout.splitlines()), count)
            self.assertEqual(result.returncode, 0 if failure == "none" else 1)

    def test_changed_scripts_keep_three_block_depth_and_ci_executes_controls(self):
        for name in ("publication-workflow", "test-publication-workflow"):
            self.assertLessEqual(depth(ast.parse((ROOT / "scripts" / (name + ".py")).read_text()), 0), 3)
        ci = (ROOT / ".github/workflows/ci.yml").read_text()
        for name in ("test-publication-jobs", "test-publication-workflow"):
            self.assertIn("python3 scripts/" + name + ".py", ci)
            self.assertIn("scripts/" + name + ".py", WORKFLOW)


class DraftIntakeTests(unittest.TestCase):
    def setUp(self):
        self.fixture = importlib.import_module("test-prepare-draft-intake").DraftIntakeTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        self.intake, self.release = self.fixture.signed_snapshot()
        self.version = self.release["tag_name"]
        self.source = self.release["target_commitish"]

    def hosted(self):
        return patch.dict(os.environ, GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted", GITHUB_REF="refs/heads/main",
                          MESH_PUBLICATION_DRAFT=str(self.intake), MESH_PUBLICATION_RUN="42")

    def test_scheduler_requires_explicit_codec_and_keeps_logical_and_physical_separate(self):
        destination = self.root / "scheduler"
        environment = os.environ | {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/heads/main"}
        result = subprocess.run([sys.executable, str(ROOT / "scripts/publication-workflow.py"), "draft", self.version, self.source,
            str(destination), "--intake", str(self.intake), "--run", "42"], env=environment, capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"MESH_RELEASE_TOOL", result.stderr)
        with self.hosted(), patch.object(workflow.draft_intake.release_tool, "materialize", side_effect=self.materialize_control), \
                patch.object(workflow.draft_intake.release_worker_index, "validate_index"), \
                patch.object(workflow.draft_intake.release_documents, "selection_projection", return_value=[]):
            workflow.draft(self.version, self.source, destination, self.intake, 42)
        self.assertEqual({path.name for path in destination.iterdir()}, set(workflow.draft_intake.SIGNED))
        self.assertNotIn("draft.json", {path.name for path in destination.iterdir()})
        self.assertTrue((self.intake / "draft.json").exists())

    def materialize_control(self, _snapshot, _release, destination, _inventory):
        destination.mkdir()
        manifest = json.dumps({"version": self.version, "commit": self.source}).encode()
        for name in workflow.draft_intake.SIGNED:
            (destination / name).write_bytes(b"{}")
        (destination / "mesh-release.json").write_bytes(manifest)
        (destination / "mesh-update.json").write_text(json.dumps({"version": self.version, "manifest": hashlib.sha256(manifest).hexdigest()}))

    def test_native_stage_uses_the_exact_verified_directory_and_preserves_original_consumer(self):
        output = self.root / "output"
        output.mkdir()
        with self.hosted(), patch.object(installed_publication, "load_draft", return_value="original-consumer") as consume, \
                patch.object(installed_publication.draft_intake, "documents", return_value=self.release) as materialize, \
                patch.object(installed_publication.subprocess, "run", side_effect=AssertionError("no draft network access")):
            self.assertEqual(installed_publication.stage(self.root, output, "linux/amd64", self.version, self.source), "original-consumer")
        logical = self.root / "logical" / self.version
        materialize.assert_called_once_with(self.intake, self.version, self.source, logical, 42)
        consume.assert_called_once_with(self.root, output, logical, "linux/amd64", self.version, self.source, self.release["html_url"], self.intake)
        self.assertEqual(json.loads((output / "draft-intake.json").read_bytes()), self.release)

    def test_changed_source_run_missing_signature_and_changed_json_stop_before_native_consumption(self):
        for version, source, run in ((self.version, "b" * 40, "42"), (self.version, self.source, "43"),
                                     ("v0.1.999", self.source, "42")):
            self.reject_native(version, source, run)
        (self.intake / "mesh-update.sig.json").unlink()
        self.reject_native(self.version, self.source, "42")

    def reject_native(self, version, source, run):
        with self.hosted(), patch.dict(os.environ, MESH_PUBLICATION_RUN=run), \
                patch.object(installed_publication, "load_draft") as consume, self.assertRaises(ValueError):
            installed_publication.stage(self.root, self.root, "linux/amd64", version, source)
        consume.assert_not_called()

    def test_signed_identity_and_complete_namespace_refuse_wrong_source_extra_and_nonlocal_assets(self):
        changed = self.intake / "mesh-release.json"
        value = json.loads(changed.read_bytes()) | {"commit": "b" * 40}
        changed.write_text(json.dumps(value))
        with self.hosted(), self.assertRaises(ValueError):
            workflow.draft(self.version, self.source, self.root / "changed", self.intake, 42)
        for name in ("../escape.json", "draft.json", "back\\\\slash.json"):
            release = copy.deepcopy(self.release)
            release["assets"].append(dict(release["assets"][-1], name=name, id=999))
            with self.assertRaisesRegex(ValueError, "local name"):
                workflow.draft_intake.inventory(release, self.version, self.source, signed=True)

    def test_non_main_cannot_consume_even_a_valid_writer_snapshot(self):
        with self.hosted(), patch.dict(os.environ, GITHUB_REF="refs/pull/1/merge"), \
                patch.object(workflow.draft_intake, "verify") as verify, self.assertRaisesRegex(ValueError, "main-only"):
            workflow.draft(self.version, self.source, self.root / "rejected", self.intake, 42)
        verify.assert_not_called()


if __name__ == "__main__":
    unittest.main()
