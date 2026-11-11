#!/usr/bin/env python3
"""Bind the frozen policy inputs to fresh retention and completed proof bytes."""
import json
from pathlib import Path
import shutil
import sys


def stage(root, version, dist, proofs):
    input_path = root / "input.json"
    value = json.loads(input_path.read_bytes())
    if value["version"] != version:
        raise ValueError("publication version differs from the frozen source selection")
    value["retained"] = value["history"]
    value["history"] = json.loads((root / "fresh-history.json").read_bytes())
    input_path.write_text(json.dumps(value, sort_keys=True) + "\n")
    candidate = root / "documents" / version
    candidate.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(dist / "mesh-release.json", candidate / "mesh-release.json")
    for path in proofs.glob("*.json"):
        shutil.copyfile(path, candidate / path.name)
    rollback = []
    for path in (proofs / "rollback").glob("*.rollback.json"):
        data = json.loads(path.read_bytes())
        rollback.append({"forward": data["forward"], "rollback": path.name.removesuffix(".rollback.json")})
        shutil.copyfile(path, candidate / path.name)
    (candidate / "rollback-index.json").write_text(json.dumps(rollback, sort_keys=True) + "\n")


if __name__ == "__main__":
    stage(*(Path(argument) if index != 1 else argument for index, argument in enumerate(sys.argv[1:])))
