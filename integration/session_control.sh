#!/usr/bin/env bash
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -z ${MESH:-} ]]; then
  MESH="$repo_root/mesh"
  (cd "$repo_root" && go build -o "$MESH" ./cmd/mesh)
fi
exec python3 - "$MESH" "$repo_root/integration/helpers" <<'PY'
import json
import os
from pathlib import Path
import socket
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
sys.path.insert(0, sys.argv[2])
from mesh_control import round_trip
from terminal_window import Fixture, Terminal, eventually, require


def command(fixture, environment, *args):
    return subprocess.run([fixture.binary, *args], env=environment, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=4)


def catalog(state):
    response = round_trip(str(state / "daemon.sock"), {"type": "session.list", "requestId": "control-list"})
    require(response.get("type") == "session.listed", f"catalog failed: {response}")
    return {row["id"] for row in response.get("sessions", [])}


def signals(fixture, remote):
    state = fixture.remote if remote else fixture.local
    environment = fixture.environment if remote else fixture.environment | {"MESH_STATE_DIR": str(state)}
    for name in ("TERM", "term", "SIGTERM"):
        shell = 'trap "echo got-term; exit 7" TERM; echo ready; while :; do sleep 0.05; done'
        if remote:
            response = round_trip(str(state / "daemon.sock"), {
                "type": "session.create", "requestId": "control-create-" + name,
                "command": ["/bin/sh", "-c", shell], "cwd": str(fixture.root),
            })
            require(response.get("type") == "session.created", f"create failed: {response}")
            sid = response["sessionId"]
            terminal = Terminal([fixture.binary, sid], environment, fixture.root)
            fixture.terminals.append(terminal)
            terminal.expect("ready")
        else:
            terminal = Terminal([fixture.binary, "local", "--", "/bin/sh", "-c", shell],
                                environment, fixture.root)
            fixture.terminals.append(terminal)
            terminal.expect("ready")
            rows = eventually(lambda: fixture.sessions(state), "local session did not appear")
            require(len(rows) == 1, f"unexpected live sessions: {rows}")
            sid = rows[0]["id"]
        directory = state / "s" / sid
        refused = command(fixture, environment, "rm", sid)
        require(refused.returncode != 0 and b"still" in refused.stderr and directory.exists(),
                f"live rm was not refused: {refused.stdout!r} {refused.stderr!r}")
        invalid = command(fixture, environment, "sig", sid, "bogus")
        require(invalid.returncode != 0 and b"sent" not in invalid.stdout,
                f"bogus signal reported success: {invalid.stdout!r} {invalid.stderr!r}")
        sent = command(fixture, environment, "sig", sid, name)
        require(sent.returncode == 0, f"sig {name} failed: {sent.stderr!r}")
        eventually(lambda: b"got-term" in command(fixture, environment, "logs", sid).stdout,
                   f"sig {name} did not reach {'remote' if remote else 'local'} process", timeout=3)
        eventually(lambda: json.loads((directory / "meta.json").read_text()).get("state") == "exited",
                   "signalled process did not exit")
        if remote:
            def reconciled_exit():
                response = round_trip(str(state / "daemon.sock"), {
                    "type": "session.list", "requestId": "control-exit-list",
                })
                return any(row["id"] == sid and row["state"] == "exited"
                           for row in response.get("sessions", []))
            eventually(reconciled_exit, "daemon did not reconcile the worker exit")
        if not remote:
            local_catalog = catalog(state)
            require(sid in local_catalog, "local daemon did not adopt the session")
        removed = command(fixture, environment, "rm", sid)
        require(removed.returncode == 0, f"rm exited failed: {removed.stderr!r}")
        require(sid.encode() not in command(fixture, environment, "ls", "--all").stdout,
                "removed session remained in ls")
        require(sid not in catalog(state), "removed session remained in daemon catalog after reconcile")
        require(not directory.exists(), "daemon removal left the session directory")
        print(f"PASS: {'remote' if remote else 'local'} sig {name} and catalog removal", flush=True)


def unknown_probe(fixture):
    state = fixture.local
    directory = state / "s" / "PR0B"
    directory.mkdir(parents=True)
    (directory / "meta.json").write_text(json.dumps({
        "id": "PR0B", "state": "running", "command": ["sh"], "cwd": str(fixture.root),
        "createdAt": "2026-08-30T12:00:00Z",
    }))
    with socket.socket(socket.AF_UNIX) as listener, socket.socket(socket.AF_UNIX) as queued:
        listener.bind(str(directory / "sock"))
        listener.listen(0)
        queued.connect(str(directory / "sock"))
        result = command(fixture, fixture.environment, "rm", "PR0B")
        require(result.returncode != 0 and directory.exists() and not (directory / ".forgotten").exists(),
                f"timed-out probe permitted removal: {result.stdout!r} {result.stderr!r}")
    shutil.rmtree(directory)
    print("PASS: rm refuses an inconclusive worker probe", flush=True)


with tempfile.TemporaryDirectory(prefix="mesh-session-control-") as root:
    fixture = Fixture(sys.argv[1], root)
    try:
        unknown_probe(fixture)
        fixture.start_local_daemon()
        signals(fixture, False)
        fixture.stop_daemon()
        fixture.start_local_daemon()
        require(not catalog(fixture.local), "removed local sessions returned after daemon restart")
        fixture.stop_daemon()
        fixture.start_remote()
        signals(fixture, True)
    finally:
        fixture.close()
PY
