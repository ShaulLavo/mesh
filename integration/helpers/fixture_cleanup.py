#!/usr/bin/env python3
"""Fixture teardown waits for workers before deleting their state directories."""

import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor

from terminal_window import (
    PROMPT,
    Fixture,
    eventually,
    require,
    round_trip,
    run_outside_containing_session,
)


class TeardownReached:
    def __init__(self):
        self.reached = threading.Event()

    def close(self):
        self.reached.set()


def exercise(binary, root):
    fixture = Fixture(binary, root)
    terminal = fixture.window()
    terminal.expect(PROMPT)
    session_id, shell_pid = fixture.shell_identity(terminal)
    worker_pid = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(shell_pid)]))
    socket_path = fixture.local / "s" / session_id / "sock"
    arguments = subprocess.check_output(["ps", "-o", "args=", "-p", str(worker_pid)]).decode()
    require("session-worker" in arguments and str(socket_path.parent) in arguments,
            f"shell parent is not the fixture worker: {arguments}")
    for state in (fixture.local, fixture.remote):
        stale = state / "s" / "stale" / "sock"
        stale.parent.mkdir(parents=True)
        with socket.socket(socket.AF_UNIX) as listener:
            listener.bind(str(stale))
    reached = TeardownReached()
    fixture.terminals.append(reached)
    with ThreadPoolExecutor(max_workers=1) as executor:
        os.kill(worker_pid, signal.SIGSTOP)
        closer = executor.submit(fixture.close)
        try:
            require(reached.reached.wait(timeout=4), "fixture did not reach terminal teardown")
            try:
                closer.result(timeout=0.1)
            except TimeoutError:
                pass
            else:
                raise RuntimeError("fixture.close returned while its worker was suspended")
        finally:
            os.kill(worker_pid, signal.SIGCONT)
            closer.result(timeout=4)
            eventually(lambda: not socket_path.exists(), "fixture worker did not finish shutdown")
    require(not socket_path.exists(), "fixture.close retained a live worker socket")
    print("PASS: fixture teardown waits for final worker writes")


def exercise_unattached(binary, root):
    fixture = Fixture(binary, root)
    fixture.start_local_daemon()
    created = round_trip(str(fixture.local / "daemon.sock"), {
        "type": "session.create", "requestId": "fixture-unattached",
        "command": ["/bin/sh", "-c", "printf retained-output"], "cwd": str(fixture.root),
    })
    require(created.get("type") == "session.created", f"create failed: {created}")
    session_id = created["sessionId"]
    socket_path = fixture.local / "s" / session_id / "sock"
    pid = fixture.metadata(session_id)["pid"]

    def reaped():
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return True
        return False

    eventually(reaped, "unattached command was not reaped")
    require(socket_path.exists(), "worker did not retain output for its first attachment")
    fixture.close()
    require(not socket_path.exists(), "unattached worker retained its socket after teardown")
    metadata = json.loads((socket_path.parent / "meta.json").read_text())
    require(metadata["state"] == "exited", f"worker did not finish its metadata: {metadata}")
    print("PASS: fixture teardown acknowledges an exited never-attached worker")


def main():
    with tempfile.TemporaryDirectory(prefix="mesh-fixture-cleanup-") as root:
        exercise(sys.argv[1], root)
    with tempfile.TemporaryDirectory(prefix="mesh-fixture-unattached-") as root:
        exercise_unattached(sys.argv[1], root)


if __name__ == "__main__":
    sys.exit(run_outside_containing_session(main))
