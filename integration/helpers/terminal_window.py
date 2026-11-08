#!/usr/bin/env python3
"""Exercise terminal windows through real PTYs, workers and a WebSocket daemon."""

import base64
from contextlib import ExitStack
import contextvars
import errno
import hashlib
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import time

from isolation import protected_cleanup, refuse_live_paths, termination_cleanup
from owned_command import DEFAULTS
from worker_failure import WorkerFailure

sys.dont_write_bytecode = True
from mesh_control import round_trip


PROMPT = b"MESH_PROMPT> "
# The job's owner kills every descendant GRACE seconds after cancellation, so a
# fixture's whole cleanup must end well before then. Product waits are unchanged;
# a cleanup that cannot finish in this interval reports it and keeps its root.
CLEANUP_SECONDS = DEFAULTS["grace"] - 5


class CleanupDeadline:
    """One absolute allowance shared by every wait in a fixture cleanup."""

    def __init__(self, seconds, clock=time.monotonic):
        self.clock = clock
        self.end = clock() + seconds

    def remaining(self, limit, what):
        left = min(limit, self.end - self.clock())
        if left <= 0:
            raise RuntimeError(f"fixture cleanup ran out of time before {what}")
        return left

    def within(self, seconds):
        """A nested deadline that ends after seconds, or with this one if that is sooner."""
        nested = CleanupDeadline(0, self.clock)
        nested.end = min(self.end, self.clock() + seconds)
        return nested


ACTIVE_CLEANUP = contextvars.ContextVar("fixture_cleanup", default=None)


def cleanup_wait(limit, what):
    """A wait's timeout: its own limit, cut to what remains of an active fixture cleanup."""
    deadline = ACTIVE_CLEANUP.get()
    return limit if deadline is None else deadline.remaining(limit, what)


def receive_by(connection, length, deadline, what):
    """Read exactly length bytes before the deadline; fewer only when the peer closes first."""
    data = b""
    while len(data) < length:
        connection.settimeout(deadline.remaining(float("inf"), what))
        try:
            chunk = connection.recv(length - len(data))
        except TimeoutError as error:
            raise RuntimeError(f"fixture cleanup ran out of time before {what}") from error
        if not chunk:
            return data
        data += chunk
    return data
ANSI = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def eventually(check, message, timeout=4):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.01)
    raise RuntimeError(message)


class Terminal:
    def __init__(self, command, environment, cwd, timeout=4):
        self.pid, self.master = pty.fork()
        if self.pid == 0:
            os.chdir(cwd)
            fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
            os.execvpe(command[0], command, environment)
        self.output = bytearray()
        self.status = None
        self.timeout = timeout

    def drain(self):
        while select.select([self.master], [], [], 0)[0]:
            try:
                chunk = os.read(self.master, 65536)
            except OSError as error:
                if error.errno == errno.EIO:
                    break
                raise
            if not chunk:
                break
            self.output.extend(chunk)
            require(len(self.output) < 2 << 20, "terminal output exceeded the fixture limit")
            # Bubble Tea queries terminal capabilities before drawing a picker.
            # Answer the conventional xterm queries so the fixture models a terminal.
            replies = (
                (b"\x1b[6n", b"\x1b[1;1R"),
                (b"\x1b[c", b"\x1b[?1;2c"),
                (b"\x1b[>c", b"\x1b[>0;0;0c"),
                (b"\x1b[?2026$p", b"\x1b[?2026;2$y"),
            )
            for query, response in replies:
                if query in chunk:
                    self._reply(response)
        return bytes(self.output)

    def _reply(self, contents):
        try:
            self.send(contents)
        except OSError as error:
            # Buffered queries can outlive the PTY slave during teardown.
            if error.errno != errno.EIO:
                raise

    def send(self, contents):
        if isinstance(contents, str):
            contents = contents.encode()
        os.write(self.master, contents)

    def expect(self, marker, since=0):
        if isinstance(marker, str):
            marker = marker.encode()
        def printed():
            if marker in self.drain()[since:]:
                return True
            status = self.poll()
            if status is not None and marker in self.drain()[since:]:
                return True
            require(status is None, f"terminal client exited with status {status} before printing {marker!r}")
            return False
        try:
            eventually(printed, f"terminal did not print {marker!r} within {self.timeout}s", timeout=self.timeout)
        except RuntimeError as error:
            raise RuntimeError(f"{error}; output={bytes(self.output[-3000:])!r}") from error
        return bytes(self.output)

    def poll(self):
        if self.status is None:
            waited, status = os.waitpid(self.pid, os.WNOHANG)
            if waited:
                self.status = os.waitstatus_to_exitcode(status)
        return self.status

    def expect_exit(self, timeout=4):
        def exited():
            self.drain()
            return self.poll() is not None
        eventually(exited, "terminal client did not exit", timeout)

    def crash(self):
        if self.poll() is None:
            os.kill(self.pid, signal.SIGKILL)
            self.expect_exit(cleanup_wait(4, f"terminal {self.pid} exited"))

    def resize(self, cols, rows):
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        os.kill(self.pid, signal.SIGWINCH)

    def expect_size(self, cols, rows):
        start = len(self.drain())
        self.send(f'until [ "$(stty size)" = "{rows} {cols}" ]; do sleep .01; done; printf "__SIZE__"; stty size\n')
        self.expect(f"__SIZE__{rows} {cols}", since=start)

    def close(self):
        self.crash()
        os.close(self.master)


