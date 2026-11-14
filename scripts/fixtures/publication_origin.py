"""Serve immutable draft assets locally while production TUF TLS stays intact."""
import http.server
import os
from pathlib import Path
import select
import shutil
import socket
import ssl
import subprocess
import threading
import time
import urllib.parse

from fixtures import published_releases, tls


class Assets(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            path = self.server.origin.asset(urllib.parse.urlsplit(self.path).path)
            self.send_response(200)
            self.send_header("Content-Length", str(path.stat().st_size))
            self.end_headers()
            with path.open("rb") as source:
                shutil.copyfileobj(source, self.wfile, 1 << 20)
        except (OSError, RuntimeError):
            self.send_error(404, "approved release asset unavailable")

    def log_message(self, *_arguments):
        pass


class Tunnel(http.server.BaseHTTPRequestHandler):
    def do_CONNECT(self):
        origin = self.server.origin
        if self.path not in ("github.com:443", "tuf-repo-cdn.sigstore.dev:443"):
            self.send_error(403)
            return
        with origin.lock:
            origin.connections.add(self.connection)
        try:
            self.route(origin)
        except OSError:
            pass
        finally:
            with origin.lock:
                origin.connections.discard(self.connection)

    def route(self, origin):
        if self.path == "tuf-repo-cdn.sigstore.dev:443":
            self.forward_trust(origin)
            return
        self.send_response(200, "Connection established")
        self.end_headers()
        self.wfile.flush()
        with origin.context.wrap_socket(self.connection, server_side=True) as secure:
            origin.track(secure)
            try:
                secure.settimeout(30)
                Assets(secure, self.client_address, self.server)
            finally:
                origin.untrack(secure)

    def forward_trust(self, origin):
        with socket.create_connection(("tuf-repo-cdn.sigstore.dev", 443), timeout=10) as upstream:
            with origin.lock:
                origin.connections.add(upstream)
                origin.trust_connections += 1
            try:
                self.send_response(200, "Connection established")
                self.end_headers()
                self.wfile.flush()
                pump(self.connection, upstream, origin.stopping)
            finally:
                origin.untrack(upstream)

    def log_message(self, *_arguments):
        pass


def pump(client, upstream, stopping):
    while not stopping.is_set():
        readable, _, _ = select.select((client, upstream), (), (), 0.2)
        if any(not transfer(source, upstream if source is client else client) for source in readable):
            return


def transfer(source, target):
    data = source.recv(65536)
    if not data:
        return False
    target.sendall(data)
    return True


class Origin:
    def __init__(self, root, directory, inspector):
        self.root, self.directory, self.inspector = Path(root), Path(directory), Path(inspector)
        self.lock, self.stopping = threading.Lock(), threading.Event()
        self.connections, self.trust_connections, self.public_fetches = set(), 0, 0
        self.head_path, self.server, self.thread = None, None, None
        self.certificates = self.root / "publication-tls"
        self.certificates.mkdir()
        tls.fixture_certificates(self.certificates)
        roots = ssl.create_default_context().get_ca_certs(binary_form=True)
        bundle = "".join(ssl.DER_cert_to_PEM_cert(root) for root in roots)
        bundle += (self.certificates / "cert.pem").read_text()
        self.bundle = self.certificates / "system-and-fixture.pem"
        self.bundle.write_text(bundle)
        self.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.context.load_cert_chain(self.certificates / "server.pem", self.certificates / "server.key")

    def start(self):
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Tunnel)
        self.server.origin = self
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.05})
        self.thread.start()

    def environment(self):
        address = "http://127.0.0.1:" + str(self.server.server_port)
        result = {"SSL_CERT_FILE": str(self.bundle), "SSL_CERT_DIR": str(self.certificates / "empty-certs"),
                  "HTTPS_PROXY": address, "HTTP_PROXY": address, "NO_PROXY": "127.0.0.1,localhost"}
        if os.environ.get("MESH_PUBLICATION_TRUST_CACHE"):
            result["MESH_RELEASE_TRUST_CACHE"] = os.environ["MESH_PUBLICATION_TRUST_CACHE"]
        return result

    def track(self, connection):
        with self.lock:
            self.connections.add(connection)

    def untrack(self, connection):
        with self.lock:
            self.connections.discard(connection)

    def asset(self, path):
        if path == "/ShaulLavo/mesh/releases/latest/download/mesh-update-root.json" and self.head_path:
            return self.head_path
        prefix = "/ShaulLavo/mesh/releases/download/"
        parts = path.removeprefix(prefix).split("/")
        if not path.startswith(prefix) or len(parts) != 2 or any(Path(part).name != part or part in (".", "..") for part in parts):
            raise RuntimeError("request is outside approved release assets")
        version, name = parts
        local = self.directory / version / name
        if local.is_file():
            return local
        public = self.root / "public" / version / name
        if not public.exists():
            data = published_releases.download(published_releases.ORIGIN + version + "/" + name, 128 << 20)
            public.parent.mkdir(parents=True, exist_ok=True)
            public.write_bytes(data)
            with self.lock:
                self.public_fetches += 1
        return public

    def head(self, directory, version):
        path = Path(directory) / version / "mesh-update-root.json"
        if not path.exists():
            path = self.asset("/ShaulLavo/mesh/releases/download/" + version + "/mesh-update-root.json")
        self.head_path = path

    def inspect(self, state, output):
        self.read("inspect", state, output)

    def ledger(self, state, output):
        self.read("ledger", state, output)

    def seed(self, state, version):
        self.read("seed", state, state.parent / "seed.json", version)

    def read(self, kind, state, output, version=""):
        environment = os.environ | self.environment() | {"HOME": str(state.parent / "home"),
            "MESH_STATE_DIR": str(state), "MESH_CONFIG_DIR": str(state.parent / "config"),
            "MESH_INSTALLED_READ_ROOT": str(state.parent), "MESH_INSTALLED_READ_STATE": str(state),
            "MESH_INSTALLED_READ_KIND": kind, "MESH_INSTALLED_READ_VERSION": version,
            "MESH_INSTALLED_READ_OUTPUT": str(output.relative_to(state.parent))}
        result = subprocess.run([str(self.inspector), "-test.run=^TestInstalledHostRead$"], env=environment,
                                capture_output=True, text=True, timeout=90 if kind == "seed" else 15, check=False)
        if result.returncode:
            raise RuntimeError("publication product " + kind + " failed: " + result.stdout[-4096:] + result.stderr[-4096:])

    def close(self):
        self.stopping.set()
        with self.lock:
            connections = list(self.connections)
        for connection in connections:
            close_connection(connection)
        if self.server:
            self.shutdown()
            self.server.server_close()
        if self.thread:
            self.thread.join(timeout=2)
        if self.thread and self.thread.is_alive():
            raise RuntimeError("owned publication transport did not stop")
        deadline = time.monotonic() + 2
        while self.connections and time.monotonic() < deadline:
            time.sleep(0.01)
        if self.connections:
            raise RuntimeError("owned publication transport connections remain active")
        return {"proxyStopped": True, "trustTLS": "unmodified public TLS tunnel", "trustConnections": self.trust_connections,
                "publicAssetFetches": self.public_fetches, "testSignatureRootUsed": False}

    def shutdown(self):
        if self.thread and self.thread.is_alive():
            self.server.shutdown()


def close_connection(connection):
    try:
        connection.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass
    connection.close()
