#!/usr/bin/env python3
"""Exercise terminal windows through real PTYs, workers and a WebSocket daemon."""

import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import time

sys.dont_write_bytecode = True
from mesh_control import round_trip


PROMPT = b"MESH_PROMPT> "
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
    def __init__(self, command, environment, cwd):
        self.pid, self.master = pty.fork()
        if self.pid == 0:
            os.chdir(cwd)
            fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
            os.execvpe(command[0], command, environment)
        self.output = bytearray()
        self.status = None

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
                    self.send(response)
        return bytes(self.output)

    def send(self, contents):
        if isinstance(contents, str):
            contents = contents.encode()
        os.write(self.master, contents)

    def expect(self, marker, since=0):
        if isinstance(marker, str):
            marker = marker.encode()
        try:
            eventually(lambda: marker in self.drain()[since:], f"terminal did not print {marker!r}")
        except RuntimeError as error:
            raise RuntimeError(f"{error}; output={bytes(self.output[-3000:])!r}") from error
        return bytes(self.output)

    def poll(self):
        if self.status is None:
            waited, status = os.waitpid(self.pid, os.WNOHANG)
            if waited:
                self.status = os.waitstatus_to_exitcode(status)
        return self.status

    def expect_exit(self):
        def exited():
            self.drain()
            return self.poll() is not None
        eventually(exited, "terminal client did not exit")

    def crash(self):
        if self.poll() is None:
            os.kill(self.pid, signal.SIGKILL)
            self.expect_exit()

    def resize(self, cols, rows):
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        os.kill(self.pid, signal.SIGWINCH)

    def close(self):
        self.crash()
        os.close(self.master)


class Fixture:
    def __init__(self, binary, root):
        self.binary = str(Path(binary).resolve())
        self.root = Path(root)
        self.helpers = Path(__file__).resolve().parent
        self.local = self.root / "local"
        self.remote = self.root / "remote"
        self.config = self.root / "config"
        self.config.mkdir()
        self.terminals = []
        self.daemon = None
        self.daemon_log = None
        self.identity_requests = 0
        self.environment = os.environ.copy()
        for key in ("MESH_SESSION_ID", "MESH_HOST_ID", "MESH_DEPTH", "BASH_ENV", "ENV"):
            self.environment.pop(key, None)
        self.environment.update({
            "MESH_STATE_DIR": str(self.local),
            "MESH_CONFIG_DIR": str(self.config),
            "SHELL": str(self.helpers / "window_shell.sh"),
            "TERM": "xterm-256color",
            "NO_COLOR": "1",
        })

    def window(self, take=False):
        command = [self.binary, "--window"]
        if take:
            command.append("--take")
        terminal = Terminal(command, self.environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def seed_session(self, cwd):
        command = [self.binary, "local", "--", str(self.helpers / "window_shell.sh"), "retained-argument"]
        terminal = Terminal(command, self.environment, cwd)
        self.terminals.append(terminal)
        terminal.expect(PROMPT)
        return terminal

    def metadata(self, session_id):
        return json.loads((self.local / "s" / session_id / "meta.json").read_text())

    def local_listing(self):
        result = subprocess.run([self.binary, "ls"], env=self.environment, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=3)
        require(result.returncode == 0, f"local listing failed: {result.stdout!r}")
        return result.stdout.decode()

    def daemon_listing(self):
        response = round_trip(str(self.local / "daemon.sock"), {"type": "session.list", "requestId": "terminal-fixture"})
        require(response.get("type") == "session.listed", f"daemon listing failed: {response}")
        return {session["id"]: session for session in response.get("sessions", [])}

    def start_local_daemon(self):
        require(self.daemon is None, "fixture daemon is already running")
        self.daemon_log = open(self.root / "daemon.log", "ab")
        self.daemon = subprocess.Popen(
            [self.binary, "daemon", "--tailnet-port", "0", "--ssh-port", "0"],
            env=self.environment, stdin=subprocess.DEVNULL, stdout=self.daemon_log, stderr=subprocess.STDOUT,
        )
        eventually(lambda: (self.local / "daemon.sock").exists(), "local daemon did not create its socket")

    def stop_daemon(self):
        if self.daemon is None:
            return
        self.daemon.terminate()
        try:
            self.daemon.wait(timeout=2)
        except subprocess.TimeoutExpired:
            self.daemon.kill()
            self.daemon.wait()
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

    def shell_identity(self, terminal):
        self.identity_requests += 1
        marker = f"__SESSION_{self.identity_requests}__"
        start = len(terminal.drain())
        terminal.send("printf '" + marker + "%s__PID__%s__END__\\n' \"$MESH_SESSION_ID\" \"$$\"\n")
        pattern = re.compile(marker.encode() + rb"([0-9A-Z]+)__PID__([0-9]+)__END__")
        match = eventually(lambda: pattern.search(terminal.drain()[start:]), "shell did not report its session identity")
        return match[1].decode(), int(match[2])

    def start_remote(self):
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
            [self.binary, "daemon", "--tailnet-port", str(port), "--ssh-port", "0"],
            env=environment, stdin=subprocess.DEVNULL, stdout=self.daemon_log, stderr=subprocess.STDOUT,
        )
        eventually(lambda: (self.remote / "daemon.sock").exists(), "remote daemon did not create its socket")
        response = round_trip(str(self.remote / "daemon.sock"), {"type": "host.info", "requestId": "terminal-fixture"})
        require(response.get("type") == "host.info.result", f"remote identity request failed: {response}")
        self.remote_id = response["host"]["id"]
        def listening():
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    return True
            except OSError:
                return False
        eventually(listening, "remote daemon did not open its WebSocket listener")
        (self.config / "hosts.json").write_text(json.dumps({
            "version": 1,
            "hosts": [{"alias": "pc", "id": self.remote_id, "meshIdentity": response["host"]["meshIdentity"],
                       "tailscaleName": "pc.fixture.test", "addresses": ["127.0.0.1"],
                       "endpoint": f"ws://127.0.0.1:{port}/mesh"}],
        }))

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

    def close(self):
        for state in (self.local, self.remote):
            for session in self.sessions(state):
                try:
                    with socket.socket(socket.AF_UNIX) as connection:
                        connection.settimeout(0.3)
                        connection.connect(str(state / "s" / session["id"] / "sock"))
                        request = json.dumps({"type": "session.signal", "signal": "kill"}).encode()
                        connection.sendall(b"\x01" + struct.pack(">I", len(request)) + request)
                except OSError:
                    pass
        for terminal in self.terminals:
            terminal.close()
        self.stop_daemon()


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
    start = len(terminal.drain())
    terminal.send('until [ "$(stty size)" = "37 109" ]; do sleep .01; done; printf "__SIZE__"; stty size\n')
    terminal.expect(b"__SIZE__37 109", since=start)
    terminal.resize(80, 24)
    start = len(terminal.drain())
    terminal.send('until [ "$(stty size)" = "24 80" ]; do sleep .01; done; printf "__SIZE__"; stty size\n')
    terminal.expect(b"__SIZE__24 80", since=start)


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
    eventually(lambda: restored_id not in fixture.local_listing() and restored_id not in fixture.daemon_listing(),
               "x did not forget the interrupted session locally and in the daemon")
    require(not (fixture.local / "s" / restored_id).exists(), "x left the forgotten session directory")
    forgotten.send(b"\x1b")
    forgotten.expect_exit()


