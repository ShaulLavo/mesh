#!/usr/bin/env python3
"""Fixture teardown waits for workers before deleting their state directories."""

import os
import signal
import subprocess
import sys
import tempfile
import threading

sys.dont_write_bytecode = True
from terminal_window import Fixture, PROMPT, eventually, require, run_outside_containing_session


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
    reached = TeardownReached()
    fixture.terminals.append(reached)
    closed = threading.Event()
    errors = []

    def close():
        try:
            fixture.close()
        except Exception as error:
            errors.append(error)
        finally:
            closed.set()

    os.kill(worker_pid, signal.SIGSTOP)
    closer = threading.Thread(target=close)
    closer.start()
    try:
        require(reached.reached.wait(timeout=4), "fixture did not reach terminal teardown")
        require(not closed.wait(timeout=0.1), "fixture.close returned while its worker was suspended")
    finally:
        os.kill(worker_pid, signal.SIGCONT)
        closer.join(timeout=4)
        eventually(lambda: not socket_path.exists(), "fixture worker did not finish shutdown")
    require(not closer.is_alive(), "fixture.close did not finish after its worker resumed")
    require(not errors, f"fixture.close failed: {errors}")
    require(not socket_path.exists(), "fixture.close retained a live worker socket")
    print("PASS: fixture teardown waits for final worker writes")


def main():
    with tempfile.TemporaryDirectory(prefix="mesh-fixture-cleanup-") as root:
        exercise(sys.argv[1], root)


if __name__ == "__main__":
    sys.exit(run_outside_containing_session(main))
