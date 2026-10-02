"""Bounded, read-only context for owned benchmark children and signal sends."""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time


def emit(record):
    print(json.dumps({"actor_pid": os.getpid(), "timestamp_ns": time.time_ns(), **record}, sort_keys=True),
          file=sys.stderr, flush=True)


def process_snapshot(pid):
    if sys.platform != "linux":
        return {"status": "unsupported"}
    try:
        with Path(f"/proc/{pid}/stat").open() as source:
            fields = source.read(4096).rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return {"status": "unknown"}
        return {"status": "observed", "start_ticks": int(fields[19]), "parent_pid": int(fields[1]),
                "rss_bytes": int(fields[21]) * os.sysconf("SC_PAGE_SIZE")}
    except (OSError, ValueError, IndexError):
        return {"status": "unknown"}


def record_signal(pid, sig, reason, ownership, incarnation=None):
    record = {"event": "bench.signal_send", "target_pid": pid, "signal": int(sig), "reason": reason,
              "ownership": ownership, "incarnation": incarnation or process_snapshot(pid)}
    emit(record)
    return record


def signal_child(process, sig, reason):
    if process.poll() is None:
        record_signal(process.pid, sig, reason, {"kind": "popen_child", "parent_pid": os.getpid()})
        process.send_signal(sig)


def signal_self(sig, reason):
    record_signal(os.getpid(), sig, reason, {"kind": "self"})
    os.kill(os.getpid(), sig)


def memory_counts(path):
    try:
        with (path / "memory.events").open() as source:
            values = dict(line.split() for line in source.read(4096).splitlines())
        return {"status": "available", "oom": int(values["oom"]), "oom_kill": int(values["oom_kill"])}
    except PermissionError:
        return {"status": "no_access"}
    except (OSError, ValueError, KeyError):
        return {"status": "unavailable"}


def memory_before(pid):
    result = {"attribution": "cgroup_aggregate_context_only", "status": "unsupported", "scopes": []}
    if sys.platform != "linux":
        return result
    try:
        with Path(f"/proc/{pid}/cgroup").open() as source:
            groups = source.read(65536).splitlines()
        group = next(row[3:] for row in groups if row.startswith("0::"))
        mount = Path("/sys/fs/cgroup").resolve()
        path = (mount / group.lstrip("/")).resolve()
        path.relative_to(mount)
    except PermissionError:
        return {**result, "status": "no_access"}
    except (OSError, ValueError, StopIteration):
        return {**result, "status": "unavailable"}
    scopes = []
    # Parent counters can cover the memory ceiling above the child's scope.
    for _ in range(8):
        scopes.append({"path": str(path), "before": memory_counts(path)})
        if path == mount:
            break
        path = path.parent
    return {**result, "status": "captured", "scopes": scopes}


def memory_after(before):
    scopes = []
    for scope in before["scopes"]:
        after = memory_counts(Path(scope["path"]))
        delta = {}
        for key in ("oom", "oom_kill"):
            first, last = scope["before"].get(key), after.get(key)
            delta[key] = last - first if first is not None and last is not None and last >= first else None
        scopes.append({**scope, "after": after, "delta": delta})
    return {**before, "scopes": scopes}


