#!/usr/bin/env python3
from pathlib import Path
import sys


def escaped_owner_output(log, reference, marker):
    return r"\n\nlast output:\n" + marker in log


if __name__ == "__main__":
    sys.exit(0 if escaped_owner_output(Path(sys.argv[1]).read_text(encoding="utf-8"), sys.argv[2], sys.argv[3]) else 1)
