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
import urllib.error
import warnings
import zipfile

from fixtures import release_worker_completion as completion

ROOT = Path(__file__).resolve().parent.parent
WORKFLOW = (ROOT / ".github/workflows/installed-catchup.yml").read_text()
controls = importlib.import_module("test-publication-jobs")
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
        result = subprocess.run(["python3", str(ROOT / "scripts/publication-jobs.py"), "inventory", str(directory),
            "linux/amd64", self.harness], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["tasks"])
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            cli(["inventory", str(self.directory), "linux/amd64", "main"])

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
                "digest": "sha256:" + hashlib.sha256(archive.read_bytes()).hexdigest(), "expired": False}, archive

    def test_same_run_pinned_child_archives_feed_real_cli_aggregate_for_all_platforms(self):
        outputs = self.root / "complete"
        outputs.mkdir()
        for platform in workflow.RUNNERS:
            self.roundtrip(platform, outputs)
        completion.validate(outputs, self.fixture.context, self.harness, self.fixture.fixture.selection)

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
        artifacts = [{"id": number + 1, "name": row["artifact"], "size_in_bytes": 10,
                      "digest": "sha256:" + "a" * 64, "expired": False} for number, row in enumerate(rows)]
        expired = copy.deepcopy(artifacts)
        expired[0]["expired"] = True
        extra = artifacts + [artifacts[0] | {"name": artifacts[0]["name"] + "extra"}]
        for selected in (artifacts[:-1], artifacts + [artifacts[0]], expired, extra):
            with patch.object(workflow.installed_artifacts, "pages", return_value=selected), \
                    patch.object(workflow, "fetch_child") as transfer, self.assertRaisesRegex(ValueError, "exact complete same-run"):
                workflow.fetch("repo", 42, self.directory, "linux/amd64", self.harness, self.root / "rejected")
            transfer.assert_not_called()
        self.assertFalse((self.root / "rejected").exists())

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
        for name in ("native-worker-input.noindex", "native-tools.noindex", "publication-trust"):
            (self.root / name).mkdir()
        workflow.cleanup(self.root)
        self.assertFalse((self.root / "native-tools.noindex").exists())

    def test_workflow_dependencies_fail_closed_and_keep_finalizer_and_five_pins_unchanged(self):
        native = wiring.job(WORKFLOW, "publication-native")
        aggregate = wiring.job(WORKFLOW, "publication-aggregate")
        self.assertIn("needs: publication-plan", native)
        self.assertIn("matrix: ${{ fromJSON(needs.publication-plan.outputs.tasks) }}", native)
        self.assertIn("needs: [publication-plan, publication-native]", aggregate)
        for job in (wiring.job(WORKFLOW, "publication-plan"), native, aggregate):
            self.assertIn("github.ref == 'refs/heads/main'", job)
            self.assertNotIn("continue-on-error", job)
            self.assertNotRegex(job, r"^    if: always\(\)")
            self.assertIn("ref: ${{ inputs.harness_source || github.event.pull_request.head.sha || github.sha }}", job)
        self.assertIn("timeout-minutes: 30", native)
        self.assertIn("--worker-registry", native)
        self.assertIn("installed_artifacts.py native", native)
        self.assertLess(native.index("main-only publication caller and real native manager"), native.index("actions/setup-go"))
        self.assertIn("--publication-task", native)
        self.assertIn("PUBLICATION_TASK: ${{ matrix.task }}", native)
        self.assertIn("name: ${{ matrix.artifact }}", native)
        self.assertIn("name: installed-publication-${{ matrix.key }}", aggregate)
        self.assertNotIn("publication-trust", aggregate)
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
            "RELEASE_VERSION": "v0.1.99", "RELEASE_SOURCE": self.harness, "PLATFORM": "darwin/arm64", "MESH_HARNESS_SOURCE": self.harness}
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
        self.temporary = workflow.tempfile.TemporaryDirectory(prefix="publication-workflow-", dir=os.environ.get("TMPDIR", "/tmp"))
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.asset = {"id": 1, "name": "mesh-release.json", "size": 1}

    def draft_response(self, assets):
        value = {"isDraft": True, "tagName": "v0.1.99", "assets": assets}
        return type("Response", (), {"stdout": json.dumps(value)})()

    def hosted(self):
        return patch.dict(os.environ, GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted", GITHUB_REF="refs/heads/main")

    def test_complete_draft_json_is_acquired_without_archives_or_signing_inputs(self):
        manifest = json.dumps({"version": "v0.1.99", "commit": "a" * 40}).encode()
        assets = [self.asset | {"size": len(manifest)}, {"id": 2, "name": "mesh_linux_amd64.tar.gz", "size": 999}]
        with self.hosted(), patch.object(workflow.subprocess, "run", return_value=self.draft_response(assets)), \
                patch.object(workflow, "download_draft_asset", side_effect=lambda _item, target: target.write_bytes(manifest)) as fetch:
            workflow.draft("v0.1.99", "a" * 40, self.root / "draft")
        fetch.assert_called_once_with(assets[0], self.root / "draft/mesh-release.json")
        self.assertEqual({path.name for path in (self.root / "draft").iterdir()}, {"mesh-release.json"})

    def test_duplicate_nonlocal_and_oversized_draft_inventories_stop_before_transfer(self):
        for assets in ([self.asset, self.asset], [self.asset | {"name": "../escape.json"}], [self.asset | {"size": workflow.MAX_ARCHIVE + 1}]):
            self.reject_draft_inventory(assets)

    def reject_draft_inventory(self, assets):
        with self.hosted(), patch.object(workflow.subprocess, "run", return_value=self.draft_response(assets)), \
                patch.object(workflow, "download_draft_asset") as fetch, self.assertRaisesRegex(ValueError, "acquisition bound"):
            workflow.draft("v0.1.99", "a" * 40, self.root / "draft")
        fetch.assert_not_called()
        self.assertFalse((self.root / "draft").exists())

    def test_changed_draft_source_and_truncated_assets_fail(self):
        manifest = json.dumps({"version": "v0.1.99", "commit": "b" * 40}).encode()
        asset = self.asset | {"size": len(manifest)}
        with self.hosted(), patch.object(workflow.subprocess, "run", return_value=self.draft_response([asset])), \
                patch.object(workflow, "download_draft_asset", side_effect=lambda _item, target: target.write_bytes(manifest)), \
                self.assertRaisesRegex(ValueError, "exact release source"):
            workflow.draft("v0.1.99", "a" * 40, self.root / "changed")
        with patch.dict(os.environ, GH_TOKEN="offline-control"), patch.object(workflow.urllib.request, "build_opener") as builder:
            builder.return_value.open.return_value = io.BytesIO(b"")
            with self.assertRaisesRegex(ValueError, "pinned size"):
                workflow.download_draft_asset(self.asset, self.root / "truncated.json")

    def test_redirect_drops_authorization_and_stream_is_size_bounded(self):
        redirect = urllib.error.HTTPError("https://api.github.com/asset", 302, "Found", {"Location": "https://release-assets.githubusercontent.com/object"}, None)
        opener = unittest.mock.Mock()
        opener.open.side_effect = redirect
        self.addCleanup(redirect.close)
        with patch.dict(os.environ, GH_TOKEN="offline-control"), patch.object(workflow.urllib.request, "build_opener", return_value=opener), \
                patch.object(workflow.urllib.request, "urlopen", return_value=io.BytesIO(b"x")) as fetch:
            workflow.download_draft_asset(self.asset, self.root / "asset.json")
        fetch.assert_called_once_with("https://release-assets.githubusercontent.com/object", timeout=30)
        self.assertEqual((self.root / "asset.json").read_bytes(), b"x")
        with patch.dict(os.environ, GH_TOKEN="offline-control"), patch.object(workflow.urllib.request, "build_opener") as builder:
            builder.return_value.open.return_value = io.BytesIO(b"too much")
            with self.assertRaisesRegex(RuntimeError, "exceeds its pinned size"):
                workflow.download_draft_asset(self.asset, self.root / "oversized.json")

    def test_non_main_or_non_draft_inputs_stop_before_asset_acquisition(self):
        hosted = {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/heads/main"}
        response = type("Response", (), {"stdout": json.dumps({"isDraft": False, "tagName": "v0.1.99", "assets": [self.asset]})})()
        for changes in ({"GITHUB_REF": "refs/pull/1/merge"}, {"RUNNER_ENVIRONMENT": "self-hosted"}, {}):
            with patch.dict(os.environ, hosted | changes), patch.object(workflow.subprocess, "run", return_value=response), \
                    patch.object(workflow, "download_draft_asset") as fetch, self.assertRaises(ValueError):
                workflow.draft("v0.1.99", "a" * 40, self.root / "draft")
            fetch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
