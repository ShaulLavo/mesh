#!/usr/bin/env python3
"""Stop a private served fixture without racing detached workers' final writes."""
from pathlib import Path
import os
import signal
import sys
import time

from failed_fixture import owned_processes, same_process, stop_owned


def is_worker(pid):
    try:
        return b"session-worker" in Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
    except (FileNotFoundError, ProcessLookupError):
        return False


def signal_command(pid, identity, sig):
    try:
        descriptor = os.pidfd_open(pid)
        try:
            if same_process(pid, identity):
                signal.pidfd_send_signal(descriptor, sig)
        finally:
            os.close(descriptor)
    except (FileNotFoundError, ProcessLookupError):
        pass


def stop_commands(processes, sig, sent):
    for pid, identity in processes.items():
        key = (pid, identity[0], sig)
        if key in sent or is_worker(pid):
            continue
        signal_command(pid, identity, sig)
        sent.add(key)


def workers_finished(root, processes):
    alive = any(same_process(pid, identity) for pid, identity in processes.items())
    return not alive and not any(root.glob("origin-state/s/*/sock"))


def await_workers(root, binary, processes):
    sent = set()
    started = time.monotonic()
    print(f"waiting for served fixture workers at {root}", flush=True)
    while time.monotonic() - started < 10:
        processes.update(owned_processes(root, binary))
        sig = signal.SIGTERM if time.monotonic() - started < 2 else signal.SIGKILL
        stop_commands(processes, sig, sent)
        if workers_finished(root, processes):
            return
        time.sleep(0.025)
    raise RuntimeError(f"served workers did not finish their final writes; retain {root}")


def finish_workers(root, binary):
    if not Path("/proc/self/stat").exists():
        raise RuntimeError(f"cannot prove fixture process ownership on this host; retain {root}")
    processes = owned_processes(root, binary)
    try:
        await_workers(root, binary, processes)
    except BaseException:
        stop_owned(root, binary, processes)
        raise


if __name__ == "__main__":
    finish_workers(Path(sys.argv[1]).resolve(), sys.argv[2])