class Fixture:
    def __init__(self, binary, root):
        refuse_live_paths({"fixture root": str(root)})
        self.binary = str(Path(binary).resolve())
        self.root = Path(root)
        self.validate_destinations()
        self.helpers = Path(__file__).resolve().parent
        self.local = self.root / "local"
        self.remote = self.root / "remote"
        self.config = self.root / "config"
        self.config.mkdir()
        self.terminals = []
        self.daemon = None
        self.daemon_log = None
        self.daemon_args = []
        self.identity_requests = 0
        home = self.root / "fixture-home"
        home.mkdir()
        tools = home / "bin"
        tools.mkdir()
        (tools / "python3").symlink_to(Path(sys.executable).resolve())
        self.environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "TERM", "LANG", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS") if key in os.environ}
        self.environment.update({
            "PATH": str(tools) + os.pathsep + self.environment.get("PATH", os.defpath),
            "HOME": str(home),
            "MESH_STATE_DIR": str(self.local),
            "MESH_CONFIG_DIR": str(self.config),
            "SHELL": str(self.helpers / "window_shell.sh"),
            "TERM": "xterm-256color",
            "NO_COLOR": "1",
            "PYTHONDONTWRITEBYTECODE": "1",
            "GOTRACEBACK": "all",
        })

    def validate_destinations(self):
        names = ("local", "remote", "config", "config/hosts.json", "remote-config", "fixture-home",
                 "fixture-home/bin", "fixture-home/bin/python3", "bin", "bin/tailscale", "tailscale.json", "daemon.log")
        refuse_live_paths({"fixture root": str(self.root)} | {name: str(self.root / name) for name in names})

    def window(self, take=False):
        self.validate_destinations()
        command = [self.binary, "--window"]
        if take:
            command.append("--take")
        terminal = Terminal(command, self.environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def seed_session(self, cwd):
        self.validate_destinations()
        command = [self.binary, "local", "--", str(self.helpers / "window_shell.sh"), "retained-argument"]
        terminal = Terminal(command, self.environment, cwd)
        self.terminals.append(terminal)
        terminal.expect(PROMPT)
        return terminal

    def metadata(self, session_id):
        return json.loads((self.local / "s" / session_id / "meta.json").read_text())

    def local_listing(self):
        self.validate_destinations()
        result = subprocess.run([self.binary, "ls", "--all"], env=self.environment, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=3)
        require(result.returncode == 0, f"local listing failed: {result.stdout!r}")
        return result.stdout.decode()

    def daemon_listing(self):
        response = round_trip(str(self.local / "daemon.sock"), {"type": "session.list", "requestId": "terminal-fixture"})
        require(response.get("type") == "session.listed", f"daemon listing failed: {response}")
        return {session["id"]: session for session in response.get("sessions", [])}

    def start_local_daemon(self):
        self.validate_destinations()
        require(self.daemon is None, "fixture daemon is already running")
        self.daemon_log = open(self.root / "daemon.log", "ab")
        self.daemon = subprocess.Popen(
            [self.binary, "daemon", "--tailnet-port", "0", "--ssh-port", "0", *self.daemon_args],
            env=self.environment, stdin=subprocess.DEVNULL, stdout=self.daemon_log, stderr=subprocess.STDOUT,
        )
        eventually(lambda: (self.local / "daemon.sock").exists(), "local daemon did not create its socket")

    def stop_daemon(self):
        if self.daemon is None:
            return
        self.daemon.terminate()
        try:
            self.daemon.wait(timeout=cleanup_wait(2, "the daemon stopped"))
        except subprocess.TimeoutExpired:
            self.daemon.kill()
            # Still running after KILL is a failure to report, not a reason to wait forever.
            self.daemon.wait(timeout=cleanup_wait(5, "the daemon exited after KILL"))
        self.daemon = None
        self.daemon_log.close()
        self.daemon_log = None

    def crash_worker(self, terminal, session_id, shell_pid):
        parent = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(shell_pid)]))
        arguments = subprocess.check_output(["ps", "-o", "args=", "-p", str(parent)]).decode()
        directory = str(self.local / "s" / session_id)
        require("session-worker" in arguments and directory in arguments, f"shell parent is not the fixture worker: {arguments}")
        os.kill(parent, signal.SIGKILL)
        terminal.expect_exit()
        eventually(lambda: session_id in self.local_listing() and "interrupted" in self.local_listing(),
                   "worker crash was not reported as interrupted")

    def sessions(self, state):
        result = []
        for metadata in sorted((state / "s").glob("*/meta.json")):
            try:
                value = json.loads(metadata.read_text())
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if value.get("state") in ("running", "detached"):
                result.append(value)
        return result

    def inspect(self, state, session_id):
        response = round_trip(str(state / "s" / session_id / "sock"), {
            "type": "session.inspect", "sessionId": session_id,
            "requestId": "terminal-fixture", "previewCols": 80, "previewRows": 8,
        })
        require(response.get("type") == "session.inspected", f"inspection failed: {response}")
        return response["inspection"]

    def attached(self, state, session_id):
        return self.inspect(state, session_id)["attached"]

    def wait_local_detached(self, session_id, message):
        eventually(lambda: not self.attached(self.local, session_id)
                   and self.metadata(session_id).get("state") == "detached", message)

    def tab(self, terminal_id, command=None):
        self.validate_destinations()
        """Open a session from a terminal that names itself, the way a terminal
        emulator exports a per-tab identifier."""
        environment = dict(self.environment, MESH_TERMINAL_ID=terminal_id)
        argv = command or [self.binary, "local", "--", str(self.helpers / "window_shell.sh"), "retained-argument"]
        terminal = Terminal(argv, environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def binding_path(self, terminal_id):
        """Bindings are filed under a hash of the terminal identity, so the
        state directory never carries a terminal's own identifier as a
        filename. Deriving it here pins that on-disk contract."""
        digest = hashlib.sha256(b"mesh-terminal-v1\x00mesh\x00MESH_TERMINAL_ID=" + terminal_id.encode()).digest()
        return self.local / "terminals" / (base64.urlsafe_b64encode(digest).rstrip(b"=").decode() + ".json")

    def bound_session(self, terminal_id):
        path = self.binding_path(terminal_id)
        try:
            return json.loads(path.read_text()).get("sessionId")
        except (FileNotFoundError, json.JSONDecodeError):
            return None

    def back(self, terminal_id):
        self.validate_destinations()
        environment = dict(self.environment, MESH_TERMINAL_ID=terminal_id)
        terminal = Terminal([self.binary, "back"], environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def back_output(self, terminal_id):
        self.validate_destinations()
        environment = dict(self.environment, MESH_TERMINAL_ID=terminal_id)
        result = subprocess.run([self.binary, "back"], env=environment, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=5)
        return result.stdout.decode()

    def shell_identity(self, terminal):
        self.identity_requests += 1
        marker = f"__SESSION_{self.identity_requests}__"
        start = len(terminal.drain())
        terminal.send("printf '" + marker + "%s__PID__%s__END__\\n' \"$MESH_SESSION_ID\" \"$$\"\n")
        pattern = re.compile(marker.encode() + rb"([0-9A-Z]+)__PID__([0-9]+)__END__")
        match = eventually(lambda: pattern.search(terminal.drain()[start:]), "shell did not report its session identity")
        return match[1].decode(), int(match[2])

    def start_remote(self, initial_binary=None):
        self.validate_destinations()
        bindir = self.root / "bin"
        bindir.mkdir()
        (bindir / "tailscale").symlink_to(self.helpers / "fake_tailscale")
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({
            "BackendState": "Running",
            "Self": {"DNSName": "pc.fixture.test.", "HostName": "pc", "TailscaleIPs": ["127.0.0.1"], "Online": True},
            "Peer": {},
        }))
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        environment = self.environment | {
            "MESH_STATE_DIR": str(self.remote),
            "MESH_CONFIG_DIR": str(self.root / "remote-config"),
            "MESH_FAKE_TAILSCALE_STATUS": str(status),
            "PATH": str(bindir) + os.pathsep + self.environment["PATH"],
        }
        self.daemon_log = open(self.root / "daemon.log", "wb")
        self.daemon = subprocess.Popen(
            [str(initial_binary or self.binary), "daemon", "--tailnet-port", str(port), "--ssh-port", "0", *self.daemon_args],
            env=environment, stdin=subprocess.DEVNULL, stdout=self.daemon_log, stderr=subprocess.STDOUT,
        )
        eventually(lambda: (self.remote / "daemon.sock").exists(), "remote daemon did not create its socket")
        response = round_trip(str(self.remote / "daemon.sock"), {"type": "host.info", "requestId": "terminal-fixture"})
        require(response.get("type") == "host.info.result", f"remote identity request failed: {response}")
        self.remote_id = response["host"]["id"]
        source = subprocess.run([self.binary, "device", "identity", "--json"], env=self.environment,
                                capture_output=True, check=True, timeout=5)
        source_id = json.loads(source.stdout)["id"]
        subprocess.run([self.binary, "device", "approve", "--allow-root", "--", source_id],
                       env=environment, capture_output=True, check=True, timeout=5)

        def listening():
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    return True
            except OSError:
                return False
        eventually(listening, "remote daemon did not open its WebSocket listener")
        self.validate_destinations()
        (self.config / "hosts.json").write_text(json.dumps({
            "version": 1,
            "hosts": [{"id": self.remote_id, "meshIdentity": response["host"]["meshIdentity"],
                       "tailscaleName": "pc.fixture.test", "addresses": ["127.0.0.1"],
                       "endpoint": f"ws://127.0.0.1:{port}/mesh"}],
        }))
        os.chmod(self.config / "hosts.json", 0o600)
        if initial_binary is None:
            self.adopt_remote_name(response["host"])

    def adopt_remote_name(self, host):
        subprocess.run([self.binary, "serve", "ls"],
                       env=self.environment, capture_output=True, check=True, timeout=5)
        claim = json.loads((self.config / "machine-names" / (self.remote_id + ".json")).read_text())
        require(claim["id"] == self.remote_id and claim["machineName"] == host["machineName"],
                "authenticated service read did not adopt the destination declaration")

    def nested(self):
        self.start_remote()
        terminal = self.window()
        terminal.expect(PROMPT)
        outer_id, outer_pid = self.shell_identity(terminal)
        start = len(terminal.drain())
        terminal.send(shlex.quote(self.binary) + " pc\n")
        terminal.expect(PROMPT, since=start)
        inner_id, inner_pid = self.shell_identity(terminal)
        require(inner_id != outer_id, "remote command did not enter a new session")
        eventually(lambda: self.inspect(self.local, outer_id).get("nested"), "outer worker did not register the remote attachment")
        require(self.attached(self.remote, inner_id), "remote shell is not attached")
        return terminal, outer_id, outer_pid, inner_id, inner_pid

    def close(self, allowance=CLEANUP_SECONDS):
        """Stop terminals, the daemon and every worker within one absolute allowance.

        Termination signals cannot interrupt or restart it. An exception means
        something may still use the fixture root, so its caller must keep it.
        """
        deadline = ACTIVE_CLEANUP.get() or CleanupDeadline(allowance)
        token = ACTIVE_CLEANUP.set(deadline)
        try:
            with protected_cleanup():
                self.close_within(deadline)
        finally:
            ACTIVE_CLEANUP.reset(token)

    def close_within(self, deadline):
        with ExitStack() as connections:
            with ExitStack() as processes:
                self.stop_processes_later(processes)
                workers = self.request_worker_kills(connections, deadline)
            for socket_path, connection in workers:
                self.await_worker(socket_path, connection, deadline)

    def stop_processes_later(self, processes):
        processes.callback(self.stop_daemon)
        for terminal in self.terminals:
            processes.callback(terminal.close)

    def request_worker_kills(self, connections, deadline):
        workers = []
        sockets = [path for state in {self.local, self.remote} for path in (state / "s").glob("*/sock")]
        for socket_path in sockets:
            connection = self.request_worker_kill(connections, socket_path, deadline)
            if connection is not None:
                workers.append((socket_path, connection))
        return workers

    def request_worker_kill(self, connections, socket_path, deadline):
        connection = connections.enter_context(socket.socket(socket.AF_UNIX))
        connection.settimeout(deadline.remaining(0.3, f"worker {socket_path} accepted a kill"))
        try:
            connection.connect(str(socket_path))
        except OSError as error:
            if error.errno in (errno.ENOENT, errno.ECONNREFUSED):
                return None
            raise
        request = json.dumps({"type": "session.kill", "sessionId": socket_path.parent.name}).encode()
        try:
            connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
        except (BrokenPipeError, ConnectionResetError):
            pass
        return connection

    def await_worker(self, socket_path, connection, deadline):
        with WorkerFailure(self, socket_path, connection, deadline):
            self.await_worker_completion(socket_path, connection, deadline)

    def await_worker_completion(self, socket_path, connection, deadline):
        # Match the product's control exchange deadline, which includes kill
        # escalation and the retained output of a never-attached command.
        exchange = deadline.within(10)
        what = f"worker {socket_path} answered its kill"
        try:
            kind = receive_by(connection, 1, exchange, what)
            if kind:
                require(kind == b"\x01", f"worker {socket_path}: unexpected completion frame")
                length = struct.unpack(">I", self.complete(receive_by(connection, 4, exchange, what), 4))[0]
                require(length <= 4 << 20, f"worker {socket_path}: oversized completion")
                response = json.loads(self.complete(receive_by(connection, length, exchange, what), length))
                require(response.get("type") == "ok", f"worker {socket_path}: kill failed: {response}")
        except (BrokenPipeError, ConnectionResetError):
            pass
        # A finishing worker can close or reset without accepting the kill.
        # Neither peer closure nor acknowledgment covers its final disk writes.
        eventually(lambda: not socket_path.exists(),
                   f"fixture worker {socket_path} did not finish writing its session data",
                   deadline.remaining(4, f"worker {socket_path} removed its socket"))

    @staticmethod
    def complete(data, length):
        require(len(data) == length, "daemon closed an incomplete response")
        return data


def nested_detach(fixture):
    terminal, outer_id, outer_pid, inner_id, inner_pid = fixture.nested()
    start = len(terminal.drain())
    terminal.send(b"\x1d")
    terminal.expect(PROMPT, since=start)
    require(terminal.poll() is None, "ctrl+] detached the outer window")
    require(fixture.shell_identity(terminal) == (outer_id, outer_pid), "ctrl+] did not return to the local shell")
    eventually(lambda: not fixture.attached(fixture.remote, inner_id), "ctrl+] did not detach the remote session")
    eventually(lambda: not fixture.inspect(fixture.local, outer_id).get("nested"), "inner detach left a nesting registration")
    os.kill(inner_pid, 0)

    start = len(terminal.drain())
    terminal.send(shlex.quote(fixture.binary) + " pc -r\n")
    terminal.expect(PROMPT, since=start)
    require(fixture.shell_identity(terminal) == (inner_id, inner_pid), "remote resume changed the shell process")
    terminal.send(b"\x1e")
    terminal.expect_exit()
    eventually(lambda: not fixture.attached(fixture.local, outer_id), "ctrl+^ did not detach the local window")
    require(fixture.attached(fixture.remote, inner_id), "ctrl+^ detached the nested remote attachment")
    os.kill(outer_pid, 0)
    os.kill(inner_pid, 0)


def nested_resize(fixture):
    terminal, _, _, _, _ = fixture.nested()
    terminal.resize(109, 37)
    terminal.expect_size(109, 37)
    terminal.resize(80, 24)
    terminal.expect_size(80, 24)


def window_death(fixture):
    terminal, outer_id, outer_pid, inner_id, inner_pid = fixture.nested()
    terminal.send("printf 'REMOTE_SURVIVAL_MARKER\\n'\n")
    terminal.expect(b"\rREMOTE_SURVIVAL_MARKER\r\n")
    terminal.crash()
    fixture.wait_local_detached(outer_id, "crashed window detach was not published")
    require(fixture.attached(fixture.remote, inner_id), "window death detached the remote client")
    os.kill(outer_pid, 0)
    os.kill(inner_pid, 0)
    restored = fixture.window()
    restored.expect("on pc/" + inner_id)
    start = len(restored.drain())
    restored.send(b"\r")
    eventually(lambda: fixture.attached(fixture.local, outer_id), "resume prompt did not attach the local session")
    restored.expect(b"REMOTE_SURVIVAL_MARKER", since=start)
    require(fixture.shell_identity(restored) == (inner_id, inner_pid), "window resume did not return to the same remote shell")


def window_entry(fixture):
    refused = subprocess.run([fixture.binary, "--window"], env=fixture.environment, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=3)
    require(refused.returncode != 0 and b"terminal" in refused.stdout.lower(), f"non-TTY window was accepted: {refused.stdout!r}")
    first = fixture.window(take=True)
    before_prompt = first.expect(PROMPT).split(PROMPT, 1)[0]
    require(ANSI.sub(b"", before_prompt).strip() == b"", f"fresh window printed before the shell prompt: {before_prompt!r}")
    first_identity = fixture.shell_identity(first)
    first.send(b"\x1d")
    first.expect_exit()
    fixture.wait_local_detached(first_identity[0], "first window detach was not published")

    # Two terminals race the same detached session. Exactly one resumes it;
    # the other starts fresh, and neither loses its attachment.
    second = fixture.window(take=True)
    third = fixture.window(take=True)
    second.expect(PROMPT)
    third.expect(PROMPT)
    identities = [fixture.shell_identity(second), fixture.shell_identity(third)]
    require(identities.count(first_identity) == 1, f"concurrent --take did not claim once: {identities}")
    require(identities[0][0] != identities[1][0], "two windows claimed the same session")
    require(second.poll() is None and third.poll() is None, "opening a window stole an active attachment")
    for session_id, _ in identities:
        require(fixture.attached(fixture.local, session_id), "concurrent window left a session detached")

    fourth = fixture.window()
    fourth.expect(PROMPT)
    require(fixture.shell_identity(fourth)[0] not in [identity[0] for identity in identities], "ordinary window stole an attached session")
    count = len(fixture.sessions(fixture.local))
    start = len(fourth.drain())
    fourth.send(shlex.quote(fixture.binary) + " --window\n")
    fourth.expect("Already inside a Mesh session", since=start)
    fourth.expect(PROMPT, since=start)
    require(len(fixture.sessions(fixture.local)) == count, "window inside a session created another worker")


def relaunch_interrupted(fixture, old_id, old_metadata):
    terminal = fixture.window()
    terminal.expect("interrupted")
    terminal.expect(old_id)
    start = len(terminal.drain())
    terminal.send(b"\r")
    terminal.expect(PROMPT, since=start)
    session_id, shell_pid = fixture.shell_identity(terminal)
    require(session_id != old_id and shell_pid != old_metadata["pid"], "relaunch reused an interrupted session identity or process")
    metadata = fixture.metadata(session_id)
    require(metadata["command"] == [fixture.environment["SHELL"]], f"recovery did not open the configured shell: {metadata}")
    require(metadata.get("recoveredFrom") == old_id, f"replacement lost its previous attempt: {metadata}")
    require(metadata["cwd"] == old_metadata["cwd"], f"relaunch changed recorded directory: {metadata}")
    start = len(terminal.drain())
    terminal.send("printf '__DIRECTORY__%s__END__\\n' \"$PWD\"\n")
    terminal.expect("__DIRECTORY__" + old_metadata["cwd"] + "__END__", since=start)
    require(old_id in fixture.local_listing(), "recovery discarded the previous attempt")
    return terminal, session_id, shell_pid


def wait_for_forgotten_session(fixture, session_id):
    # The daemon publishes catalog retirement before removing the directory.
    eventually(lambda: session_id not in fixture.local_listing() and session_id not in fixture.daemon_listing()
               and not (fixture.local / "s" / session_id).exists(),
               "x did not finish forgetting the interrupted session")


def window_relaunch(fixture):
    project = fixture.root / "recorded-project"
    project.mkdir()
    original = fixture.seed_session(project)
    original_id, original_pid = fixture.shell_identity(original)
    recorded = fixture.metadata(original_id)
    fixture.start_local_daemon()
    eventually(lambda: original_id in fixture.daemon_listing(), "daemon did not import the original local session")
    fixture.crash_worker(original, original_id, original_pid)
    replacement, replacement_id, replacement_pid = relaunch_interrupted(fixture, original_id, recorded)
    eventually(lambda: original_id in fixture.daemon_listing() and replacement_id in fixture.daemon_listing(),
               "online recovery did not retain both daemon catalog records")
    require((fixture.local / "s" / original_id).exists(), "online recovery removed the previous session directory")

    fixture.stop_daemon()
    require(not (fixture.local / "daemon.sock").exists(), "daemon socket survived shutdown")
    replacement_record = fixture.metadata(replacement_id)
    fixture.crash_worker(replacement, replacement_id, replacement_pid)
    restored, restored_id, restored_pid = relaunch_interrupted(fixture, replacement_id, replacement_record)
    fixture.start_local_daemon()
    eventually(lambda: replacement_id in fixture.daemon_listing() and restored_id in fixture.daemon_listing(),
               "daemon restart lost a retained recovery attempt")
    require((fixture.local / "s" / replacement_id).exists(), "daemon removed a retained previous attempt")

    # A failed create must leave the only durable launch recipe available.
    restored_record = fixture.metadata(restored_id)
    fixture.crash_worker(restored, restored_id, restored_pid)
    project.rmdir()
    failed = Terminal([fixture.binary, "recover", restored_id, "--command"], fixture.environment, fixture.root)
    fixture.terminals.append(failed)
    failed.expect_exit()
    require(failed.status != 0, "relaunch from a missing working directory succeeded")
    require(fixture.metadata(restored_id) == restored_record, "failed relaunch changed the interrupted metadata")
    require(restored_id in fixture.local_listing(), "failed relaunch forgot the local record")
    require(restored_id in fixture.daemon_listing(), "failed relaunch removed the daemon record")

    forgotten = fixture.window()
    forgotten.expect("interrupted")
    forgotten.expect(restored_id)
    forgotten.send(b"x")
    wait_for_forgotten_session(fixture, restored_id)
    forgotten.send(b"\x1b")
    forgotten.expect_exit()


def tab_bindings(fixture):
    # Two terminals, two sessions. The point of the binding is that each tab
    # returns to its own session, not to whichever one was most recent.
    first = fixture.tab("tab-one")
    first.expect(PROMPT)
    first_id, first_pid = fixture.shell_identity(first)
    second = fixture.tab("tab-two")
    second.expect(PROMPT)
    second_id, second_pid = fixture.shell_identity(second)
    require(first_id != second_id, f"both terminals opened the same session {first_id}")

    eventually(lambda: fixture.bound_session("tab-one") == first_id,
               f"tab-one was not bound to {first_id}")
    eventually(lambda: fixture.bound_session("tab-two") == second_id,
               f"tab-two was not bound to {second_id}")

    # The host dies under both of them.
    fixture.crash_worker(first, first_id, first_pid)
    fixture.crash_worker(second, second_id, second_pid)

    # tab-one returns to its own session. Recovery mints a new id, and the
    # binding has to follow it or the next crash recovers the wrong thing.
    returned = fixture.back("tab-one")
    returned.expect(PROMPT)
    recovered_id, _ = fixture.shell_identity(returned)
    require(recovered_id != first_id, f"recovery reused the interrupted id {first_id}")
    require(fixture.metadata(recovered_id).get("recoveredFrom") == first_id,
            f"session {recovered_id} was not recovered from {first_id}")
    eventually(lambda: fixture.bound_session("tab-one") == recovered_id,
               f"tab-one still points at {first_id} instead of its replacement {recovered_id}")

    # tab-two is untouched by what tab-one did.
    require(fixture.bound_session("tab-two") == second_id,
            "recovering tab-one moved tab-two's binding")

    # A terminal that has opened nothing explains itself instead of guessing.
    unbound = fixture.back_output("tab-three")
    require("has not opened a Mesh session yet" in unbound,
            f"an unbound terminal did not explain itself: {unbound!r}")

    returned.send(b"exit\n")
    returned.expect_exit()



# A completed, harmless sequence. Nothing in this fixture opens a clipboard.
CLIPBOARD_OSC = b"\x1b]52;c;" + base64.b64encode(b"mesh-fixture-only") + b"\x07"
CLIPBOARD_DONE = b"__OSC52_COMPLETE__"
CLIPBOARD_BOOT = b"__CLIPBOARD_BOOT__"
CLIPBOARD_READY = b"__FRESH_READY__"
CLIPBOARD_SSH = ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY", "MESH_175_SSH_LOG")


def clipboard_command(fault):
    """A session-leading PTY command: input is instructions, never echoed payload."""
    settings = termios.tcgetattr(0)
    settings[3] &= ~termios.ECHO
    termios.tcsetattr(0, termios.TCSANOW, settings)
    os.write(1, b"__RAW_ONLY_BEFORE_CAPTURE__\r\n\x1b[2J\x1b[H" + CLIPBOARD_BOOT + b"\n")
    for line in sys.stdin:
        clipboard_instruction(line.strip(), fault)
    return 0


def clipboard_instruction(instruction, fault):
    if instruction == "identity":
        observation = {"sid": os.environ.get("MESH_SESSION_ID"), "pid": os.getpid(),
                       "ssh": {name: os.environ[name] for name in CLIPBOARD_SSH if name in os.environ},
                       "display": {name: os.environ[name] for name in ("DISPLAY", "WAYLAND_DISPLAY") if name in os.environ}}
        os.write(1, b"__IDENTITY__" + json.dumps(observation).encode() + b"__END__\n")
        return
    if instruction == "emit":
        emitted = {"omit": b"", "wrong": b"\x1b]52;c;d3Jvbmc=\x07"}.get(fault, CLIPBOARD_OSC)
        os.write(1, emitted + CLIPBOARD_DONE + b"\n")
        return
    if instruction == "fresh" and fault != "absent-ready":
        os.write(1, CLIPBOARD_READY + b"\n")


class ClipboardFixture(Fixture):
    """U4's resources live under the reviewed owner's root until its descendant join.

    Fixture.close cooperates with workers; it is not join proof. This subclass
    never removes its state or binary. Only Owner.finish does that after ECHILD.
    """

    def __init__(self, binary, root, deadline, fault):
        self.deadline, self.fault = deadline, fault
        self.resources = {"root": str(root), "binary": binary, "sessions": [], "clients": [], "daemon": None}
        self.root = Path(root)
        self.record_resources()
        super().__init__(binary, root)

    def record_resources(self):
        (self.root / "resources.json").write_text(json.dumps(self.resources, sort_keys=True))

    def command(self, arguments, environment):
        return subprocess.run(arguments, env=environment, stdin=subprocess.DEVNULL, capture_output=True,
                              check=True, timeout=self.deadline.remaining(5, "fixture command"))

    def wait(self, check, message, limit=4):
        return eventually(check, message, self.deadline.remaining(limit, message))

    def control(self, path, request):
        data = json.dumps(request).encode()
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(self.deadline.remaining(4, "control connect"))
            connection.connect(str(path))
            connection.sendall(b"\x01" + struct.pack(">I", len(data)) + data)
            header = self.complete(receive_by(connection, 5, self.deadline, "control header"), 5)
            require(header[0] == 1, "clipboard control returned a non-control frame")
            length = struct.unpack(">I", header[1:])[0]
            require(length <= 4 << 20, "clipboard control response exceeded its bound")
            return json.loads(self.complete(receive_by(connection, length, self.deadline, "control body"), length))

    def start_destination(self):
        # Private loopback only, with the same fake discovery and device approval
        # as the reviewed terminal adapter. No installed daemon or host config.
        bindir = self.root / "bin"
        bindir.mkdir()
        (bindir / "tailscale").symlink_to(self.helpers / "fake_tailscale")
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({"BackendState": "Running", "Self": {
            "DNSName": "pc.fixture.test.", "HostName": "pc", "TailscaleIPs": ["127.0.0.1"], "Online": True}, "Peer": {}}))
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        environment = self.environment | {"MESH_STATE_DIR": str(self.remote),
            "MESH_CONFIG_DIR": str(self.root / "remote-config"), "MESH_FAKE_TAILSCALE_STATUS": str(status),
            "PATH": str(bindir) + os.pathsep + self.environment["PATH"]}
        arguments = [self.binary, "daemon", "--tailnet-port", str(port), "--ssh-port", "0"]
        self.resources["daemon"] = {"command": arguments, "state": str(self.remote)}
        self.record_resources()
        self.daemon_log = open(self.root / "daemon.log", "wb")
        self.daemon = subprocess.Popen(arguments, env=environment, stdin=subprocess.DEVNULL,
                                       stdout=self.daemon_log, stderr=subprocess.STDOUT)
        self.wait(self.destination_started, "clipboard destination did not start")
        response = self.control(self.remote / "daemon.sock", {"type": "host.info", "requestId": "clipboard-host"})
        require(response.get("type") == "host.info.result", f"clipboard host lookup failed: {response}")
        source = self.command([self.binary, "device", "identity", "--json"], self.environment)
        self.command([self.binary, "device", "approve", "--allow-root", "--", json.loads(source.stdout)["id"]], environment)
        (self.config / "hosts.json").write_text(json.dumps({"version": 1, "hosts": [{
            "id": response["host"]["id"], "meshIdentity": response["host"]["meshIdentity"],
            "tailscaleName": "pc.fixture.test", "addresses": ["127.0.0.1"],
            "endpoint": f"ws://127.0.0.1:{port}/mesh"}]}))
        (self.config / "hosts.json").chmod(0o600)
        self.command([self.binary, "serve", "ls"], self.environment)

    def destination_started(self):
        require(self.daemon.poll() is None,
                f"clipboard daemon exited with {self.daemon.returncode}; root={self.root}; files={list(self.remote.glob('*'))}")
        return (self.remote / "daemon.sock").exists()

    def create_destination_session(self):
        arguments = [str(Path(sys.executable).resolve()), "-B", str(Path(__file__).resolve()), "--clipboard-command", self.fault]
        self.resources["sessions"].append({"command": arguments, "state": str(self.remote), "cwd": str(self.root)})
        self.record_resources()
        response = self.control(self.remote / "daemon.sock", {"type": "session.create", "requestId": "clipboard-create",
            "command": arguments, "cwd": str(self.root), "cols": 80, "rows": 24, "term": "xterm-256color"})
        require(response.get("type") == "session.created", f"clipboard create failed: {response}")
        self.sid = response["sessionId"]
        self.resources["sessions"][0]["sid"] = self.sid
        self.record_resources()
        self.wait(lambda: (self.remote / "s" / self.sid / "meta.json").exists(), "clipboard metadata absent")
        self.child_pid = json.loads((self.remote / "s" / self.sid / "meta.json").read_text())["pid"]

    def attach_destination(self):
        arguments = [self.binary, self.sid]
        self.resources["clients"].append({"command": arguments, "lastSeq": None})
        self.record_resources()
        terminal = Terminal(arguments, self.environment, self.root,
                            timeout=self.deadline.remaining(4, "native attachment"))
        self.terminals.append(terminal)
        self.resources["clients"][-1]["pid"] = terminal.pid
        self.record_resources()
        terminal.expect(CLIPBOARD_BOOT)
        self.identity(terminal)
        return terminal

    def observation(self):
        response = self.control(self.remote / "s" / self.sid / "sock", {
            "type": "session.inspect", "sessionId": self.sid, "previewCols": 80, "previewRows": 24})
        require(response.get("type") == "session.inspected", f"clipboard inspection failed: {response}")
        return response["inspection"]

    def identity(self, terminal):
        start = len(terminal.drain())
        terminal.send(b"identity\n")
        terminal.timeout = self.deadline.remaining(4, "native identity")
        terminal.expect(b"__END__", since=start)
        payload = terminal.drain()[start:].split(b"__IDENTITY__", 1)[1].split(b"__END__", 1)[0]
        identity = json.loads(payload)
        require(identity == {"sid": self.sid, "pid": self.child_pid, "ssh": {}, "display": {}},
                f"controlled command has wrong identity/environment: {identity}")
        require(self.observation()["attached"], "native client did not attach the destination session")
        require(len(self.sessions(self.remote)) == 1, "native attach created another destination session")
        require(not self.sessions(self.local), "native attach created a local session")
        return identity

    def detach(self, terminal):
        terminal.send(b"\x1d")
        terminal.expect_exit(self.deadline.remaining(4, "native detach"))
        require(terminal.status == 0, f"native detach failed: {terminal.status}")
        self.wait(lambda: not self.observation()["attached"], "native detach stayed attached")


