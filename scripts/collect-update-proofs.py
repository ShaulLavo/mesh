#!/usr/bin/env python3
"""Collect content-addressed proof assets without overwriting differing bytes."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tempfile

from fixtures import release_worker_groups, release_worker_index


def batch_keys(matrix):
    return {f"{row['key']}-{row['shard']}": (row["platform"], row["shard"]) for row in matrix["include"]}


def batch_artifact_ids(matrix, uploads):
    if set(uploads) != set(batch_keys(matrix)):
        raise ValueError("release requires one upload for each scheduled proof batch")
    ids = list(uploads.values())
    if len(set(ids)) != len(ids) or any(not re.fullmatch(r"[1-9][0-9]*", artifact) for artifact in ids):
        raise ValueError("proof batches must reference distinct artifact IDs")
    return ",".join(ids)


def check_batches(directory, matrix, run, attempt, manifest):
    expected = batch_keys(matrix)
    candidates = {item["platform"]["os"] + "/" + item["platform"]["arch"]: item for item in manifest["artifacts"]}
    seen = set()
    for path in directory.iterdir():
        value = json.loads((path / "proof-batch.json").read_bytes())
        identity = (value["platform"], value["shard"])
        key = f"{value['platform'].replace('/', '-')}-{value['shard']}"
        recorded = value["attempt"]
        if (key in seen or expected.get(key) != identity or value["run"] != run
                or not re.fullmatch(r"[1-9][0-9]*", recorded) or int(recorded) > int(attempt)
                or path.name != f"update-proofs-{key}-attempt-{recorded}"):
            raise ValueError(f"proof batch has a wrong run, attempt or platform: {path.name}")
        build = json.loads((path / "candidate-reader.json").read_bytes())["build"]
        candidate = candidates[value["platform"]]
        if (build["platform"] != candidate["platform"] or build["digest"] != candidate["binarySha256"]
                or build["version"] != manifest["version"] or build["commit"] != manifest["commit"] or build["modified"]):
            raise ValueError(f"proof batch ran against another candidate: {path.name}")
        seen.add(key)
    if seen != set(expected):
        raise ValueError("release proof batch uploads are incomplete")


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


def main():
    if sys.argv[1:] == ["--artifact-ids"]:
        ids = batch_artifact_ids(json.loads(os.environ["PROOF_MATRIX"]), json.loads(os.environ["PROOF_UPLOADS"]))
        with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
            output.write(f"ids={ids}\n")
        return
    if len(sys.argv) == 4 and sys.argv[1] == "--check-batches":
        check_batches(Path(sys.argv[2]), json.loads(os.environ["PROOF_MATRIX"]), os.environ["GITHUB_RUN_ID"],
                      os.environ["GITHUB_RUN_ATTEMPT"], json.loads(Path(sys.argv[3]).read_bytes()))
        return
    if len(sys.argv) != 5:
        raise SystemExit("usage: collect-update-proofs.py SHARDS OUTPUT SELECTION SOURCE")
    collect(Path(sys.argv[1]), Path(sys.argv[2]), json.loads(Path(sys.argv[3]).read_bytes()), sys.argv[4])


if __name__ == "__main__":
    main()
