#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="$repo_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
python3 - "$MESH" <<'PY'
import json
import os
from pathlib import Path
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time

binary = str(Path(sys.argv[1]).resolve())

def receive(connection, count):
    data = bytearray()
    while len(data) < count:
        part = connection.recv(count - len(data))
        if not part:
            raise AssertionError("worker closed before acknowledgement")
        data.extend(part)
    return bytes(data)

def request(path, message):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(path))
        payload = json.dumps(message).encode()
        connection.sendall(struct.pack(">BI", 1, len(payload)) + payload)
        kind, length = struct.unpack(">BI", receive(connection, 5))
        assert kind == 1 and length <= 4 << 20, (kind, length)
        return json.loads(receive(connection, length))

with tempfile.TemporaryDirectory(prefix="mesh-cp-") as temporary:
    root = Path(temporary)
    directory = root / "s" / "7K3D"
    directory.mkdir(parents=True, mode=0o700)
    environment = os.environ | {"MESH_STATE_DIR": str(root), "MESH_HOST_ID": "checkpoint-host"}
    command = "for ((i=0;i<300;i++)); do printf 'line-%03d\\r\\n' \"$i\"; done; printf '\\033[31mSAVED-RENDERED\\033[0m\\r\\n'; read -r line"
    worker = subprocess.Popen([binary, "session-worker", "--id", "7K3D", "--dir", str(directory),
                               "--cols", "80", "--rows", "6", "--", "/bin/bash", "--noprofile", "--norc", "-c", command],
                              cwd=root, env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    leader = None
    try:
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            assert worker.poll() is None, worker.stderr.read().decode()
            try:
                preview = request(directory / "sock", {"type": "session.inspect", "sessionId": "7K3D", "previewCols": 80, "previewRows": 6})
                if "SAVED-RENDERED" in "\n".join(preview.get("inspection", {}).get("preview", [])):
                    break
            except (FileNotFoundError, ConnectionRefusedError):
                pass
            time.sleep(0.02)
        else:
            raise AssertionError("worker never rendered command output")
        leader = json.loads((directory / "meta.json").read_text())["pid"]
        reply = request(directory / "sock", {"type": "session.recovery-command", "sessionId": "7K3D",
                                              "recoveryCommand": {"argv": ["/bin/true", "literal argument"], "cwd": str(root)}})
        assert reply["type"] == "ok", reply
        acknowledged = (directory / "recovery.json").read_bytes()
        worker.kill()
        worker.wait(timeout=5)
        assert (directory / "recovery.json").read_bytes() == acknowledged
        record = json.loads(acknowledged)
        text = "\n".join(record["lines"])
        assert record["version"] == 1 and record["hostId"] == "checkpoint-host"
        assert "SAVED-RENDERED" in text and "line-299" in text and "line-000" not in text, text
        assert "\x1b" not in text and len(record["lines"]) <= 256
        assert sum(len(line.encode()) for line in record["lines"]) <= 128 << 10
        assert record["restart"]["argv"][1] == "literal argument"
        assert record["checkpointAt"] and (directory / "recovery.json").stat().st_mode & 0o777 == 0o600
        assert json.loads((directory / "meta.json").read_text())["state"] != "exited"
    finally:
        if worker.poll() is None:
            worker.kill()
            worker.wait(timeout=5)
        if leader is not None:
            try:
                os.killpg(leader, signal.SIGKILL)
            except ProcessLookupError:
                pass
        worker.stderr.close()
print("PASS: acknowledged rendered checkpoint survives worker SIGKILL with no exit handler")
PY
