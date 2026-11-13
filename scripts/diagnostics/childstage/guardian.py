"""Own only the launched subtree and reap it even after controller SIGKILL."""
import json
import os
import select
import signal
import subprocess
import sys
from pathlib import Path

import backend


def publish(value):
    try:
        os.write(sys.stdout.fileno(), (json.dumps(value) + "\n").encode())
    except BrokenPipeError:
        pass


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


def watch(target):
    announced = False
    while True:
        code = target.poll()
        if code is not None and not announced:
            publish({"exit": code})
            announced = True
        if not select.select([sys.stdin], [], [], 0.02)[0]:
            continue
        if os.read(sys.stdin.fileno(), 1):
            raise RuntimeError("owned guardian accepts EOF only")
        return


def main():
    if len(sys.argv) < 5 or os.geteuid() == 0:
        raise SystemExit("normal-user owned guardian requires descriptors and target")
    log, read, ready = map(int, sys.argv[1:4])
    signal.signal(signal.SIGTERM, terminate)
    previous = backend.subreaper()
    owner = backend.KernelRun(Path("/unused"), "guardian")
    target = None
    try:
        target = subprocess.Popen(sys.argv[4:], pass_fds=(read, ready), stdout=log, stderr=log)
        owner.own(target.pid)
        os.close(read)
        os.close(ready)
        os.close(log)
        publish({"root": target.pid, "startTicks": backend.start_ticks(target.pid)})
        watch(target)
    finally:
        remaining = owner.cleanup()
        backend.subreaper(previous)
        if remaining or backend.child_pids(os.getpid()):
            raise RuntimeError("guardian subtree cleanup incomplete")


if __name__ == "__main__":
    main()
