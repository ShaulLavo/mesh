#!/usr/bin/env python3
"""Last-resort cleanup of this benchmark's exact scratch executable and scripts."""
import os
from pathlib import Path
import signal
import sys
import time


def owned(root):
    targets = {str(root / name).encode() for name in ("mesh", "run.py", "workload.py")}
    targets.update(str(path).encode() for path in root.glob("*.test"))
    found = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        try:
            command = (entry / "cmdline").read_bytes().split(b"\0")
            stat = (entry / "stat").read_text().rsplit(")", 1)[1].split()
            executable = os.readlink(entry / "exe").encode()
            if stat[0] != "Z" and (targets.intersection(command) or executable in targets):
                found.append((int(entry.name), int(stat[19]), command))
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            continue
    return found


def send(pid, start, sig):
    try:
        stat = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        if int(stat[19]) == start:
            os.kill(pid, sig)
    except (FileNotFoundError, ProcessLookupError):
        pass


def main():
    root = Path(sys.argv[1]).resolve()
    if root.parent != Path("/tmp") or not root.name.startswith("mesh-m5-"):
        raise SystemExit("cleanup requires the exact Pi scratch root")
    for pid, start, command in owned(root):
        if str(root / "run.py").encode() in command:
            send(pid, start, signal.SIGTERM)
    deadline = time.monotonic() + 10
    while owned(root) and time.monotonic() < deadline:
        time.sleep(0.2)
    for sig in (signal.SIGHUP, signal.SIGTERM, signal.SIGKILL):
        for pid, start, _ in owned(root):
            send(pid, start, sig)
        time.sleep(0.5)
    remaining = owned(root)
    if remaining:
        raise SystemExit(f"scratch processes remain: {[row[0] for row in remaining]}")
    print("Pi scratch process check: clean")


if __name__ == "__main__":
    main()