def clipboard_marker(fixture):
    fixture.start_destination()
    fixture.create_destination_session()
    live = fixture.attach_destination()
    start = len(live.drain())
    require(CLIPBOARD_OSC not in live.output, "OSC was emitted before the live test interval")
    live.send(b"emit\n")
    live.timeout = fixture.deadline.remaining(4, "live completion")
    live.expect(CLIPBOARD_DONE, since=start)
    interval = live.drain()[start:]
    require(interval.count(CLIPBOARD_OSC) == 1, "live interval did not forward the exact OSC52 once")
    require(interval.index(CLIPBOARD_OSC) < interval.index(CLIPBOARD_DONE), "visible completion preceded OSC52")
    preview = fixture.wait(lambda: clipboard_preview(fixture), "inspection did not reach visible completion")
    text = "\n".join(preview["preview"]).encode()
    require(CLIPBOARD_OSC not in text and base64.b64encode(b"mesh-fixture-only") not in text,
            "inspection leaked the completed clipboard sequence")
    fixture.detach(live)
    fresh = fixture.attach_destination()
    fresh.expect(CLIPBOARD_DONE)
    require(b"\x1b[2J\x1b[H" in fresh.drain(), "fresh capture did not repaint the screen")
    require(b"__RAW_ONLY_BEFORE_CAPTURE__" not in fresh.output, "fresh attach replayed raw history")
    # A new native CLI invocation has no LastSeq. The old visible marker proves
    # a nonempty capture; a new response proves input and live output are ready.
    boundary = len(fresh.drain())
    fresh.send(b"fresh\n")
    fresh.timeout = fixture.deadline.remaining(0.5 if fixture.fault == "absent-ready" else 4, "fresh readiness")
    clipboard_expect_ready(fixture, fresh, boundary)
    require(CLIPBOARD_OSC not in fresh.drain(), "fresh capture replayed the completed OSC52")
    fixture.identity(fresh)
    fixture.detach(fresh)
    print(f"PASS: clipboard SID={fixture.sid} child={fixture.child_pid}; live OSC52=1; ready fresh capture OSC52=0", flush=True)


