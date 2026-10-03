#!/usr/bin/env python3
import hashlib
import http.server
import os
from pathlib import Path
import re
import shlex
import shutil
import socket
import struct
import subprocess
import tempfile
import threading
import unittest


PAYLOAD = b"pinned CI tool fixture\n"
DIGEST = hashlib.sha256(PAYLOAD).hexdigest()
WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/ci.yml"
DOWNLOADS = re.findall(
    r'(curl [^\n]+)\n\s+(echo "([a-f0-9]{64})  [^\n]+\| sha256sum -c -)',
    WORKFLOW.read_text(),
)


class DownloadFixture(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.attempts += 1
        if self.server.attempts <= self.server.resets:
            self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
            self.connection.close()
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(PAYLOAD)))
        self.end_headers()
        self.wfile.write(PAYLOAD)

    def log_message(self, *args):
        pass


@unittest.skipUnless(
    os.name == "posix" and all(shutil.which(tool) for tool in ("bash", "curl", "sha256sum")),
    "download fixtures require POSIX sockets, Bash, curl, and sha256sum",
)
class DownloadTests(unittest.TestCase):
    def run_download(self, download, resets, digest=DIGEST):
        command, checksum, pinned_digest = download
        server = http.server.HTTPServer(("127.0.0.1", 0), DownloadFixture)
        server.attempts = 0
        server.resets = resets
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}/tool"
            command = re.sub(r"https://\S+", url, command)
            checksum = checksum.replace(pinned_digest, digest)
            with tempfile.TemporaryDirectory(prefix="mesh-ci-download-") as scratch:
                result = subprocess.run(
                    ["bash", "-euc", command + "\n" + checksum],
                    env={**os.environ, "RUNNER_TEMP": scratch, "NO_PROXY": "127.0.0.1"},
                    capture_output=True,
                    text=True,
                    timeout=140,
                )
            return result, server.attempts
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_reset_recovers(self):
        self.assertEqual(len(DOWNLOADS), 3)
        for download in DOWNLOADS:
            with self.subTest(command=download[0]):
                result, attempts = self.run_download(download, resets=1)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(attempts, 2)
                self.assertIn(": OK", result.stdout)
                self.assertIn("curl: (56)", result.stderr)

    def test_reset_exhausts(self):
        for download in DOWNLOADS:
            with self.subTest(command=download[0]):
                result, attempts = self.run_download(download, resets=10)
                self.assertEqual(result.returncode, 56, result.stderr)
                self.assertEqual(attempts, 4)
                self.assertEqual(result.stdout, "")

    def test_checksum_rejects(self):
        for download in DOWNLOADS:
            with self.subTest(command=download[0]):
                result, attempts = self.run_download(download, resets=0, digest="0" * 64)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(attempts, 1)
                self.assertIn(": FAILED", result.stdout)

    def test_retry_bounds(self):
        for command, _, _ in DOWNLOADS:
            args = shlex.split(command)
            self.assertIn("--retry-all-errors", args)
            for flag, value in (("--retry", "3"), ("--retry-max-time", "120"),
                                ("--connect-timeout", "10"), ("--max-time", "30")):
                self.assertEqual(args[args.index(flag) + 1], value)
            self.assertNotIn("--retry-delay", args)


if __name__ == "__main__":
    unittest.main()
