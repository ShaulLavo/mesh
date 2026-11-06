#!/usr/bin/env python3
"""An active SSH grant retires without ending its persistent session."""
import json

from session_lifecycle import LifecycleFixture, exercise_owned, marker
from ssh_sessions import SSHFixture
from terminal_window import PROMPT, eventually, require


class RevocationFixture(SSHFixture, LifecycleFixture):
    def device(self, name):
        state = self.root / name
        result = self.command("device", "identity", "--json", environment=self.environment | {"MESH_STATE_DIR": str(state)})
        identity = json.loads(result.stdout)["id"]
        self.command("device", "approve", "--allow-root", "--", identity)
        return identity, state / "identity.key"


def revocation(fixture):
    a_id, a_key = fixture.device("a")
    b_id, b_key = fixture.device("b")
    require(a_id != b_id and a_key != b_key, "SSH devices A and B share an identity")
    fixture.start_ssh(keys=(a_key, b_key))
    fixture.capture_tree()
    local = fixture.terminal("local", "--daemon", "--", str(fixture.helpers / "window_shell.sh"))
    local.expect(PROMPT)
    sid, child = fixture.shell_identity(local)
    worker, _ = fixture.session_processes(sid)
    a = fixture.ssh_terminal([sid], key=a_key)
    a.expect(PROMPT)
    local.expect_exit()
    require(marker(fixture, a, "ACTIVE_DEVICE_A") == (sid, child), "A did not actively attach to the original session")
    require(fixture.attached(fixture.local, sid) and a.poll() is None, "A was not attached immediately before revoke")
    if fixture.control != "skip-revoke":
        fixture.command("device", "revoke", "--", a_id)
    a.expect_exit()
    eventually(lambda: not fixture.attached(fixture.local, sid), "revoked A attachment did not retire")
    require(fixture.alive(worker) and fixture.alive(child), "revocation ended the worker or child")
    require(fixture.metadata(sid)["pid"] == child, "revocation replaced the persistent process")
    denied = fixture.exec_ssh("ls", key=a_key)
    require(denied.returncode == 255 and b"Permission denied" in denied.stderr,
            f"fresh A was not denied: {denied.returncode}, {denied.stdout!r}, {denied.stderr!r}")
    b = fixture.ssh_terminal([sid], key=b_key)
    b.expect(PROMPT)
    if fixture.control == "close-b":
        b.crash()
    require(marker(fixture, b, "ACTIVE_DEVICE_B") == (sid, child), "B did not positively attach to the same session")
    require(fixture.attached(fixture.local, sid) and b.poll() is None, "B is not attached")
    fixture.same_session(sid, worker, child, b)
    print(f"PASS: revoked active SSH A {a_id}, fresh A denied, B {b_id} attached same {sid}, worker {worker}, child {child}")


def run_revocation(binary, arguments):
    require(arguments[0] == "revocation" and len(arguments) <= 2, "unknown SSH scenario")
    control = arguments[1] if len(arguments) == 2 else None
    require(control in (None, "skip-revoke", "close-b"), "unknown SSH revocation control")
    exercise_owned(RevocationFixture, binary, revocation, control)
