#!/usr/bin/env python3
"""Collect content-addressed proof assets without overwriting differing bytes."""
import hashlib
from pathlib import Path
import re
import sys


def collect(source, destination):
    count = 0
    for path in source.glob("*/proofs/**/*.json"):
        name = path.name
        match = re.fullmatch(r"([0-9a-f]{64})(\.rollback)?\.json", name)
        if not match:
            raise ValueError(f"unexpected proof asset: {name}")
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != match[1]:
            raise ValueError(f"proof asset digest mismatch: {name}")
        directory = destination / "rollback" if match[2] else destination
        directory.mkdir(parents=True, exist_ok=True)
        target = directory / name
        if target.exists() and target.read_bytes() != data:
            raise ValueError(f"proof asset replacement refused: {name}")
        target.write_bytes(data)
        count += 1
    if not count:
        raise ValueError("no native proof assets found")


if __name__ == "__main__":
    collect(Path(sys.argv[1]), Path(sys.argv[2]))
