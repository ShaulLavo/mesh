"""Exercise the real detached pipe worker over isolated daemon sockets."""
import base64
import fcntl
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time

from durable_create import drop_reply, receipt
from durable_output import bound
from mesh_control import round_trip
from session_lifecycle import LifecycleFixture
from terminal_window import eventually, print_diagnostics

MIB = 1 << 20


class PipeFixture(LifecycleFixture):
    def start_local_daemon(self):
        super().start_local_daemon()
        eventually(self.ready, "daemon did not accept fresh connections")

    def ready(self):
        try:
            return exchange(self, {"type": "host.info", "requestId": "ready"}).get("host")
        except (ConnectionError, FileNotFoundError):
            return None

    def crash_daemon(self):
        self.daemon.kill()
        self.daemon.wait(timeout=10)
        self.daemon = None
        self.daemon_log.close()
        self.daemon_log = None

    def request_worker_kill(self, connections, socket_path, deadline):
        ref = json.loads((socket_path.parent / ".reservation").read_text())
        if ref.get("purpose") not in ("pipe-execution", "line-execution"):
            return super().request_worker_kill(connections, socket_path, deadline)
        conn = connections.enter_context(socket.socket(socket.AF_UNIX))
        conn.settimeout(deadline.remaining(1, "connect the pipe teardown"))
        try:
            conn.connect(str(socket_path))
        except (ConnectionRefusedError, FileNotFoundError):
            return None
        send(conn, bound("kill", {"sessionId": ref["id"], "instance": ref["instance"]}))
        return conn


def exchange(fixture, request, timeout=25):
    return round_trip(str(fixture.local / "daemon.sock"), request, timeout=timeout)


def send(conn, request):
    raw = json.dumps(request).encode()
    conn.sendall(b"\x01" + len(raw).to_bytes(4, "big") + raw)


def creation(fixture, key, command, stdin="closed"):
    return dict(type="execution.create", requestId=key, requestKey=key, kind="pipe",
                cwd=str(fixture.root), command=command, stdin=stdin)


def create(fixture, key, command, stdin="closed"):
    return receipt(exchange(fixture, creation(fixture, key, command, stdin)))


def wait(fixture, job):
    reply = exchange(fixture, bound("wait", job, timeoutMillis=20000))
    assert reply.get("executionWait") == "completed", reply
    done = receipt(reply)
    assert done["kind"] == "pipe", done
    return done["result"]


def logs(fixture, job, stream, cursor="", size=65536):
    """Read one page, retrying while the capture writer holds the stream lock.

    The product bounds every lock wait and answers `state.unavailable`, a
    retryable code, when a commit keeps the lock longer on a loaded host. A
    client is meant to ask again, so the only limit here is a hang cap.
    """
    request = bound("logs", job, stream=stream, cursor=cursor, maxBytes=size)
    deadline = time.monotonic() + 60
    while True:
        remaining = deadline - time.monotonic()
        response = exchange(fixture, request, timeout=max(1, min(25, remaining)))
        if response.get("errorCode") != "state.unavailable" or time.monotonic() > deadline:
            return response
        time.sleep(.05)


def page(fixture, job, stream, cursor="", size=65536):
    response = logs(fixture, job, stream, cursor, size)
    assert response.get("type") == "execution.receipt", response
    return response["executionPage"]


def decode(p):
    return base64.b64decode(p["bytesBase64"], validate=True)


def cli(fixture, *args):
    result = subprocess.run([fixture.binary, "session", *args, "--json"],
                            env=fixture.environment, stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, check=False, timeout=15)
    assert result.returncode == 0 and not result.stderr, (result.stdout, result.stderr)
    return json.loads(result.stdout)["data"]


