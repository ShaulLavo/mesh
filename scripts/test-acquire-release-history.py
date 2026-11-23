#!/usr/bin/env python3
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import threading
import unittest
import urllib.parse
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

    def page(self, items):
        return mock.Mock(stdout=json.dumps(items).encode())

    def server_error(self, status):
        return HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], stderr=f"gh: HTTP {status}: Server Error".encode())

    def test_api_reads_pages_until_a_partial_page(self):
        first = [self.page([{"id": 1}, {"id": 2}]), self.page([{"id": 3}, {"id": 4}]), self.page([{"id": 5}])]
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=first + first) as command:
            self.assertEqual(HISTORY.api("endpoint", 2), [{"id": number} for number in range(1, 6)])
        # A second full read confirms that no page boundary moved during the first.
        self.assertEqual([call.args[0] for call in command.call_args_list],
                         [["gh", "api", f"endpoint?per_page=2&page={page}"] for page in (1, 2, 3, 1, 2, 3)])

    def test_api_answers_a_single_page_with_one_request(self):
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=[self.page([{"id": 1}])]) as command:
            self.assertEqual(HISTORY.api("endpoint", 2), [{"id": 1}])
        command.assert_called_once()

    def test_api_retries_server_errors_on_the_failed_page_only(self):
        outcomes = [self.page([{"id": 1}]), self.server_error(502), self.server_error(503), self.page([]),
                    self.page([{"id": 1}]), self.page([])]
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=outcomes) as command, \
                mock.patch.object(HISTORY.time, "sleep") as sleep:
            self.assertEqual(HISTORY.api("endpoint", 1), [{"id": 1}])
        self.assertEqual([call.args[0][2] for call in command.call_args_list],
                         ["endpoint?per_page=1&page=1"] + ["endpoint?per_page=1&page=2"] * 3
                         + ["endpoint?per_page=1&page=1", "endpoint?per_page=1&page=2"])
        self.assertEqual([call.args[0] for call in sleep.call_args_list], list(HISTORY.SERVER_ERROR_RETRIES[:2]))

    def listing(self, items, mutations):
        """Serve items newest first by offset, like the GitHub list API, applying
        mutations[n](items) right after the n-th request has been answered."""
        requests = []

        def run(command, **_):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(command[2]).query)
            size, page = int(query["per_page"][0]), int(query["page"][0])
            batch = items[(page - 1) * size:page * size]
            requests.append(page)
            if len(requests) in mutations:
                mutations[len(requests)](items)
            return mock.Mock(stdout=json.dumps(batch).encode())
        return run, requests

    def test_api_does_not_skip_a_release_deleted_between_pages(self):
        releases = [{"id": number} for number in range(10, 0, -1)]
        run, _ = self.listing(releases, {1: lambda items: items.pop(1)})
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=run):
            found = HISTORY.api("endpoint", 4)
        # Release 9 was deleted after the first page; release 6 still exists and
        # slides into the first page's range, so a single pass would miss it.
        self.assertEqual([item["id"] for item in found], [10, 8, 7, 6, 5, 4, 3, 2, 1])

    def test_api_rereads_when_a_release_is_published_between_pages(self):
        releases = [{"id": number} for number in range(10, 0, -1)]
        run, _ = self.listing(releases, {1: lambda items: items.insert(0, {"id": 11})})
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=run):
            found = HISTORY.api("endpoint", 4)
        self.assertEqual([item["id"] for item in found], list(range(11, 0, -1)))

    def test_api_does_not_accept_a_read_where_a_published_release_came_and_went(self):
        releases = [{"id": number} for number in range(6, 0, -1)]
        # A release appears before page 2 (so page 2 repeats id 5) and is gone
        # before page 3, which would hide release 3 from a read that only deduplicated.
        # It happens in both of the first two reads, so they would otherwise agree.
        run, _ = self.listing(releases, {1: lambda items: items.insert(0, {"id": 100}), 2: lambda items: items.pop(0),
                                         3: lambda items: items.insert(0, {"id": 101}), 4: lambda items: items.pop(0)})
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=run):
            found = HISTORY.api("endpoint", 2)
        self.assertEqual([item["id"] for item in found], [6, 5, 4, 3, 2, 1])

    def test_api_rereads_when_a_draft_is_published_between_reads(self):
        releases = [{"id": 2, "draft": True}, {"id": 1, "draft": False}]
        run, _ = self.listing(releases, {5: lambda items: items[0].update(draft=False)})
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=run):
            found = HISTORY.api("endpoint", 1)
        self.assertEqual([(item["id"], item["draft"]) for item in found], [(2, False), (1, False)])

    def test_api_refuses_a_page_that_repeats_an_id(self):
        repeated = self.page([{"id": 1}, {"id": 1}])
        with mock.patch.object(HISTORY.subprocess, "run", return_value=repeated) as command:
            with self.assertRaisesRegex(RuntimeError, "did not hold still"):
                HISTORY.api("endpoint", 2)
        self.assertEqual(command.call_count, HISTORY.LISTING_READS)

    def test_api_refuses_a_listing_that_never_holds_still(self):
        releases = [{"id": number} for number in range(1000, 0, -1)]
        churn = {number: (lambda items: items.pop(1)) for number in range(1, 200, 2)}
        run, _ = self.listing(releases, churn)
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=run):
            with self.assertRaisesRegex(RuntimeError, "did not hold still"):
                HISTORY.api("endpoint", 30)

    def test_api_gives_up_after_bounded_server_error_retries(self):
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=self.server_error(502)) as command, \
                mock.patch.object(HISTORY.time, "sleep"):
            with self.assertRaises(HISTORY.subprocess.CalledProcessError) as caught:
                HISTORY.api("endpoint", 30)
        self.assertEqual(command.call_count, len(HISTORY.SERVER_ERROR_RETRIES) + 1)
        self.assertIn("HTTP 502", caught.exception.__notes__[1])

    def test_api_does_not_retry_client_errors_or_timeouts(self):
        client = HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], stderr=b"gh: HTTP 404: Not Found")
        timeout = HISTORY.subprocess.TimeoutExpired(["gh", "api"], 120)
        for error in (client, timeout):
            with mock.patch.object(HISTORY.subprocess, "run", side_effect=error) as command, \
                    mock.patch.object(HISTORY.time, "sleep") as sleep:
                with self.assertRaises(type(error)):
                    HISTORY.api("endpoint", 30)
            command.assert_called_once()
            sleep.assert_not_called()

    def test_api_failure_keeps_original_exception_and_captured_streams(self):
        failure = HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], output=b"partial page", stderr=b"HTTP 404")
        timeout = HISTORY.subprocess.TimeoutExpired(["gh", "api"], 120, output=b"partial timeout", stderr=b"connection pending")
        for error in (failure, timeout):
            with mock.patch.object(HISTORY.subprocess, "run", side_effect=error):
                self.assert_api_failure(error)

    def assert_api_failure(self, error):
        with self.assertRaises(type(error)) as caught:
            HISTORY.api("endpoint", 30)
        self.assertIs(caught.exception, error)
        self.assertIn(error.stdout.decode(), error.__notes__[0])
        self.assertIn(error.stderr.decode(), error.__notes__[1])

    def test_api_failure_bounds_retained_output(self):
        error = HISTORY.subprocess.CalledProcessError(1, ["gh", "api"], output=b"x" * 40000, stderr=b"\xff")
        with mock.patch.object(HISTORY.subprocess, "run", side_effect=error):
            with self.assertRaises(HISTORY.subprocess.CalledProcessError):
                HISTORY.api("endpoint", 30)
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
            self.assertEqual(api.call_args.args, ("repos/ShaulLavo/mesh/releases/42/assets", HISTORY.ASSET_PAGE_SIZE))

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
