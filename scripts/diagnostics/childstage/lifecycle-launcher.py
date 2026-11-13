"""Finite Python parent for synthetic Go thread exit and exec controls only."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys


def main():
    worker, mode, output = sys.argv[1:]
    child = subprocess.Popen([worker, mode])
    try:
        code = child.wait(timeout=15)
    finally:
        if child.poll() is None:
            child.kill()
        child.wait(timeout=3)
    expected = -signal.SIGKILL if mode == "lifecycle-kill" else 0
    Path(output).write_text(json.dumps({"worker": child.pid, "parent": os.getpid(), "mode": mode,
                                       "expected": expected, "exit": code}) + "\n")
    if code != expected:
        raise RuntimeError("synthetic lifecycle outcome differs from its control")


if __name__ == "__main__":
    main()
