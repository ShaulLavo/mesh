"""Adapter to the delivered adoption producer's genuine Sigstore/TUF TLS origin."""
import json
import socket
from pathlib import Path
from fixtures import installed_clock


class Origin:
    def __init__(self, root):
        self.root = Path(root)
        self.address = json.loads((self.root / "proxy.json").read_text())["address"]

    def environment(self):
        return {"SSL_CERT_FILE": str(self.root / "fixture-ca.pem"), "SSL_CERT_DIR": str(self.root / "empty-certs"),
            "HTTPS_PROXY": self.address, "HTTP_PROXY": self.address, "NO_PROXY": "127.0.0.1,localhost"}

    @installed_clock.timed("serve", lambda self, directory: Path(directory).name)
    def serve_tree(self, directory):
        files = {path.relative_to(directory).as_posix(): str(path.relative_to(self.root))
                 for path in Path(directory).rglob("*") if path.is_file() and path.name != "mesh"}
        request(self.root, {"put": files})

    @installed_clock.timed("head", lambda self, directory, version: version)
    def head(self, directory, version):
        path = Path(directory) / version / "mesh-update-root.json"
        request(self.root, {"put": {"/ShaulLavo/mesh/releases/latest/download/mesh-update-root.json": str(path.relative_to(self.root))}})

    def seed(self, state, version):
        request(self.root, {"seed": {"state": str(state), "version": version}}, timeout=90)

    def inspect(self, state, output):
        request(self.root, {"inspect": str(state), "output": str(output.relative_to(self.root))})

    def ledger(self, state, output):
        request(self.root, {"ledger": str(state), "output": str(output.relative_to(self.root))})


def request(root, value, timeout=15):
    with socket.socket(socket.AF_UNIX) as connection:
        deadline_ns = installed_clock.parent_deadline(timeout)
        connection.settimeout(installed_clock.remaining_until(deadline_ns, timeout))
        connection.connect(str(Path(root) / "control.sock"))
        connection.settimeout(installed_clock.remaining_until(deadline_ns, timeout))
        connection.sendall(json.dumps(value).encode() + b"\n")
        connection.settimeout(installed_clock.remaining_until(deadline_ns, timeout))
        with connection.makefile("rb") as stream:
            reply = json.loads(stream.readline())
    if reply != {"ok": True}:
        raise RuntimeError("installed fixture producer did not complete")


def sign(root, files, output=None, stop=False):
    request(root, {"files": [str(Path(path).relative_to(root)) for path in files],
        "output": str(Path(output).relative_to(root)) if output else "", "stop": stop})
