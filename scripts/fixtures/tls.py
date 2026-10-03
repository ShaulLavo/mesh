"""Process-local certificate files and actual TLS verification controls."""
import hashlib
import json
import os
import socketserver
import ssl
import subprocess
import threading
from pathlib import Path

SOURCES = Path(__file__).with_name("tls-probe")


def fixture_certificates(root):
    def openssl(*args):
        subprocess.run(["openssl", *args], cwd=root, check=True, capture_output=True, timeout=15)

    (root / "ca.cnf").write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
        "[dn]\nCN=Mesh disposable fixture CA\n[ext]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "cert.pem",
            "-days", "1", "-config", "ca.cnf")
    for name, dns in (("server", "github.com,DNS:api.github.com"), ("wrong-host", "wrong.fixture.test")):
        (root / (name + ".ext")).write_text("basicConstraints=critical,CA:FALSE\n"
            "keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + dns + "\n")
        openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", name + ".key", "-out", name + ".csr",
                "-subj", "/CN=" + dns.split(",")[0])
        openssl("x509", "-req", "-in", name + ".csr", "-CA", "cert.pem", "-CAkey", "ca.key", "-CAcreateserial",
                "-out", name + ".pem", "-days", "1", "-extfile", name + ".ext")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "wrong-ca.key", "-out", "wrong-ca.pem",
            "-days", "1", "-config", "ca.cnf")
    (root / "self-signed.cnf").write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
        "[dn]\nCN=github.com\n[ext]\nsubjectAltName=DNS:github.com\nbasicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\n")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "self-signed.key", "-out", "self-signed.pem",
            "-days", "1", "-config", "self-signed.cnf")


def certificate_environment(root):
    return {"SSL_CERT_FILE": str(root / "cert.pem"), "SSL_CERT_DIR": str(root / "empty-certs")}


def probe_tls(root, probe, environment, certificate, key, expected, launcher="direct"):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(root / certificate, root / key)

    class Handler(socketserver.BaseRequestHandler):
        def handle(self):
            try:
                with context.wrap_socket(self.request, server_side=True) as secure:
                    secure.recv(1)
            except (ssl.SSLError, ConnectionResetError, BrokenPipeError):
                pass

    with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
        thread = threading.Thread(target=server.handle_request, daemon=True)
        thread.start()
        command = [str(probe), "127.0.0.1:" + str(server.server_address[1]), "github.com"]
        if launcher == "production-shell":
            command = ["/bin/sh", "-c", 'exec "$@"', "mesh-fixture-tls", *command]
        try:
            result = subprocess.run(command, env=environment, capture_output=True, timeout=15, check=False)
        finally:
            server.socket.close()
            thread.join(timeout=5)
    output = result.stderr.decode(errors="replace")
    kind = output.split(": ", 1)[0] if output else "other"
    if result.returncode == 0 and result.stdout == b"trusted\n":
        kind = "trusted"
    evidence = {"returncode": result.returncode, "class": kind,
        "stdout": result.stdout.decode(errors="replace"), "stderr": output}
    if kind != expected or result.returncode != (0 if expected == "trusted" else 1):
        raise RuntimeError("TLS fixture control " + expected + " failed: " + json.dumps(evidence))
    return evidence


def prepare_fixture_tls(root, event):
    probe = root / "tls-probe"
    subprocess.run(["go", "build", "-trimpath", "-o", str(probe), str(SOURCES)],
                   check=True, capture_output=True, timeout=60)
    environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
    environment.update(HOME=str(root), **certificate_environment(root))
    missing = environment | {"SSL_CERT_FILE": str(root / "missing.pem")}
    wrong = environment | {"SSL_CERT_FILE": str(root / "wrong-ca.pem")}
    for launcher in ("direct", "production-shell"):
        for name, cert, key, expected, child in (
            ("fixture-ca", "server.pem", "server.key", "trusted", environment),
            ("missing-ca", "server.pem", "server.key", "certificate", missing),
            ("wrong-ca", "server.pem", "server.key", "certificate", wrong),
            ("self-signed", "self-signed.pem", "self-signed.key", "certificate", environment),
            ("wrong-hostname", "wrong-host.pem", "wrong-host.key", "hostname", environment),
        ):
            result = probe_tls(root, probe, child, cert, key, expected, launcher)
            event("tls-control", control=name, launcher=launcher, **result)
    event("fixture-tls", provider="Go1.27 child-only certificate files", trustStoreModified=False,
        shellExecutable="/bin/sh", shellProbe="exec positional probe arguments", productionPlistChanged=False)
    return certificate_environment(root)


def verify_artifacts(original, primary_error, event):
    try:
        if any(hashlib.sha256(binary.read_bytes()).hexdigest() != expected for binary, expected in original.items()):
            raise RuntimeError("fixture changed an input artifact")
    except (OSError, RuntimeError) as error:
        if primary_error is not None:
            primary_error.add_note("fixture artifact validation: " + str(error))
            return
        raise
    event("fixture-artifacts-unchanged", trustStoreModified=False)
