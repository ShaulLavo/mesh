"""Real worker, lost reply, daemon replacement and compact terminal receipts."""
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys

from mesh_control import round_trip
from terminal_window import Fixture, eventually


def exchange(fixture, request):
    return round_trip(str(fixture.local / "daemon.sock"), request)


def drop_reply(fixture, request):
    payload = json.dumps(request).encode()
    with socket.socket(socket.AF_UNIX) as connection:
        connection.connect(str(fixture.local / "daemon.sock"))
        connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)


def receipt(response):
    assert response.get("type") == "execution.receipt", response
    return response["executionReceipt"]


def exercise(fixture):
    fixture.start_local_daemon()
    counter = fixture.root / "effects"
    command = ["/bin/sh", "-c", 'printf x >> "$1"; sleep 15', "owned-child", str(counter)]
    create = dict(type="execution.create", requestId="lost-create", requestKey="owned-job",
                  kind="pty", cwd=str(fixture.root), command=command)
    drop_reply(fixture, create)
    eventually(lambda: counter.exists(), "keyed child did not start")
    meta_path = eventually(lambda: next((fixture.local / "s").glob("*/meta.json"), None), "worker metadata missing")
    first = meta_path.parent.name
    eventually(lambda: (meta_path.parent / "sock").exists(), "worker socket missing")
    fixture.stop_daemon()
    fixture.start_local_daemon()
    lookup = dict(type="execution.request", requestId="lookup", requestKey="owned-job")
    original = receipt(exchange(fixture, lookup))
    assert original["sessionId"] == first and original["keyScope"] == "local-account", original
    equal = receipt(exchange(fixture, dict(create, requestId="equal", cols=80, rows=24, term="dumb")))
    assert equal["instance"] == original["instance"] and counter.read_bytes() == b"x", equal
    conflict = exchange(fixture, dict(create, requestId="conflict", command=["/bin/false"]))
    assert conflict.get("errorCode") == "request.key_conflict", conflict
    host = exchange(fixture, dict(type="host.info", requestId="host"))["host"]["id"]
    args = [fixture.binary, "session", "create", "--host", host, "--json", "--kind", "pty",
            "--request-key", "fast-exit", "--cwd", str(fixture.root), "--", "/bin/sh", "-c", "exit 7"]
    completed = subprocess.run(args, env=fixture.environment, capture_output=True, text=True, timeout=10, check=False)
    assert completed.returncode == 0 and not completed.stderr, (completed.stdout, completed.stderr)
    fast = json.loads(completed.stdout)
    assert fast["operation"] == "sessions.create" and fast["status"] == "ok", fast
    fast_lookup = dict(type="execution.request", requestId="fast-lookup", requestKey="fast-exit")
    terminal = eventually(lambda: terminal_receipt(fixture, fast_lookup, fast["data"]), "fast exit never became terminal")
    assert terminal["result"]["termination"] == {"kind": "exit", "code": 7, "signal": None}, terminal
    assert terminal["result"]["capture"] == "not_recorded" and terminal["receiptRetainUntil"], terminal
    symlink_cwd(fixture)
    print(json.dumps(dict(first=first, instance=original["instance"], sideEffects=counter.read_text(), terminal=terminal)), flush=True)


def symlink_cwd(fixture):
    real = fixture.root / "physical-cwd"
    alias = fixture.root / "lexical-cwd"
    real.mkdir()
    alias.symlink_to(real, target_is_directory=True)
    request = dict(type="execution.create", requestId="symlink-create", requestKey="symlink-cwd",
                   kind="pty", cwd=str(alias), command=["/bin/sh", "-c", "printf accepted > effect"])
    created = receipt(exchange(fixture, request))
    lookup = dict(type="execution.request", requestId="symlink-lookup", requestKey="symlink-cwd")
    terminal = eventually(lambda: terminal_receipt(fixture, lookup, created), "symlink cwd never became terminal")
    assert terminal["result"]["termination"]["code"] == 0, terminal
    assert (real / "effect").read_text() == "accepted"
    meta = json.loads((fixture.local / "s" / created["sessionId"] / "meta.json").read_text())
    assert meta["cwd"] == str(alias), meta


def terminal_receipt(fixture, request, expected):
    response = exchange(fixture, request)
    if response.get("type") == "error":
        assert response.get("errorCode") == "request.outcome_unknown", response
        value = response["executionReceipt"]
        assert value["phase"] == "uncertain" and value["result"] is None, response
        assert value["acceptance"] == "durable", response
    else:
        value = receipt(response)
    assert value["instance"] == expected["instance"] and value["sessionId"] == expected["sessionId"], value
    return value if value["phase"] == "terminal" else None


def main():
    root = Path(os.environ["HOME"]).parent / "durable-create"
    root.mkdir()
    fixture = Fixture(sys.argv[1], root)
    try:
        exercise(fixture)
    finally:
        fixture.close()


if __name__ == "__main__":
    main()
