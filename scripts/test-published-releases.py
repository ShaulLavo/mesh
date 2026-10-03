"""Public archive identities and joined historical bridge receipts."""
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path

from fixtures import published_releases as published


class PublishedInputsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.platform = {"os": "darwin", "arch": "arm64"}
        self.assets = {}
        self.manifests = []
        self.receipts = []
        compatibility = dict(zip(published.COMPATIBILITY_FIELDS, (10, 10, 10, 1, 1, 1, 1)))
        for version in published.VERSIONS:
            payload = ("external public download fixture " + version).encode()
            output = io.BytesIO()
            with tarfile.open(fileobj=output, mode="w:gz") as archive:
                entry = tarfile.TarInfo("mesh")
                entry.size = len(payload)
                archive.addfile(entry, io.BytesIO(payload))
            data = output.getvalue()
            artifact = {"platform": self.platform, "archive": "mesh_darwin_arm64.tar.gz",
                "sha256": published.digest(data), "binarySha256": published.digest(payload)}
            manifest = {"schema": 1, "version": version, "commit": "a" * 40,
                "artifacts": [artifact], "compatibility": compatibility | {"transitions": []}}
            self.assets[published.ORIGIN + version + "/" + artifact["archive"]] = data
            self.manifests.append(manifest)
        for before, after in zip(self.manifests, self.manifests[1:]):
            receipt = compatibility | {"schema": 1, "platform": self.platform,
                "fromDigest": before["artifacts"][0]["binarySha256"],
                "toDigest": after["artifacts"][0]["binarySha256"],
                "retainedOpenedCandidateState": True, "sessionsPreserved": True, "recoveryRecordsPreserved": True}
            data = json.dumps(receipt).encode()
            proof = published.digest(data)
            transition = {key: receipt[key] for key in ("platform", "fromDigest", "toDigest")}
            transition["proof"] = proof
            after["compatibility"]["transitions"].append(transition)
            self.assets[published.ORIGIN + after["version"] + "/" + proof + ".json"] = data
            self.receipts.append((data, transition, after["compatibility"]))
        self.requests = []

    def acquire(self):
        for manifest in self.manifests:
            self.assets[published.ORIGIN + manifest["version"] + "/mesh-release.json"] = json.dumps(manifest).encode()
        def fetcher(address, maximum):
            self.requests.append(address)
            data = self.assets[address]
            self.assertLessEqual(len(data), maximum)
            return data
        return published.acquire(self.root, self.platform, fetcher)

    def test_real_download_bytes_and_joined_receipts_are_retained(self):
        releases = self.acquire()
        self.assertEqual([row["manifest"]["version"] for row in releases], list(published.VERSIONS))
        for row in releases:
            artifact = row["manifest"]["artifacts"][0]
            self.assertEqual(published.digest(row["executable"].read_bytes()), artifact["binarySha256"])
            self.assertIn("mesh-release.json", row["files"])
        evidence = json.loads((self.root / "verified.json").read_text())
        self.assertEqual(len(evidence["archives"]), 3)
        self.assertEqual([(row["fromVersion"], row["toVersion"]) for row in evidence["bridges"]],
            [("v0.1.149", "v0.1.151"), ("v0.1.151", "v0.1.159")])
        self.assertTrue(all(address.startswith(published.ORIGIN) for address in self.requests))

    def test_archive_digest_mismatch_refuses_execution(self):
        self.manifests[0]["artifacts"][0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "archive digest mismatch"):
            self.acquire()

    def test_executable_digest_mismatch_refuses_execution(self):
        self.manifests[0]["artifacts"][0]["binarySha256"] = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "executable digest mismatch"):
            self.acquire()

    def test_missing_joined_bridge_refuses_execution(self):
        self.manifests[1]["compatibility"]["transitions"] = []
        with self.assertRaisesRegex(RuntimeError, "joined transition"):
            self.acquire()

    def test_receipt_digest_mismatch_refuses_execution(self):
        data, transition, compatibility = self.receipts[0]
        with self.assertRaisesRegex(RuntimeError, "receipt digest mismatch"):
            published.joined_receipt(data + b" ", transition, compatibility)

    def test_receipt_join_mismatch_refuses_execution(self):
        data, transition, compatibility = self.receipts[0]
        transition = transition | {"fromDigest": "0" * 64}
        with self.assertRaisesRegex(RuntimeError, "transition mismatch"):
            published.joined_receipt(data, transition, compatibility)

    def test_receipt_compatibility_mismatch_refuses_execution(self):
        data, transition, compatibility = self.receipts[0]
        with self.assertRaisesRegex(RuntimeError, "compatibility mismatch"):
            published.joined_receipt(data, transition, compatibility | {"stateReadMax": 11})

    def test_receipt_requires_preserved_sessions(self):
        data, transition, compatibility = self.receipts[0]
        receipt = json.loads(data)
        receipt["sessionsPreserved"] = False
        data = json.dumps(receipt).encode()
        with self.assertRaisesRegex(RuntimeError, "retained state and sessions"):
            published.joined_receipt(data, transition | {"proof": published.digest(data)}, compatibility)

    def test_asset_path_cannot_escape_release_directory(self):
        with self.assertRaisesRegex(RuntimeError, "basename"):
            published.fetch(self.root, "v0.1.149", "../mesh", 10, lambda *_: b"")


if __name__ == "__main__":
    unittest.main()
