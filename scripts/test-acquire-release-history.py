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


if __name__ == "__main__":
    unittest.main()
