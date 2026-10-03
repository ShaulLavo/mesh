#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  root=$(mktemp -d)
  trap 'rm -rf -- "$root"' EXIT
  MESH="$root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 - "$MESH" "$repo_root/integration/helpers" <<'PY'
import base64
import json
import os
import select
from pathlib import Path
import socket
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
sys.path.insert(0, sys.argv[2])
from mesh_control import round_trip
from terminal_window import Fixture, Terminal, PROMPT, eventually, require
from recovery_transactions import build_control_client

with tempfile.TemporaryDirectory(prefix="mesh-control-auth-") as directory:
    fixture = Fixture(sys.argv[1], Path(directory))
    try:
        fixture.start_remote()
        host = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]
        port = int(host["endpoint"].split(":")[-1].split("/")[0])
        with socket.create_connection(("127.0.0.1", port), timeout=2) as raw:
            key = base64.b64encode(os.urandom(16)).decode()
            raw.sendall(("GET /mesh HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\n"
                         "Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
                         f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
            require(raw.recv(4096).startswith(b"HTTP/1.1 426"), "unauthenticated control upgraded")
        original = Terminal([fixture.binary, "pc"], fixture.environment, fixture.root)
        fixture.terminals.append(original)
        original.expect(PROMPT)
        session_id, shell_pid = fixture.shell_identity(original)
        metadata = json.loads((fixture.remote / "s" / session_id / "meta.json").read_text())
        second_environment = fixture.environment | {"MESH_STATE_DIR": str(fixture.root / "second-device")}
        def command(environment, *args):
            return subprocess.run([fixture.binary, *args], env=environment, capture_output=True, timeout=6)
        first_identity = json.loads(command(fixture.environment, "device", "identity", "--json").stdout)["id"]
        second_identity = json.loads(command(second_environment, "device", "identity", "--json").stdout)["id"]
        local_destination = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
        updater = command(local_destination, "update", "trust", "--", first_identity)
        require(updater.returncode == 0, f"independent update trust failed: {updater.stderr!r}")
        build_control_client(fixture)
        probe = subprocess.Popen([str(fixture.root / "control-client"), host["endpoint"], fixture.remote_id,
                                  "grant-lifetime"], env=fixture.environment, stdin=subprocess.PIPE,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            require(select.select([probe.stdout], [], [], 5)[0], "grant-lifetime probe did not connect")
            require(probe.stdout.readline() == b"READY\n", "grant-lifetime probe did not report readiness")
            removed = command(local_destination, "device", "revoke", "--", first_identity)
            require(removed.returncode == 0, f"temporary revocation failed: {removed.stderr!r}")
            restored = command(local_destination, "device", "approve", "--allow-root", "--", first_identity)
            require(restored.returncode == 0, f"same-key reapproval failed: {restored.stderr!r}")
            stdout, stderr = probe.communicate(b"\n", timeout=8)
            require(probe.returncode == 0, f"same-key reapproval healed an old socket: {stdout!r} {stderr!r}")
            os.kill(shell_pid, 0)
            require(json.loads((fixture.remote / "s" / session_id / "meta.json").read_text())["pid"] == metadata["pid"],
                    "same-key grant retirement changed the retained command")
        finally:
            if probe.poll() is None:
                probe.kill()
                probe.wait()
        enrolled = command(local_destination, "device", "approve", "--allow-root", "--", second_identity)
        require(enrolled.returncode == 0, f"second-device enrollment failed: {enrolled.stderr!r}")
        revoked = command(local_destination, "device", "revoke", "--", first_identity)
        require(revoked.returncode == 0, f"device revocation failed: {revoked.stderr!r}")
        original.expect_exit()
        denied = command(fixture.environment, "ls", "pc")
        require(denied.returncode != 0, "revoked device listed sessions")
        os.kill(shell_pid, 0)
        original.close()
        fixture.terminals.remove(original)
        fixture.stop_daemon()
        os.kill(shell_pid, 0)
        status = fixture.root / "tailscale.json"
        environment = fixture.environment | {
            "MESH_STATE_DIR": str(fixture.remote), "MESH_CONFIG_DIR": str(fixture.root / "remote-config"),
            "MESH_FAKE_TAILSCALE_STATUS": str(status),
            "PATH": str(fixture.root / "bin") + os.pathsep + fixture.environment["PATH"],
        }
        fixture.daemon_log = open(fixture.root / "replacement.log", "wb")
        fixture.daemon = subprocess.Popen([fixture.binary, "daemon", "--tailnet-port", str(port), "--ssh-port", "0"],
                                         env=environment, stdin=subprocess.DEVNULL,
                                         stdout=fixture.daemon_log, stderr=subprocess.STDOUT)
        def ready():
            try:
                response = round_trip(str(fixture.remote / "daemon.sock"), {"type": "session.list", "requestId": "retained"})
                return any(row["id"] == session_id for row in response.get("sessions", []))
            except (OSError, RuntimeError):
                return False
        eventually(ready, "replacement daemon did not discover retained worker")
        retained = json.loads((fixture.remote / "s" / session_id / "meta.json").read_text())
        require(retained["pid"] == metadata["pid"], "daemon replacement changed retained command PID")
        approved = Terminal([fixture.binary, "pc", "-r"], second_environment, fixture.root)
        fixture.terminals.append(approved)
        approved.expect(PROMPT)
        require(fixture.shell_identity(approved) == (session_id, shell_pid), "another approved device reached a different session")
        print("PASS: old active/read-only and passive sockets retired after same-key reapproval; fresh grant connected; raw controls denied; dual-authority passive terminal retired on device revocation; retained worker survived daemon replacement and reattached from another approved device")
    finally:
        fixture.close()
PY
