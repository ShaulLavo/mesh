"""One post-failure Go stack dump from the worker behind a fixture-owned socket."""
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import struct
import sys

from failed_fixture import process_identity, same_process
from process_diagnostics import emit


STACK_BYTES = 128 << 10
STACK_LINES = 160


def stack_frame(line):
    if line.startswith("\t") and re.search(r"\.go:\d+", line):
        location = line.strip().split(" +", 1)[0]
        return location[location.index("/internal/"):] if "/internal/" in location else Path(location).name
    if re.match(r"(?:github\.com/shaul/mesh/|runtime\.|internal/|syscall\.|os\.|io\.|net\.|sync\.)", line):
        return line.rsplit("(", 1)[0]
    return None


def stack_summary(data):
    """Keep worker wait states and code locations, never runtime arguments or registers."""
    blocks, current = [], None
    for line in data.decode(errors="replace").splitlines():
        header = re.fullmatch(r"goroutine (\d+).*\[([^\]]+)\]:", line)
        if header:
            current = [f"goroutine {header[1]} [{header[2]}]:"]
            blocks.append(current)
            continue
        if current is None:
            continue
        frame = stack_frame(line)
        if frame is not None:
            current.append(frame)
    relevant = [block for block in blocks if any("/internal/worker." in frame for frame in block)]
    return [line[:256] for block in relevant for line in block][:STACK_LINES]


def child_snapshot(directory, worker_pid):
    try:
        with (directory / "meta.json").open() as source:
            metadata = json.loads(source.read(16384))
        pid = metadata["pid"]
        identity = process_identity(pid)
        if identity is not None and identity[1] == worker_pid:
            return {"pid": pid, "start_ticks": identity[0], "parent_pid": identity[1]}
    except (OSError, ValueError, KeyError, TypeError):
        pass
    return None


class WorkerFailure:
    def __init__(self, fixture, socket_path, connection, deadline):
        self.fixture, self.path, self.deadline = fixture, socket_path, deadline
        self.fd = None
        self.worker = None
        self.child = None
        self.unavailable = None
        try:
            self.arm(connection)
        except (OSError, ValueError, AttributeError, RuntimeError) as error:
            self.unavailable = type(error).__name__

    def arm(self, connection):
        if sys.platform != "linux":
            raise RuntimeError("Linux peer credentials and pidfd required")
        pid, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        if uid != os.getuid():
            raise RuntimeError("worker peer belongs to another user")
        self.fd = os.pidfd_open(pid)
        identity = process_identity(pid)
        proc = Path(f"/proc/{pid}")
        args = (proc / "cmdline").read_bytes().split(b"\0")
        directory = self.path.parent.resolve()
        expected = [b"--dir", os.fsencode(directory)]
        pairs = [args[index:index + 2] for index in range(len(args) - 1)]
        if args[1:2] != [b"session-worker"] or expected not in pairs:
            raise RuntimeError("peer is not this fixture session worker")
        if directory.parent not in (self.fixture.local / "s", self.fixture.remote / "s"):
            raise RuntimeError("socket is outside the fixture states")
        if not os.path.samefile(proc / "exe", self.fixture.binary) or identity is None or not same_process(pid, identity):
            raise RuntimeError("worker incarnation unavailable")
        self.worker = {"pid": pid, "start_ticks": identity[0], "parent_pid": identity[1]}
        self.child = child_snapshot(directory, pid)

    def __enter__(self):
        return self

    def __exit__(self, kind, error, traceback):
        try:
            if kind is not None:
                self.capture(error)
        except Exception as diagnostic_error:
            emit({"event": "fixture.worker_failure_unavailable", "error": type(diagnostic_error).__name__})
        finally:
            if self.fd is not None:
                os.close(self.fd)
        return False

    def capture(self, error):
        record = {"session": self.path.parent.name, "failure": str(error)[:1024], "failure_capped": len(str(error)) > 1024, "worker": self.worker,
                  "child_at_arm": self.child, "unavailable": self.unavailable}
        if self.worker is not None:
            record.update(self.dump())
        destination = self.fixture.root / f"failure-worker-{self.path.parent.name}.json"
        destination.write_text(json.dumps(record, indent=2) + "\n")
        emit({"event": "fixture.worker_failure", **record})

    def dump(self):
        child = self.child
        observed = process_identity(child["pid"]) if child is not None else None
        child_live = child is not None and observed == (child["start_ticks"], child["parent_pid"])
        log = self.path.parent / "worker.log"
        offset = log.stat().st_size
        # SIGQUIT is deliberately fatal. Only an already-failed fixture worker is
        # stopped, through its pinned handle, and its original exception still escapes.
        signal.pidfd_send_signal(self.fd, signal.SIGQUIT)
        remaining = max(0, min(0.2, self.deadline.end - self.deadline.clock()))
        exited = bool(select.select([self.fd], [], [], remaining)[0])
        with log.open("rb") as source:
            source.seek(offset)
            data = source.read(STACK_BYTES)
        return {"child_still_live": child_live, "signal": "SIGQUIT", "worker_exited": exited,
                "stack_read_capped": len(data) == STACK_BYTES, "stack": stack_summary(data)}
