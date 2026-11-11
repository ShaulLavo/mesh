"""Database checkpoints and daemon writes for the isolated native release proof."""

import json
from pathlib import Path
import socket
import sqlite3
import struct
import sys


def database_snapshot(source, destination):
    with sqlite3.connect(source) as original, sqlite3.connect(destination) as copy:
        original.backup(copy)


def schema(path):
    with sqlite3.connect(path) as database:
        definitions = database.execute(
            "SELECT type,name,tbl_name,sql FROM sqlite_master "
            "WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name"
        ).fetchall()
        history = database.execute(
            "SELECT version_id,is_applied FROM goose_db_version ORDER BY id"
        ).fetchall()
    return {"definitions": definitions, "history": history}


def request(state, value):
    value["requestId"] = "rollback-proof-" + value["type"]
    payload = json.dumps(value).encode()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(10)
        connection.connect(str(state / "daemon.sock"))
        connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
        with connection.makefile("rb") as stream:
            header = stream.read(5)
            assert len(header) == 5 and header[0] == 1, header
            length = struct.unpack(">I", header[1:])[0]
            assert 0 < length <= 4 << 20, length
            response = json.loads(stream.read(length))
    assert response["type"] != "error", response
    return response


def candidate_writes(state):
    site = state / "rollback-site"
    site.mkdir(exist_ok=True)
    (site / "index.html").write_text("rollback proof\n")
    service = {"name": "rollback-proof", "kind": "static", "target": str(site),
               "displayName": "Candidate rollback proof", "isolate": True}
    response = request(state, {"type": "service.upsert", "service": service})
    assert response["type"] == "service.upserted", response
    host = request(state, {"type": "host.info"})["host"]
    response = request(state, {"type": "host.rename", "rename": {
        "targetId": host["id"], "machineName": "rollback-proof",
        "expectedRevision": host["nameRevision"],
    }})
    assert response["type"] == "host.renamed", response
    (state / "rollback-name.before").write_bytes((state / "machine-name.json").read_bytes())
    verify_writes(state)


def verify_writes(state):
    with sqlite3.connect(state / "mesh.db") as database:
        service = database.execute(
            "SELECT kind,target,display_name,isolate FROM services WHERE name='rollback-proof'"
        ).fetchone()
        assert service == ("static", str(state / "rollback-site"), "Candidate rollback proof", 1), service
        facts = database.execute("SELECT COUNT(*) FROM cached_session_facts").fetchone()[0]
        assert facts > 0, "candidate did not cache retained session facts"
    assert (state / "machine-name.json").read_bytes() == (state / "rollback-name.before").read_bytes(), \
        "candidate-written machine name changed on rollback"
    response = request(state, {"type": "service.list"})
    assert any(row["name"] == "rollback-proof" for row in response["services"]), response
    host = request(state, {"type": "host.info"})["host"]
    assert host["machineName"] == "rollback-proof", host


def boundary_writes(path):
    # This uses only the shared session row. Final-state writes use the actual daemon.
    with sqlite3.connect(path) as database:
        changed = database.execute(
            "UPDATE sessions SET last_output_sequence=last_output_sequence+1"
        ).rowcount
        assert changed > 0, "missing retained session cache"


def main(args):
    mode, *values = args
    if mode == "snapshot":
        database_snapshot(*values)
    elif mode == "schema":
        Path(values[1]).write_text(json.dumps(schema(values[0]), sort_keys=True))
    elif mode == "boundary-writes":
        boundary_writes(values[0])
    elif mode == "candidate-writes":
        candidate_writes(Path(values[0]))
    elif mode == "verify-writes":
        verify_writes(Path(values[0]))
    else:
        raise ValueError(mode)


if __name__ == "__main__":
    main(sys.argv[1:])