def journal_context(kind, started, ended):
    result = {"kind": kind, "attribution": "context_only", "status": "unsupported", "events": []}
    if sys.platform != "linux":
        return result
    executable = shutil.which("journalctl")
    if executable is None:
        return {**result, "status": "unavailable"}
    command = [executable, "--no-pager", "--output=json", "--lines=16",
               "--output-fields=MESSAGE,_PID,_SYSTEMD_UNIT,_SYSTEMD_CGROUP,__REALTIME_TIMESTAMP",
               "--since", f"@{int(max(started - 1, ended - 60))}", "--until", f"@{int(ended) + 1}",
               "--grep", "(?i)oom|out of memory|killed process"]
    command += ["--dmesg"] if kind == "kernel" else ["--unit=systemd-oomd.service"]
    try:
        with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
            process = subprocess.Popen(command, stdout=output, stderr=errors)
            expired = False
            try:
                process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                signal_child(process, signal.SIGKILL, "diagnostic-journal-budget")
                expired = True
            try:
                process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                return {**result, "status": "unavailable", "detail": "journal child remains unreaped after SIGKILL"}
            if expired:
                return {**result, "status": "timeout"}
            output.seek(0)
            data = output.read(16384)
            errors.seek(0)
            error = errors.read(2048).decode(errors="replace").lower()
    except OSError:
        return {**result, "status": "unavailable"}
    status = "available" if data else "no_matching_events"
    if "permission" in error or "not seeing messages" in error or "access denied" in error:
        status = "no_access"
    elif "unrecognized option" in error or "invalid option" in error:
        status = "unsupported"
    elif process.returncode and not (process.returncode == 1 and not data and not error):
        status = "unavailable"
    events = []
    for line in data.splitlines()[:16]:
        try:
            row = json.loads(line)
            events.append({key: str(value)[:512] for key, value in row.items()
                           if key in ("MESSAGE", "_PID", "_SYSTEMD_UNIT", "_SYSTEMD_CGROUP", "__REALTIME_TIMESTAMP")})
        except (ValueError, AttributeError):
            continue
    return {**result, "status": status, "events": events, "output_capped": len(data) == 16384,
            "detail": error[:256], "exit_code": process.returncode}


class ChildObservation:
    def __init__(self, pid, reason):
        self.pid = pid
        self.reason = reason
        self.started = time.time()
        self.incarnation = process_snapshot(pid)
        self.last_rss = {"status": "unknown" if sys.platform == "linux" else "unsupported", "bytes": None}
        self.memory = memory_before(pid)
        self.sent = []
        self.stopped = threading.Event()
        self.sample()
        self.thread = threading.Thread(target=self.watch, daemon=True)
        self.thread.start()

    def sample(self):
        observed = process_snapshot(self.pid)
        if observed.get("start_ticks") != self.incarnation.get("start_ticks"):
            return
        if observed.get("rss_bytes", 0) > 0:
            self.last_rss = {"status": "observed", "bytes": observed["rss_bytes"], "observed_at_ns": time.time_ns()}

    def watch(self):
        while not self.stopped.wait(0.05):
            self.sample()

    def close(self):
        self.stopped.set()
        self.thread.join()

    def send_group(self, sig, reason):
        self.sent.append(record_signal(self.pid, sig, reason,
                                       {"kind": "popen_process_group", "group_id": self.pid}, self.incarnation))
        os.killpg(self.pid, sig)

    def finish(self, returncode):
        self.close()
        if returncode != -signal.SIGKILL:
            return
        matched = [record["reason"] for record in self.sent if record["signal"] == signal.SIGKILL]
        record = {"event": "bench.child_sigkill", "target_pid": self.pid, "reason": self.reason,
                  "incarnation": self.incarnation, "last_observed_rss": self.last_rss,
                  "matching_owner_signals": matched, "signal_log_scope": "this_child_owner",
                  "attribution": "owner_signal_recorded" if matched else "unexplained"}
        if not matched:
            ended = time.time()
            record.update(memory_events=memory_after(self.memory),
                          journal=[journal_context(kind, self.started, ended) for kind in ("kernel", "systemd-oomd")])
        emit(record)


def run_observed(command, *, reason, check=False, **options):
    with subprocess.Popen(command, **options) as process:
        observation = ChildObservation(process.pid, reason)
        try:
            output, error = process.communicate()
            observation.finish(process.returncode)
        finally:
            observation.close()
        if process.returncode == -signal.SIGKILL or check and process.returncode:
            raise subprocess.CalledProcessError(process.returncode, command, output, error)
        return subprocess.CompletedProcess(command, process.returncode, output, error)
