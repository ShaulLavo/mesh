#!/usr/bin/env python3
"""Run one Mesh command in a PTY and accept its public-serve prompt."""

import errno
import os
import pty
import select
import signal
import sys
import time


PROMPT = b"Continue? [y/N] "
MAXIMUM_OUTPUT = 1 << 20
COMMAND_TIMEOUT = 8


def terminate(pid):
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + 1
    while time.monotonic() < deadline:
        waited, status = os.waitpid(pid, os.WNOHANG)
        if waited:
            return status
        time.sleep(0.01)
    try:
        os.kill(pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    return os.waitpid(pid, 0)[1]


def main():
    command = sys.argv[1:]
    if command[:1] == ["--"]:
        command = command[1:]
    if not command:
        raise SystemExit("usage: confirm_public_serve.py -- COMMAND [ARG ...]")

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe(command[0], command, os.environ.copy())

    output = bytearray()
    confirmed = False
    status = None
    end_of_stream = False
    deadline = time.monotonic() + COMMAND_TIMEOUT
    try:
        while status is None or not end_of_stream:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                if status is None:
                    terminate(pid)
                raise SystemExit("public confirmation command timed out")
            readable, _, _ = select.select([master], [], [], min(remaining, 0.1))
            if readable:
                try:
                    chunk = os.read(master, 4096)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    chunk = b""
                if chunk:
                    output.extend(chunk)
                    os.write(sys.stdout.fileno(), chunk)
                    if len(output) > MAXIMUM_OUTPUT:
                        terminate(pid)
                        raise SystemExit("public confirmation output exceeded fixture limit")
                    if not confirmed and PROMPT in output:
                        os.write(master, b"y\n")
                        confirmed = True
                else:
                    end_of_stream = True
            if status is None:
                waited, child_status = os.waitpid(pid, os.WNOHANG)
                if waited:
                    status = child_status
    finally:
        os.close(master)

    if not confirmed:
        raise SystemExit("Mesh command exited without a public confirmation prompt")
    raise SystemExit(os.waitstatus_to_exitcode(status))


if __name__ == "__main__":
    main()
