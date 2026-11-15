#!/usr/bin/env python3
"""Complete offline proof corpus and fixed physical publication controls."""
import hashlib
import importlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from fixtures import release_documents as documents, release_tool

groups = importlib.import_module("test-release-worker-groups")
VERSION = "v0.1.324"


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
    page = {"schema": 1, "epoch": 1, "page": 0, "entries": [], "back": []}
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