class ClipboardReadinessTimeout(RuntimeError):
    pass


def clipboard_expect_ready(fixture, terminal, boundary):
    deadline = time.monotonic() + terminal.timeout
    while time.monotonic() < deadline:
        status = terminal.poll()
        require(status is None, f"clipboard readiness client exited with status {status}")
        if CLIPBOARD_READY in terminal.drain()[boundary:]:
            return
        time.sleep(0.01)
    # A missing marker is a valid control only after a new, acknowledged exchange
    # proves the intended command, session and native attachment are still alive.
    fixture.identity(terminal)
    raise ClipboardReadinessTimeout("live clipboard command omitted __FRESH_READY__")


def clipboard_expected_failure(fault, error):
    if fault == "absent-ready":
        return isinstance(error, ClipboardReadinessTimeout)
    return fault in ("omit", "wrong") and str(error) == "live interval did not forward the exact OSC52 once"


def clipboard_preview(fixture):
    observation = fixture.observation()
    return observation if CLIPBOARD_DONE.decode() in "\n".join(observation.get("preview", [])) else None


def clipboard_campaign(binary, root):
    deadline = CleanupDeadline(25)
    token = ACTIVE_CLEANUP.set(deadline)
    try:
        for fault in ("none", "omit", "wrong", "absent-ready"):
            clipboard_case(binary, root, deadline, fault)
    finally:
        ACTIVE_CLEANUP.reset(token)


