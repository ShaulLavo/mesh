"""Dedicated-key metadata through a real daemon, with an attached owner worker."""

import base64
import json
import shlex
import os
from pathlib import Path
import subprocess
import sys

from terminal_window import Fixture, PROMPT, Terminal, require
from mesh_control import round_trip


def invoke(binary, environment, *args, code=0):
    result = subprocess.run([binary, "device", "scope", *args, "--json"],
                            env=environment, capture_output=True, text=True,
                            timeout=6, check=False)
    require(result.returncode == code, f"scope {args}: {result.stdout} {result.stderr}")
    require(not result.stderr, result.stderr)
    value = json.loads(result.stdout)
    require(value["schemaVersion"] == 1 and len(value["requestId"]) == 26, value)
    require(value["operation"].startswith("scope."), value)
    return value


def exercise(fixture):
    fixture.start_remote()
    owner = Terminal([fixture.binary, "pc"], fixture.environment, fixture.root)
    fixture.terminals.append(owner)
    owner.expect(PROMPT)
    session_id, shell_pid = fixture.shell_identity(owner)
    sentinel = "SCOPED-OUTPUT-MUST-NOT-BE-DISCLOSED"
    owner.send(f"printf '{sentinel}\\n'\n")
    owner.expect(sentinel.encode())
    owner.expect(PROMPT)
    require(fixture.shell_identity(owner) == (session_id, shell_pid), "sentinel command did not complete")
    dedicated = fixture.root / "scope-key"
    empty_state = fixture.root / "scope-normal-state-absent"
    caller = fixture.environment | {"MESH_STATE_DIR": str(empty_state)}
    public = invoke(fixture.binary, caller, "keygen", "--key-dir", str(dedicated))["data"]["identity"]
    target = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
    granted = invoke(fixture.binary, target, "grant", "--identity", public,
                     "--permissions", "metadata", "--expires-in", "1h",
                     "--expected-revision", "0")
    require(granted["data"]["changed"], granted)
    for section in ("host", "sessions", "services"):
        value = invoke(fixture.binary, caller, "read", "--host", fixture.remote_id,
                       "--key-dir", str(dedicated), "--section", section)
        data = value["data"]
        require(data["subject"]["principal"] == public, value)
        require(data["subject"]["class"] == "scoped", value)
        require(data["subject"]["permissions"] == ["metadata"], value)
        require(sentinel not in json.dumps(value), value)
        if section == "sessions":
            require(any(row["id"] == session_id for row in data["sessions"]), value)
    require(not empty_state.exists(), "scope commands created the normal identity or cache")
    denied_frames(fixture, dedicated, session_id, shell_pid, owner, caller)
    owner.send("printf 'OWNER-STILL-ATTACHED\\n'\n")
    require(fixture.shell_identity(owner) == (session_id, shell_pid), "denied frames displaced the owner")
    revoked = invoke(fixture.binary, target, "revoke", "--identity", public,
                     "--expected-revision", "1")
    require(revoked["data"]["changed"], revoked)
    denied = invoke(fixture.binary, caller, "read", "--host", fixture.remote_id,
                    "--key-dir", str(dedicated), code=1)
    require(denied["errors"][0]["code"] == "host.authentication_failed", denied)
    os.kill(shell_pid, 0)
    owner.send("printf 'WORKER-SURVIVED-REVOKE\\n'\n")
    require(fixture.shell_identity(owner) == (session_id, shell_pid), "revoke changed or killed the worker")
    require(not empty_state.exists(), "failed read used normal fallback state")
    print("PASS: dedicated scoped TLS metadata; no output disclosure, owner attachment change, normal state or fallback; revocation preserves worker")


def denied_frames(fixture, dedicated, session_id, shell_pid, owner, environment):
    marker = fixture.root / "denied-command-marker"
    copied = fixture.root / "denied-copy"
    secret = fixture.root / "disclosure-sentinel"
    secret.write_text("DENIED-OUTPUT-SENTINEL")
    os.chmod(secret, 0o600)
    command = ["/bin/sh", "-c", 'printf forbidden > "$1"', "probe", str(marker)]
    controls = [
        {"type": "session.create", "command": command, "cwd": str(fixture.root), "cols": 80, "rows": 24},
        {"type": "session.attach"}, {"type": "session.attach-detached"},
        {"type": "terminal.resize", "cols": 19, "rows": 7},
        {"type": "session.kill"}, {"type": "session.signal", "signal": "term"},
        {"type": "session.recover", "recoveryAction": "command", "cols": 80, "rows": 24},
        {"type": "session.recovery-command", "recoveryCommand": {"argv": command, "cwd": str(fixture.root)}},
        {"type": "session.remove"}, {"type": "session.inspect", "cols": 1, "rows": 1},
        {"type": "session.list", "lean": False}, {"type": "session.logs"},
        {"type": "session.recovery"},
        {"type": "copy.request", "copy": {"action": "download.begin", "path": str(secret)}},
        {"type": "copy.request", "copy": {"action": "upload.begin", "path": str(copied),
         "info": {"name": "denied-copy", "directory": False, "digest": "0" * 64}}},
        {"type": "service.upsert", "service": {"name": "scoped-denied", "kind": "static", "target": str(fixture.root)}},
        {"type": "service.start", "serviceName": "scoped-denied"},
        {"type": "service.stop", "serviceName": "scoped-denied"},
    ]
    probes = []
    for index, control in enumerate(controls):
        payload = control | {"requestId": f"scope-denied-{index}", "sessionId": session_id}
        probes.append({"kind": 1, "sessionId": session_id,
                       "payload": base64.b64encode(json.dumps(payload).encode()).decode()})
    input_bytes = f"printf forbidden > {shlex.quote(str(marker))}".encode() + bytes([10])
    probes.append({"kind": 3, "sessionId": session_id,
                   "payload": base64.b64encode(input_bytes).decode()})
    host = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]
    result = subprocess.run([sys.argv[3], host["endpoint"], fixture.remote_id,
                             "scoped-denial", str(dedicated)],
                            input=json.dumps(probes), text=True, capture_output=True,
                            env=environment, timeout=12, check=False)
    require(result.returncode == 0 and "scoped frames denied" in result.stdout,
            f"scoped framed denial failed: {result.stdout} {result.stderr}")
    require(not marker.exists() and not copied.exists(), "denied control wrote a mutation marker")
    rows = round_trip(str(fixture.remote / "daemon.sock"),
                      {"type": "session.list", "requestId": "owner-after-denials", "lean": True})
    require([row["id"] for row in rows["sessions"]] == [session_id], "denied create/recover changed sessions")
    services = round_trip(str(fixture.remote / "daemon.sock"),
                          {"type": "service.list", "requestId": "owner-after-service-denials"})
    require(not any(row["name"] == "scoped-denied" for row in services.get("services", [])),
            "denied service control published a route")
    recovery = fixture.remote / "s" / session_id / "recovery.json"
    require(not recovery.exists() or str(marker) not in recovery.read_text(), "denied recovery control wrote a recipe")
    os.kill(shell_pid, 0)
    require(fixture.shell_identity(owner) == (session_id, shell_pid),
            "denied attach/input/resize/kill changed the owner's session")


def main():
    fixture = Fixture(sys.argv[1], Path(sys.argv[2]))
    try:
        exercise(fixture)
    finally:
        fixture.close()


if __name__ == "__main__":
    main()
