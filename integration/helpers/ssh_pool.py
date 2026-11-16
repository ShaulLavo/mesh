#!/usr/bin/env python3
"""Exercise command-scoped ownership on one reused authenticated SSH transport."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts/bench"))
import ssh_command as bench  # noqa: E402 - shared isolated fixture and teardown
from ssh_agents import transport_inodes  # noqa: E402
from ssh_direct import alive, recorded_pids  # noqa: E402
from terminal_window import Terminal, eventually, require  # noqa: E402

TIMEOUT = 120


def run(command, environment, expected=b"", stderr=b"", code=0, payload=b""):
    result = bench.checked_run(command, environment, payload=payload)
    require((result.returncode, result.stdout, result.stderr) == (code, expected, stderr),
            f"command outcome: {result.returncode}, {result.stdout[:200]!r}, {result.stderr[:200]!r}")
    return result


def start(stack, command, environment):
    process = subprocess.Popen(command, env=environment, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    stack.callback(bench.stop_process, process)
    return process


def exercise(fixture):
    command = bench.prepare_mesh(fixture)[:-len(bench.COMMAND)-1]
    environment = fixture.environment
    with ExitStack() as stack:
        run(command + ["-T", "--", "/bin/true"], environment)
        first = transport_inodes(fixture)
        require(len(first) == 1, f"expected one authenticated transport: {first}")
        launch_options(command, fixture)
        concurrency_and_cancellation(command, fixture, stack)
        output_and_slow_sibling(command, fixture, stack)
        require(transport_inodes(fixture) == first, "commands did not retain their shared transport")
        malformed_control(fixture)
        unavailable_identity(command, fixture)
        idle_cleanup(fixture)
        transport_loss(command, fixture, stack)
    require(not fixture.daemon_listing() and not list((fixture.local / "s").glob("*")),
            "ordinary commands created persistent sessions")


def launch_options(command, fixture):
    environment = fixture.environment
    directory = fixture.root / "working dir"
    directory.mkdir()
    run(command + ["--cwd", str(directory), "--env", "CI=first", "--env", "CI=last=ok", "-T", "--",
                   "/bin/sh", "-c", "printf '%s|%s' \"$PWD\" \"$CI\""],
        environment, (str(directory) + "|last=ok").encode())
    home = Path(environment["HOME"])
    (home / "relative").mkdir()
    run(command + ["--cwd", "relative", "--", "/bin/pwd"], environment, str(home / "relative").encode() + b"\n")
    failed = bench.checked_run(command + ["--cwd", str(directory / "absent"), "--", "/bin/true"], environment)
    require(failed.returncode != 0 and b"working directory" in failed.stderr, f"invalid cwd: {failed}")
    run(command + ["--", "/bin/sh", "-c", "cat; sleep 0.1; printf tail; printf err >&2; exit 7"],
        environment, b"inputtail", b"err", 7, b"input")
    run(command + ["--", "/bin/printf", "<%s>", "two words", "'quoted'", ""],
        environment, b"<two words><'quoted'><>")
    run(command + ["--", "/bin/sh", "-c", "kill -TERM $$"], environment, code=143)
    terminal = Terminal(command + ["--no-pty", "--", "/bin/sh", "-c", "test ! -t 0 && test ! -t 1 && printf pipe"],
                        environment, fixture.root, timeout=TIMEOUT)
    fixture.terminals.append(terminal)
    terminal.expect_exit()
    require(terminal.status == 0 and terminal.drain() == b"pipe", "--no-pty allocated a remote terminal")


def concurrency_and_cancellation(command, fixture, stack):
    marker = fixture.root / "cancel-pids"
    script = f"sleep 120 & printf '%s %s\\n' $$ $! > '{marker}'; wait"
    canceled = start(stack, command + ["--", "/bin/sh", "-c", script], fixture.environment)
    pids = eventually(lambda: recorded_pids(marker), "remote process group did not start", timeout=TIMEOUT)
    sibling = start(stack, command + ["--env", "SIBLING=kept", "--", "/bin/sh", "-c", "sleep 0.2; printf %s \"$SIBLING\""], fixture.environment)
    canceled.kill()
    canceled.communicate(timeout=TIMEOUT)
    output, error = sibling.communicate(timeout=TIMEOUT)
    require((sibling.returncode, output, error) == (0, b"kept", b""), "cancellation killed a sibling")
    eventually(lambda: not any(alive(pid) for pid in pids), f"cancellation retained processes: {pids}", timeout=TIMEOUT)
    directories = [fixture.root / f"parallel-{index}" for index in range(4)]
    for directory in directories:
        directory.mkdir()
    with ThreadPoolExecutor(max_workers=4) as executor:
        results = list(executor.map(lambda index: bench.checked_run(command + ["--cwd", str(directories[index]), "--env", f"INDEX={index}", "--", "/bin/sh", "-c", "sleep 0.1; printf '%s|%s' \"$INDEX\" \"$PWD\""], fixture.environment), range(4)))
    require(all(result.returncode == 0 and result.stdout == f"{index}|{directories[index]}".encode() and result.stderr == b""
                for index, result in enumerate(results)), "concurrent launch directory/environment leaked")


def output_and_slow_sibling(command, fixture, stack):
    size = 4 << 20
    script = f"import os,threading; t=threading.Thread(target=lambda:os.write(2,b'e'*{size})); t.start(); os.write(1,b'o'*{size}); t.join()"
    run(command + ["--", "python3", "-c", script], fixture.environment, b"o" * size, b"e" * size)
    slow = start(stack, command + ["--", "python3", "-c", "import os; os.write(1,b's'*(4<<20))"], fixture.environment)
    # Keep the consumer idle while a sibling completes. This is an outcome check.
    run(command + ["--", "/bin/echo", "sibling"], fixture.environment, b"sibling\n")
    output, error = slow.communicate(timeout=TIMEOUT)
    require((slow.returncode, output, error) == (0, b"s" * size, b""), "slow consumer lost bytes or final status")


def malformed_control(fixture):
    import socket
    with socket.socket(socket.AF_UNIX) as connection:
        connection.connect(str(fixture.local / "ssh-command.sock"))
        connection.sendall(b"x")
        result = json.loads(connection.recv(4096))
        require(result["code"] != 0, "missing descriptors were accepted")


def idle_cleanup(fixture):
    children = Path(f"/proc/{os.getpid()}/task/{os.getpid()}/children").read_text().split()
    helpers = [int(pid) for pid in children if b"ssh-helper" in Path(f"/proc/{pid}/cmdline").read_bytes()]
    require(len(helpers) == 1, f"expected one owned helper before idle cleanup: {helpers}")
    socket = fixture.local / "ssh-command.sock"
    require(socket.stat().st_mode & 0o777 == 0o600, "helper socket is not private")
    eventually(lambda: not socket.exists() and not alive(helpers[0]),
               "idle helper retained its listener/process", timeout=TIMEOUT)
    require(not transport_inodes(fixture), "idle cleanup retained an authenticated transport")


def unavailable_identity(command, fixture):
    path = fixture.config / "hosts.json"
    original = path.read_bytes()
    document = json.loads(original)
    document["hosts"][0]["meshIdentity"] = "A" * 43
    try:
        path.write_text(json.dumps(document))
        denied = bench.checked_run(command + ["--", "/bin/true"], fixture.environment)
        require(denied.returncode != 0, "changed pinned destination reused an authenticated transport")
    finally:
        path.write_bytes(original)
    run(command + ["--", "/bin/true"], fixture.environment)


def transport_loss(command, fixture, stack):
    marker = fixture.root / "accepted-once"
    process = start(stack, command + ["--", "/bin/sh", "-c", f"printf accepted >> '{marker}'; exec sleep 120"], fixture.environment)
    eventually(marker.exists, "remote command was not accepted", timeout=TIMEOUT)
    args = fixture.daemon.args
    fixture.stop_daemon()
    process.communicate(timeout=TIMEOUT)
    require(process.returncode != 0 and marker.read_bytes() == b"accepted", "lost transport replayed or hid an uncertain command")
    fixture.daemon_log = (fixture.root / "daemon.log").open("ab")
    fixture.daemon = subprocess.Popen(args, env=fixture.environment, stdin=subprocess.DEVNULL,
                                      stdout=fixture.daemon_log, stderr=subprocess.STDOUT)
    eventually(fixture.listening, "replacement fixture daemon did not listen", timeout=TIMEOUT)
    run(command + ["--", "/bin/true"], fixture.environment)
    require(marker.read_bytes() == b"accepted", "future reconnect replayed a previous command")
    time.sleep(0.2)
    host = bench.round_trip(str(fixture.local / "daemon.sock"), {"type": "host.info", "requestId": "revoke"})["host"]
    revoked = bench.checked_run([fixture.binary, "device", "revoke", "--", host["id"]], fixture.environment)
    require(revoked.returncode == 0, f"fixture identity revocation failed: {revoked}")
    # Existing transport must fail after the destination revokes its caller.
    eventually(lambda: not transport_inodes(fixture), "revocation did not close the reused transport", timeout=TIMEOUT)
    denied = bench.checked_run(command + ["--", "/bin/true"], fixture.environment)
    require(denied.returncode != 0, "revoked identity reconnected")


def main():
    owner = bench.Descendants()
    root = None
    report = {}
    try:
        with bench.allocation_signals():
            root = Path(tempfile.mkdtemp(prefix="ms-pool-", dir=os.environ.get("MESH_SHORT_TMP") or tempfile.gettempdir()))
        fixture = bench.SSHFixture(sys.argv[1], root)
        try:
            exercise(fixture)
        finally:
            fixture.stop_daemon()
    finally:
        with bench.protected_cleanup():
            bench.finish(owner, root, report)
    print("PASS: pooled SSH argv, cwd/env, no-PTY, streams/EOF/status, concurrent channels, cancellation/process cleanup, identity, revocation, transport failure and no replay")


if __name__ == "__main__":
    with bench.termination_cleanup():
        main()
