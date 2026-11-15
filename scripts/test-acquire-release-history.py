#!/usr/bin/env python3
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("history", Path(__file__).with_name("acquire-release-history.py"))
HISTORY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HISTORY)


class HistoryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mh-")
        self.addCleanup(self.temporary.cleanup)
        self.cache = Path(self.temporary.name)
        self.data = b"published receipt bytes"
        self.digest = hashlib.sha256(self.data).hexdigest()
        self.asset = {"id": 1, "name": self.digest + ".json", "digest": "sha256:" + self.digest,
                      "browser_download_url": "https://example.invalid/proof", "size": len(self.data),
                      "updated_at": "2026-11-11T00:00:00Z"}

    def test_immutable_cache_hit_checks_the_real_bytes(self):
        with mock.patch.object(HISTORY, "download", return_value=self.data) as download:
            self.assertEqual(HISTORY.cache_asset(self.asset, self.cache, 100), (self.data, False))
            self.assertEqual(HISTORY.cache_asset(self.asset, self.cache, 100), (self.data, True))
            self.assertEqual(download.call_count, 1)
            (self.cache / self.digest).write_bytes(b"changed")
            with self.assertRaisesRegex(ValueError, "corrupt immutable cache"):
                HISTORY.cache_asset(self.asset, self.cache, 100)

    def test_missing_api_digest_downloads_once_per_asset_identity(self):
        self.asset["digest"] = None
        with mock.patch.object(HISTORY, "download", return_value=self.data) as download:
            HISTORY.cache_asset(self.asset, self.cache, 100)
            self.asset["digest"] = None
            self.assertEqual(HISTORY.cache_asset(self.asset, self.cache, 100), (self.data, True))
            self.assertEqual(download.call_count, 1)

    def test_replaced_api_asset_is_not_hidden_by_missing_digest_cache(self):
        self.asset["digest"] = None
        with mock.patch.object(HISTORY, "download", return_value=self.data):
            HISTORY.cache_asset(self.asset, self.cache, 100)
        self.asset.update(digest=None, updated_at="2026-11-12T00:00:00Z")
        with mock.patch.object(HISTORY, "download", return_value=b"replacement") as download:
            data, hit = HISTORY.cache_asset(self.asset, self.cache, 100)
            self.assertEqual(data, b"replacement")
            self.assertFalse(hit)
            self.assertEqual(download.call_count, 1)

    def test_acquisition_and_api_digest_failure_block_the_release(self):
        with mock.patch.object(HISTORY, "download", side_effect=RuntimeError("bounded acquisition failed")):
            with self.assertRaisesRegex(RuntimeError, "bounded acquisition failed"):
                HISTORY.cache_asset(self.asset, self.cache, 100)
        with mock.patch.object(HISTORY, "download", return_value=b"other bytes"):
            with self.assertRaisesRegex(ValueError, "digest changed"):
                HISTORY.cache_asset(self.asset, self.cache, 100)

    def test_release_and_asset_api_pages_are_both_exhausted(self):
        result = mock.Mock(stdout=json.dumps([[{"id": 1}], [{"id": 2}]]).encode())
        with mock.patch.object(HISTORY.subprocess, "run", return_value=result) as command:
            self.assertEqual(HISTORY.api("endpoint"), [{"id": 1}, {"id": 2}])
            self.assertIn("--paginate", command.call_args.args[0])
            self.assertIn("--slurp", command.call_args.args[0])

    def test_api_failure_keeps_original_exception_and_captured_streams(self):
        failure = HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], output=b"partial page", stderr=b"HTTP 503")
        timeout = HISTORY.subprocess.TimeoutExpired(["gh", "api"], 120, output=b"partial timeout", stderr=b"connection pending")
        for error in (failure, timeout):
            with mock.patch.object(HISTORY.subprocess, "run", side_effect=error):
                self.assert_api_failure(error)

    def assert_api_failure(self, error):
        with self.assertRaises(type(error)) as caught:
            HISTORY.api("endpoint")
        self.assertIs(caught.exception, error)
        self.assertIn(error.stdout.decode(), error.__notes__[0])
        self.assertIn(error.stderr.decode(), error.__notes__[1])

    def test_api_failure_bounds_retained_output(self):
        error = HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], output=b"x" * 40000, stderr=b"\xff")
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=error):
            with self.assertRaises(HISTORY.subprocess.CalledProcessError):
                HISTORY.api("endpoint")
        self.assertTrue(error.__notes__[0].endswith(" [truncated]"))
        self.assertLess(len(error.__notes__[0]), 33000)
        self.assertIn("\ufffd", error.__notes__[1])

    def test_truncated_embedded_assets_use_the_paginated_asset_api(self):
        assets = [{"name": f"asset-{index}.txt", "digest": "sha256:" + self.digest} for index in range(101)]
        item = {"tag_name": "v0.1.306", "id": 42, "published_at": "2026-11-11T00:00:00Z", "assets": assets[:100]}
        documents = self.cache / "documents"
        with mock.patch.object(HISTORY, "api", return_value=assets) as api:
            release, _ = HISTORY.acquire_release(item, self.cache, documents, threading.Event())
            self.assertEqual(len(release["assets"]), 101)
            self.assertEqual(release["assets"][-1]["name"], "asset-100.txt")
            self.assertEqual(api.call_args.args[0], "repos/ShaulLavo/mesh/releases/42/assets?per_page=100")

    def test_small_embedded_asset_list_does_not_skip_the_authoritative_index(self):
        assets = [{"name": "retained-page.json", "digest": "sha256:" + self.digest}]
        item = {"tag_name": "v0.1.306", "id": 42, "published_at": "2026-11-11T00:00:00Z", "assets": []}
        with mock.patch.object(HISTORY, "api", return_value=assets):
            release, _ = HISTORY.acquire_release(item, self.cache, self.cache / "documents", threading.Event())
            self.assertEqual(release["assets"][0]["name"], "retained-page.json")

    def signed_assets(self):
        item = {"tag_name": "v0.1.324", "id": 42, "target_commitish": "a" * 40, "draft": False,
                "published_at": "2026-11-15T00:00:00Z"}
        payloads = {name: ("exact physical " + name).encode() for name in HISTORY.release_documents.PHYSICAL}
        assets = [{"id": number + 1, "name": name, "state": "uploaded", "size": len(raw),
                   "digest": "sha256:" + hashlib.sha256(raw).hexdigest(), "browser_download_url": name}
                  for number, (name, raw) in enumerate(payloads.items())]
        assets.extend({"id": 10 + number, "name": name, "state": "uploaded", "size": 40,
                       "digest": "sha256:" + "b" * 64} for number, name in enumerate((
                           "mesh_linux_amd64.tar.gz", "mesh_linux_arm64.tar.gz", "mesh_darwin_arm64.tar.gz")))
        return item, assets, payloads

    def test_signed_history_preserves_the_public_manifest_byte_limit(self):
        self.assertEqual(HISTORY.LIMITS["mesh-release.json"], 1 << 20)
        item, assets, _payloads = self.signed_assets()
        raw = b" " * (1 << 20)
        asset = next(asset for asset in assets if asset["name"] == "mesh-release.json")
        asset.update(size=len(raw), digest="sha256:" + hashlib.sha256(raw).hexdigest())
        destination = self.cache / "capture"
        destination.mkdir()
        with mock.patch.object(HISTORY, "download", return_value=raw) as download:
            HISTORY.capture_history(item, [asset], self.cache, destination, threading.Event())
            self.assertEqual((destination / "mesh-release.json").read_bytes(), raw)
            self.assertEqual(download.call_args.args[1], len(raw))
            asset["size"] += 1
            with self.assertRaisesRegex(ValueError, "actual uploaded API"):
                HISTORY.capture_history(item, [asset], self.cache, destination, threading.Event())
        self.assertEqual(download.call_count, 1)

    def materialize_control(self, count):
        def materialize(capture, release, destination, _inventory):
            snapshot = json.loads((capture / "history.json").read_bytes())
            self.assertIs(snapshot["draft"], False)
            self.assertNotIn("intake_run", snapshot)
            self.assertEqual({item["name"] for item in snapshot["assets"]}, set(HISTORY.release_documents.PHYSICAL))
            self.assertEqual({path.name for path in capture.iterdir()}, set(HISTORY.release_documents.PHYSICAL) | {"history.json"})
            destination.mkdir()
            names = list(HISTORY.release_documents.FIXED) + [HISTORY.release_documents.ENVELOPE]
            names.extend(f"{number:064x}.worker-group.json" for number in range(count))
            for name in names:
                (destination / name).write_bytes(b"{}")
            manifest = {"version": release["tag_name"], "commit": release["target_commitish"], "artifacts": [
                {"archive": name, "sha256": "b" * 64} for name in ("mesh_linux_amd64.tar.gz", "mesh_linux_arm64.tar.gz", "mesh_darwin_arm64.tar.gz")]}
            (destination / "mesh-release.json").write_text(json.dumps(manifest))
            return {"schema": 1, "binding": {"version": release["tag_name"], "source": release["target_commitish"], "releaseId": release["id"], "writerRun": 77},
                "documents": [{"name": name, "size": (destination / name).stat().st_size,
                               "sha256": hashlib.sha256((destination / name).read_bytes()).hexdigest()} for name in names]}
        return materialize

    def test_signed_history_constant_four_metadata_downloads_and_logical_updater_identity(self):
        item, assets, payloads = self.signed_assets()
        for count in (1, 1000):
            output = self.cache / ("signed-" + str(count))
            with mock.patch.object(HISTORY, "api", return_value=assets) as api, \
                    mock.patch.object(HISTORY, "download", side_effect=lambda url, _limit: payloads[url]) as download, \
                    mock.patch.object(HISTORY.release_tool, "materialize_history", side_effect=self.materialize_control(count)), \
                    mock.patch.object(HISTORY.release_worker_index, "validate_index") as semantic:
                result, _hits = HISTORY.acquire_release(item, self.cache, output, threading.Event())
            self.assertEqual(api.call_count, 1)
            self.assertEqual(download.call_count, 4 if count == 1 else 0)
            self.assertEqual(len(result["assets"]), count + 10)
            logical = {entry["name"] for entry in result["assets"]}
            self.assertIn("mesh-update.json", logical)
            self.assertNotIn(HISTORY.release_documents.BUNDLE, logical)
            self.assertTrue(all("id" not in entry for entry in result["assets"]))
            self.assertEqual(json.loads((output / (item["tag_name"] + ".physical.json")).read_bytes())["assets"], assets)
            semantic.assert_called_once()

    def test_flat_signed_or_failed_current_bundle_never_becomes_unsigned_history(self):
        item, assets, payloads = self.signed_assets()
        without = [asset for asset in assets if asset["name"] != HISTORY.release_documents.BUNDLE]
        with mock.patch.object(HISTORY, "api", return_value=without), self.assertRaisesRegex(ValueError, "current authenticated bundle"):
            HISTORY.acquire_release(item, self.cache, self.cache / "refused-flat", threading.Event())
        with mock.patch.object(HISTORY, "api", return_value=assets), mock.patch.object(HISTORY, "download", side_effect=lambda url, _limit: payloads[url]), \
                mock.patch.object(HISTORY.release_tool, "materialize_history", side_effect=ValueError("untrusted bundle")):
            with self.assertRaisesRegex(ValueError, "untrusted bundle"):
                HISTORY.acquire_release(item, self.cache, self.cache / "refused-bundle", threading.Event())
        self.assertFalse((self.cache / "refused-bundle" / item["tag_name"]).exists())

    def test_native_hash_mismatch_and_missing_actual_metadata_pin_are_refused(self):
        item, assets, _payloads = self.signed_assets()
        assets[0]["digest"] = None
        with mock.patch.object(HISTORY, "api", return_value=assets), self.assertRaisesRegex(ValueError, "actual uploaded API"):
            HISTORY.acquire_release(item, self.cache, self.cache / "missing-pin", threading.Event())
        with self.assertRaisesRegex(ValueError, "native inventory"):
            HISTORY.history_references([], {"artifacts": [{"archive": "native.tar.gz", "sha256": "b" * 64}]},
                                       [{"name": "native.tar.gz", "digest": "sha256:" + "c" * 64}])


if __name__ == "__main__":
    unittest.main()
