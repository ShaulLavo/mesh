#!/usr/bin/env python3
"""Plan 013 lifecycle observations, under the existing isolation owner."""
import json
import os
from pathlib import Path
import signal
import socket
import sqlite3
import subprocess
import sys
import time

from isolation import termination_cleanup
from process_tree import Kernel, Tree
from terminal_window import (
    ACTIVE_CLEANUP, CLEANUP_SECONDS, CleanupDeadline, Fixture, PROMPT, Terminal,
    eventually, print_diagnostics, require,
)


class LifecycleFixture(Fixture):
    def __init__(self, binary, root):
        self.tree = None
        self.control = None
        super().__init__(binary, root)
        self.kernel = Kernel()
        self.kernel.require()

    def start_local_daemon(self):
        super().start_local_daemon()
        eventually(self.daemon_ready, "daemon did not accept a fresh request")

    def daemon_ready(self):
        require(self.daemon.poll() is None, "owned daemon exited before readiness")
        try:
            self.daemon_listing()
            return True
        except (FileNotFoundError, ConnectionRefusedError):
            return False

    def capture_tree(self):
        require(self.daemon is not None and self.daemon.poll() is None, "daemon is not live")
        self.tree = Tree(self.kernel, self.daemon.pid)

    def session_processes(self, sid):
        child = self.metadata(sid)["pid"]
        stat = self.kernel.stat(child)
        require(stat is not None, f"session child {child} is missing")
        worker = stat.ppid
        require(self.tree.edge(worker, self.daemon.pid), "worker is not a live child of the owned daemon")
        require(self.tree.edge(child, worker), "session leader is not the owned worker's child")
        arguments = Path(f"/proc/{worker}/cmdline").read_bytes().split(b"\0")
        directory = str(self.local / "s" / sid).encode()
        require(arguments[1] == b"session-worker" and arguments[arguments.index(b"--dir") + 1] == directory,
                f"unexpected worker arguments: {arguments}")
        require(Path(f"/proc/{worker}/exe").resolve() == Path(self.binary), "worker uses a different binary")
        require(worker != child, "metadata pid names the worker, not the session child")
        print(f"IDENTITY: session {sid}, worker {worker}, child {child}", flush=True)
        return worker, child

    def alive(self, pid):
        return not self.kernel.exited(self.tree.proved[pid].fd)

    def same_session(self, sid, worker, child, terminal):
        require(self.alive(worker) and self.alive(child), f"session {sid} lost its worker or child")
        require(self.metadata(sid)["pid"] == child, f"session {sid} changed metadata PID")
        require(self.shell_identity(terminal) == (sid, child), "attachment reached a different session process")

    def terminal(self, *arguments):
        terminal = Terminal([self.binary, *arguments], self.environment, self.root)
        self.terminals.append(terminal)
        return terminal

    def command(self, *arguments, environment=None):
        result = subprocess.run([self.binary, *arguments], env=environment or self.environment,
                                stdin=subprocess.DEVNULL, capture_output=True, timeout=4)
        require(result.returncode == 0, f"command {arguments} failed: {result.stderr!r}")
        return result

    def database_state(self, sid):
        with sqlite3.connect(self.local / "mesh.db", timeout=1) as database:
            row = database.execute("SELECT state FROM sessions WHERE id = ?", (sid,)).fetchone()
        return None if row is None else row[0]

    def close_within(self, deadline):
        try:
            super().close_within(deadline)
        finally:
            if self.tree is not None:
                require(self.tree.close(deadline.end), "process handles did not close within cleanup allowance")


def marker(fixture, terminal, value):
    start = len(terminal.drain())
    # Split the word in the input, so an echoed command cannot satisfy the observation.
    terminal.send(f"printf '%s%s\\n' '{value[:4]}' '{value[4:]}'\n")
    terminal.expect(value + "\r\n", since=start)
    return fixture.shell_identity(terminal)


def daemon_absent(fixture):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(0.3)
        require(connection.connect_ex(str(fixture.local / "daemon.sock")) != 0,
                "a daemon accepted connections during the absent-daemon exchange")