def exact_io(fixture):
    request = creation(fixture, "split", [sys.executable, "-B", "-c", r"""
import os, sys
try:
    fd = os.open('/dev/tty', os.O_RDWR)
except OSError:
    pass
else:
    os.close(fd)
    raise SystemExit(99)
sys.stdout.buffer.write(sys.stdin.buffer.read())
sys.stderr.buffer.write('é漢字\t\n'.encode() + bytes([0, 255]))
raise SystemExit(7)
"""], "open")
    job = receipt(exchange(fixture, request))
    assert job["kind"] == "pipe"
    payload = b"A\0\xff\r\n" + "é".encode()
    write = bound("input", job, writeKey="write", inputOffset="0",
                  inputBytes=base64.b64encode(payload).decode())
    drop_reply(fixture, write)
    lookup = bound("input", job, writeKey="write")
    accepted = eventually(lambda: exchange(fixture, lookup).get("inputReceipt"), "stdin receipt missing")
    assert accepted == dict(writeKey="write", offset="0", length=len(payload), closed=False)
    fixture.crash_daemon()
    fixture.start_local_daemon()
    inspect = receipt(exchange(fixture, bound("inspect", job)))
    assert inspect["instance"] == job["instance"] and inspect["phase"] == "live", inspect
    assert exchange(fixture, write)["inputReceipt"] == accepted
    conflict = exchange(fixture, dict(write, inputBytes="eA=="))
    assert conflict["errorCode"] == "input.key_conflict", conflict
    stale = exchange(fixture, dict(write, writeKey="stale"))
    assert stale["errorCode"] == "input.offset_mismatch", stale
    eof = bound("input", job, writeKey="eof", inputOffset=str(len(payload)), inputEOF=True)
    drop_reply(fixture, eof)
    result = wait(fixture, job)
    assert result["termination"]["code"] == 7 and result["capture"] == "complete", result
    assert exchange(fixture, eof)["inputReceipt"] == dict(writeKey="eof", offset=str(len(payload)), length=0, closed=True)
    unknown = exchange(fixture, dict(eof, writeKey="unknown"))
    assert unknown["errorCode"] == "input.outcome_unknown", unknown
    stdout = page(fixture, job, "stdout", size=3)
    tail = page(fixture, job, "stdout", stdout["nextCursor"])
    stderr = page(fixture, job, "stderr")
    assert decode(stdout) + decode(tail) == payload and tail["eof"]
    assert decode(stderr) == "é漢字\t\n".encode() + b"\0\xff" and stderr["eof"]
    mismatch = exchange(fixture, bound("logs", job, stream="stderr", cursor=stdout["nextCursor"], maxBytes=64))
    assert mismatch["errorCode"] == "logs.cursor_expired", mismatch
    host = fixture.ready()["id"]
    options = ("--host", host, job["sessionId"], "--instance", job["instance"])
    assert cli(fixture, "inspect", *options)["phase"] == "terminal"
    assert cli(fixture, "input", *options, "--write-key", "eof")["closed"]
    assert decode(cli(fixture, "logs", *options, "--stream", "stderr")) == decode(stderr)
    proc = subprocess.run([fixture.binary, "session", "create", "--host", host, "--json",
                           "--kind", "pipe", "--cwd", str(fixture.root), "--request-key", "closed-default",
                           "--", "/bin/sh", "-c", "test -z \"$(cat)\" && printf closed"],
                          env=fixture.environment, capture_output=True, text=True, timeout=15, check=False)
    assert proc.returncode == 0 and not proc.stderr, (proc.stdout, proc.stderr)
    closed = json.loads(proc.stdout)["data"]
    assert wait(fixture, closed)["termination"]["code"] == 0
    assert decode(page(fixture, closed, "stdout")) == b"closed"
    removed = receipt(exchange(fixture, bound("remove", job)))
    assert removed["removal"] == "removed", removed
    expired = exchange(fixture, bound("logs", job, stream="stderr", maxBytes=64))
    assert expired["errorCode"] == "result.expired" and not expired["executionPage"]["available"], expired


def backpressure(fixture):
    release = fixture.root / "allow-read"
    job = create(fixture, "backpressure", ["/bin/sh", "-c",
                 'while [ ! -f "$1" ]; do sleep .02; done; wc -c', "fixture", str(release)], "open")
    data = b"X" * MIB
    fill = bound("input", job, writeKey="fill", inputOffset="0", inputBytes=base64.b64encode(data).decode())
    assert exchange(fixture, fill)["inputReceipt"]["length"] == MIB
    more = bound("input", job, writeKey="more", inputOffset=str(MIB), inputEOF=True, inputBytes="IQ==")
    busy = exchange(fixture, more)
    assert busy["errorCode"] == "input.backpressure", busy
    release.touch()

    def finish_input():
        response = exchange(fixture, more)
        if response.get("errorCode") == "input.backpressure":
            return None
        return response["inputReceipt"]

    assert eventually(finish_input, "accepted bytes did not drain")["closed"]
    assert wait(fixture, job)["termination"]["code"] == 0
    assert decode(page(fixture, job, "stdout")).strip() == str(MIB+1).encode()


