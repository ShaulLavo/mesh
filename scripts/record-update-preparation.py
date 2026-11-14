#!/usr/bin/env python3
"""Bind once-built native proof tools to the actual clean preparation checkout."""
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

TOOLS = ("candidate", "worker-probe", "startup-fault-candidate", "boundary")


def record(directory, expected_source):
    source = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    clean = not subprocess.check_output(["git", "status", "--porcelain"], text=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", source) or source != expected_source or not clean:
        raise ValueError("preparation checkout changed or is not clean")
    platform = subprocess.check_output(["go", "env", "GOOS", "GOARCH"], text=True).split()
    if len(platform) != 2:
        raise ValueError("preparation platform is unavailable")
    hashes = {}
    for name in TOOLS:
        with (directory / name).open("rb") as stream:
            hashes[name] = hashlib.file_digest(stream, "sha256").hexdigest()
    value = {"schema": 1, "source": {"commit": source, "clean": clean},
             "platform": dict(zip(("os", "arch"), platform, strict=True)), "sha256": hashes}
    (directory / "preparation.json").write_text(json.dumps(value, sort_keys=True) + "\n")
    return value


if __name__ == "__main__":
    record(Path(sys.argv[1]), sys.argv[2])