SCENARIOS = {
    "nested-detach": nested_detach,
    "nested-resize": nested_resize,
    "window-death": window_death,
    "window-entry": window_entry,
    "window-relaunch": window_relaunch,
}


def main():
    scenario, binary = sys.argv[1:]
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("terminal fixture exceeded 25 seconds")))
    signal.alarm(25)
    with tempfile.TemporaryDirectory(prefix="mesh-window-") as directory:
        fixture = Fixture(binary, directory)
        try:
            SCENARIOS[scenario](fixture)
        except Exception:
            if fixture.daemon_log is not None:
                fixture.daemon_log.flush()
                print((fixture.root / "daemon.log").read_text(), file=sys.stderr)
            for terminal in fixture.terminals:
                print(f"terminal {terminal.pid}: {terminal.drain()[-3000:]!r}", file=sys.stderr)
            raise
        finally:
            signal.alarm(0)
            fixture.close()
    print(f"PASS: {scenario} through PTYs and persistent workers")


def run():
    try:
        main()
        return 0
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


def run_outside_containing_session(runner=run):
    # The verification command can itself run inside Mesh. Reparent the test
    # coordinator so its fresh PTYs do not inherit that unrelated worker path.
    read_end, write_end = os.pipe()
    intermediate = os.fork()
    if intermediate:
        os.close(write_end)
        os.waitpid(intermediate, 0)
        result = os.read(read_end, 1)
        os.close(read_end)
        return 0 if result == b"0" else 1
    os.close(read_end)
    intermediate_pid = os.getpid()
    if os.fork():
        os._exit(0)
    while os.getppid() == intermediate_pid:
        time.sleep(0.001)
    os.setsid()
    code = runner() or 0
    sys.stdout.flush()
    sys.stderr.flush()
    os.write(write_end, str(code).encode())
    os.close(write_end)
    os._exit(code)


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session())
