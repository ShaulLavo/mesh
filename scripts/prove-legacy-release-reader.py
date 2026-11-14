#!/usr/bin/env python3
"""Make the original v0.1.249 CLI decode the generated schema 1 assets."""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import signal
import socketserver
import ssl
import subprocess
import tempfile
import threading
import time

from fixtures import tls


class Origin(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


class Response(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.reads.append(self.path)
        data = self.server.assets.get(self.path)
        if data is None:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


class Proxy(http.server.BaseHTTPRequestHandler):
    def do_CONNECT(self):
        if self.path not in ("github.com:443", "api.github.com:443"):
            self.send_error(403)
            return
        self.send_response(200)
        self.end_headers()
        try:
            with self.server.context.wrap_socket(self.connection, server_side=True) as secure:
                Response(secure, self.client_address, self.server)
        except (ConnectionResetError, BrokenPipeError, ssl.SSLError):
            pass
        self.close_connection = True

    def log_message(self, *_):
        pass


def asset_path(version, name):
    return f"/ShaulLavo/mesh/releases/download/{version}/{name}"


def inspect(binary, manifest, environment, output):
    result = subprocess.run([str(binary), "update", "--check", "--local", "--json", "--details",
                             "--version", manifest["version"]], env=environment,
                            capture_output=True, timeout=30, check=True)
    output.write_bytes(result.stdout)
    value = json.loads(result.stdout)
    if value["release"] != manifest:
        raise ValueError("original decoder returned a different manifest")
    return value


def wait_daemon(binary, environment, process):
    for _ in range(120):
        if process.poll() is not None:
            raise RuntimeError("original reader daemon exited during setup")
        result = subprocess.run([str(binary), "ls", "--daemon"], env=environment,
                                capture_output=True, timeout=5, check=False)
        if result.returncode == 0:
            return
        time.sleep(0.05)
    raise RuntimeError("original reader daemon did not start within its existing proof budget")


def exercise(args, root, server):
    manifest_bytes = args.manifest.read_bytes()
    candidate = json.loads(manifest_bytes)
    intermediate_bytes = (args.history / "v0.1.255" / "mesh-release.json").read_bytes()
    intermediate = json.loads(intermediate_bytes)
    source = json.loads((args.history / "v0.1.249" / "mesh-release.json").read_bytes())
    platform = {"os": "linux", "arch": "amd64"}
    source_digest = next(item["binarySha256"] for item in source["artifacts"] if item["platform"] == platform)
    if hashlib.sha256(args.binary.read_bytes()).hexdigest() != source_digest:
        raise ValueError("original v0.1.249 executable identity differs from its published manifest")
    server.assets = {asset_path(candidate["version"], "mesh-release.json"): manifest_bytes,
                     asset_path("v0.1.255", "mesh-release.json"): intermediate_bytes,
                     "/repos/ShaulLavo/mesh/releases?per_page=32": json.dumps([{"tag_name": "v0.1.255", "draft": False, "prerelease": False}]).encode()}
    for transition in intermediate["compatibility"]["transitions"]:
        name = transition["proof"] + ".json"
        server.assets[asset_path("v0.1.255", name)] = (args.history / "v0.1.255" / name).read_bytes()
    for transition in candidate["compatibility"]["transitions"]:
        name = transition["proof"] + ".json"
        server.assets[asset_path(candidate["version"], name)] = (args.proofs / name).read_bytes()
    environment = {key: os.environ[key] for key in ("PATH", "LANG", "TMPDIR") if key in os.environ}
    environment.update(HOME=str(root), MESH_STATE_DIR=str(root / "state"), MESH_CONFIG_DIR=str(root / "config"),
                       XDG_CONFIG_HOME=str(root / "xdg-config"), XDG_CACHE_HOME=str(root / "cache"),
                       XDG_STATE_HOME=str(root / "xdg-state"), XDG_RUNTIME_DIR=str(root / "runtime"),
                       HTTPS_PROXY="http://127.0.0.1:" + str(server.server_port), NO_PROXY="127.0.0.1,localhost",
                       **tls.certificate_environment(root))
    (root / "runtime").mkdir(mode=0o700)
    with (args.output / "original-daemon.log").open("wb") as log:
        daemon = subprocess.Popen([str(args.binary), "daemon", "--tailnet-port=0", "--ssh-port=0"], env=environment, stdout=log, stderr=log)
        try:
            inspect_with_daemon(args, candidate, manifest_bytes, source_digest, environment, daemon, server)
        finally:
            terminate(daemon)
    (args.output / "requests.json").write_text(json.dumps(server.reads, indent=2) + "\n")
    if hashlib.sha256(args.binary.read_bytes()).hexdigest() != source_digest:
        raise ValueError("original reader executable changed during validation")


def terminate(process):
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def inspect_with_daemon(args, candidate, manifest_bytes, source_digest, environment, daemon, server):
    wait_daemon(args.binary, environment, daemon)
    inspect(args.binary, candidate, environment, args.output / "exact-manifest.json")
    bridge_fixture = json.loads(manifest_bytes)
    bridge_fixture["compatibility"]["transitions"] = [item for item in candidate["compatibility"]["transitions"] if item["fromDigest"] != source_digest]
    server.assets[asset_path(candidate["version"], "mesh-release.json")] = json.dumps(bridge_fixture).encode()
    preview = inspect(args.binary, bridge_fixture, environment, args.output / "receipt-reader.json")
    if not reached_helper_review(preview):
        raise RuntimeError("original receipt reader did not reach the installation review after verifying both receipts")
    intermediate = json.loads((args.history / "v0.1.255" / "mesh-release.json").read_bytes())
    source = next(item["binarySha256"] for item in intermediate["artifacts"] if item["platform"] == {"os": "linux", "arch": "amd64"})
    transition = next(item for item in candidate["compatibility"]["transitions"] if item["fromDigest"] == source)
    path = asset_path(candidate["version"], transition["proof"] + ".json")
    original = server.assets[path]
    server.assets[path] = original + b" "
    rejected = inspect(args.binary, bridge_fixture, environment, args.output / "corrupt-receipt-refused.json")
    if reached_helper_review(rejected) or path not in server.reads:
        raise RuntimeError("original reader did not refuse the corrupt generated receipt")


def reached_helper_review(preview):
    return any("bridge readiness: read bridge helper ownership:" in review.get("cause", "")
               for review in preview.get("reviews", {}).values())


def run_origin(args, root, context):
    with Origin(("127.0.0.1", 0), Proxy) as server:
        server.context = context
        server.reads = []
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            exercise(args, root, server)
        finally:
            server.shutdown()
            thread.join(timeout=5)


def main():
    signal.signal(signal.SIGTERM, terminate_run)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("history", type=Path)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("proofs", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="mlr-", dir=os.environ.get("MESH_SHORT_TMP")) as temporary:
        root = Path(temporary)
        tls.fixture_certificates(root)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(root / "server.pem", root / "server.key")
        run_origin(args, root, context)


def terminate_run(signum, _frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    main()
