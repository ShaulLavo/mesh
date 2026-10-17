#!/usr/bin/env python3
"""Copy real files through an authenticated daemon without creating sessions."""
import base64
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import struct
import subprocess
import sys
import tempfile

from mesh_control import receive, round_trip
from terminal_window import Fixture, eventually, require


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def copy(fixture, *args, success=True):
    result = subprocess.run([fixture.binary, "cp", *map(str, args)], env=fixture.environment,
                            stdin=subprocess.DEVNULL, capture_output=True, timeout=180)
    require((result.returncode == 0) == success, f"mesh cp {args}: {result.returncode} {result.stderr!r}")
    return result


def listing(fixture):
    response = round_trip(str(fixture.remote / "daemon.sock"),
                          {"type": "session.list", "requestId": "copy-fixture"})
    require(response["type"] == "session.listed", f"catalog: {response}")
    return response.get("sessions", [])


def exercise(fixture):
    fixture.start_remote()
    root = fixture.root
    received = root / "received"
    received.mkdir()
    source = root / "source.bin"
    with source.open("wb") as stream:
        for _ in range(65):
            stream.write(os.urandom(1 << 20))
        stream.write(b"\x00\xfftail\x00")
    expected = digest(source)
    before = listing(fixture)
    sessions_before = set((fixture.remote / "s").glob("*"))
    copy(fixture, source, f"pc:{received}/")
    uploaded = received / source.name
    require(uploaded.stat().st_size > 64 << 20 and digest(uploaded) == expected,
            "large binary upload changed or truncated")
    downloaded = root / "downloaded.bin"
    downloaded.write_bytes(b"old destination")
    copy(fixture, f"pc:{uploaded}", downloaded)
    require(digest(downloaded) == expected, "large binary download changed or failed to replace")

    tree = root / "tree"
    (tree / "nested" / "empty").mkdir(parents=True)
    (tree / ".git").mkdir()
    (tree / ".git" / "HEAD").write_text("ref: refs/heads/main\n")
    (tree / ".env").write_text("FIXTURE=copy-this-file\n")
    executable = tree / "nested" / "run"
    executable.write_bytes(b"#!/bin/sh\nprintf 'copy-fixture\\n'\n")
    executable.chmod(0o751)
    copy(fixture, tree, f"pc:{received}", success=False)
    copy(fixture, "-r", tree, f"pc:{received}")
    remote_tree = received / "tree"
    require((remote_tree / "nested" / "empty").is_dir(), "empty directory disappeared")
    require((remote_tree / ".env").read_bytes() == (tree / ".env").read_bytes(), "dotfile disappeared")
    require((remote_tree / ".git" / "HEAD").is_file(), "repository metadata disappeared")
    require((remote_tree / "nested" / "run").stat().st_mode & 0o777 == 0o751, "executable mode changed")
    (remote_tree / "keep").write_text("destination only")
    executable.write_text("replacement")
    copy(fixture, "-r", tree, f"pc:{received}")
    require((remote_tree / "keep").read_text() == "destination only", "directory merge deleted a file")
    pulled = root / "pulled"
    copy(fixture, "-r", f"pc:{remote_tree}", pulled)
    require((pulled / "nested" / "run").read_text() == "replacement", "recursive download changed")
    require((pulled / "nested" / "empty").is_dir(), "recursive download omitted empty dir")
    require((pulled / "keep").read_text() == "destination only", "recursive download omitted file")

    outside = root / "outside"
    outside.mkdir()
    destination = received / "trap"
    destination.symlink_to(outside, target_is_directory=True)
    copy(fixture, "-r", tree, f"pc:{destination}", success=False)
    require(not list(outside.iterdir()), "destination symlink escaped copy root")
    (tree / "link").symlink_to(source)
    copy(fixture, "-r", tree, f"pc:{received}", success=False)
    require(not (remote_tree / "link").exists(), "source symlink was followed")
    (tree / "link").unlink()

    # Send an unfinished upload over a real connection and then drop it.
    # The destination must remain unchanged, and another copy must still work.
    canceled = received / "canceled.bin"
    canceled.write_bytes(b"original")
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(fixture.remote / "daemon.sock"))
        for operation in (
            {"action": "upload.begin", "path": str(canceled), "info": {"name": source.name}},
            {"action": "upload.chunk", "data": base64.b64encode(os.urandom(65536)).decode()},
        ):
            payload = json.dumps({"type": "copy.request", "requestId": "abort", "copy": operation}).encode()
            connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
            header = receive(connection, 5)
            reply = json.loads(receive(connection, struct.unpack(">I", header[1:])[0]))
            require(reply["type"] == "copy.result", f"unfinished upload rejected: {reply}")
    eventually(lambda: not any("mesh-copy" in entry.name for entry in received.iterdir()),
               "disconnect left staging files")
    require(canceled.read_bytes() == b"original", "disconnect replaced destination with partial data")
    copy(fixture, tree / ".env", f"pc:{canceled}")
    require(canceled.read_bytes() == (tree / ".env").read_bytes(), "copy after disconnect failed")
    require(listing(fixture) == before, "copy created a saved session")
    require(set((fixture.remote / "s").glob("*")) == sessions_before, "copy created session folders")
    require(not any("mesh-copy" in entry.name for entry in root.rglob("*")), "copy left temporary artifacts")
    print("PASS: authenticated >64MiB incompressible binary upload/download, atomic overwrite, recursive merge, "
          "empty directories, dotfiles, modes, symlink refusal, disconnect cleanup, no saved sessions")


def main():
    def interrupted(signum, _frame):
        raise TimeoutError(f"copy fixture interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    with tempfile.TemporaryDirectory(prefix="mesh-cp-", dir=os.environ.get("MESH_SHORT_TMP")) as root:
        fixture = Fixture(sys.argv[1], Path(root))
        try:
            exercise(fixture)
        finally:
            fixture.close()


if __name__ == "__main__":
    main()
