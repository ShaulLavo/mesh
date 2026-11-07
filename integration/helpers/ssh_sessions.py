#!/usr/bin/env python3
"""Exercise stock OpenSSH against the shipped daemon and persistent PTYs."""

import json
import os
import socket
import subprocess
import sys

from terminal_window import Fixture, Terminal, PROMPT, eventually, require, run_fixture, run_outside_containing_session


class SSHFixture(Fixture):
    def start_ssh(self, keys=None):
        self.local.mkdir(exist_ok=keys is not None)
        if keys is None:
            self.key = self.root / "client-key"
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.key)], check=True)
            authorized = self.local / "authorized_keys"
            authorized.write_bytes(self.key.with_suffix(".pub").read_bytes())
            authorized.chmod(0o600)
        else:
            self.key = keys[0]
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
        self.daemon = subprocess.Popen([self.binary, "daemon", "--tailnet-port", "0", "--ssh-port", str(self.port)],
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

    def ssh_command(self, command=(), tty=False, key=None):
        return ["ssh", "-F", "/dev/null", "-p", str(self.port), "-i", str(self.key if key is None else key),
                "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=2",
                "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
                "-o", "LogLevel=ERROR", *( ["-tt"] if tty else ["-T"] ),
                "mesh@127.0.0.2", *command]

    def ssh_terminal(self, command=(), key=None):
        terminal = Terminal(self.ssh_command(command, tty=True, key=key), self.environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def exec_ssh(self, *command, key=None):
        return subprocess.run(self.ssh_command(command, key=key), env=self.environment, stdin=subprocess.DEVNULL,
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
    remote.expect_size(101, 37)

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
    eventually(lambda: fixture.daemon_listing()[session_id]["state"] == "exited",
               "daemon did not publish the resumed shell exit")

    fresh = fixture.ssh_terminal()
    fresh.expect(b"start", since=0)
    fresh.send("n")
    picker_end = fresh.expect(b"\x1b[?1049l").rindex(b"\x1b[?1049l")
    fresh.expect(PROMPT, since=picker_end)
    new_id, _ = fixture.shell_identity(fresh)
    require(new_id != session_id, "new reused the exited session")
    start = len(fresh.drain())
    fresh.send("printf '__TERM__%s__\\n' \"$TERM\"\n")
    fresh.expect(b"__TERM__xterm-256color__", since=start)
    fresh.send("exit\n")
    fresh.expect_exit()
    require(fresh.status == 0, "fresh shell did not exit successfully")


def main():
    if sys.argv[2:]:
        from ssh_revocation import run_revocation
        return run_revocation(sys.argv[1], sys.argv[2:])
    run_fixture(SSHFixture, sys.argv[1], "mesh-ssh-session-", exercise, "SSH fixture exceeded 25 seconds")
    print("PASS: SSH picker, attach, resize, detach, steal, crash survival, new shell and exit status")


def run():
    try:
        main()
        return 0
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    # A real ancestry walk finds the invoking Mesh worker even with its environment
    # cleared. Orphan the fixture so local attachments get ordinary detach keys.
    raise SystemExit(run_outside_containing_session(run))