def clipboard_case(binary, root, deadline, fault):
    directory = root / fault
    directory.mkdir()
    fixture = ClipboardFixture(binary, directory, deadline, fault)
    try:
        clipboard_marker(fixture)
        require(fault == "none", f"clipboard control {fault} unexpectedly passed")
    except RuntimeError as error:
        if not clipboard_expected_failure(fault, error):
            print_diagnostics(fixture)
            raise
        print(f"PASS: clipboard control {fault} failed behaviorally: {error}", flush=True)
    finally:
        fixture.close()
    # The accepted owner, not socket disappearance, authorizes later deletion.
    require(directory.is_dir(), "clipboard state disappeared before descendant join")


def clipboard_owned(binary):
    root = Path(os.environ["MESH_OWNED_ROOT"])
    # Register both roots before creating the copy or any fixture process.
    (root / "clipboard-resources.json").write_text(json.dumps({"state": str(root), "binary": str(root / "mesh")}))
    shutil.copy2(binary, root / "mesh")
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("clipboard budget exhausted")))
    with termination_cleanup():
        signal.alarm(25)
        try:
            clipboard_campaign(str(root / "mesh"), root)
        finally:
            signal.alarm(0)
    return 0


def run_owned_clipboard(binary):
    # Reuse C's controller/owner unchanged. Its admission captures the binary,
    # command and root before setup, and it observes owner AND launcher exit.
    from owned_command import Controller, Triggers
    from process_tree import Kernel
    Kernel().require()
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    triggers = Triggers()
    root = Path(tempfile.mkdtemp(prefix="m-clipboard-"))
    controller = Controller(root, triggers)
    with open(root / "output.log", "w") as output:
        job = controller.start("clipboard", {"stdout": output, "stderr": subprocess.STDOUT},
            command=[sys.executable, "-B", str(Path(__file__).resolve()), "--clipboard-owned", str(Path(binary).resolve())],
            inherit_env=True, budget=25, scratch_parent=str(root))
    controller.wait([job])
    print((root / "output.log").read_text(), end="", flush=True)
    if not controller.proved():
        print(f"FAIL: clipboard owner/launcher join unknown; kept {root}: {job.outcome}", file=sys.stderr)
        return 1
    status = job.outcome.get("returncode", 1)
    shutil.rmtree(root)
    trigger = controller.check()
    return trigger.status if trigger is not None else status