def bulk(fixture, paused):
    gate = fixture.root / ("gate-paused" if paused else "gate-tail")
    chunks = 10 if paused else 25
    producer = """
import os, pathlib, sys, time
gate, count, paused = pathlib.Path(sys.argv[1]), int(sys.argv[2]), sys.argv[3] == 'True'
for i in range(count):
    os.write(1, bytes([i + 1]) * (1 << 20))
    os.write(2, bytes([i + 101]) * (1 << 20))
    if not paused:
        while not (gate / str(i)).exists():
            time.sleep(.005)
if paused:
    os.write(1, b'tail!')
    os.write(2, b'error tail!')
"""
    gate.mkdir()
    if paused:
        # Obtain valid zero cursors while the producer is stopped.
        producer = "import sys, time; sys.stdin.buffer.read()\n" + producer
    job = create(fixture, gate.name, [sys.executable, "-B", "-c", producer, str(gate), str(chunks), str(paused)],
                 "open" if paused else "closed")
    starts = {stream: page(fixture, job, stream)["earliestCursor"] for stream in ("stdout", "stderr")}
    if paused:
        assert exchange(fixture, bound("input", job, writeKey="go", inputOffset="0", inputEOF=True))["inputReceipt"]["closed"]
        with socket.socket(socket.AF_UNIX) as slow:
            slow.connect(str(fixture.local / "daemon.sock"))
            send(slow, bound("logs", job, stream="stdout", maxBytes=MIB))
            result = wait(fixture, job)
            assert result["capture"] == "complete", result
        verify_suffix(fixture, job, starts, chunks)
    else:
        tail_bulk(fixture, job, starts, chunks)
        assert wait(fixture, job)["capture"] == "complete"


def tail_bulk(fixture, job, cursors, chunks):
    for index in range(chunks):
        for stream, base in (("stdout", 1), ("stderr", 101)):
            expected, got = bytes([index+base]) * MIB, bytearray()
            deadline = time.monotonic() + 15
            while len(got) < MIB and time.monotonic() < deadline:
                part = page(fixture, job, stream, cursors[stream], MIB)
                got.extend(decode(part))
                cursors[stream] = part["nextCursor"]
                if len(got) < MIB:
                    time.sleep(.005)
            assert got == expected, (stream, index, len(got))
        (fixture.root / "gate-tail" / str(index)).touch()


def verify_suffix(fixture, job, starts, chunks):
    for stream, base, tail in (("stdout", 1, b"tail!"), ("stderr", 101, b"error tail!")):
        missed = logs(fixture, job, stream, starts[stream], MIB)
        assert missed["errorCode"] == "logs.cursor_expired" and missed["executionPage"]["gap"], missed
        cursor = missed["executionPage"]["earliestCursor"]
        got = bytearray()
        for _ in range(8):
            current = page(fixture, job, stream, cursor, MIB)
            got.extend(decode(current))
            cursor = current["nextCursor"]
        expected = (b"".join(bytes([i+base]) * MIB for i in range(chunks)) + tail)[-8*MIB:]
        assert got == expected and current["eof"] and current["observedEnd"] == str(chunks*MIB+len(tail))


def group_signal(fixture):
    ready = fixture.root / "signal-ready"
    program = r"""
import os, pathlib, signal, sys
role = 'leader' if os.fork() else 'child'
signal.signal(signal.SIGUSR1, lambda *_: os.write(1, (role + '\n').encode()))
(pathlib.Path(sys.argv[1]) / role).touch()
while True:
    signal.pause()
"""
    ready.mkdir()
    job = create(fixture, "group", [sys.executable, "-B", "-c", program, str(ready)])
    eventually(lambda: len(list(ready.iterdir())) == 2, "process group did not become ready")
    response = receipt(exchange(fixture, bound("signal", job, signal="usr1")))
    assert response["instance"] == job["instance"]
    lines = eventually(lambda: signal_output(fixture, job), "signal did not reach both processes")
    assert sorted(lines) == [b"child", b"leader"], lines
    stopped = exchange(fixture, bound("kill", job))
    assert stopped["executionDelivery"] == "delivered", stopped
    termination = wait(fixture, job)["termination"]
    assert termination["kind"] == "killed", termination


def signal_output(fixture, job):
    lines = decode(page(fixture, job, "stdout")).splitlines()
    return lines if len(lines) == 2 else None


def descendant_drain(fixture):
    release = fixture.root / "drain-release"
    program = """
import os, pathlib, sys, time
os.write(1, b'leader')
if os.fork():
    os._exit(0)
os.close(2)
release = pathlib.Path(sys.argv[1])
until = time.monotonic() + 15
while not release.exists() and time.monotonic() < until:
    time.sleep(.01)
release.with_suffix('.done').touch()
"""
    job = create(fixture, "drain", [sys.executable, "-B", "-c", program, str(release)])
    try:
        result = wait(fixture, job)
        by_stream = {stream["stream"]: stream for stream in result["streams"]}
        assert result["termination"]["code"] == 0 and result["capture"] == "incomplete", result
        assert by_stream["stdout"]["closeReason"] == "drain_timeout" and not by_stream["stdout"]["eof"], result
        assert by_stream["stderr"]["eof"] and decode(page(fixture, job, "stdout")) == b"leader", result
    finally:
        release.touch()
        eventually(lambda: release.with_suffix(".done").exists(), "descriptor holder did not exit")