def daemon_restart(fixture):
    fixture.start_local_daemon()
    fixture.capture_tree()
    relay = fixture.terminal("local", "--daemon", "--", str(fixture.helpers / "window_shell.sh"))
    relay.expect(PROMPT)
    relay_sid, relay_child = marker(fixture, relay, "BEFORE_RESTART")
    relay_worker, _ = fixture.session_processes(relay_sid)

    # Create through the daemon, then detach before the direct attach. Two clients
    # must not steal one another's session before the failure being measured.
    seed = fixture.terminal("local", "--daemon", "--", str(fixture.helpers / "window_shell.sh"))
    seed.expect(PROMPT)
    direct_sid, direct_child = fixture.shell_identity(seed)
    direct_worker, _ = fixture.session_processes(direct_sid)
    require(direct_sid != relay_sid, "direct and relayed clients share a session")
    seed.send(b"\x1d")
    seed.expect_exit()
    direct = fixture.terminal("attach", direct_sid)
    direct.expect(PROMPT)
    require(marker(fixture, direct, "DIRECT_BEFORE_RESTART") == (direct_sid, direct_child), "wrong direct session")
    for sid, terminal in ((relay_sid, relay), (direct_sid, direct)):
        require(fixture.attached(fixture.local, sid) and terminal.poll() is None, f"session {sid} was not attached before death")
        require(fixture.database_state(sid) in ("running", "detached"), f"daemon did not publish live session {sid}")
    require(all(fixture.alive(pid) for pid in (relay_worker, relay_child, direct_worker, direct_child)),
            "a worker or child was not live immediately before daemon death")
    if fixture.control == "close-direct":
        direct.crash()

    fixture.daemon.kill()
    fixture.daemon.wait(timeout=4)
    fixture.daemon_log.close()
    fixture.daemon = None
    fixture.daemon_log = None
    relay.expect_exit()
    daemon_absent(fixture)
    require(marker(fixture, direct, "DIRECT_WHILE_DAEMON_ABSENT") == (direct_sid, direct_child), "direct exchange changed identity")
    daemon_absent(fixture)
    # Keep the original test's check beyond delayed five-second lifecycle kills.
    time.sleep(5.2)
    require(direct.poll() is None, "direct client exited with daemon")
    require(marker(fixture, direct, "DIRECT_STILL_WITHOUT_DAEMON") == (direct_sid, direct_child), "direct exchange failed")
    daemon_absent(fixture)
    for sid, worker, child in ((relay_sid, relay_worker, relay_child), (direct_sid, direct_worker, direct_child)):
        require(fixture.alive(worker) and fixture.alive(child), f"session {sid} died with daemon")
        require(fixture.metadata(sid)["pid"] == child, f"session {sid} changed PID")

    fixture.start_local_daemon()
    for sid in (relay_sid, direct_sid):
        eventually(lambda: fixture.database_state(sid) in ("running", "detached"), f"daemon did not rediscover {sid}")
    resumed = fixture.terminal("attach", "--daemon", relay_sid)
    resumed.expect(PROMPT)
    require(marker(fixture, resumed, "AFTER_RESTART") == (relay_sid, relay_child), "relay reattach changed process")
    fixture.same_session(relay_sid, relay_worker, relay_child, resumed)
    fixture.same_session(direct_sid, direct_worker, direct_child, direct)
    resumed.send(b"\x1d")
    resumed.expect_exit()
    print(f"PASS: daemon restart preserved relayed {relay_sid} and direct {direct_sid}, exact workers and children")


