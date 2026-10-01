#!/usr/bin/env python3
"""Retain failed PTY evidence and stop only processes belonging to its private root."""
import json
import os
from pathlib import Path
import signal
import shutil
import time


def process_identity(pid):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] != "Z":
            return int(fields[19]), int(fields[1])
    except (FileNotFoundError, ProcessLookupError):
        pass
    return None


def owned_processes(root, binary):
    root = Path(root).resolve()
    identities, owned = {}, {}
    if not Path("/proc").is_dir():
        return owned
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        pid = int(entry.name)
        identity = process_identity(pid)
        if identity is None:
            continue
        identities[pid] = identity
        try:
            cwd = Path(os.readlink(entry / "cwd"))
            if cwd.is_relative_to(root):
                owned[pid] = identity
                continue
            if os.readlink(entry / "exe") != str(Path(binary).resolve()):
                continue
            args = (entry / "cmdline").read_bytes().split(b"\0")
            if len(args) < 6 or args[1] != b"session-worker":
                continue
            directory = Path(os.fsdecode(args[args.index(b"--dir") + 1])).resolve()
            if directory.parent in (root / "local/s", root / "remote/s"):
                owned[pid] = identity
        except (FileNotFoundError, ProcessLookupError, PermissionError, ValueError, IndexError):
            continue
    # Capture children before teardown can orphan a partially published PTY.
    previous = -1
    while previous != len(owned):
        previous = len(owned)
        owned.update({pid: identity for pid, identity in identities.items() if identity[1] in owned})
    return owned


def same_process(pid, identity):
    current = process_identity(pid)
    return current is not None and current[0] == identity[0]


def stop_owned(root, binary, captured):
    captured.update(owned_processes(root, binary))
    for sig in (signal.SIGTERM, signal.SIGKILL):
        for pid, identity in captured.items():
            try:
                descriptor = os.pidfd_open(pid)
                try:
                    if same_process(pid, identity):
                        signal.pidfd_send_signal(descriptor, sig)
                finally:
                    os.close(descriptor)
            except (FileNotFoundError, ProcessLookupError):
                pass
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline and any(same_process(pid, identity) for pid, identity in captured.items()):
            time.sleep(0.05)
    remaining = {pid: identity for pid, identity in captured.items() if same_process(pid, identity)}
    remaining.update(owned_processes(root, binary))
    if remaining:
        raise RuntimeError(f"failed fixture retains owned processes {sorted(remaining)} at {root}")


def retain_evidence(fixture):
    terminals = [bytes(terminal.output).decode(errors="replace") for terminal in fixture.terminals]
    saved = fixture.root / "failure-state"
    if saved.exists():
        return
    saved.mkdir()
    copy_errors = []
    sources = [fixture.root / "daemon.log"]
    for state in (fixture.local, fixture.remote):
        sources.extend(state.rglob("*"))
    for source in sources:
        if not source.is_file() or source.is_symlink():
            continue
        destination = saved / source.relative_to(fixture.root)
        destination.parent.mkdir(parents=True, exist_ok=True)
        try:
            shutil.copyfile(source, destination)
        except FileNotFoundError as error:
            copy_errors.append(str(error))
    processes = {}
    for pid, identity in owned_processes(fixture.root, fixture.binary).items():
        try:
            processes[pid] = {"startTicks": identity[0], "parentPid": identity[1],
                              "wchan": Path(f"/proc/{pid}/wchan").read_text(),
                              "cgroup": Path(f"/proc/{pid}/cgroup").read_text()}
        except (FileNotFoundError, ProcessLookupError):
            pass
    path = fixture.root / "failure-evidence.json"
    path.write_text(json.dumps({"root": str(fixture.root), "terminals": terminals,
                               "processes": processes, "copyErrors": copy_errors}, indent=2))
    print(f"FAILURE evidence retained at {fixture.root}", flush=True)
