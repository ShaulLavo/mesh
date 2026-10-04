"""Public archive identities and joined historical bridge receipts."""
import io
import json
import socket
import ssl
import sys
import tarfile
import time
import urllib.error
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

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
        self.assertEqual(len(self.requests), 2)
        self.assertFalse(any(path.is_file() for path in self.root.rglob("*")))

    def test_executable_digest_mismatch_refuses_execution(self):
        self.manifests[0]["artifacts"][0]["binarySha256"] = "0" * 64
        with self.assertRaisesRegex(RuntimeError, "executable digest mismatch"):
            self.acquire()
        self.assertEqual(len(self.requests), 2)
        self.assertFalse(any(path.is_file() for path in self.root.rglob("*")))

    def test_manifest_schema_mismatch_is_not_retried_or_cached(self):
        self.manifests[0]["schema"] = 2
        with self.assertRaisesRegex(RuntimeError, "schema mismatch"):
            self.acquire()
        self.assertEqual(len(self.requests), 1)
        self.assertFalse(any(path.is_file() for path in self.root.rglob("*")))

    def test_invalid_manifest_content_is_not_retried_or_cached(self):
        def fetcher(address, _maximum):
            self.requests.append(address)
            return b"invalid manifest JSON"
        with self.assertRaises(json.JSONDecodeError):
            published.acquire(self.root, self.platform, fetcher)
        self.assertEqual(len(self.requests), 1)
        self.assertFalse(any(path.is_file() for path in self.root.rglob("*")))

    def test_acquisition_receipt_mismatch_publishes_no_inputs(self):
        data, transition, _compatibility = self.receipts[0]
        self.assets[published.ORIGIN + published.VERSIONS[1] + "/" + transition["proof"] + ".json"] = data + b" "
        with self.assertRaisesRegex(RuntimeError, "receipt digest mismatch"):
            self.acquire()
        self.assertEqual(len(self.requests), len(set(self.requests)))
        self.assertFalse(any(path.is_file() for path in self.root.rglob("*")))

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


class ControlledResponse:
    def __init__(self, control, kind):
        self.control = control
        self.kind = kind
        self.url = "http://fixture.invalid/asset" if kind == "redirect" else published.ORIGIN + "fixture"

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def close(self):
        state = json.loads(self.control.read_text())
        state["closed"] = state.get("closed", 0) + 1
        self.control.write_text(json.dumps(state))

    def read(self, maximum):
        if self.kind == "stall-read":
            time.sleep(1)
        if self.kind == "partial-reset":
            raise ConnectionResetError("fixture partial response reset")
        return b"x" * maximum if self.kind == "oversize" else b"verified fixture bytes"


class ControlledOpener:
    def __init__(self, control, handlers):
        self.control = control
        self.handlers = handlers

    def open(self, address, timeout):
        state = json.loads(self.control.read_text())
        attempt = len(state["attempts"])
        state["attempts"].append(time.monotonic())
        state["timeouts"].append(timeout)
        state["trusted"] = all(handler.proxies == {} for handler in self.handlers
            if isinstance(handler, urllib.request.ProxyHandler)) and all(
            handler._context.verify_mode == ssl.CERT_REQUIRED and handler._context.check_hostname
            for handler in self.handlers if isinstance(handler, urllib.request.HTTPSHandler))
        self.control.write_text(json.dumps(state))
        kind = state["outcomes"][min(attempt, len(state["outcomes"]) - 1)]
        if kind == "dns":
            raise urllib.error.URLError(socket.gaierror(socket.EAI_NONAME, "fixture DNS failure"))
        if kind == "nested-dns":
            raise urllib.error.URLError(urllib.error.URLError(socket.gaierror(socket.EAI_AGAIN, "fixture DNS failure")))
        if kind == "reset":
            raise urllib.error.URLError(ConnectionResetError("fixture connection reset"))
        if kind == "tls":
            raise urllib.error.URLError(ssl.SSLCertVerificationError("fixture certificate rejected"))
        if kind.startswith("http-"):
            raise urllib.error.HTTPError(address, int(kind[5:]), "fixture HTTP failure", {}, ControlledResponse(self.control, kind))
        if kind == "stall-open":
            time.sleep(1)
        return ControlledResponse(self.control, kind)


class BoundedDownloadTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.control = self.root / "control.json"
        self.worker = self.root / "external-provider.py"
        source = Path(__file__).resolve()
        self.worker.write_text(f'''import runpy
import sys
from pathlib import Path
sys.path.insert(0, {str(source.parent)!r})
from fixtures import published_releases as published
provider = runpy.run_path({str(source)!r}, run_name="external_fetch_fixture")
published.urllib.request.build_opener = lambda *handlers: provider["ControlledOpener"](Path({str(self.control)!r}), handlers)
published.download_worker(*sys.argv[1:])
''')
        self.addCleanup(patch.stopall)
        patch.object(published, "DOWNLOAD_WORKER", (sys.executable, str(self.worker)), create=True).start()
        patch.object(published, "DOWNLOAD_BUDGET", 3.0, create=True).start()
        patch.object(published, "DOWNLOAD_BACKOFF", (0.02, 0.04), create=True).start()
        patch.object(published.urllib.request, "build_opener",
            side_effect=lambda *handlers: ControlledOpener(self.control, handlers)).start()

    def state(self):
        return json.loads(self.control.read_text())

    def download(self, outcomes):
        self.control.write_text(json.dumps({"outcomes": outcomes, "attempts": [], "timeouts": []}))
        return published.download(published.ORIGIN + "fixture", 64)

    def test_known_good_trusted_download_closes_response(self):
        self.assertEqual(self.download(["good"]), b"verified fixture bytes")
        self.assertEqual(len(self.state()["attempts"]), 1)
        self.assertEqual(self.state()["closed"], 1)
        self.assertTrue(self.state()["trusted"])

    def test_transient_dns_reset_and_eligible_server_failures_recover(self):
        for kind in ("dns", "nested-dns", "reset", "partial-reset", "http-500", "http-502", "http-503", "http-504"):
            with self.subTest(kind=kind):
                self.assertEqual(self.download([kind, "good"]), b"verified fixture bytes")
                attempts = self.state()["attempts"]
                self.assertEqual(len(attempts), 2)
                self.assertGreaterEqual(attempts[1] - attempts[0], 0.02)
                self.assertLess(self.state()["timeouts"][1], self.state()["timeouts"][0])
                self.assertTrue(self.state()["trusted"])
                expected_closed = 2 if kind == "partial-reset" or kind.startswith("http-") else 1
                self.assertEqual(self.state()["closed"], expected_closed)

    def test_exhaustion_is_finite_and_does_not_cache(self):
        self.control.write_text(json.dumps({"outcomes": ["dns"], "attempts": [], "timeouts": []}))
        root = self.root / "assets"
        (root / published.VERSIONS[0]).mkdir(parents=True)
        with self.assertRaisesRegex(Exception, "DNS failure"):
            published.fetch(root, published.VERSIONS[0], "mesh-release.json", 64, published.download)
        attempts = self.state()["attempts"]
        self.assertEqual(len(attempts), 3)
        self.assertGreaterEqual(attempts[1] - attempts[0], 0.02)
        self.assertGreaterEqual(attempts[2] - attempts[1], 0.04)
        self.assertEqual(list(root.rglob("*.*")), [])

    def test_permanent_http_tls_redirect_and_size_failures_are_not_retried(self):
        for kind in ("http-400", "http-403", "http-404", "http-429", "http-501", "http-505", "tls", "redirect", "oversize"):
            with self.subTest(kind=kind):
                with self.assertRaises(Exception):
                    self.download([kind, "good"])
                self.assertEqual(len(self.state()["attempts"]), 1)
                self.assertTrue(self.state()["trusted"])
                expected_closed = 0 if kind == "tls" else 1
                self.assertEqual(self.state().get("closed", 0), expected_closed)

    def test_total_budget_bounds_stalled_dns_and_response_read(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.3, create=True).start()
        for kind in ("stall-open", "stall-read"):
            with self.subTest(kind=kind):
                started = time.monotonic()
                with self.assertRaisesRegex(TimeoutError, "total deadline"):
                    self.download([kind])
                self.assertLess(time.monotonic() - started, 0.8)
                self.assertEqual(len(self.state()["attempts"]), 1)
                state = self.control.read_bytes()
                time.sleep(0.05)
                self.assertEqual(self.control.read_bytes(), state)

    def test_backoff_cannot_escape_total_budget(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.3, create=True).start()
        patch.object(published, "DOWNLOAD_BACKOFF", (1, 1), create=True).start()
        started = time.monotonic()
        with self.assertRaisesRegex(TimeoutError, "total deadline"):
            self.download(["dns", "good"])
        self.assertLess(time.monotonic() - started, 0.8)
        self.assertEqual(len(self.state()["attempts"]), 1)


if __name__ == "__main__":
    unittest.main()
