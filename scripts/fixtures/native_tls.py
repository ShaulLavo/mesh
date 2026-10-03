"""Process-local TLS provider for unchanged native Darwin fixture executables."""
import hashlib
import json
import os
import shutil
import socketserver
import ssl
import struct
import subprocess
import tempfile
import threading
from pathlib import Path

SOURCES = Path(__file__).with_name("native-tls")


def macho_signature_flags(data):
    if data[:4] != b"\xcf\xfa\xed\xfe":
        raise ValueError("fixture requires a thin 64-bit Darwin Mach-O executable")
    count = struct.unpack_from("<I", data, 16)[0]
    offset = 32
    flags = []
    for _ in range(count):
        command, size = struct.unpack_from("<II", data, offset)
        if size < 8 or offset + size > len(data):
            raise ValueError("invalid Mach-O load command")
        if command == 0x19 and data[offset + 8:offset + 24].split(b"\0", 1)[0] == b"__RESTRICT":
            raise RuntimeError("restricted Mach-O cannot use the native TLS fixture")
        if command == 0x1d:
            start, length = struct.unpack_from("<II", data, offset + 8)
            blob = data[start:start + length]
            magic, total, entries = struct.unpack_from(">III", blob)
            if magic != 0xfade0cc0 or total != len(blob):
                raise ValueError("invalid Mach-O signature superblob")
            directories = []
            for index in range(entries):
                _, entry = struct.unpack_from(">II", blob, 12 + index * 8)
                if struct.unpack_from(">I", blob, entry)[0] == 0xfade0c02:
                    directories.append(struct.unpack_from(">I", blob, entry + 12)[0])
            if not directories:
                raise ValueError("Mach-O signature has no code directory")
            flags.extend(directories)
        offset += size
    return flags


def verify_injectable_binaries(binaries):
    rows = []
    for binary in binaries:
        data = binary.read_bytes()
        flags = macho_signature_flags(data)
        if any(flag & (0x10000 | 0x800 | 0x2000) for flag in flags):
            raise RuntimeError("hardened-runtime or restricted signed Mesh artifact cannot use the native TLS fixture")
        rows.append({"binary": binary.name, "sha256": hashlib.sha256(data).hexdigest(),
                     "signatureFlags": [hex(flag) for flag in flags], "hardenedRuntime": False})
    return rows


def fixture_certificates(root):
    def openssl(*args):
        subprocess.run(["openssl", *args], cwd=root, check=True, capture_output=True, timeout=15)

    (root / "ca.cnf").write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
        "[dn]\nCN=Mesh disposable fixture CA\n[ext]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "cert.pem",
            "-days", "1", "-config", "ca.cnf")
    openssl("x509", "-in", "cert.pem", "-outform", "DER", "-out", "ca.der")
    for name, dns in (("server", "github.com,DNS:api.github.com"), ("wrong-host", "wrong.fixture.test")):
        (root / (name + ".ext")).write_text("basicConstraints=critical,CA:FALSE\n"
            "keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:" + dns + "\n")
        openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", name + ".key", "-out", name + ".csr",
                "-subj", "/CN=" + dns.split(",")[0])
        openssl("x509", "-req", "-in", name + ".csr", "-CA", "cert.pem", "-CAkey", "ca.key", "-CAcreateserial",
                "-out", name + ".pem", "-days", "1", "-extfile", name + ".ext")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "wrong-ca.key", "-out", "wrong-ca.pem",
            "-days", "1", "-config", "ca.cnf")
    openssl("x509", "-req", "-in", "server.csr", "-CA", "wrong-ca.pem", "-CAkey", "wrong-ca.key", "-CAcreateserial",
            "-out", "wrong-ca-server.pem", "-days", "1", "-extfile", "server.ext")
    (root / "self-signed.cnf").write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ext\n"
        "[dn]\nCN=github.com\n[ext]\nsubjectAltName=DNS:github.com\nbasicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\n")
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "self-signed.key", "-out", "self-signed.pem",
            "-days", "1", "-config", "self-signed.cnf")


def probe_tls(root, probe, environment, certificate, key, expected):
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
        try:
            result = subprocess.run([str(probe), "127.0.0.1:" + str(server.server_address[1]), "github.com"],
                                    env=environment, capture_output=True, timeout=15)
        finally:
            server.socket.close()
            thread.join(timeout=5)
        output = result.stderr.decode(errors="replace")
        if expected == "trusted" and result.returncode == 0 and result.stdout == b"trusted\n":
            return ""
        if expected != "trusted" and result.returncode == 1 and output.startswith(expected + ": "):
            return output
        raise RuntimeError("native TLS fixture control " + expected + " failed: " + output)


def prepare_native_tls(root, binaries, event):
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_OS") != "macOS":
        raise RuntimeError("Darwin TLS fixture requires a disposable GitHub macOS runner")
    rows = verify_injectable_binaries(binaries)
    anchor = (root / "ca.der").read_bytes()
    (root / "fixture-anchor.h").write_text("static const unsigned char fixture_ca_der[] = {" +
        ",".join(str(byte) for byte in anchor) + "};\nstatic const char fixture_root[] = " + json.dumps(str(root)) + ";\n")
    library = root / "fixture-anchor.dylib"
    probe = root / "tls-probe"
    subprocess.run(["go", "build", "-trimpath", "-o", str(probe), str(SOURCES / "probe")],
                   check=True, capture_output=True, timeout=60)
    verify_injectable_binaries([probe])
    environment = {key: value for key, value in os.environ.items() if not key.startswith("DYLD_")}
    environment.update(HOME=str(root), SSL_CERT_FILE=str(root / "cert.pem"), SSL_CERT_DIR=str(root / "empty-certs"))
    error = probe_tls(root, probe, environment, "server.pem", "server.key", "certificate")
    event("native-tls-control", control="injection-unavailable", result="certificate", error=error)
    subprocess.run(["clang", "-Wall", "-Wextra", "-Werror", "-dynamiclib", "-framework", "Security",
                    "-framework", "CoreFoundation", "-I", str(root), str(SOURCES / "anchor.c"), "-o", str(library)],
                   check=True, capture_output=True, timeout=30)
    injected = environment | {"DYLD_INSERT_LIBRARIES": str(library)}
    for name, cert, key, expected, child in (
        ("fixture-ca", "server.pem", "server.key", "trusted", injected),
        ("wrong-ca", "wrong-ca-server.pem", "server.key", "certificate", injected),
        ("self-signed", "self-signed.pem", "self-signed.key", "certificate", injected),
        ("wrong-hostname", "wrong-host.pem", "wrong-host.key", "hostname", injected),
    ):
        error = probe_tls(root, probe, child, cert, key, expected)
        event("native-tls-control", control=name, result=expected, error=error)
    with tempfile.TemporaryDirectory(prefix="mesh-tls-outside-", dir=root.parent) as directory:
        outside = Path(directory) / "tls-probe"
        shutil.copyfile(probe, outside)
        outside.chmod(0o755)
        error = probe_tls(root, outside, injected, "server.pem", "server.key", "certificate")
        event("native-tls-control", control="outside-fixture-descendant", result="certificate", error=error)
    event("fixture-trust", provider="process-local SecTrust fixture CA", caDigest=hashlib.sha256(anchor).hexdigest(),
          unchangedBinaries=rows, trustStoreModified=False)
    return {"DYLD_INSERT_LIBRARIES": str(library)}
