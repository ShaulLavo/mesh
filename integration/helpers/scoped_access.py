"""Dedicated-key metadata through a real daemon, with an attached owner worker."""

import json
import os
from pathlib import Path
import subprocess
import sys

from terminal_window import Fixture, PROMPT, Terminal, require


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
    owner.send("printf 'OWNER-STILL-ATTACHED\\n'\n")
    owner.expect(b"OWNER-STILL-ATTACHED")
    owner.expect(PROMPT)
    revoked = invoke(fixture.binary, target, "revoke", "--identity", public,
                     "--expected-revision", "1")
    require(revoked["data"]["changed"], revoked)
    denied = invoke(fixture.binary, caller, "read", "--host", fixture.remote_id,
                    "--key-dir", str(dedicated), code=1)
    require(denied["errors"][0]["code"] == "host.authentication_failed", denied)
    os.kill(shell_pid, 0)
    owner.send("printf 'WORKER-SURVIVED-REVOKE\\n'\n")
    owner.expect(b"WORKER-SURVIVED-REVOKE")
    owner.expect(PROMPT)
    require(not empty_state.exists(), "failed read used normal fallback state")
    print("PASS: dedicated scoped TLS metadata; no output disclosure, owner attachment change, normal state or fallback; revocation preserves worker")


def main():
    fixture = Fixture(sys.argv[1], Path(sys.argv[2]))
    try:
        exercise(fixture)
    finally:
        fixture.close()


if __name__ == "__main__":
    main()