def worker_crash(fixture):
    fixture.capture_tree()
    count = fixture.root / "worker-crash.count"
    request = creation(fixture, "worker-crash", ["/bin/sh", "-c",
                       'printf x >> "$1"; printf prefix; exec /bin/sleep 60', "fixture", str(count)], "open")
    job = receipt(exchange(fixture, request))
    worker, child = fixture.session_processes(job["sessionId"])
    try:
        eventually(lambda: decode(page(fixture, job, "stdout")) == b"prefix", "capture did not publish prefix")
        input_request = bound("input", job, writeKey="accepted", inputOffset="0", inputBytes="eA==")
        assert exchange(fixture, input_request)["inputReceipt"]["length"] == 1
        assert fixture.kernel.send(fixture.tree.proved[worker].fd, signal.SIGKILL)
        eventually(lambda: not fixture.alive(worker), "pipe worker survived SIGKILL")
        fixture.crash_daemon()
        fixture.start_local_daemon()
        response = exchange(fixture, request)
        assert response["errorCode"] == "request.outcome_unknown" and count.read_bytes() == b"x", response
        assert response["executionReceipt"]["instance"] == job["instance"], response
        assert exchange(fixture, input_request)["inputReceipt"]["length"] == 1
        missing = exchange(fixture, bound("input", job, writeKey="missing"))
        assert missing["errorCode"] == "input.outcome_unknown", missing
        retained = page(fixture, job, "stdout")
        assert decode(retained) == b"prefix" and not retained["eof"], retained
    finally:
        for pid in (worker, child):
            if fixture.alive(pid):
                fixture.kernel.send(fixture.tree.proved[pid].fd, signal.SIGKILL)
                eventually(lambda: not fixture.alive(pid), "owned crash fixture PID survived")
        fixture.tree.close()
        fixture.tree = None


def launch_crash(fixture, stage, did_start):
    count = fixture.root / (stage + ".count")
    request = creation(fixture, stage, ["/bin/sh", "-c", 'printf x >> "$1"; cat', "fixture", str(count)], "open")
    hold = fixture.local / (".pipe-hold-" + stage)
    hold.write_text(stage)
    dropped = Path(str(hold) + ".reached")
    drop_reply(fixture, request)
    sid = eventually(lambda: dropped.read_text() if dropped.exists() else None, "launch boundary not reached")
    fixture.crash_daemon()
    hold.unlink()
    fixture.start_local_daemon()
    again = exchange(fixture, request)
    replay = again["executionReceipt"]
    assert replay["sessionId"] == sid, replay
    if did_start:
        assert again["type"] == "execution.receipt" and replay["publication"] == "published", again
        assert exchange(fixture, bound("input", replay, writeKey="eof", inputOffset="0", inputEOF=True))["inputReceipt"]["closed"]
        assert wait(fixture, replay)["termination"]["code"] == 0
        assert count.read_bytes() == b"x"
    else:
        assert again["errorCode"] == "request.outcome_unknown" and not count.exists(), again
        assert not (fixture.local / "s" / sid / "sock").exists()


def crash_after_spawn(fixture):
    key = "after_spawn"
    count = fixture.root / "after_spawn.count"
    request = creation(fixture, key, ["/bin/sh", "-c",
                       'echo "$$" >> "$1"; exec /bin/cat', "fixture", str(count)], "open")
    before = fixture.local / ".pipe-hold-before_worker_launch"
    reached = before.with_name(before.name + ".reached")
    reached.unlink(missing_ok=True)
    before.write_text(key)
    drop_reply(fixture, request)
    sid = eventually(lambda: reached.read_text() if reached.exists() else None, "launch boundary not reached")
    directory = fixture.local / "s" / sid
    hold = directory / ".execution-hold-after_spawn"
    hold.touch()
    before.unlink()
    try:
        marker = hold.with_name(hold.name + ".reached")
        eventually(marker.exists, "worker did not reach its after_spawn boundary")
        pid = int(eventually(lambda: count.read_text().strip() if count.exists() else None, "child did not run"))
        assert (directory / ".launching").exists() and not (directory / "sock").exists()
        reservation = json.loads((directory / ".reservation").read_text())
        fixture.crash_daemon()
        fixture.start_local_daemon()
        assert (directory / ".launching").exists()
        uncertain = exchange(fixture, request)
        assert uncertain["errorCode"] == "request.outcome_unknown", uncertain
        assert uncertain["executionReceipt"]["instance"] == reservation["instance"], uncertain
    finally:
        hold.unlink(missing_ok=True)
    job = eventually(lambda: adopted_pipe(fixture, key), "daemon did not rediscover the released worker")
    assert job["sessionId"] == sid and job["instance"] == reservation["instance"], job
    assert fixture.metadata(sid)["pid"] == pid
    assert exchange(fixture, bound("input", job, writeKey="eof", inputOffset="0", inputEOF=True))["inputReceipt"]["closed"]
    assert wait(fixture, job)["termination"]["code"] == 0
    assert count.read_text().splitlines() == [str(pid)]


