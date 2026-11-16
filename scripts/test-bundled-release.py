#!/usr/bin/env python3
"""Complete offline proof corpus and fixed physical publication controls."""
import hashlib
import contextlib
import importlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from fixtures import release_documents as documents, release_tool
from fixtures import draft_intake, installed_publication, release_worker_completion

groups = importlib.import_module("test-release-worker-groups")
VERSION = "v0.1.324"


def legacy_corpus_baseline():
    inventory = release_worker_completion.fleet_baselines.load() | {"firstPublishedUpdater": None}
    return patch.object(release_worker_completion.fleet_baselines, "load", return_value=inventory)


def legacy_corpus_source(root):
    scripts = root / "scripts"
    shutil.copytree(documents.ROOT / "scripts/fixtures", scripts / "fixtures")
    shutil.copyfile(documents.ROOT / "scripts/publication-jobs.py", scripts / "publication-jobs.py")
    baseline = scripts / "fixtures/fleet-baselines.json"
    inventory = json.loads(baseline.read_text()) | {"firstPublishedUpdater": None}
    baseline.write_text(json.dumps(inventory))
    return root


class NativeGroup(groups.Bundle):
    def startup(self):
        values = super().startup()
        for entry in values.values():
            entry["release"]["version"] = VERSION
            entry["failedImage"]["version"] = VERSION
        return values


def write(path, value):
    raw = groups.groups.encode(value)
    path.write_bytes(raw)
    return hashlib.sha256(raw).hexdigest()


def source_group(proofs, workers, platform, registry, selection, references, transitions, rollback):
    full = workers[-1]["version"] in documents.legacy_versions()
    bundle = NativeGroup(proofs / "groups/leaves", workers, platform=platform, registry_digest=registry,
                         profile="full" if full else "forward-only")
    reference = bundle.reference()
    (proofs / "groups/leaves" / reference["name"]).rename(proofs / reference["name"])
    references.append(reference)
    selected = groups.groups.validate_group_assets(bundle.context, workers, reference, groups.index.resolver(proofs))
    forward = hashlib.sha256(bundle.forward).hexdigest()
    (proofs / (forward + ".json")).write_bytes(bundle.forward)
    if full:
        raw = groups.groups.encode(selected["rollback"])
        digest = hashlib.sha256(raw).hexdigest()
        (proofs / "rollback" / (digest + ".rollback.json")).write_bytes(raw)
        rollback.append({"forward": forward, "rollback": digest})
    transitions.append({"platform": platform, "fromDigest": bundle.context["sourceDigest"], "toDigest": "c" * 64, "proof": forward})
    selection["baselines"].append({"version": workers[-1]["version"], "artifact": workers[-1]["artifact"], "rollback": full})


def corpus(root):
    dist, proofs, history = root / "dist", root / "proofs", root / "history"
    dist.mkdir()
    (proofs / "groups/leaves").mkdir(parents=True)
    (proofs / "rollback").mkdir()
    history.mkdir()
    selection = {"workers": [], "baselines": []}
    refs, registries, transitions, rollback, artifacts = [], {}, [], [], []
    for platform in groups.index.PLATFORMS:
        add_platform(dist, proofs, platform, selection, refs, registries, transitions, rollback, artifacts)
    manifest = {"schema": 1, "version": VERSION, "commit": groups.SOURCE, "artifacts": artifacts,
        "compatibility": {"stateReadMin": 1, "stateReadMax": 2, "stateWrite": 2,
            "workerMin": 1, "workerMax": 2, "workerWrite": 2, "journalVersion": 1, "transitions": transitions}}
    manifest_digest = write(dist / "mesh-release.json", manifest)
    candidates = [{"version": VERSION, "commit": groups.SOURCE, "protocol": 2, "artifact": item} for item in artifacts]
    write(dist / "mesh-workers.json", selection["workers"] + candidates)
    groups.index.stage(dist, proofs, {"selection": selection, "sources": refs, "registries": registries}, groups.SOURCE)
    page = {"schema": 1, "epoch": 1, "page": 0, "entries": [], "back": None}
    page_digest = write(dist / "mesh-anchors-control.json", page)
    metadata = {"schema": 1, "epoch": 1, "trust": 1, "version": VERSION, "manifest": manifest_digest,
        "publishedAt": "2026-11-15T00:00:00Z", "position": {"anchor": True, "ordinal": 0},
        "direct": {"anchorsFrom": 0, "since": []}, "rollback": rollback,
        "page": {"tag": VERSION, "name": "mesh-anchors-control.json", "sha256": page_digest}}
    metadata_digest = write(dist / "mesh-update.json", metadata)
    write(dist / "mesh-update-root.json", {"format": 1, "epoch": 1,
        "head": {"tag": VERSION, "name": "mesh-update.json", "sha256": metadata_digest}})
    retained = historical(root, history, selection)
    write(dist / "mesh-retention.json", retained)
    (proofs / "rollback-index.json").write_text("[]")
    return dist, proofs, history, retained, manifest


