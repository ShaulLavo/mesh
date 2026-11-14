"""Bounded, read-only evidence before a failed original attachment is cleaned up."""
import base64
import json
from pathlib import Path
import stat
import subprocess
import sys
import time
import traceback

from fixtures import installed_clock, installed_listener_evidence as evidence

FAILURE = "attachment-failure.json"
MAXIMUM = 65536


def selected(path, fields):
    value = json.loads(evidence.read_private(path, MAXIMUM))
    return {key: value[key] for key in fields if key in value}


def worker_tail(path):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError("not a regular worker log")
    with path.open("rb") as stream:
        stream.seek(max(0, path.stat().st_size - 8192))
        return stream.read(8192).decode(errors="replace")


def collect(expected):
    state = Path(expected["state"])
    session = state / "s" / expected["session"]
    journal = evidence.attempt(lambda: selected(state / "update/installation.json", ("phase", "request")))
    request = journal.pop("request", {})
    journal["version"] = request.get("manifest", {}).get("version")
    return {"schema": 1, "scope": "original attachment resume failure before cleanup",
            "observedMonotonicNs": time.monotonic_ns(), "client": expected["client"],
            "processes": {name: evidence.attempt(lambda: evidence.process(pid))
                          for name, pid in expected["processes"].items()},
            "expectedProcessStamps": expected["stamps"],
            "workerMeta": evidence.attempt(lambda: selected(session / "meta.json",
                ("id", "pid", "state", "lastAttachedAt", "detachedAt", "exitedAt", "exitCode"))),
            "workerPtyTail": evidence.attempt(lambda: worker_tail(session / "worker.log")),
            "journal": journal, "challenge": expected["challenge"],
            "resumeStack": expected["resumeStack"],
            "clientPtyTail": evidence.decode_transcript(expected["transcript"]),
            "limits": {"seconds": 2, "bytes": MAXIMUM, "signals": False, "controlRequests": False},
            "missingObservation": "original client transport error chain is unavailable if that binary swallowed it"}


def capture(case, token, start):
    record = {"schema": 1, "status": "unavailable"}
    started = time.monotonic()
    try:
        budget = installed_clock.timeout(2)
        transcript = evidence.attempt(case.terminal.snapshot)
        if isinstance(transcript, bytes):
            transcript = base64.b64encode(transcript).decode()
        processes = {"client": case.terminal.pid, "worker": case.worker, "shell": case.shell,
                     "listener": case.listener["pid"]}
        expected = {"state": str(case.state), "session": case.session,
                    "client": {"pid": case.terminal.pid, "exitStatus": case.terminal.poll()},
                    "processes": processes, "stamps": case.process_stamps,
                    "challenge": {"token": token, "clientOutputStart": start}, "transcript": transcript,
                    "resumeStack": [{"file": Path(frame.filename).name, "function": frame.name, "line": frame.lineno}
                                    for frame in traceback.extract_stack(limit=8)]}
        budget = installed_clock.timeout(min(budget, 2 - (time.monotonic() - started)))
        result = subprocess.run([sys.executable, "-m", "fixtures.installed_attachment_evidence"],
                                cwd=Path(__file__).resolve().parents[1], input=json.dumps(expected),
                                capture_output=True, text=True, timeout=budget, check=False)
        if result.returncode or len(result.stdout.encode()) > MAXIMUM:
            raise RuntimeError("attachment evidence collector failed or exceeded its output bound")
        record = json.loads(result.stdout)
        if not isinstance(record, dict):
            raise ValueError("attachment evidence is not a record")
    except Exception as error:
        record = {"schema": 1, "status": "unavailable", "errorType": type(error).__name__}
    record["elapsedSeconds"] = time.monotonic() - started
    encoded = json.dumps(record) + "\n"
    if len(encoded.encode()) > MAXIMUM:
        encoded = json.dumps({"schema": 1, "status": "unavailable", "errorType": "OutputBound"}) + "\n"
    evidence.attempt(lambda: (case.root / FAILURE).write_text(encoded))


def expect(case, token, start):
    try:
        return case.terminal.expect("__RESUMED__" + token + "__", since=start)
    except RuntimeError:
        evidence.attempt(lambda: capture(case, token, start))
        raise


def retain(root, output, name):
    if (root / FAILURE).exists():
        evidence.attempt(lambda: (output / (name + "-" + FAILURE)).write_bytes(
            evidence.read_private(root / FAILURE, MAXIMUM)))


if __name__ == "__main__":
    payload = sys.stdin.buffer.read(16385)
    if len(payload) > 16384:
        raise ValueError("attachment evidence input exceeds its bound")
    encoded = json.dumps(collect(json.loads(payload)))
    if len(encoded.encode()) > MAXIMUM:
        raise ValueError("attachment evidence output exceeds its bound")
    print(encoded)
