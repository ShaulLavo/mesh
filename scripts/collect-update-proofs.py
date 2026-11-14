#!/usr/bin/env python3
"""Collect content-addressed proof assets without overwriting differing bytes."""
import hashlib
import json
from pathlib import Path
import re
import shutil
import sys
import tempfile

from fixtures import release_worker_groups, release_worker_index


def collect(source, destination, selection=None, expected_source=None):
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".collect-", dir=destination.parent) as temporary:
        staging = Path(temporary)
        collect_private(source, staging, selection, expected_source)
        shutil.copytree(staging, destination, dirs_exist_ok=True)


def collect_private(source, destination, selection, expected_source):
    count = 0
    companions = list(source.glob("*/proofs/*.worker-groups.json"))
    paths = [path for path in source.glob("*/proofs/*.json") if release_worker_groups.RECEIPT.fullmatch(path.name)]
    paths += list(source.glob("*/proofs/rollback/*.rollback.json"))
    for path in paths:
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
    for path in companions + list(source.glob("*/proofs/groups/leaves/*.worker-group.json")):
        data = path.read_bytes()
        match = release_worker_groups.ASSET.fullmatch(path.name)
        if not match or len(data) > release_worker_groups.MAX_DOCUMENT or release_worker_groups.digest(data) != match[1]:
            raise ValueError("native worker group asset differs from its content address")
        directory = destination if path in companions else destination / "groups" / "leaves"
        directory.mkdir(parents=True, exist_ok=True)
        target = directory / path.name
        if target.exists() and target.read_bytes() != data:
            raise ValueError("worker group asset replacement refused")
        target.write_bytes(data)
    if selection is not None:
        frozen = release_worker_index.collect_validation(source, destination, selection, expected_source)
        (destination / "groups" / "frozen.json").write_bytes(release_worker_groups.encode(frozen))
    elif companions:
        raise ValueError("grouped collection requires the authoritative frozen selection and source")


if __name__ == "__main__":
    if len(sys.argv) != 5:
        raise SystemExit("usage: collect-update-proofs.py SHARDS OUTPUT SELECTION SOURCE")
    collect(Path(sys.argv[1]), Path(sys.argv[2]), json.loads(Path(sys.argv[3]).read_bytes()), sys.argv[4])
