"""Synthetic direct Python -> Go worker ancestry; never starts Mesh."""
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    worker, mode, output = sys.argv[1:]
    with Path(output).open("xb") as stream:
        child = subprocess.Popen([worker, mode], stdout=stream, stderr=subprocess.STDOUT)
        try:
            code = child.wait(timeout=15)
        finally:
            if child.poll() is None:
                child.kill()
            child.wait(timeout=3)
    value = json.loads(Path(output).read_bytes())
    if code or value["worker"] != child.pid or value["parent"] != os.getpid():
        raise RuntimeError("synthetic worker was not the direct Python child")


if __name__ == "__main__":
    main()
