"""Payload-free syscall observation of explicitly owned Linux fixture actors."""
import hashlib
import json
import os
import re
import signal
import subprocess
import threading
import time
from pathlib import Path

from fixtures import installed_clock
from fixtures import installed_transport_lifecycle as lifecycle

RAW = "read,write,readv,writev,recvfrom,sendto,recvmsg,sendmsg,recvmmsg,sendmmsg"
READS = ("read", "readv", "recvfrom", "recvmsg", "recvmmsg")
WRITES = ("write", "writev", "sendto", "sendmsg", "sendmmsg")
CALLS = RAW + ",socket,connect,getsockname,getpeername,getsockopt,accept,accept4,shutdown,close,dup,dup2,dup3,exit,exit_group"
MAX_BYTES = 8 << 20
MAX_ROWS = 32768
LINE = re.compile(r"^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?(\d+\.\d+)\s+(.*)$")
CALL = re.compile(r"(\w+)\((0x[0-9a-f]+|\d+)")
RETURN = re.compile(r"\)\s+=\s+(-?0x[0-9a-f]+|-?\d+)(?:\s+([A-Z][A-Z0-9_]+))?")


def identity(pid):
    root = Path("/proc") / str(pid)
    fields = (root / "stat").read_text().rsplit(") ", 1)[1].split()
    with (root / "exe").open("rb") as image:
        digest = hashlib.file_digest(image, "sha256").hexdigest()
    return {"pid": pid, "uid": root.stat().st_uid, "ppid": int(fields[1]),
            "birthTicks": int(fields[19]), "sha256": digest}


def sockets(pid):
    root = Path("/proc") / str(pid)
    found = {}
    descriptors = list((root / "fd").iterdir())
    if len(descriptors) > 256:
        raise ValueError("trace actor descriptor inventory exceeds its bound")
    for fd in descriptors:
        try:
            target = os.readlink(fd)
        except FileNotFoundError:
            continue
        if target.startswith("socket:["):
            found[target[8:-1]] = int(fd.name)
    return socket_rows(root, found)


def socket_rows(root, found):
    rows = []
    for name in ("tcp", "tcp6"):
        data = lifecycle.read_bounded(root / "net" / name, 256 << 10)
        rows.extend(socket_row(line.split(), found) for line in data.decode().splitlines()[1:])
    return [row for row in rows if row is not None]


def socket_row(fields, found):
    if len(fields) < 10 or fields[9] not in found:
        return None
    return {"fd": found[fields[9]], "inode": fields[9], "state": fields[3],
            "local": lifecycle.tcp_endpoint(fields[1]), "remote": lifecycle.tcp_endpoint(fields[2])}


def tasks(pid):
    paths = list((Path("/proc") / str(pid) / "task").iterdir())
    if len(paths) > 256:
        raise ValueError("trace actor thread inventory exceeds its bound")
    return {int(path.name): tracer(path) for path in paths}


def tracer(path):
    fields = dict(line.split(":", 1) for line in (path / "status").read_text().splitlines())
    return int(fields["TracerPid"].strip())


def parse_line(line, pid):
    match = LINE.match(line)
    if match is None:
        return {"kind": "tracer", "detail": line[:2048]}
    bracket, plain, stamp, detail = match.groups()
    row = {"kind": "syscall", "tid": int(bracket or plain or pid),
           "unixNano": int(stamp.replace(".", "").ljust(len(stamp.split(".")[0]) + 9, "0")), "detail": detail}
    call, result = CALL.search(detail), RETURN.search(detail)
    if call and call[1] not in ("exit", "exit_group"):
        row.update(call=call[1], fd=int(call[2], 0))
    if call and call[1] in ("exit", "exit_group"):
        row.update(call=call[1], exitStatus=int(call[2], 0))
    resumed = re.match(r"<\.\.\. (\w+) resumed>", detail)
    if resumed:
        row.update(call=resumed[1], resumed=True)
    if result:
        row.update(result=int(result[1], 0), errno=result[2])
    return row


