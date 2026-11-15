#!/usr/bin/env python3
"""Offline member selection and bounded legacy projection controls."""
import ast
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from fixtures import release_documents as documents
from fixtures import installed_origin, installed_releases, installed_publication
from types import SimpleNamespace


def write(directory, name, value):
    raw = json.dumps(value, sort_keys=True).encode()
    (directory / name).write_bytes(raw)
    return hashlib.sha256(raw).hexdigest()


def depth(node, level=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        level = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        level += 1
    return max((depth(child, level) for child in ast.iter_child_nodes(node)), default=level)


class DocumentsTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="release-documents-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.logical = self.root / "candidate"
        self.logical.mkdir()
        self.manifest = {"schema": 1, "version": "v0.1.324", "artifacts": [], "compatibility": {"transitions": []}}
        self.history = []
        self.versions = documents.legacy_versions()
        for number, platform in enumerate(documents.release_worker_index.PLATFORMS):
            self.manifest["artifacts"].append({"platform": platform, "binarySha256": f"{number + 1:064x}"})
        for index, version in enumerate(self.versions):
            self.add_source(index, version)
        write(self.logical, "mesh-release.json", self.manifest)

    def add_source(self, index, version):
        original = {"schema": 1, "version": version, "artifacts": []}
        for number, target in enumerate(self.manifest["artifacts"]):
            source = f"{100 + 3 * index + number:064x}"
            original["artifacts"].append({"platform": target["platform"], "binarySha256": source})
            receipt = {"schema": 1, "platform": target["platform"], "fromDigest": source, "toDigest": target["binarySha256"]}
            raw = json.dumps(receipt, sort_keys=True).encode()
            digest = hashlib.sha256(raw).hexdigest()
            (self.logical / (digest + ".json")).write_bytes(raw)
            self.manifest["compatibility"]["transitions"].append(receipt | {"proof": digest})
        directory = self.root / version
        directory.mkdir()
        digest = write(directory, "mesh-release.json", original)
        self.history.append({"tag": version, "assets": [{"name": "mesh-release.json", "sha256": digest}]})

    def project(self):
        manifest = json.loads((self.logical / "mesh-release.json").read_bytes())
        return documents.legacy_projection(manifest, self.logical, self.root, self.history)

    def test_union_keeps_every_supported_source_on_all_platforms(self):
        self.assertEqual(self.versions, ["v0.1.249", "v0.1.255", "v0.1.257"])
        names = self.project()
        self.assertEqual(len(names), 9)
        expected = {item["proof"] + ".json" for item in self.manifest["compatibility"]["transitions"]}
        self.assertEqual(set(names), expected)
        self.assertEqual(documents.legacy_versions({"hosts": [{"daemon": "v0.1.4"}]},
                         {"requiredRollback": ["v0.1.5"]}, ["v0.1.6"]), ["v0.1.4", "v0.1.5", "v0.1.6"])

    def test_projection_does_not_grow_with_unrelated_proof_history(self):
        expected = self.project()
        for index in range(1000):
            (self.logical / f"{index:064x}.worker-group.json").write_bytes(b'{}')
        self.assertEqual(self.project(), expected)

    def test_missing_ambiguous_wrong_platform_and_changed_receipts_fail(self):
        transition = self.manifest["compatibility"]["transitions"].pop()
        write(self.logical, "mesh-release.json", self.manifest)
        with self.assertRaisesRegex(ValueError, "one exact"):
            self.project()
        self.manifest["compatibility"]["transitions"].extend([transition, transition])
        write(self.logical, "mesh-release.json", self.manifest)
        with self.assertRaisesRegex(ValueError, "one exact"):
            self.project()
        self.manifest["compatibility"]["transitions"].pop()
        write(self.logical, "mesh-release.json", self.manifest)
        (self.logical / (transition["proof"] + ".json")).write_bytes(b'{}')
        with self.assertRaisesRegex(ValueError, "authenticated reference"):
            self.project()

    def test_historical_api_hash_and_source_platform_are_required(self):
        self.history[0]["assets"][0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "authenticated reference"):
            self.project()

    def test_members_are_explicit_and_keep_full_semantic_validator(self):
        for name in documents.FIXED:
            if name != "mesh-release.json":
                write(self.logical, name, {})
        write(self.logical, "mesh-update.json", {"version": self.manifest["version"], "rollback": [],
              "page": {"tag": "v0.1.300", "name": "prior-page.json", "sha256": "0" * 64}})
        (self.logical / "rollback-index.json").write_bytes(b'[]')
        leaf = "a" * 64 + ".worker-group.json"
        (self.logical / leaf).write_bytes(b'{}')
        with patch.object(documents.release_worker_index, "validate_index", return_value={}) as semantic:
            with patch.object(documents.release_worker_index, "closure", return_value=[leaf]):
                names = documents.members(self.logical)
        semantic.assert_called_once()
        self.assertIn(leaf, names)
        self.assertNotIn("rollback-index.json", names)
        self.assertEqual(len(names), len(documents.FIXED) + 9 + 1)

    def test_new_code_stays_within_three_blocks(self):
        self.assertLessEqual(depth(ast.parse(Path(documents.__file__).read_text())), 3)
        self.assertLessEqual(depth(ast.parse(Path(__file__).read_text())), 3)


class PrivateProducerTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="private-bundle-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.directory = self.root / "releases" / "v0.1.324"
        self.directory.mkdir(parents=True)

    def test_producer_packs_explicit_members_before_signing_and_declares_physical_only_layout(self):
        producer = object.__new__(installed_releases.Releases)
        producer.root, producer.directory = self.root, self.directory.parent
        producer.metadata, producer.document_members = {}, {"v0.1.324": {"leaf.worker-group.json"}}
        producer.manifests = {"v0.1.324": {"version": "v0.1.324", "commit": "a" * 40,
            "artifacts": [{"platform": platform, "archive": "mesh_" + platform["os"] + "_" + platform["arch"] + ".tar.gz"}
                          for platform in documents.release_worker_index.PLATFORMS],
            "compatibility": {"transitions": [{"proof": "b" * 64}]}}}
        (self.directory / "planning-helper.json").write_text("must not be packed")
        with patch.object(installed_origin, "request") as request:
            producer.publish.__wrapped__(producer, "v0.1.324", 0, [{"forward": "b" * 64, "rollback": "c" * 64}])
        value = request.call_args.args[1]
        self.assertEqual(value["documents"]["directory"], "releases/v0.1.324")
        self.assertEqual(value["documents"]["output"], "releases/v0.1.324/mesh-documents.tar.gz")
        members = set(value["documents"]["members"])
        self.assertTrue({"mesh-release.json", "mesh-update.json", "mesh-update-root.json", "leaf.worker-group.json",
                         "b" * 64 + ".json", "c" * 64 + ".rollback.json"} <= members)
        self.assertNotIn("planning-helper.json", members)
        self.assertNotIn(documents.ENVELOPE, members)
        root = json.loads((self.directory / "mesh-update-root.json").read_bytes())
        self.assertEqual(root["head"]["sha256"], hashlib.sha256((self.directory / "mesh-update.json").read_bytes()).hexdigest())
        physical = json.loads((self.directory / "physical-assets.json").read_bytes())
        self.assertIn(documents.BUNDLE, physical)
        self.assertNotIn("mesh-update.json", physical)

    def test_read_only_serving_never_invokes_the_signing_role_or_exposes_members(self):
        names = list(documents.PHYSICAL)
        (self.directory / "physical-assets.json").write_text(json.dumps(names))
        for name in names + ["mesh-update.json", "proof.worker-group.json"]:
            (self.directory / name).write_bytes(b"offline physical selection")
        files = installed_origin.physical_files(self.root, self.directory.parent)
        self.assertEqual(set(files), {"v0.1.324/" + name for name in names})
        self.assertNotIn("v0.1.324/mesh-update.json", files)
        (self.directory / "physical-assets.json").unlink()
        with self.assertRaisesRegex(RuntimeError, "declared bundled physical layout"):
            installed_origin.physical_files(self.root, self.directory.parent)

    def test_published_hop_metadata_uses_authenticated_logical_retention_without_flat_http(self):
        digest = "d" * 64
        (self.directory / "mesh-retention.json").write_text(json.dumps([{"tag": "v0.1.323",
            "assets": [{"name": "mesh-update.json", "sha256": digest}]}]))
        releases = SimpleNamespace(directory=self.directory.parent, verification={"version": "v0.1.324"})
        self.assertEqual(installed_publication.logical_digest(releases, "v0.1.323", "mesh-update.json"), digest)
        with self.assertRaisesRegex(ValueError, "logical retention reference"):
            installed_publication.logical_digest(releases, "v0.1.322", "mesh-update.json")


if __name__ == "__main__":
    unittest.main()