def checkpoint(fixture, sid):
    response = fixture.inspect(fixture.local, sid)
    require("ACKNOWLEDGED_CHECKPOINT" in "\n".join(response["preview"]), "checkpoint output was not rendered")
    from mesh_control import round_trip
    reply = round_trip(str(fixture.local / "s" / sid / "sock"), {
        "type": "session.recovery-command", "sessionId": sid,
        "recoveryCommand": {"argv": ["/bin/true", "literal argument"], "cwd": str(fixture.root)},
    })
    require(reply.get("type") == "ok", f"checkpoint command was not acknowledged: {reply}")
    saved = (fixture.local / "s" / sid / "recovery.json").read_bytes()
    record = json.loads(saved)
    require("ACKNOWLEDGED_CHECKPOINT" in "\n".join(record["lines"]), "acknowledged checkpoint lacks rendered marker")
    require(record["restart"]["argv"] == ["/bin/true", "literal argument"], "checkpoint lacks acknowledged command")
    require(record["checkpointAt"], "checkpoint lacks its completion time")
    return saved


def worker_death(fixture):
    fixture.start_local_daemon()
    fixture.capture_tree()
    seed = fixture.terminal("local", "--daemon", "--", str(fixture.helpers / "window_shell.sh"))
    seed.expect(PROMPT)
    sid, child = fixture.shell_identity(seed)
    worker, _ = fixture.session_processes(sid)
    seed.send(b"\x1d")
    seed.expect_exit()
    terminal = fixture.terminal("attach", sid)
    terminal.expect(PROMPT)
    require(marker(fixture, terminal, "ACKNOWLEDGED_CHECKPOINT") == (sid, child), "direct attach changed the child")
    saved = checkpoint(fixture, sid)
    require(fixture.attached(fixture.local, sid), "session was not attached before worker death")
    require(fixture.alive(worker) and fixture.alive(child), "worker or child died before SIGKILL")
    if fixture.control != "skip-worker-kill":
        fixture.kernel.send(fixture.tree.proved[worker].fd, signal.SIGKILL)
    eventually(lambda: not fixture.alive(worker), "owned worker did not exit after SIGKILL")
    terminal.expect_exit()
    eventually(lambda: fixture.control != "drop-child-observer" and not fixture.alive(child),
               "controlled session child did not exit after worker death")
    listing = fixture.command("ls", "--all").stdout.decode()
    require(sid in listing and "interrupted" in listing, f"local listing did not report {sid} interrupted: {listing}")
    eventually(lambda: fixture.daemon_listing().get(sid, {}).get("state") == "interrupted",
               f"daemon did not classify the same session {sid} interrupted")
    require(fixture.metadata(sid)["id"] == sid and fixture.metadata(sid)["pid"] == child, "interruption replaced session identity")
    require((fixture.local / "s" / sid / "recovery.json").read_bytes() == saved, "worker death changed acknowledged checkpoint")
    print(f"PASS: killed owned worker {worker}, observed actual child {child} exit, same {sid} interrupted, checkpoint unchanged")


def exercise_owned(fixture_type, binary, scenario, control=None):
    # isolate.sh/verify_runner admitted this whole subtree before the shell built
    # anything. Only that owner removes it, after ECHILD; the controller also
    # proves owner and launcher exit before deleting shared binaries/records.
    root = Path(os.environ["HOME"]).parent / "f"
    require(os.environ.get("MESH_INTEGRATION_ENTRY"), "lifecycle fixture needs the isolation entry")
    root.mkdir()
    fixture = fixture_type(binary, root)
    fixture.control = control
    with termination_cleanup():
        try:
            scenario(fixture)
        except BaseException:
            print_diagnostics(fixture)
            raise
        finally:
            close_owned(fixture)


def close_owned(fixture):
    deadline = CleanupDeadline(CLEANUP_SECONDS)
    token = ACTIVE_CLEANUP.set(deadline)
    try:
        fixture.close()
    finally:
        ACTIVE_CLEANUP.reset(token)


def main():
    scenario, binary, *controls = sys.argv[1:]
    controls_allowed = {"daemon-restart": {"close-direct"}, "worker-death": {"skip-worker-kill", "drop-child-observer"}}
    control = controls[0] if controls else None
    require(not controls or len(controls) == 1 and control in controls_allowed[scenario], "unknown lifecycle control")
    scenarios = {"daemon-restart": daemon_restart, "worker-death": worker_death}
    exercise_owned(LifecycleFixture, binary, scenarios[scenario], control)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        raise SystemExit(1)