SCENARIOS = {
    "nested-detach": nested_detach,
    "nested-resize": nested_resize,
    "window-death": window_death,
    "window-entry": window_entry,
    "window-relaunch": window_relaunch,
    "tab-bindings": tab_bindings,
    "clipboard-marker": clipboard_marker,
}


def print_diagnostics(fixture):
    if fixture.daemon_log is not None:
        fixture.daemon_log.flush()
        print((fixture.root / "daemon.log").read_text(), file=sys.stderr)
    for terminal in fixture.terminals:
        print(f"terminal {terminal.pid}: {terminal.drain()[-3000:]!r}", file=sys.stderr)


def close_fixture(fixture, directory, allowance=CLEANUP_SECONDS):
    """Delete the fixture root only after its cleanup proved that nothing still uses it."""
    # Subclasses extend close. One deadline and one protection span all of it, before,
    # inside and after the base cleanup, and an earlier deadline is never replaced.
    deadline = ACTIVE_CLEANUP.get() or CleanupDeadline(allowance)
    token = ACTIVE_CLEANUP.set(deadline)
    try:
        with protected_cleanup():
            fixture.close()
            deadline.remaining(float("inf"), "the fixture root could be deleted")
    except BaseException:
        print(f"FAIL: fixture cleanup did not finish; kept {directory}", file=sys.stderr, flush=True)
        raise
    finally:
        ACTIVE_CLEANUP.reset(token)
    shutil.rmtree(directory)