class Trace:
    def __init__(self, directory, role, expected, *, executable="/usr/bin/strace", privileged=True, parent=None):
        self.role, self.expected = role, expected
        self.path = Path(directory) / (role + ".jsonl")
        self.executable, self.privileged = executable, privileged
        self.process, self.thread = None, None
        self.written, self.rows, self.dropped = 0, 0, 0
        self.errors, self.progress, self.status = [], {}, None
        self.pending = {}
        self.lock = threading.Lock()
        self.stream = None
        self.tracer_pid = None
        self.tool = None
        self.parent_deadline = parent

    def start(self):
        if identity(self.expected["pid"]) != self.expected:
            raise RuntimeError("trace actor ownership or birth changed before attach")
        self.stream = self.path.open("x", encoding="utf-8")
        self.path.chmod(0o600)
        with Path(self.executable).open("rb") as tool:
            tool_digest = hashlib.file_digest(tool, "sha256").hexdigest()
        version = subprocess.check_output([self.executable, "--version"], text=True, timeout=budget(self.parent_deadline)).splitlines()[0]
        self.tool = {"executable": str(self.executable), "sha256": tool_digest, "version": version[:256]}
        command = [self.executable, "-f", "-b", "execve", "-qq", "-ttt", "-T", "-yy", "-e", "trace=" + CALLS,
                   "-e", "raw=" + RAW, "-p", str(self.expected["pid"])]
        if self.privileged:
            command = ["sudo", "-n", "--"] + command
        self.process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True)
        self.thread = threading.Thread(target=self.pump, daemon=True)
        self.thread.start()

    def pump(self):
        try:
            self.read_pipe()
        except Exception as error:  # noqa: BLE001 - Collector bugs invalidate observation, never the product.
            self.errors.append(type(error).__name__)

    def read_pipe(self):
        while chunk := self.process.stderr.readline(4097):
            if len(chunk) > 4096 or not chunk.endswith(b"\n"):
                self.dropped += 1
                continue
            self.consume(chunk)

    def consume(self, chunk):
        if self.errors:
            self.dropped += 1
            return
        try:
            self.record(parse_line(chunk.decode(errors="replace").strip(), self.expected["pid"]))
        except Exception as error:  # noqa: BLE001 - Keep draining the pipe after an observer writer or parser failure.
            self.errors.append(type(error).__name__)

    def record(self, row):
        self.complete_call(row)
        data = json.dumps(row, separators=(",", ":")) + "\n"
        with self.lock:
            if self.rows >= MAX_ROWS or self.written + len(data.encode()) > MAX_BYTES:
                self.dropped += 1
                return
            self.stream.write(data)
            self.stream.flush()
            self.rows += 1
            self.written += len(data.encode())
            if row.get("result", 0) > 0 and "fd" in row and row.get("call") in READS + WRITES:
                key = (row["call"], row["fd"])
                self.progress[key] = self.progress.get(key, 0) + row["result"]

    def complete_call(self, row):
        if row.get("resumed"):
            pending = self.pending.pop(row["tid"], {})
            if "fd" in pending:
                row["fd"] = pending["fd"]
        if "<unfinished ...>" in row.get("detail", ""):
            self.pending[row["tid"]] = {key: row[key] for key in ("call", "fd") if key in row}
        if len(self.pending) > 256:
            self.errors.append("PendingThreadBound")
            self.pending.clear()

    def armed(self):
        if self.process.poll() is not None or self.dropped or self.errors:
            raise RuntimeError("external tracer exited or lost coverage before interruption")
        current = tasks(self.expected["pid"])
        tracing = set(current.values())
        if len(tracing) != 1 or 0 in tracing:
            return False
        self.tracer_pid = next(iter(tracing))
        if os.getpgid(self.tracer_pid) != self.process.pid:
            raise RuntimeError("actor is attached to an unowned tracer")
        return True

    def require_identity(self):
        if identity(self.expected["pid"]) != self.expected:
            raise RuntimeError("tracing changed the original actor identity or parent")

    def close(self):
        if self.status is not None:
            return
        if self.process is None:
            if self.stream:
                self.stream.close()
            return
        if self.process.poll() is None:
            self.interrupt()
        self.status = self.process.wait(timeout=budget())
        self.thread.join(timeout=budget())
        if self.thread.is_alive():
            raise RuntimeError("trace pipe collector did not stop")
        self.process.stderr.close()
        self.stream.close()

    def interrupt(self):
        if os.getpgid(self.process.pid) != self.process.pid:
            raise RuntimeError("owned tracer group changed before detach")
        if self.privileged:
            subprocess.run(["sudo", "-n", "/bin/kill", "-INT", "--", "-" + str(self.process.pid)],
                           check=True, capture_output=True, timeout=budget())
            return
        os.killpg(self.process.pid, signal.SIGINT)

    def summary(self):
        return {"identity": self.expected, "file": self.path.name, "rows": self.rows,
                "bytes": self.written, "dropped": self.dropped, "errors": self.errors,
                "tracerPid": self.tracer_pid, "exitStatus": self.status, "tool": self.tool,
                "limits": {"bytes": MAX_BYTES, "rows": MAX_ROWS, "lineBytes": 4096},
                "effects": {"ptraceStopsEverySyscall": True, "parentUnchanged": True,
                            "privilegedTracer": self.privileged, "goErrorChainAvailable": False,
                            "syscallInjection": False, "followsThreadsAndForks": True, "detachesOnChildExec": True}}


def budget(parent=None):
    maximum = installed_clock.timeout(120)
    return maximum if parent is None else installed_clock.remaining_until(parent, maximum)


def deadline(timeout=120, parent=None):
    own = installed_clock.parent_deadline(timeout)
    return own if parent is None else min(own, parent)


def wait(predicate, message, timeout=120, *, parent=None, stop=None):
    end = deadline(timeout, parent)
    while time.monotonic_ns() < end:
        if stop is not None and stop.is_set():
            raise RuntimeError("observer stopped during attachment")
        if predicate():
            return
        time.sleep(installed_clock.remaining_until(end, 0.01))
    raise RuntimeError(message)
