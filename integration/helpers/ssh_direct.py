#!/usr/bin/env python3
"""Exercise ephemeral Mesh SSH with real PTYs and forced transport loss."""
import json
import os
import signal
import subprocess
import sys
import tempfile

from mesh_control import round_trip
from ssh_sessions import SSHFixture
from terminal_window import Terminal, PROMPT, eventually, require


def alive(pid):
    result = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True)
    return result.returncode == 0 and not result.stdout.lstrip().startswith(b"Z")


def exercise(fixture):
    fixture.start_ssh()
    host = round_trip(str(fixture.local / "daemon.sock"), {"type": "host.info", "requestId": "direct"})["host"]
    subprocess.run([fixture.binary, "device", "approve", "--allow-root", "--", host["id"]],
                   env=fixture.environment, capture_output=True, check=True, timeout=4)
    (fixture.config / "hosts.json").write_text(json.dumps({"version": 1, "hosts": [{
        "id": host["id"], "meshIdentity": host["meshIdentity"], "addresses": ["127.0.0.2"],
        "endpoint": "ws://127.0.0.2:7777/mesh"}]}))
    command = [fixture.binary, "ssh", "--ssh-port", str(fixture.port), host["id"]]
    before = fixture.daemon_listing()
    before_paths = set(fixture.local.rglob("*"))
    for index in range(10):
        result = subprocess.run(command + ["--", "sh", "-c", "cat; printf 'tail'; printf 'error' >&2; exit 7"],
                                input=b"input", env=fixture.environment, capture_output=True, timeout=4)
        require((result.returncode, result.stdout, result.stderr) == (7, b"inputtail", b"error"),
                f"command/EOF/status {index}: {result}")
    exact = subprocess.run(command + ["--", "printf", "<%s>", "two words", "'quoted'", ""],
                           env=fixture.environment, capture_output=True, timeout=4)
    require(exact.returncode == 0 and exact.stdout == b"<two words><'quoted'><>", f"argv changed: {exact}")
    shell = subprocess.run(command, input=b"printf 'no-pty-shell\\n'\nexit 0\n", env=dict(fixture.environment, SHELL="/bin/sh"),
                           capture_output=True, timeout=4)
    # The destination account's configured shell is the fixture's interactive bash wrapper.
    require(b"no-pty-shell" in shell.stdout, f"default shell did not run: {shell}")
    require(fixture.daemon_listing() == before, "quick commands added catalog sessions")
    require(set(fixture.local.rglob("*")) == before_paths, "quick commands wrote session/worker/recovery artifacts")

    for tty in (False, True):
        marker = fixture.root / ("pty-pids" if tty else "exec-pids")
        script = f"sleep 120 & child=$!; printf '%s %s\\n' $$ $child > '{marker}'; wait"
        if tty:
            client = Terminal(command + ["--", "sh", "-c", script], fixture.environment, fixture.root)
            fixture.terminals.append(client)
            crash = client.crash
        else:
            client = subprocess.Popen(command + ["--", "sh", "-c", script], env=fixture.environment,
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            def crash():
                client.kill()
                client.communicate(timeout=4)
        try:
            eventually(marker.exists, "remote command did not record its children")
            pids = [int(value) for value in marker.read_text().split()]
            require(all(alive(pid) for pid in pids), f"remote command was not live: {pids}")
            crash()
            eventually(lambda: not any(alive(pid) for pid in pids), f"disconnect left processes alive: {pids}")
        finally:
            crash()
            if marker.exists():
                for value in marker.read_text().split():
                    try:
                        os.kill(int(value), signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    terminal = Terminal(command, fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect(PROMPT)
    terminal.expect_size(80, 24)
    terminal.resize(101, 37)
    terminal.expect_size(101, 37)
    foreground = fixture.root / "foreground-pid"
    terminal.send(f"sh -c 'echo $$ > {foreground}; exec sleep 120'\n")
    eventually(foreground.exists, "foreground job did not start")
    foreground_pid = int(foreground.read_text())
    terminal.send(b"\x03")
    eventually(lambda: not alive(foreground_pid), "Ctrl-C did not end the foreground job")
    terminal.send("printf '__INTERRUPTED__\\n'\n")
    terminal.expect("__INTERRUPTED__")
    terminal.send("exit 9\n")
    terminal.expect_exit()
    require(terminal.status == 9, f"PTY exit code={terminal.status}")

    terminal = Terminal(command, fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect(PROMPT)
    marker = fixture.root / "interactive-pids"
    terminal.send(f"sleep 120 & printf '%s %s\\n' $$ $! > {marker}; wait\n")
    eventually(marker.exists, "interactive job did not start")
    pids = [int(value) for value in marker.read_text().split()]
    try:
        terminal.crash()
        eventually(lambda: not any(alive(pid) for pid in pids), f"interactive disconnect left jobs: {pids}")
    finally:
        for pid in pids:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    terminal = Terminal(command + ["--", "sh", "-c", "stty -onlcr; printf '\\n'"], fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect_exit()
    require(terminal.status == 0 and terminal.drain() == b"\n", f"PTY bytes were rewritten: {terminal.output!r}")

    # A caller-only terminal definition must be installed in the destination account.
    source = fixture.root / "caller-terminfo"
    source.mkdir()
    compiled = subprocess.run(["tic", "-x", "-o", str(source), "-"],
                              input=b"mesh-ssh-fixture|Mesh SSH fixture,\n am, cols#80, lines#24, clear=\\E[H\\E[2J,\n",
                              capture_output=True, timeout=4)
    require(compiled.returncode == 0, f"fixture tic: {compiled.stderr!r}")
    environment = dict(fixture.environment, TERM="mesh-ssh-fixture", TERMINFO=str(source))
    terminal = Terminal(command + ["--", "sh", "-c", "clear; printf '__CLEAR_OK__\\n'"], environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect_exit()
    require(terminal.status == 0 and b"\x1b[H\x1b[2J" in terminal.drain(), f"custom terminal: {terminal.output!r}")
    require(fixture.daemon_listing() == before, "direct mode changed persistent sessions")
    # Existing SSH auth must still refuse an unknown device.
    badkey = fixture.root / "unknown-key"
    subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(badkey)], check=True)
    denied = fixture.ssh_command(["run {}"])
    denied[denied.index(str(fixture.key))] = str(badkey)
    result = subprocess.run(denied, env=fixture.environment, capture_output=True, timeout=4)
    require(result.returncode == 255 and b"Permission denied" in result.stderr, f"unapproved device: {result}")
    print("PASS: Mesh SSH shell, piped input/EOF, stderr/status, resize, Ctrl-C, custom terminfo, PTY and exec transport-drop cleanup, 10 commands with no session artifacts, auth denial")


def main():
    with tempfile.TemporaryDirectory(prefix="ms-ssh-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp")) as root:
        fixture = SSHFixture(sys.argv[1], root)
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
            fixture.close()


if __name__ == "__main__":
    main()
