#!/usr/bin/env python3
"""Real private Sigstore/Go-codec snapshot through unchanged semantic finalization.

No candidate executable, native service, public API or release mutation runs here.
"""
import importlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from fixtures import draft_intake, installed_clock, installed_origin, installed_releases, installed_publication
from fixtures import release_documents, release_tool, release_worker_completion as completion

corpus = importlib.import_module("test-bundled-release")
groups = importlib.import_module("test-release-worker-groups")
workflow = importlib.import_module("publication-workflow")
finalizer = importlib.import_module("verify-worker-publication")
ROOT = Path(__file__).resolve().parents[1]


class SignedPublicationTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="signed-bundle-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.clock = installed_clock.Clock({"test": "offline signed bundle"})
        active = self.clock.activate()
        active.__enter__()
        self.addCleanup(active.__exit__, None, None, None)
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.source = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
        self.producer = installed_releases.Releases(self.root, "linux/amd64", self.output)
        self.addCleanup(self.producer.close)
        self.producer.start_signer()
        self.tool = self.build_private_tool()

    def build_private_tool(self):
        directory = self.root / "private-tools"
        directory.mkdir()
        tool = directory / "mesh-release-sign"
        environment = self.producer.environment() | {"GOTOOLCHAIN": "local"}
        subprocess.run(["go", "build", "-trimpath", "-overlay", str(self.producer.overlay(1)),
                        "-o", str(tool), "./cmd/mesh-release-sign"], check=True, env=environment, timeout=300)
        release_tool.record(directory, self.source, 42, "harness", release_tool.native_target())
        (self.root / "private-trust").mkdir()
        return tool

    def signed_snapshot(self):
        control = object.__new__(corpus.CompleteCorpusTests)
        control.root = self.root
        control.dist, control.proofs, control.history, control.retained, control.manifest = corpus.corpus(self.root)
        control.logical = self.root / "logical"
        control.members = release_documents.stage(control.dist, control.proofs, control.logical)
        physical, release, aliases = control.snapshot()
        (physical / release_documents.BUNDLE).unlink()
        (physical / release_documents.ENVELOPE).unlink()
        subjects = [control.logical / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json",
                                                       "mesh-workers.json", "mesh-worker-groups.json")]
        subjects.extend(physical / name for name in draft_intake.ARCHIVES.values())
        installed_origin.sign(self.root, subjects, physical / release_documents.ENVELOPE, documents={
            "directory": "logical", "members": control.members, "binding": release_tool.binding(release),
            "output": str((physical / release_documents.BUNDLE).relative_to(self.root))})
        release["assets"] = [control.asset(path, number + 1) for number, path in enumerate(sorted(physical.iterdir())) if path.name != "draft.json"]
        (physical / "draft.json").write_text(json.dumps(release))
        return control, physical, release, aliases

    def environment(self, physical):
        return {"MESH_RELEASE_TOOL": str(self.tool), "MESH_RELEASE_TOOL_SOURCE": self.source,
                "MESH_RELEASE_TOOL_RUN": "42", "MESH_RELEASE_TOOL_ROLE": "harness",
                "MESH_RELEASE_TRUST_CACHE": str(self.root / "private-trust"),
                "MESH_PUBLICATION_TRUST_CACHE": str(self.root / "private-trust"),
                "GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/heads/main",
                "MESH_PUBLICATION_DRAFT": str(physical), "MESH_PUBLICATION_RUN": "42"}

    def test_real_signed_snapshot_to_scheduler_native_intake_aggregation_and_finalizer(self):
        control, physical, release, aliases = self.signed_snapshot()
        with patch.dict(os.environ, self.environment(physical)):
            logical = self.root / "authenticated"
            draft_intake.documents(physical, corpus.VERSION, groups.SOURCE, logical, 42)
            for name in control.members:
                self.assertEqual((logical / name).read_bytes(), (control.logical / name).read_bytes())
            tasks = workflow.matrix(logical, self.source)["include"]
            self.assertEqual(len(tasks), 15)
            self.check_native_intake(physical)
            completed = self.aggregate(logical)
            self.check_finalizer(physical, release, completed)
            self.check_go_authentication_refuses_changed_snapshot(physical, release)
        self.assertEqual(len(aliases), 9)

    def check_native_intake(self, physical):
        root, output = self.root / "native-intake", self.root / "native-evidence"
        root.mkdir()
        output.mkdir()
        with patch.object(installed_publication, "load_draft", return_value="authenticated pre-execution intake") as consume:
            installed_publication.stage(root, output, "linux/amd64", corpus.VERSION, groups.SOURCE)
        self.assertEqual(consume.call_args.args[-1], physical)

    def aggregate(self, logical):
        producer = groups.CompletionTests()
        producer.root, producer.harness = self.root / "complete-cases", self.source
        producer.root.mkdir()
        producer.release = completion.release_context(logical)
        producer.selection = json.loads((logical / "mesh-worker-groups.json").read_bytes())["selection"]
        for platform in groups.index.PLATFORMS:
            producer.write_completion(platform)
        completed = self.root / "aggregated"
        completed.mkdir()
        for native in workflow.RUNNERS:
            self.aggregate_platform(logical, producer, native, completed)
        completion.validate(completed, producer.release, self.source, producer.selection)
        return completed

    def aggregate_platform(self, logical, producer, native, completed):
        inventory = completion.task_inventory(logical, native, self.source)
        inputs = self.root / ("children-" + native.replace("/", "-"))
        inputs.mkdir()
        for number, task in enumerate(inventory["tasks"]):
            child = inputs / str(number)
            child.mkdir()
            source = producer.root / native.replace("/", "-")
            for name in ("real-draft.json", "harness-source.json", "first-published-updater.json", task["case"]):
                shutil.copyfile(source / name, child / name)
            completion.record_task(child, inventory, task, producer.release, producer.selection)
        completion.aggregate_tasks(logical, native, self.source, inputs, completed / native.replace("/", "-"))

    def check_finalizer(self, physical, release, completed):
        replies = [json.dumps(release).encode(), json.dumps([release["assets"]]).encode()]
        live = draft_intake.subprocess.check_output
        def api(command, **kwargs):
            if command[0] == "gh":
                return replies.pop(0)
            return live(command, **kwargs)
        with patch.object(finalizer.re, "findall", return_value=[self.source]), patch.object(draft_intake.subprocess, "check_output", side_effect=api):
            finalizer.verify(corpus.VERSION, groups.SOURCE, str(completed), str(physical), "42")
        self.assertEqual(replies, [])

    def check_go_authentication_refuses_changed_snapshot(self, physical, release):
        archive = physical / release_documents.BUNDLE
        original = archive.read_bytes()
        archive.write_bytes(original[:-1] + bytes([original[-1] ^ 1]))
        with self.assertRaises(ValueError):
            draft_intake.documents(physical, corpus.VERSION, groups.SOURCE, self.root / "changed", 42)
        archive.write_bytes(original)
        with self.assertRaisesRegex(ValueError, "writer run"):
            draft_intake.documents(physical, corpus.VERSION, groups.SOURCE, self.root / "other-run", 43)


if __name__ == "__main__":
    unittest.main()