def adopted_pipe(fixture, key):
    reply = exchange(fixture, dict(type="execution.request", requestId="adopt", requestKey=key))
    if reply.get("errorCode") == "request.outcome_unknown":
        return None
    result = receipt(reply)
    return result if result["publication"] == "published" else None


def held_stream_backpressure(fixture):
    ready = fixture.root / "held-stream-writer"
    gate = fixture.root / "held-stream-gate"
    program = """
import os, pathlib, sys, time
child = os.fork()
if child:
    pathlib.Path(sys.argv[1]).write_text(str(child))
    os.read(0, 1)
    os._exit(0)
while not pathlib.Path(sys.argv[2]).exists():
    time.sleep(.005)
while True:
    os.write(1, b'x' * 32768)
"""
    fixture.capture_tree()
    job = create(fixture, "held-stream", [sys.executable, "-B", "-c", program, str(ready), str(gate)], "open")
    worker, leader = fixture.session_processes(job["sessionId"])
    descendant = int(eventually(lambda: ready.read_text() if ready.exists() else None, "pipe producer did not fork"))
    assert fixture.tree.edge(descendant, leader)
    attempt = fixture.local / "execution" / "attempts" / job["instance"]
    try:
        with (attempt / "stdout" / "stream.lock").open("rb") as stream:
            fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
            gate.touch()
            eventually(lambda: blocked_pipe_writer(descendant), "producer never met pipe backpressure")
            eof = bound("input", job, writeKey="eof", inputOffset="0", inputEOF=True)
            assert exchange(fixture, eof)["inputReceipt"]["closed"]
            eventually(lambda: fixture.kernel.stat(leader) is None, "pipe leader was not reaped")
            result = wait(fixture, job)
            assert result["termination"]["code"] == 0 and result["capture"] == "not_recorded", result
            eventually(lambda: not fixture.alive(worker), "capture lock kept the worker alive")
            with (attempt / "lifetime.lock").open("rb") as lifetime:
                fcntl.flock(lifetime, fcntl.LOCK_EX | fcntl.LOCK_NB)
        assert receipt(exchange(fixture, bound("remove", job)))["removal"] == "removed"
    finally:
        for pid in (worker, leader, descendant):
            if fixture.alive(pid):
                fixture.kernel.send(fixture.tree.proved[pid].fd, signal.SIGKILL)
                eventually(lambda: not fixture.alive(pid), "held capture fixture did not stop")
        fixture.tree.close()
        fixture.tree = None


def blocked_pipe_writer(pid):
    try:
        return "pipe_write" in Path(f"/proc/{pid}/wchan").read_text()
    except FileNotFoundError:
        return False


def main():
    root = Path(os.environ["HOME"]).parent / "pipe-jobs"
    root.mkdir()
    fixture = PipeFixture(sys.argv[1], root)
    try:
        fixture.start_local_daemon()
        exact_io(fixture)
        backpressure(fixture)
        group_signal(fixture)
        descendant_drain(fixture)
        worker_crash(fixture)
        launch_crash(fixture, "before_worker_launch", False)
        launch_crash(fixture, "before_catalog_publication", True)
        crash_after_spawn(fixture)
        held_stream_backpressure(fixture)
        bulk(fixture, False)
        bulk(fixture, True)
    except BaseException:
        print_diagnostics(fixture)
        for directory in (fixture.local / "s").glob("*"):
            if not directory.is_dir():
                continue
            print(f"{directory.name}: {sorted(entry.name for entry in directory.iterdir())}", file=sys.stderr)
            for name in ("meta.json", "worker.log"):
                file = directory / name
                if file.exists():
                    print(file.read_text()[-3000:], file=sys.stderr)
        raise
    finally:
        fixture.close()
    print("PASS: split pipe streams, CLI, keyed stdin and EOF, backpressure, daemon crashes, and exact retained output")


if __name__ == "__main__":
    main()
