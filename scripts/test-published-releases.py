"""Public archive identities and joined historical bridge receipts."""
import http.server
import io
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
import urllib.error
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
        state["workerPID"] = os.getpid()
        state["trusted"] = len(self.handlers) == 3 and all(handler.proxies == {} for handler in self.handlers
            if isinstance(handler, urllib.request.ProxyHandler)) and all(
            handler._context.verify_mode == ssl.CERT_REQUIRED and handler._context.check_hostname
            for handler in self.handlers if isinstance(handler, urllib.request.HTTPSHandler))
        self.control.write_text(json.dumps(state))
        kind = state["outcomes"][min(attempt, len(state["outcomes"]) - 1)]
        if kind == "dns":
            raise urllib.error.URLError(socket.gaierror(socket.EAI_NONAME, "fixture DNS failure"))
        if kind == "nested-dns":
            raise urllib.error.URLError(urllib.error.URLError(socket.gaierror(socket.EAI_AGAIN, "fixture DNS failure")))
        if kind == "dns-permanent":
            raise urllib.error.URLError(socket.gaierror(socket.EAI_FAMILY, "fixture unsupported address family"))
        if kind == "reset":
            raise urllib.error.URLError(ConnectionResetError("fixture connection reset"))
        if kind == "tls":
            raise urllib.error.URLError(ssl.SSLCertVerificationError("fixture certificate rejected"))
        if kind in ("redirect-reset", "https-redirect"):
            handler = next(entry for entry in self.handlers if isinstance(entry, urllib.request.HTTPRedirectHandler))
            destination = "http://fixture.invalid/asset" if kind == "redirect-reset" else "https://fixture.invalid/asset"
            response = ControlledResponse(self.control, kind)
            redirected = handler.redirect_request(urllib.request.Request(address), response, 302, "fixture redirect", {}, destination)
            if kind == "redirect-reset":
                raise urllib.error.URLError(ConnectionResetError("fixture insecure redirect reset"))
            if redirected.full_url != destination:
                raise RuntimeError("fixture HTTPS redirect rejected")
            response.close()
        if kind == "redirect-http-503":
            raise urllib.error.HTTPError("http://fixture.invalid/asset", 503, "fixture insecure redirect", {}, ControlledResponse(self.control, kind))
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
        self.ready = self.root / "ready"
        self.gate = self.root / "gate"
        self.worker.write_text(f'''import runpy
import sys
import time
from pathlib import Path
if sys.argv[1] == "--slow-start":
    time.sleep(1)
    del sys.argv[1]
sys.path.insert(0, {str(source.parent)!r})
from fixtures import published_releases as published
provider = runpy.run_path({str(source)!r}, run_name="external_fetch_fixture")
published.urllib.request.build_opener = lambda *handlers: provider["ControlledOpener"](Path({str(self.control)!r}), handlers)
context = published.ssl.create_default_context()
published.ssl.create_default_context = lambda: context
if Path({str(self.gate)!r}).exists():
    Path({str(self.ready)!r}).touch()
    while Path({str(self.gate)!r}).exists():
        time.sleep(0.005)
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

    def ready_worker(self):
        # Phase tests start their short budget after imports and trust-store loading.
        self.ready.unlink(missing_ok=True)
        self.gate.touch()
        worker = subprocess.Popen((*published.DOWNLOAD_WORKER, published.ORIGIN + "fixture", "64",
            str(published.DOWNLOAD_BUDGET)), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        def cleanup():
            if worker.poll() is None:
                worker.kill()
            worker.communicate()
        self.addCleanup(cleanup)
        deadline = time.monotonic() + 10
        while not self.ready.exists():
            self.assertIsNone(worker.poll(), "fixture worker exited before readiness")
            self.assertLess(time.monotonic(), deadline, "fixture worker startup exceeded 10 seconds")
            time.sleep(0.005)
        return worker

    def release_worker(self, worker):
        def launch(*args, **kwargs):
            self.gate.unlink()
            return worker
        return patch.object(published.subprocess, "Popen", side_effect=launch)

    def test_known_good_trusted_download_closes_response(self):
        self.assertEqual(self.download(["good"]), b"verified fixture bytes")
        self.assertEqual(len(self.state()["attempts"]), 1)
        self.assertEqual(self.state()["closed"], 1)
        self.assertTrue(self.state()["trusted"])

    def test_https_redirect_keeps_standard_trusted_acquisition(self):
        self.assertEqual(self.download(["https-redirect"]), b"verified fixture bytes")
        self.assertEqual(len(self.state()["attempts"]), 1)
        self.assertEqual(self.state()["closed"], 2)
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
        with self.assertRaisesRegex(RuntimeError, "DNS failure"):
            published.fetch(root, published.VERSIONS[0], "mesh-release.json", 64, published.download)
        attempts = self.state()["attempts"]
        self.assertEqual(len(attempts), 3)
        self.assertGreaterEqual(attempts[1] - attempts[0], 0.02)
        self.assertGreaterEqual(attempts[2] - attempts[1], 0.04)
        self.assertFalse(any(path.is_file() for path in root.rglob("*")))

    def test_permanent_http_tls_redirect_and_size_failures_are_not_retried(self):
        for kind in ("http-400", "http-403", "http-404", "http-429", "http-501", "http-505", "tls", "dns-permanent", "redirect", "redirect-http-503", "redirect-reset", "oversize"):
            with self.subTest(kind=kind):
                with self.assertRaises(RuntimeError):
                    self.download([kind, "good"])
                self.assertEqual(len(self.state()["attempts"]), 1)
                self.assertTrue(self.state()["trusted"])
                expected_closed = 0 if kind in ("tls", "dns-permanent") else 1
                self.assertEqual(self.state().get("closed", 0), expected_closed)

    def test_total_budget_bounds_stalled_dns_and_response_read(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.3, create=True).start()
        for kind in ("stall-open", "stall-read"):
            with self.subTest(kind=kind):
                worker = self.ready_worker()
                started = time.monotonic()
                with self.release_worker(worker), self.assertRaisesRegex(TimeoutError, "total deadline"):
                    self.download([kind])
                self.assertLess(time.monotonic() - started, 0.6)
                self.assertEqual(len(self.state()["attempts"]), 1)
                with self.assertRaises(ProcessLookupError):
                    os.kill(self.state()["workerPID"], 0)
                state = self.control.read_bytes()
                time.sleep(0.05)
                self.assertEqual(self.control.read_bytes(), state)

    def test_worker_startup_consumes_total_budget(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.3, create=True).start()
        command = (*published.DOWNLOAD_WORKER, "--slow-start")
        workers = []
        popen = subprocess.Popen
        def launch(*args, **kwargs):
            worker = popen(*args, **kwargs)
            workers.append(worker)
            return worker
        started = time.monotonic()
        with patch.object(published, "DOWNLOAD_WORKER", command), \
                patch.object(published.subprocess, "Popen", side_effect=launch), \
                self.assertRaisesRegex(TimeoutError, "total deadline"):
            self.download(["good"])
        self.assertLess(time.monotonic() - started, 0.8)
        self.assertEqual(self.state()["attempts"], [])
        self.assertEqual(len(workers), 1)
        self.assertIsNotNone(workers[0].poll())
        with self.assertRaises(ProcessLookupError):
            os.kill(workers[0].pid, 0)

    def test_worker_launch_consumes_total_budget(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.2, create=True).start()
        popen = subprocess.Popen
        def slow_launch(*args, **kwargs):
            worker = popen(*args, **kwargs)
            time.sleep(0.3)
            return worker
        started = time.monotonic()
        with patch.object(published.subprocess, "Popen", side_effect=slow_launch), \
                self.assertRaisesRegex(TimeoutError, "total deadline"):
            self.download(["stall-open"])
        self.assertLess(time.monotonic() - started, 0.45)

    def test_backoff_cannot_escape_total_budget(self):
        patch.object(published, "DOWNLOAD_BUDGET", 0.3, create=True).start()
        patch.object(published, "DOWNLOAD_BACKOFF", (1, 1), create=True).start()
        worker = self.ready_worker()
        started = time.monotonic()
        with self.release_worker(worker), self.assertRaisesRegex(TimeoutError, "total deadline"):
            self.download(["dns", "good"])
        self.assertLess(time.monotonic() - started, 0.6)
        self.assertEqual(len(self.state()["attempts"]), 1)


class LocalReleaseServer:
    """A real HTTPS server that answers each request with the next scripted outcome."""

    def __init__(self, root):
        self.root = root
        self.outcomes = []
        self.requests = []
        (root / "server.cnf").write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
            "[dn]\nCN=127.0.0.1\n[ext]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:FALSE\n"
            "extendedKeyUsage=serverAuth\n")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "server.key",
            "-out", "server.pem", "-days", "1", "-config", "server.cnf"], cwd=root, check=True, capture_output=True, timeout=30)
        server = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                server.requests.append(time.monotonic())
                outcome = server.outcomes[min(len(server.requests), len(server.outcomes)) - 1]
                if outcome == "stall":
                    time.sleep(8)
                    return
                body = b"verified fixture bytes" if outcome == 200 else b"fixture failure"
                self.send_response(outcome)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(root / "server.pem", root / "server.key")
        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.httpd.daemon_threads = True
        self.httpd.socket = context.wrap_socket(self.httpd.socket, server_side=True)
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()
        self.address = f"https://127.0.0.1:{self.httpd.server_address[1]}/ShaulLavo/mesh/releases/download/v0.1.151/mesh_linux_amd64.tar.gz"

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()
        self.thread.join(timeout=5)


@unittest.skipUnless(shutil.which("openssl"), "openssl is required to issue the local HTTPS certificate")
class LocalServerRetryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.server = LocalReleaseServer(root)
        self.addCleanup(self.server.close)
        source = Path(__file__).resolve()
        worker = root / "trusted-worker.py"
        # The worker keeps production acquisition and trusts only the local certificate.
        worker.write_text(f'''import ssl
import sys
sys.path.insert(0, {str(source.parent)!r})
from fixtures import published_releases as published
context = ssl.create_default_context(cafile={str(root / "server.pem")!r})
published.ssl.create_default_context = lambda: context
published.download_worker(*sys.argv[1:])
''')
        self.addCleanup(patch.stopall)
        patch.object(published, "DOWNLOAD_WORKER", (sys.executable, str(worker))).start()
        patch.object(published, "DOWNLOAD_BUDGET", 20.0).start()
        patch.object(published, "ATTEMPT_LIMIT", 3.0).start()
        patch.object(published, "DOWNLOAD_BACKOFF", (0.1, 0.2)).start()

    def download(self, outcomes):
        self.server.outcomes = outcomes
        started = time.monotonic()
        try:
            return published.download(self.server.address, 64)
        finally:
            self.elapsed = time.monotonic() - started

    def test_server_error_then_success_recovers(self):
        self.assertEqual(self.download([500, 200]), b"verified fixture bytes")
        self.assertEqual(len(self.server.requests), 2)
        self.assertGreaterEqual(self.server.requests[1] - self.server.requests[0], 0.1)

    def test_stalled_attempt_is_retried_within_budget(self):
        self.assertEqual(self.download(["stall", 200]), b"verified fixture bytes")
        self.assertEqual(len(self.server.requests), 2)
        self.assertLess(self.elapsed, published.DOWNLOAD_BUDGET)

    def test_persistent_server_error_fails_after_every_attempt_within_budget(self):
        with self.assertRaises(RuntimeError) as failure:
            self.download([500])
        self.assertEqual(len(self.server.requests), len(published.DOWNLOAD_BACKOFF) + 1)
        self.assertLess(self.elapsed, published.DOWNLOAD_BUDGET)
        message = str(failure.exception)
        for detail in ("v0.1.151/mesh_linux_amd64.tar.gz", "127.0.0.1", "after 3 attempts", "HTTP Error 500"):
            self.assertIn(detail, message)

    def test_missing_asset_fails_without_retry(self):
        with self.assertRaisesRegex(RuntimeError, r"v0\.1\.151/mesh_linux_amd64\.tar\.gz from 127\.0\.0\.1 after 1 attempt: .*HTTP Error 404"):
            self.download([404, 200])
        self.assertEqual(len(self.server.requests), 1)


if __name__ == "__main__":
    unittest.main()