def run_fixture(fixture_type, binary, prefix, exercise, limit):
    """Run one scenario. The first termination signal enters the fixture's cleanup exactly once."""
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError(limit)))
    with termination_cleanup():
        signal.alarm(25)
        directory = tempfile.mkdtemp(prefix=prefix)
        try:
            fixture = fixture_type(binary, directory)
        except BaseException:
            signal.alarm(0)
            shutil.rmtree(directory)
            raise
        try:
            exercise(fixture)
        except BaseException:
            print_diagnostics(fixture)
            raise
        finally:
            signal.alarm(0)
            close_fixture(fixture, directory)


def main():
    scenario, binary = sys.argv[1:]
    if scenario == "clipboard-marker":
        require(run_owned_clipboard(binary) == 0, "owned clipboard campaign failed")
        return
    run_fixture(Fixture, binary, "mesh-window-", SCENARIOS[scenario], "terminal fixture exceeded 25 seconds")
    print(f"PASS: {scenario} through PTYs and persistent workers")


def run():
    try:
        main()
        return 0
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


def containing_worker(pid=None, limit=64):
    """The Mesh session worker above pid, found the way the product's own ancestry walk finds it."""
    pid = os.getppid() if pid is None else pid
    for _ in range(limit):
        if pid <= 1:
            return None
        try:
            arguments = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            parent = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[1])
        except (OSError, ValueError, IndexError):
            return None
        if len(arguments) > 3 and arguments[1] == b"session-worker" and b"--id" in arguments:
            return pid
        pid = parent
    return None


