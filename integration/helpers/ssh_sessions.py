#!/usr/bin/env python3
"""Exercise stock OpenSSH against the shipped daemon and persistent PTYs."""

import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
from terminal_window import Fixture, Terminal, PROMPT, eventually, require


class SSHFixture(Fixture):
    def start_ssh(self):
        self.local.mkdir()
        self.key = self.root / "client-key"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.key)], check=True)
        authorized = self.local / "authorized_keys"
        authorized.write_bytes(self.key.with_suffix(".pub").read_bytes())
        authorized.chmod(0o600)
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        (bin_dir / "tailscale").symlink_to(self.helpers / "fake_tailscale")
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({"BackendState": "Running", "Self": {
            "HostName": "mesh-ssh", "DNSName": "mesh-ssh.example.ts.net.",
            "TailscaleIPs": ["127.0.0.2"], "Online": True,
        }, "Peer": {}}))
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.2", 0))
            self.port = reservation.getsockname()[1]
        self.environment.update({"MESH_FAKE_TAILSCALE_STATUS": str(status),
                                 "PATH": str(bin_dir) + os.pathsep + self.environment["PATH"]})
        self.daemon_log = open(self.root / "daemon.log", "ab")
        self.daemon = subprocess.Popen([self.binary, "daemon", "--ssh-port", str(self.port)],
                                       env=self.environment, stdin=subprocess.DEVNULL,
                                       stdout=self.daemon_log, stderr=subprocess.STDOUT)
        eventually(self.listening, "SSH daemon did not listen")

    def listening(self):
        require(self.daemon.poll() is None, "SSH daemon exited")
        try:
            with socket.create_connection(("127.0.0.2", self.port), timeout=0.1):
                return True
        except OSError:
            return False

    def ssh_command(self, command=(), tty=False):
        return ["ssh", "-F", "/dev/null", "-p", str(self.port), "-i", str(self.key),
                "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=2",
                "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
                "-o", "LogLevel=ERROR", *( ["-tt"] if tty else ["-T"] ),
                "mesh@127.0.0.2", *command]

    def ssh_terminal(self, command=()):
        terminal = Terminal(self.ssh_command(command, tty=True), self.environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def exec_ssh(self, *command):
        return subprocess.run(self.ssh_command(command), env=self.environment, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=4)


def exercise(fixture):
    fixture.start_ssh()
    local = Terminal([fixture.binary, "local", "--daemon", "--", str(fixture.helpers / "window_shell.sh")],
                     fixture.environment, fixture.root)
    fixture.terminals.append(local)
    local.expect(PROMPT)
    session_id, shell_pid = fixture.shell_identity(local)

    remote = fixture.ssh_terminal([session_id])
    remote.expect(PROMPT)
    require(fixture.shell_identity(remote) == (session_id, shell_pid), "SSH attached a different process")
    local.expect_exit()
    remote.resize(101, 37)
    start = len(remote.drain())
    remote.send("printf '__SIZE__'; stty size\n")
    remote.expect(b"__SIZE__37 101", since=start)

    for _ in range(3):
        start = len(remote.drain())
        remote.send(b"\x1d")
        remote.expect(b"Choose a session", since=start)
        require(not fixture.attached(fixture.local, session_id), "detach left the session attached")
        remote.send(b"\r")
        eventually(lambda: fixture.attached(fixture.local, session_id), "picker did not attach the session")
        require(fixture.shell_identity(remote) == (session_id, shell_pid), "picker resumed a different process")

    remote.crash()
    eventually(lambda: not fixture.attached(fixture.local, session_id), "dead SSH client stayed attached")
    os.kill(shell_pid, 0)
    eventually(lambda: fixture.daemon_listing()[session_id]["state"] == "detached",
               "daemon did not reconcile the disconnected session")
    result = fixture.exec_ssh("ls")
    require(result.returncode == 0 and session_id.encode() in result.stdout and b"detached" in result.stdout,
            f"no-PTY ls failed: {result.returncode}, {result.stdout!r}, {result.stderr!r}")
    require(b"\x1b" not in result.stdout, "ls wrote terminal control sequences")
    for command in ((session_id,), ("sh", "-c", "touch should-not-exist"), ("ls", "extra")):
        result = fixture.exec_ssh(*command)
        require(result.returncode != 0, f"unsupported command succeeded: {command}")
    require(not (fixture.root / "should-not-exist").exists(), "SSH executed an arbitrary command")

    resumed = fixture.ssh_terminal()
    resumed.expect(b"Choose a session")
    resumed.send(b"\r")
    eventually(lambda: fixture.attached(fixture.local, session_id), "picker did not resume the session")
    require(fixture.shell_identity(resumed) == (session_id, shell_pid), "disconnect killed the original session")
    resumed.send("exit 7\n")
    resumed.expect_exit()
    require(resumed.status == 7, f"SSH lost process exit status: {resumed.status}")

    fresh = fixture.ssh_terminal()
    fresh.expect(b"start", since=0)
    fresh.send("n")
    fresh.expect(PROMPT)
    new_id, _ = fixture.shell_identity(fresh)
    require(new_id != session_id, "new reused the exited session")
    start = len(fresh.drain())
    fresh.send("printf '__TERM__%s__\\n' \"$TERM\"\n")
    fresh.expect(b"__TERM__xterm-256color__", since=start)
    fresh.send("exit\n")
    fresh.expect_exit()
    require(fresh.status == 0, "fresh shell did not exit successfully")


def main():
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("SSH fixture exceeded 25 seconds")))
    signal.alarm(25)
    with tempfile.TemporaryDirectory(prefix="mesh-ssh-session-") as directory:
        fixture = SSHFixture(sys.argv[1], directory)
        try:
            exercise(fixture)
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
    print("PASS: SSH picker, attach, resize, detach, steal, crash survival, new shell and exit status")


def run():
    try:
        main()
        return 0
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


def run_outside_containing_session():
    # A real ancestry walk finds the invoking Mesh worker even with its environment
    # cleared. Orphan the fixture so local attachments get ordinary detach keys.
    reader, writer = os.pipe()
    intermediate = os.fork()
    if intermediate:
        os.close(writer)
        os.waitpid(intermediate, 0)
        result = os.read(reader, 1)
        os.close(reader)
        return 0 if result == b"0" else 1
    os.close(reader)
    parent = os.getpid()
    if os.fork():
        os._exit(0)
    while os.getppid() == parent:
        time.sleep(0.001)
    os.setsid()
    code = run()
    sys.stdout.flush()
    sys.stderr.flush()
    os.write(writer, str(code).encode())
    os.close(writer)
    os._exit(code)


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session())
