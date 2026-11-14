"""Passive, bounded evidence for the installed cohort's initial catalog lookup."""
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import select
import socket
import sqlite3
import stat
import struct
import sys
import threading
import time

from fixtures import release_workers

MAX_RECORD_BYTES = 4 << 20
MAX_EVENTS = 1024
MARKERS = (".launching", ".reservation", "meta.json.tmp", "sock", ".forgotten", "recovery.lock")
META_FIELDS = ("id", "pid", "state", "bootId", "createdAt", "lastAttachedAt", "detachedAt", "exitedAt", "exitCode")


def unavailable(error):
    return {"unavailable": type(error).__name__, "errno": getattr(error, "errno", None)}


def file_info(path):
    try:
        value = path.lstat()
        return {"exists": True, "mode": stat.S_IFMT(value.st_mode), "bytes": value.st_size, "mtimeNs": value.st_mtime_ns}
    except FileNotFoundError:
        return {"exists": False}
    except OSError as error:
        return unavailable(error)


def metadata(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > 16384:
            raise ValueError("metadata type or size exceeds evidence bound")
        raw = stream.read(16385)
    if len(raw) > 16384:
        raise ValueError("metadata grew beyond evidence bound")
    value = json.loads(raw)
    command, cwd = value.get("command"), value.get("cwd")
    return {"bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest(), "mtimeNs": info.st_mtime_ns,
            "fields": {key: value[key] for key in META_FIELDS if key in value},
            "commandShape": {"array": isinstance(command, list), "nonempty": bool(command),
                             "nonemptyStrings": isinstance(command, list) and all(isinstance(item, str) and item for item in command)},
            "cwdShape": {"string": isinstance(cwd, str), "absolute": isinstance(cwd, str) and os.path.isabs(cwd)}}


def session_snapshot(state, session):
    directory = state / "s" / session
    info = file_info(directory)
    if info.get("mode") != stat.S_IFDIR:
        return {"directory": info, "metadata": {"unavailable": "session directory absent or not a directory"}}
    result = {"directory": info, "markers": {name: file_info(directory / name) for name in MARKERS}}
    try:
        result["metadata"] = metadata(directory / "meta.json")
    except (OSError, ValueError, TypeError, AttributeError) as error:
        result["metadata"] = unavailable(error)
    return result


def database_snapshot(state, sessions, host):
    path = state / "mesh.db"
    if file_info(path).get("mode") != stat.S_IFREG:
        raise sqlite3.OperationalError("owned database absent or not a regular file")
    uri = path.absolute().as_uri() + "?mode=ro"
    with closing(sqlite3.connect(uri, uri=True, timeout=0.05)) as database:
        database.execute("PRAGMA query_only = ON")
        database.set_progress_handler(lambda: 1, 100000)
        hosts = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 33").fetchall()
        placeholders = ",".join("?" for _ in sessions)
        rows = database.execute("SELECT host_id,id,state,created_at FROM sessions WHERE id IN (" + placeholders + ") ORDER BY host_id,id LIMIT 65", sessions).fetchall()
    if len(hosts) > 32 or len(rows) > 64:
        raise ValueError("database visibility exceeds evidence bound")
    return {"hosts": [row[0] for row in hosts], "queryHost": host,
            "rows": [dict(zip(("hostId", "id", "state", "createdAt"), row)) for row in rows],
            "localVisible": [row[1] for row in rows if row[0] == host]}


def peer_identity(connection):
    if sys.platform == "darwin":
        return {"pid": connection.getsockopt(0, 2)}
    pid, uid, gid = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    return {"pid": pid, "uid": uid, "gid": gid}


def receive_control(connection):
    header = release_workers.read_exact(connection, 5)
    length = struct.unpack(">I", header[1:])[0]
    if header[0] != 1 or length > 4 << 20:
        raise ValueError("catalog observer received invalid control frame")
    return json.loads(release_workers.read_exact(connection, length))


def observations(value):
    return {topic: {key: section[key] for key in ("ageMillis", "failing") if key in section}
            for topic, section in value.items() if topic in ("host", "sessions")}


class Observer:
    def __init__(self, state, output, members, maximum=180, identity_reader=None):
        if not 1 <= members <= 31:
            raise ValueError("catalog evidence requires one unchanged bounded group")
        self.state, self.output, self.maximum = Path(state), Path(output), maximum
        self.sessions = tuple("W" + format(index, "03X") for index in range(members))
        self.lock, self.condition = threading.RLock(), threading.Condition(threading.RLock())
        self.stopping = threading.Event()
        self.connection, self.thread, self.host = None, None, None
        self.closed, self.identity_reader = False, identity_reader
        self.published, self.previous = {}, None
        self.used, self.started = 0, time.monotonic_ns()
        self.record = {"schema": 1, "passive": True, "events": [], "positiveControl": {"passed": False},
                       "limits": {"seconds": maximum, "events": MAX_EVENTS, "bytes": MAX_RECORD_BYTES},
                       "interpretation": "Receipt order only. Added follows commit. Current age/failing describes completion, not scan start. No I/O retry or readiness decision."}

    def add(self, kind, value):
        with self.lock:
            event = {"kind": kind, "monotonicNs": time.monotonic_ns(), "unixNs": time.time_ns(), "value": value}
            size = len(json.dumps(event).encode())
            if len(self.record["events"]) >= MAX_EVENTS or self.used + size > MAX_RECORD_BYTES - 65536:
                raise ValueError("catalog evidence timeline bound exceeded")
            self.used += size
            self.record["events"].append(event)

    def start(self, identity):
        self.record["daemon"] = identity
        self.connection = socket.socket(socket.AF_UNIX)
        self.connection.settimeout(6)
        self.connection.connect(str(self.state / "daemon.sock"))
        self.record["watchPeer"] = peer_identity(self.connection)
        if self.record["watchPeer"]["pid"] != identity["pid"]:
            raise RuntimeError("catalog observer connected to another daemon PID")
        release_workers.control(self.connection, {"type": "state.watch", "requestId": "catalog-evidence", "watch": {"topics": ["host", "sessions"]}})
        initial = receive_control(self.connection)
        if initial.get("type") != "state.snapshot" or initial.get("more") or initial.get("page", 0):
            raise RuntimeError("catalog observer requires an unpaged initial snapshot")
        snapshot = initial["stateSnapshot"]
        if snapshot.get("sessions"):
            raise RuntimeError("catalog control must arm before the normal original session exists")
        self.host = snapshot["host"]["id"]
        self.record["host"] = self.host
        self.consume(initial)
        self.sample("armed")
        self.thread = threading.Thread(target=self.run, name="installed-catalog-evidence", daemon=True)
        self.thread.start()

    def consume(self, value):
        kind = value.get("type")
        if value.get("more") or value.get("page", 0) or kind in ("error", "state.resync"):
            raise RuntimeError("catalog observer lost complete watch coverage")
        event, current = value.get("stateEvent", {}), value.get("stateCurrent", {})
        snapshot = value.get("stateSnapshot", {})
        session = event.get("payload", {}).get("session", {})
        selected = {"type": kind, "seq": event.get("seq", current.get("seq", snapshot.get("seq"))),
                    "event": event.get("kind"), "session": {key: session[key] for key in ("id", "state") if key in session},
                    "current": observations(current.get("sections", snapshot.get("current", {})))}
        self.add("watch", selected)
        if event.get("kind") in ("session.added", "session.changed") and session.get("id"):
            with self.condition:
                self.published[session["id"]] = event["kind"]
                self.condition.notify_all()

    def prove(self, session, identity, host):
        # This waits only for the already normal original-session control, before cohort startup.
        with self.condition:
            received = self.condition.wait_for(lambda: session in self.published or self.stopping.is_set(), timeout=6)
        if not received or session not in self.published or identity != self.record["daemon"] or host != self.host:
            raise RuntimeError("catalog observer normal-create positive control failed")
        if not 4 <= len(session) <= 8 or not session.isascii() or not session.isalnum():
            raise ValueError("normal-create control returned an unsafe session ID")
        sessions = self.sessions + (session,)
        database = database_snapshot(self.state, sessions, self.host)
        if session not in database["localVisible"]:
            raise RuntimeError("normal-create event lacks its committed local database row")
        self.sessions = sessions
        self.record["positiveControl"] = {"passed": True, "session": session, "daemon": identity,
                                          "operation": "existing normal seed session creation", "event": self.published[session], "database": database}
        self.sample("positive-control")

    def sample(self, label):
        started = time.monotonic_ns()
        sessions = self.sessions
        value = {"sessions": {session: session_snapshot(self.state, session) for session in sessions}}
        try:
            value["database"] = database_snapshot(self.state, sessions, self.host)
        except (OSError, ValueError, sqlite3.Error) as error:
            value["database"] = unavailable(error)
        with self.lock:
            if label != "poll" or self.previous != value:
                self.add("snapshot", {"label": label, "readStartedNs": started, "readFinishedNs": time.monotonic_ns(), **value})
                self.previous = value

    def checkpoint(self, label):
        try:
            self.sample(label)
        except Exception as error:
            self.record["snapshotFailure"] = unavailable(error)

    def run(self):
        buffer = bytearray()
        try:
            self.read_loop(buffer)
        except Exception as error:
            if not self.stopping.is_set():
                self.record["observerFailure"] = unavailable(error)
        finally:
            self.stopping.set()
            with self.condition:
                self.condition.notify_all()

    def read_loop(self, buffer):
        while not self.stopping.is_set():
            if (time.monotonic_ns() - self.started) / 1e9 >= self.maximum:
                raise TimeoutError("passive observer lifetime exceeded")
            ready, _, _ = select.select([self.connection], [], [], 0.05)
            if ready:
                self.read_ready(buffer)
            self.sample("poll")

    def read_ready(self, buffer):
        chunk = self.connection.recv(65536)
        if not chunk:
            raise EOFError("catalog watch closed")
        buffer.extend(chunk)
        self.drain(buffer)

    def drain(self, buffer):
        while len(buffer) >= 5:
            length = struct.unpack(">I", buffer[1:5])[0]
            if buffer[0] != 1 or length > 4 << 20:
                raise ValueError("catalog watch frame exceeds protocol bound")
            if len(buffer) < length + 5:
                return
            self.consume(json.loads(buffer[5:length + 5]))
            del buffer[:length + 5]

    def stop(self, label):
        if self.closed:
            return
        self.stopping.set()
        if self.connection:
            self.connection.close()
        if self.thread:
            self.thread.join(timeout=3)
        stopped = self.thread is None or not self.thread.is_alive()
        if not stopped:
            raise RuntimeError("owned catalog observer did not stop")
        self.closed = True
        try:
            self.checkpoint(label)
            if self.identity_reader:
                self.record["daemonAtStop"] = self.identity_reader()
        except Exception as error:
            self.record["identityFailure"] = unavailable(error)
        finally:
            self.record["observerStopped"] = stopped
            self.record["elapsedSeconds"] = (time.monotonic_ns() - self.started) / 1e9
            release_workers.save(self.output, self.record)


def daemon_identity(case):
    from fixtures import installed_native, native_services
    manager = case.units.managers["daemon"]
    pid = manager.manager_pid()
    image = native_services.mapped_image(pid, case.binary)
    identity = image | {"processStamp": installed_native.process_stamp(pid)}
    if sys.platform == "linux":
        identity["kernelBootId"] = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    if image["sha256"] != case.pair["daemon"]["artifact"]["binarySha256"] or manager.manager_pid() != pid:
        raise RuntimeError("post-seed actual daemon image differs from intended published baseline")
    return identity


def io_boundary(session, phase):
    path = os.environ.get("MESH_CATALOG_IO_TIMELINE")
    if not path:
        return
    try:
        target = Path(path)
        if target.exists() and target.stat().st_size >= 16384:
            return
        with target.open("a") as stream:
            stream.write(json.dumps({"session": session, "phase": phase, "monotonicNs": time.monotonic_ns(), "unixNs": time.time_ns()}) + "\n")
    except OSError:
        pass