def add_platform(dist, proofs, platform, selection, refs, registries, transitions, rollback, artifacts):
    workers = groups.selected(309, platform)
    selection["workers"].extend(groups.groups.member(item) for item in workers)
    registry = groups.groups.encode({"schema": 1, "platform": platform, "workers": workers})
    digest = hashlib.sha256(registry).hexdigest()
    registries[json.dumps(platform, sort_keys=True)] = {"bytes": registry.decode(), "sha256": digest}
    policy = json.loads((documents.ROOT / "scripts/upgrade-policy.json").read_bytes())
    sources = sorted(set(policy["seeds"]) | set(documents.legacy_versions()) | {"v0.1.307", "v0.1.308", "v0.1.309"})
    for number in sorted(int(version.rsplit(".", 1)[1]) for version in sources):
        source_group(proofs, workers[:number], platform, digest, selection, refs, transitions, rollback)
    name = "mesh_" + platform["os"] + "_" + platform["arch"] + ".tar.gz"
    data = ("opaque native archive " + name).encode()
    (dist / name).write_bytes(data)
    artifacts.append({"platform": platform, "archive": name, "sha256": hashlib.sha256(data).hexdigest(), "binarySha256": "c" * 64})


def historical(root, history, selection):
    retained = []
    for version in documents.legacy_versions():
        directory = history / version
        directory.mkdir()
        artifacts = [item["artifact"] for item in selection["baselines"] if item["version"] == version]
        digest = write(directory / "mesh-release.json", {"schema": 1, "version": version, "artifacts": artifacts})
        retained.append({"tag": version, "publishedAt": "2026-11-01T00:00:00Z",
                         "assets": [{"name": "mesh-release.json", "sha256": digest}]})
    return retained


class CompleteCorpusTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(legacy_corpus_baseline())
        self.scratch = tempfile.TemporaryDirectory(prefix="bundle-corpus-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.dist, self.proofs, self.history, self.retained, self.manifest = corpus(self.root)
        self.logical = self.root / "logical"
        self.members = documents.stage(self.dist, self.proofs, self.logical)

    def test_complete_native_matrix_keeps_every_worker_group_and_exact_canonical_receipt(self):
        index = groups.index.validate_index(self.logical / "mesh-worker-groups.json", self.manifest)
        self.assertEqual(len(index["sources"]), 66)
        self.assertEqual(len(index["selection"]["workers"]), 927)
        self.assertGreater(len(self.members), 300)
        aliases = documents.legacy_projection(self.manifest, self.logical, self.history, self.retained)
        self.assertEqual(len(aliases), 9)
        self.assertEqual(documents.selection_projection(self.logical, index), aliases)
        self.assertEqual(5 + len(documents.PHYSICAL) + len(aliases), 18)
        self.assertNotIn("rollback-index.json", self.members)
        self.assertEqual(set(self.members), {path.name for path in self.logical.iterdir()})

    def test_missing_typed_worker_evidence_fails_the_existing_semantic_validator(self):
        index = json.loads((self.logical / "mesh-worker-groups.json").read_bytes())
        reference = index["sources"][0]
        companion = json.loads((self.logical / reference["name"]).read_bytes())
        (self.logical / companion["groups"][0]["cleanup"]["name"]).unlink()
        with self.assertRaises((OSError, ValueError)):
            documents.members(self.logical)

    def snapshot(self):
        physical = self.root / "physical" / VERSION
        physical.mkdir(parents=True)
        aliases = documents.legacy_projection(self.manifest, self.logical, self.history, self.retained)
        for name in documents.PHYSICAL + tuple(aliases):
            source = self.logical / name
            (physical / name).write_bytes(source.read_bytes() if source.exists() else b"opaque signed physical control")
        checksums = []
        for name in draft_intake.ARCHIVES.values():
            shutil.copyfile(self.dist / name, physical / name)
            checksums.append(hashlib.sha256((physical / name).read_bytes()).hexdigest() + "  " + name + "\n")
        (physical / "checksums.txt").write_text("".join(checksums))
        (physical / "mesh.rb").write_text("offline Cask")
        (self.logical / documents.ENVELOPE).write_bytes((physical / documents.ENVELOPE).read_bytes())
        release = {"repository": "ShaulLavo/mesh", "intake_run": 42, "frozen_baselines": [], "draft": True, "id": 123,
            "tag_name": VERSION, "target_commitish": groups.SOURCE, "url": "https://api.github.com/repos/ShaulLavo/mesh/releases/123",
            "html_url": "https://github.com/ShaulLavo/mesh/releases/tag/" + VERSION, "assets": []}
        release["assets"] = [self.asset(path, number + 1) for number, path in enumerate(sorted(physical.iterdir()))]
        (physical / "draft.json").write_text(json.dumps(release))
        return physical, release, aliases

    @staticmethod
    def asset(path, number):
        return {"name": path.name, "id": number, "size": path.stat().st_size, "state": "uploaded",
                "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()}

    def materialize_control(self, _snapshot, _release, destination, _inventory):
        shutil.copytree(self.logical, destination)

    def test_physical_snapshot_feeds_full_semantic_native_intake_and_fifteen_tasks(self):
        physical, release, _aliases = self.snapshot()
        output = self.root / "verified"
        with patch.object(release_tool, "materialize", side_effect=self.materialize_control):
            observed = draft_intake.documents(physical, VERSION, groups.SOURCE, output, 42)
        self.assertEqual(observed, release)
        self.assertFalse((output / "draft.json").exists())
        self.assertEqual(len(observed["assets"]), 18)
        counts = {platform["os"] + "/" + platform["arch"]: len(release_worker_completion.task_inventory(
            output, platform["os"] + "/" + platform["arch"], groups.SOURCE)["tasks"]) for platform in groups.index.PLATFORMS}
        self.assertEqual(counts, {"linux/amd64": 7, "linux/arm64": 4, "darwin/arm64": 4})
        native_root = self.root / "native-input"
        native_root.mkdir()
        evidence = self.root / "evidence"
        evidence.mkdir()
        environment = {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/heads/main",
                       "MESH_PUBLICATION_DRAFT": str(physical), "MESH_PUBLICATION_RUN": "42"}
        with patch.dict(os.environ, environment), patch.object(release_tool, "materialize", side_effect=self.materialize_control):
            with patch.object(installed_publication, "load_draft", return_value="authenticated native consumer") as consumer:
                self.assertEqual(installed_publication.stage(native_root, evidence, "linux/amd64", VERSION, groups.SOURCE), "authenticated native consumer")
        self.assertEqual(consumer.call_args.args[-1], physical)

    def test_missing_or_wrong_but_byte_valid_alias_is_rejected_by_semantic_projection(self):
        physical, release, aliases = self.snapshot()
        missing = physical / aliases[0]
        missing.unlink()
        release["assets"] = [item for item in release["assets"] if item["name"] != aliases[0]]
        (physical / "draft.json").write_text(json.dumps(release))
        with patch.object(release_tool, "materialize", side_effect=self.materialize_control):
            with self.assertRaisesRegex(ValueError, "complete supported legacy projection"):
                draft_intake.documents(physical, VERSION, groups.SOURCE, self.root / "missing", 42)
        other = next(item["proof"] + ".json" for item in self.manifest["compatibility"]["transitions"]
                     if item["proof"] + ".json" not in aliases)
        shutil.copyfile(self.logical / other, physical / other)
        release["assets"].append(self.asset(physical / other, 9999))
        (physical / "draft.json").write_text(json.dumps(release))
        with patch.object(release_tool, "materialize", side_effect=self.materialize_control):
            with self.assertRaisesRegex(ValueError, "complete supported legacy projection"):
                draft_intake.documents(physical, VERSION, groups.SOURCE, self.root / "wrong", 42)

    def test_final_live_comparison_binds_every_physical_asset_and_declines_mutation(self):
        physical, release, _aliases = self.snapshot()
        responses = [json.dumps(release).encode(), json.dumps([release["assets"]]).encode()]
        with patch.object(draft_intake.subprocess, "check_output", side_effect=responses) as api:
            draft_intake.compare_live(physical, VERSION, groups.SOURCE, 42)
        self.assertEqual(api.call_count, 2)
        changed = json.loads(json.dumps(release))
        changed["assets"][0]["id"] += 100
        with patch.object(draft_intake.subprocess, "check_output", side_effect=[json.dumps(changed).encode(), json.dumps([changed["assets"]]).encode()]):
            with self.assertRaisesRegex(ValueError, "physical inventory"):
                draft_intake.compare_live(physical, VERSION, groups.SOURCE, 42)

    def test_owned_upload_calls_are_constant_with_thirteen_declared_metadata_assets(self):
        controls = importlib.import_module("test-upload-signed-draft")
        physical, release, aliases = self.snapshot()
        candidate = self.root / "candidate"
        candidate.mkdir()
        candidate_release = dict(release, assets=[item for item in release["assets"] if item["name"] in draft_intake.NAMES])
        for name in draft_intake.NAMES:
            shutil.copyfile(physical / name, candidate / name)
        (candidate / "draft.json").write_text(json.dumps(candidate_release))
        initial = dict(release, assets=[item for item in release["assets"] if item["name"] in draft_intake.NAMES + ("mesh.rb",)])
        for name in documents.PHYSICAL:
            shutil.copyfile(physical / name, self.dist / name)
        paths = [self.dist / name for name in documents.PHYSICAL] + [self.proofs / name for name in aliases]
        counts = []
        for number in (1, 1000):
            (self.dist / documents.BUNDLE).write_bytes(b"opaque logical document\n" * number)
            fake = controls.FakeCLI(initial)
            self.publish_control(controls, fake, candidate, paths)
            counts.append(len(fake.calls))
            self.assertEqual(sum(call[0] == "POST" for call in fake.calls), 13)
        self.assertEqual(counts, [30, 30])

    def publish_control(self, controls, fake, candidate, paths):
        api = controls.uploader
        with patch.dict(os.environ, GITHUB_REPOSITORY="ShaulLavo/mesh", GITHUB_RUN_ID="42"), contextlib.chdir(self.root), \
                patch.object(api.subprocess, "run", side_effect=fake.run), patch.object(api.time, "sleep", side_effect=fake.sleep), \
                patch.object(api.time, "monotonic", side_effect=lambda: fake.clock), patch.object(api.time, "time", side_effect=lambda: fake.clock):
            api.Publisher(api.API(), candidate, VERSION, groups.SOURCE, paths).run()

    @unittest.skipUnless(os.environ.get("MESH_TEST_RELEASE_TOOL"), "real CLI supplied by the owned build runner")
    def test_actual_go_packer_preserves_complete_exact_inventory_and_is_deterministic(self):
        tool = Path(os.environ["MESH_TEST_RELEASE_TOOL"])
        expected_source = os.environ["MESH_TEST_RELEASE_TOOL_SOURCE"]
        release_tool.validate_identity(release_tool.identity(tool), expected_source, release_tool.native_target(), release_tool.digest(tool))
        binding = {"version": VERSION, "source": groups.SOURCE, "releaseId": 123, "writerRun": 42,
                   "platforms": sorted(groups.index.PLATFORMS, key=lambda item: (item["os"], item["arch"]))}
        (self.root / "binding.json").write_text(json.dumps(binding))
        (self.root / "members.json").write_text(json.dumps(self.members))
        first = self.pack(tool, "first.tar.gz")
        second = self.pack(tool, "second.tar.gz")
        self.assertEqual(first, second)
        self.assertEqual(first["binding"], binding)
        expected = [{"name": name, "sha256": hashlib.sha256((self.logical / name).read_bytes()).hexdigest(),
                     "size": (self.logical / name).stat().st_size} for name in self.members]
        self.assertEqual(first["documents"], expected)
        self.assertEqual(first["archive"]["size"], (self.root / "first.tar.gz").stat().st_size)
        self.assertEqual((self.root / "first.tar.gz").read_bytes(), (self.root / "second.tar.gz").read_bytes())
        print(json.dumps({"completeCorpus": {"members": len(expected), "logicalBytes": sum(item["size"] for item in expected),
            "compressedBytes": first["archive"]["size"], "physicalAssets": 18}}, sort_keys=True))

    def pack(self, tool, name):
        return json.loads(subprocess.check_output([str(tool), "pack-documents", str(self.logical), str(self.root / "members.json"),
            str(self.root / "binding.json"), str(self.root / name)], timeout=180))


if __name__ == "__main__":
    suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
    for name in ("test-release-documents", "test-release-tool", "test-upload-signed-draft"):
        suite.addTests(unittest.defaultTestLoader.loadTestsFromModule(importlib.import_module(name)))
    raise SystemExit(not unittest.TextTestRunner().run(suite).wasSuccessful())
