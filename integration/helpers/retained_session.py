"""Cooperative teardown for the update proofs' one private terminal session."""

import json
import os
import re
import socket
import stat
import struct
import sys
import time
import uuid
from contextlib import ExitStack, contextmanager

from isolation import protected_cleanup
from mesh_control import receive, remaining_socket_time
from retained_process import RetainedProcess


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def peer_pid(connection):
    if sys.platform == "darwin":
        return connection.getsockopt(0, 2)  # SOL_LOCAL / LOCAL_PEERPID
    return struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]


def send_frame(connection, deadline, kind, body):
    connection.settimeout(remaining_socket_time(deadline))
    connection.sendall(struct.pack(">BI", kind, len(body)) + body)


def read_frame(connection, deadline):
    header = receive(connection, 5, deadline_ns=deadline, response_header=True)
    kind, length = struct.unpack(">BI", header)
    require(length <= 4 << 20, "fixture worker frame exceeds its bound")
    return kind, receive(connection, length, deadline_ns=deadline)


def expect_control(connection, deadline, expected, session):
    while True:
        kind, body = read_frame(connection, deadline)
        if kind != 1:
            continue
        message = json.loads(body)
        require(message.get("type") == expected and message.get("sessionId") == session,
                f"expected {expected} for fixture session {session}")
        return message


def expect_shell(connection, deadline, header, challenge):
    marker = re.compile(re.escape(challenge) + rb":([1-9][0-9]*):ready")
    tail = b""
    while True:
        kind, body = read_frame(connection, deadline)
        if kind != 2:
            require(kind != 1, "fixture worker ended before shell challenge")
            continue
        require(body[:8] == header and len(body) >= 16, "unexpected fixture shell output frame")
        output = tail + body[16:]
        match = marker.search(output)
        if match is not None:
            return int(match[1])
        tail = output[-128:]


@contextmanager
def confirm_cleanup(root):
    with protected_cleanup(), ExitStack() as cleanup:
        yield cleanup
    (root / "cleanup-pending").unlink(missing_ok=True)


class RetainedSession:
    def __init__(self, state, binary, shell):
        self.sessions = state / "s"
        self.binary = binary
        self.command = [str(shell), "mesh-fixture-" + uuid.uuid4().hex]
        self.directory = None
        self.worker = None
        self.shell = None

    def check_empty(self):
        require(not self.session_directories(), "retained session needs an empty fixture state directory")

    def session_directories(self):
        require(not self.sessions.is_symlink(), "fixture sessions path is a symlink")
        try:
            return list(self.sessions.iterdir())
        except FileNotFoundError:
            return []

    def locate(self):
        entries = self.session_directories()
        if not entries and self.directory is None:
            return None
        require(len(entries) == 1, "could not identify the only fixture session")
        directory = entries[0]
        require(not directory.is_symlink() and directory.is_dir()
                and re.fullmatch(r"[A-Z0-9]{1,8}", directory.name), "invalid fixture session directory")
        require(self.directory is None or self.directory == directory, "fixture session directory changed")
        self.directory = directory
        return directory

    def metadata(self):
        path = self.directory / "meta.json"
        require(stat.S_ISREG(path.lstat().st_mode), "invalid fixture session metadata")
        meta = json.loads(path.read_text())
        require(meta.get("id") == self.directory.name and meta.get("command") == self.command,
                "fixture session metadata does not match the registered command")
        return meta

    def worker_command_matches(self, command):
        return (command.startswith(f"{self.binary} session-worker ")
                and f" --dir {self.directory} " in command
                and command.endswith(" -- " + " ".join(self.command)))

    @contextmanager
    def connect(self, deadline):
        path = self.directory / "sock"
        require(stat.S_ISSOCK(path.lstat().st_mode), "fixture worker endpoint is not a Unix socket")
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(remaining_socket_time(deadline))
            connection.connect(str(path))
            pid = peer_pid(connection)
            if self.worker is None:
                self.worker = RetainedProcess(pid, matches=self.worker_command_matches)
            require(pid == self.worker.pid and self.worker.is_current(), "fixture worker socket peer changed")
            yield connection

    def capture_shell(self, pid):
        if self.shell is None:
            self.shell = RetainedProcess(pid, parent=self.worker)
        require(pid == self.shell.pid and self.shell.is_current(), "fixture shell changed")

    def capture(self, session, reported_pid):
        require(self.locate() == self.sessions / session, "listed session differs from the fixture session")
        meta = self.metadata()
        require(meta.get("pid") == reported_pid, "shell PID differs from fixture metadata")
        with self.connect(time.monotonic_ns() + 10_000_000_000):
            self.capture_shell(reported_pid)

    def check_current(self):
        require(self.metadata().get("pid") == self.shell.pid and self.shell.is_current(), "fixture shell changed")
        with self.connect(time.monotonic_ns() + 10_000_000_000):
            pass

    def close(self):
        with ExitStack() as references:
            references.callback(self.release_references)
            if self.locate() is None:
                return
            if self.worker is not None and self.worker.reference.exited():
                self.confirm_exit()
                return
            self.exit_shell()

    def release_references(self):
        for process in (self.shell, self.worker):
            if process is not None:
                process.close()

    def exit_shell(self):
        session = self.directory.name
        shell_pid = self.metadata().get("pid")
        deadline = time.monotonic_ns() + 10_000_000_000
        header = session.encode().ljust(8, b"\0")
        challenge = uuid.uuid4().hex.encode()
        with self.connect(deadline) as connection:
            self.capture_shell(shell_pid)
            send_frame(connection, deadline, 1, json.dumps({"type": "session.attach", "sessionId": session}).encode())
            expect_control(connection, deadline, "session.attached", session)
            command = b'\x15printf "' + challenge + b':%s:ready\\n" "$$"\n'
            send_frame(connection, deadline, 3, header + command)
            require(expect_shell(connection, deadline, header, challenge) == shell_pid and self.shell.is_current(),
                    "fixture shell did not confirm its original identity")
            # The historical worker reaps its PTY child before emitting session.exit.
            send_frame(connection, deadline, 3, header + b"exit 0\n")
            result = expect_control(connection, deadline, "session.exit", session)
            require(result.get("exitCode") == 0, "fixture shell did not exit normally")
        require(self.worker.reference.exited(remaining_socket_time(deadline)), "fixture worker did not exit")
        self.confirm_exit()

    def confirm_exit(self):
        meta = self.metadata()
        require(self.shell is not None and meta.get("pid") == self.shell.pid and self.shell.reference.exited()
                and meta.get("state") == "exited" and meta.get("exitCode") == 0
                and not os.path.lexists(self.directory / "sock"), "fixture shell reap and socket removal unconfirmed")
