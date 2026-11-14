"""Opt-in fixture-only recurrence ledger and birth-checked failed-attempt cleanup."""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import signal
import sys

from fixtures import release_worker_evidence as evidence


CAP = 2
FAILURE = re.compile(r'^worker: start /bin/sh: child-start path="/bin/sh" errno=1 stage=unavailable: fork/exec /bin/sh: operation not permitted$', re.MULTILINE)


def eligible(row):
    failure = row.get("startupFailure", {})
    log = failure.get("workerLogTail")
    return sys.platform == "linux" and row["shellPid"] == 0 and isinstance(log, str) and FAILURE.search(log) is not None


@contextmanager
def ledger():
    name = os.environ.get("MESH_WORKER_EPERM_LEDGER")
    if not name:
        yield None
        return
    if sys.platform != "linux" or os.uname().machine != "x86_64":
        raise ValueError("EPERM unit requires one Linux AMD64 native shard/profile")
    import fcntl
    path = Path(name).resolve()
    with path.with_suffix(path.suffix + ".lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise RuntimeError("EPERM recurrence ledger is busy; cohort startup refused") from error
        value = json.loads(path.read_bytes())
        validate_ledger(value)
        ensure_capacity(value)
        yield (path, value)


def validate_ledger(value):
    if not isinstance(value, dict) or value.get("schema") != 1 or "cap" not in value:
        raise ValueError("invalid EPERM recurrence ledger")
    if value["cap"] is not None and (type(value["cap"]) is not int or value["cap"] != CAP):
        raise ValueError("EPERM cap must be null for normal CI or two for bounded diagnostics")
    if not isinstance(value.get("events"), list):
        raise ValueError("invalid EPERM recurrence events")


def ensure_capacity(value):
    if value["cap"] is not None and len(value["events"]) >= value["cap"]:
        raise RuntimeError("EPERM recurrence cap reached; no further worker spawn permitted")


def persist(ledger_state, save):
    path, value = ledger_state
    save(path, value)


def capture(ledger_state, row, error, directory, deadline, save):
    from fixtures import release_worker_snapshot
    path, value = ledger_state
    attempt = {"attempt": len(row.get("startupAttempts", [])) + 1, "outcome": "EPERM",
               "pid": row["pid"], "shellPid": row["shellPid"], "startupFailure": row["startupFailure"],
               "exception": {"type": type(error).__name__, "message": str(error)[:8192]},
               "logPath": str(directory / "worker.log"), "cohort": row["epermCohort"],
               "arguments": row["startupArguments"], "deadlineMonotonic": row["startupDeadlineMonotonic"],
               "selectedImage": row["startupImage"]}
    row.setdefault("startupAttempts", []).append(attempt)
    value["events"].append({"session": row["session"], "version": row["version"], "digest": row["digest"], "failure": attempt})
    # Count and retain the original failure before running any optional diagnostic.
    save(path, value)
    attempt["retainedLog"] = retain_log(directory / "worker.log", path.parent / ("eperm-" + str(len(value["events"])) + ".worker.log"))
    attempt["snapshot"] = safe_snapshot(release_worker_snapshot.capture, row["startupFailure"], deadline)
    save(path, value)
    limit = "no count gate" if value["cap"] is None else str(value["cap"])
    print(f"EPERM recurrence {len(value['events'])}/{limit}: {row['session']} {row['version']} pid {row['pid']}; retained in {path}; original failure: {str(error)[:4096]}", file=sys.stderr)
    return attempt


def retain_log(source, destination):
    with source.open("rb") as stream:
        data = stream.read(65537)
    destination.write_bytes(data[:65536])
    return {"path": str(destination), "bytes": min(len(data), 65536), "byteLimitReached": len(data) > 65536}


def safe_snapshot(operation, failure, deadline):
    try:
        cwd = failure.get("workerAtObservation", {}).get("cwd", os.getcwd())
        return operation(failure, cwd if isinstance(cwd, str) else os.getcwd(), deadline)
    except Exception as error:
        return dict(evidence.unavailable(error), cleanupConfirmed=False)


def image(path):
    with Path(path).open("rb") as stream:
        mapped = evidence.image_digest(stream.fileno())
    return {key: mapped[key] for key in ("device", "inode", "bytes", "sha256")}


def verify_image(path, original, row):
    if image(path) != original or original["sha256"] != row["digest"]:
        raise RuntimeError("selected original EPERM worker image changed; retry refused")
    mapped = row["startupFailure"].get("mappedImage", {})
    if "sha256" in mapped and mapped["sha256"] != original["sha256"]:
        raise RuntimeError("mapped failed worker differs from selected image; retry refused")


def reconcile(process, failure):
    # The EPERM child never published a shell PID. Do not invent one or kill by a stale PID.
    if process.poll() is not None:
        process.wait(timeout=0)
        return {"worker": "reaped", "shell": "no published child PID"}
    original = failure.get("workerAtObservation", {}).get("stat", {}).get("startTicks")
    with evidence.Startup(process.pid) as current:
        actual = current.before.get("stat", {}).get("startTicks")
        if original is None or actual != original:
            raise RuntimeError("failed worker birth identity unavailable or changed; retry refused")
        os.kill(process.pid, signal.SIGKILL)
    process.wait(timeout=0.1)
    return {"worker": "birth-checked and reaped", "shell": "no published child PID"}


def stop_failed(worker):
    original = worker["startupFailure"].get("workerAtObservation", {}).get("stat", {}).get("startTicks")
    with evidence.Startup(worker["pid"]) as current:
        actual = current.before.get("stat", {}).get("startTicks")
        if actual is None:
            return
        if original is None or original != actual:
            raise RuntimeError("failed worker birth identity unavailable or changed during cleanup")
        os.kill(worker["pid"], signal.SIGKILL)


def diagnostic_cleanup(path, required=False):
    if not required and not path.exists():
        return {"confirmed": True, "events": 0}
    try:
        value = json.loads(path.read_bytes())
        validate_ledger(value)
        snapshots = [snapshot_cleanup(event["failure"]["snapshot"]) for event in value["events"]]
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        return {"confirmed": False, "error": type(error).__name__ + ": " + str(error)}
    return {"confirmed": all(row["confirmed"] for row in snapshots), "events": len(snapshots), "snapshots": snapshots}


def snapshot_cleanup(snapshot):
    commands = [name for name in ("kernelJournal", "kernelDmesg", "hostedSudoKernelJournal") if name in snapshot]
    records = [{"command": name, "cleanup": snapshot[name].get("cleanup", {})} for name in commands]
    confirmed = snapshot.get("cleanupConfirmed") is True and "kernelJournal" in commands and all(
        row["cleanup"].get("reaped") is True and row["cleanup"].get("stdoutClosed") is True for row in records)
    return {"confirmed": confirmed, "commands": records}
