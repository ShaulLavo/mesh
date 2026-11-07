"""Crash a real reserved worker after its checkpoint acknowledgement."""
import json
import os
from pathlib import Path
import signal
import socket
import struct
import sys

from mesh_control import receive
from session_lifecycle import LifecycleFixture
from terminal_window import eventually


def request(path, message):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(path))
        payload = json.dumps(message).encode()
        connection.sendall(struct.pack(">BI", 1, len(payload)) + payload)
        kind, length = struct.unpack(">BI", receive(connection, 5))
        assert kind == 1 and length <= 4 << 20, (kind, length)
        return json.loads(receive(connection, length))


def exercise(fixture):
    fixture.start_local_daemon()
    fixture.capture_tree()
    command = ('for ((i=0;i<300;i++)); do printf \'line-%03d\\r\\n\' "$i"; done; '
               'printf \'\\033[31mSAVED-RENDERED\\033[0m\\r\\n\'; read -r line')
    daemon_socket = str(fixture.local / "daemon.sock")
    response = request(daemon_socket, dict(type="session.create", requestId="checkpoint-create",
                          command=["/bin/bash", "--noprofile", "--norc", "-c", command],
                          cwd=str(fixture.root), cols=80, rows=6, term="dumb"))
    assert response.get("type") == "session.created", response
    sid = response["sessionId"]
    directory = fixture.local / "s" / sid
    worker, child = fixture.session_processes(sid)
    host = request(daemon_socket, dict(type="host.info", requestId="checkpoint-host"))["host"]["id"]
    try:
        eventually(lambda: rendered(directory, sid), "worker never rendered command output", timeout=8)
        reply = request(str(directory / "sock"), dict(type="session.recovery-command", sessionId=sid,
                           recoveryCommand=dict(argv=["/bin/true", "literal argument"], cwd=str(fixture.root))))
        assert reply["type"] == "ok", reply
        acknowledged = (directory / "recovery.json").read_bytes()
        assert fixture.kernel.send(fixture.tree.proved[worker].fd, signal.SIGKILL), "owned worker already exited"
        eventually(lambda: not fixture.alive(worker), "owned worker did not exit after SIGKILL", timeout=5)
        assert (directory / "recovery.json").read_bytes() == acknowledged
        record = json.loads(acknowledged)
        text = "\n".join(record["lines"])
        assert record["version"] == 1 and record["hostId"] == host and record["sessionId"] == sid
        assert "SAVED-RENDERED" in text and "line-299" in text and "line-000" not in text, text
        assert "\x1b" not in text and len(record["lines"]) <= 256
        assert sum(len(line.encode()) for line in record["lines"]) <= 128 << 10
        assert record["restart"]["argv"][1] == "literal argument"
        assert record["checkpointAt"] and (directory / "recovery.json").stat().st_mode & 0o777 == 0o600
        assert json.loads((directory / "meta.json").read_text())["state"] != "exited"
    finally:
        for pid in (worker, child):
            if fixture.alive(pid):
                fixture.kernel.send(fixture.tree.proved[pid].fd, signal.SIGKILL)


def rendered(directory, sid):
    response = request(str(directory / "sock"), dict(type="session.inspect", sessionId=sid,
                                                       previewCols=80, previewRows=6))
    return "SAVED-RENDERED" in "\n".join(response.get("inspection", {}).get("preview", []))


def main():
    root = Path(os.environ["HOME"]).parent / "crash-checkpoint"
    root.mkdir()
    fixture = LifecycleFixture(sys.argv[1], root)
    try:
        exercise(fixture)
    finally:
        fixture.close()
        if fixture.tree is not None:
            fixture.tree.close()
    # The outer Owner joins every descendant before deleting this fixture root.
    print("PASS: acknowledged rendered checkpoint survives worker SIGKILL with no exit handler")


if __name__ == "__main__":
    main()