def run_escaped(runner):
    # A subreaper between the containing worker and this process adopts the
    # orphan below that worker. Refuse rather than run with the wrong ancestry.
    worker = containing_worker()
    if worker is not None:
        print(f"FAIL: an intervening subreaper kept this fixture under Mesh session worker {worker}; "
              "ancestry-sensitive fixtures refuse to run there", file=sys.stderr)
        return 1
    try:
        return runner() or 0
    except SystemExit as stopped:
        return stopped.code if isinstance(stopped.code, int) else 1


def await_escaped(intermediate, read_end):
    """The escaped coordinator's result. This parent outlives its cleanup, so callers cannot start theirs early."""
    with protected_cleanup():
        os.waitpid(intermediate, 0)
        result = os.read(read_end, 1)
    os.close(read_end)
    return 0 if result == b"0" else 1


def run_outside_containing_session(runner=run):
    # The verification command can itself run inside Mesh. Reparent the test
    # coordinator so its fresh PTYs do not inherit that unrelated worker path.
    read_end, write_end = os.pipe()
    intermediate = os.fork()
    if intermediate:
        os.close(write_end)
        return await_escaped(intermediate, read_end)
    os.close(read_end)
    intermediate_pid = os.getpid()
    if os.fork():
        os._exit(0)
    while os.getppid() == intermediate_pid:
        time.sleep(0.001)
    os.setsid()
    code = run_escaped(runner)
    sys.stdout.flush()
    sys.stderr.flush()
    os.write(write_end, str(code).encode())
    os.close(write_end)
    os._exit(code)


if __name__ == "__main__":
    if sys.argv[1:2] == ["--clipboard-command"]:
        raise SystemExit(clipboard_command(sys.argv[2]))
    if sys.argv[1:2] == ["--clipboard-owned"]:
        raise SystemExit(clipboard_owned(sys.argv[2]))
    raise SystemExit(run_outside_containing_session())
