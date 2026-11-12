#!/usr/bin/env python3
"""Bind the frozen policy inputs to fresh retention and completed proof bytes."""
import json
from datetime import datetime, timezone
from pathlib import Path
import shutil
import sys

from fixtures import release_worker_index


def stage(root, version, dist, proofs, shards):
    input_path = root / "input.json"
    value = json.loads(input_path.read_bytes())
    if value["version"] != version:
        raise ValueError("publication version differs from the frozen source selection")
    value["retained"] = value["history"]
    value["history"] = json.loads((root / "fresh-history.json").read_bytes())
    value["publishedAt"] = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    readers = native_readers(shards)
    if len(readers) != 3:
        raise ValueError("publication requires the three actual native candidate readers")
    value["readers"] = readers
    input_path.write_text(json.dumps(value, sort_keys=True) + "\n")
    candidate = root / "documents" / version
    candidate.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(dist / "mesh-release.json", candidate / "mesh-release.json")
    frozen = json.loads((proofs / "groups" / "frozen.json").read_bytes())
    if frozen["selection"] != json.loads((root / "selection.json").read_bytes()):
        raise ValueError("frozen worker selection changed before publication")
    manifest = json.loads((dist / "mesh-release.json").read_bytes())
    index = release_worker_index.stage(dist, proofs, frozen, manifest["commit"])
    for name in release_worker_index.closure(index, release_worker_index.resolver(proofs)):
        source = proofs / name if name.endswith(".worker-groups.json") else proofs / "groups" / "leaves" / name
        shutil.copyfile(source, candidate / name)
    shutil.copyfile(dist / "mesh-worker-groups.json", candidate / "mesh-worker-groups.json")
    for path in proofs.glob("*.json"):
        shutil.copyfile(path, candidate / path.name)
    rollback = []
    for path in (proofs / "rollback").glob("*.rollback.json"):
        data = json.loads(path.read_bytes())
        rollback.append({"forward": data["forward"], "rollback": path.name.removesuffix(".rollback.json")})
        shutil.copyfile(path, candidate / path.name)
    (candidate / "rollback-index.json").write_text(json.dumps(rollback, sort_keys=True) + "\n")


def native_readers(shards):
    readers = {}
    for path in sorted(shards.glob("*/candidate-reader.json")):
        value = json.loads(path.read_bytes())
        platform = value["build"]["platform"]
        key = (platform["os"], platform["arch"])
        if key in readers and readers[key] != value:
            raise ValueError("native proof shards disagree on their candidate reader")
        readers[key] = value
    return [readers[key] for key in sorted(readers)]


if __name__ == "__main__":
    stage(*(Path(argument) if index != 1 else argument for index, argument in enumerate(sys.argv[1:])))
