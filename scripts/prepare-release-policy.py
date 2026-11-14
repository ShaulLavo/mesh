#!/usr/bin/env python3
"""Freeze the policy inputs shared by native proofs and final publication."""
import argparse
import hashlib
import json
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory", type=Path)
    parser.add_argument("documents", type=Path)
    parser.add_argument("version")
    parser.add_argument("published_at")
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    history = json.loads(args.inventory.read_bytes())
    epoch = 1
    previous = None
    waypoint = ""
    for item in reversed(history):
        metadata = args.documents / item["tag"] / "mesh-update.json"
        if not metadata.is_file():
            continue
        published = json.loads(metadata.read_bytes())
        epoch = published["epoch"]
        root = json.loads((metadata.parent / "mesh-update-root.json").read_bytes())
        updater = published.get("updater")
        if updater and updater["epoch"] == epoch + 1:
            root_bytes = (metadata.parent / "mesh-update-root.json").read_bytes()
            epoch += 1
            previous = {"tag": item["tag"], "name": "mesh-update-root.json", "sha256": hashlib.sha256(root_bytes).hexdigest()}
            waypoint = item["tag"]
            break
        previous = root.get("previous")
        if previous:
            prior = json.loads((args.documents / previous["tag"] / previous["name"]).read_bytes())
            waypoint = prior["head"]["tag"]
        break
    policy = json.loads((Path(__file__).parent / "upgrade-policy.json").read_bytes())
    value = {"version": args.version, "publishedAt": args.published_at, "epoch": epoch,
             "trust": 1, "policy": policy, "history": history, "waypoint": waypoint}
    if previous:
        value["previousRoot"] = previous
    args.output.write_text(json.dumps(value, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
