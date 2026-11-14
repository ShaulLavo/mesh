#!/usr/bin/env python3
"""Acquire every policy-selected source and verify its published binary identity."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import time

DIGEST = re.compile(r"[0-9a-f]{64}\Z")


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def acquire(tag, name, expected, directory, cache):
    if not DIGEST.fullmatch(expected) or "/" in name or "\\" in name:
        raise ValueError("invalid baseline asset identity")
    cached = cache / expected
    if cached.is_file():
        if sha256(cached) != expected:
            raise ValueError(f"immutable baseline cache changed: {expected}")
        print(f"verified baseline cache hit: {tag}/{name}", file=sys.stderr)
        return cached
    destination = directory / name
    for attempt in range(3):
        try:
            result = subprocess.run(["gh", "release", "download", tag, "--pattern", name,
                                     "--dir", str(directory), "--clobber"], check=False, timeout=60)
        except subprocess.TimeoutExpired:
            result = None
        if result is not None and result.returncode == 0:
            break
        if attempt == 2:
            raise RuntimeError(f"required baseline acquisition failed: {tag}/{name}")
        time.sleep(attempt + 1)
    if sha256(destination) != expected:
        raise ValueError(f"published baseline digest changed: {tag}/{name}")
    destination.replace(cached)
    return cached


def unpack(archive, expected, output):
    with tarfile.open(archive, "r:gz") as package:
        members = [member for member in package if member.name in ("mesh", "./mesh")]
        if len(members) != 1 or not members[0].isfile() or not 0 < members[0].size <= 128 << 20:
            raise ValueError("archive must contain one bounded regular mesh executable")
        write_executable(package.extractfile(members[0]), output)
    if sha256(output) != expected:
        raise ValueError("baseline executable differs from its manifest")
    output.chmod(0o755)


def write_executable(stream, output):
    with stream, output.open("wb") as destination:
        while block := stream.read(1 << 20):
            destination.write(block)


def prepare_item(item, output, cache):
    artifact = item["artifact"]
    release = output / ("release-" + item["version"])
    release.mkdir(exist_ok=True)
    binary = release / "mesh"
    if not binary.is_file() or sha256(binary) != artifact["binarySha256"]:
        archive = acquire(item["version"], artifact["archive"], artifact["sha256"], release, cache)
        unpack(archive, artifact["binarySha256"], binary)
    binary.chmod(0o755)
    return binary


def prepare_workers(selection, platform, output, cache):
    workers = []
    for item in selection["workers"]:
        if item["artifact"]["platform"] != platform:
            continue
        binary = prepare_item(item, output, cache)
        workers.append(dict(item, binary=str(binary.relative_to(output))))
    if not workers:
        raise ValueError("native worker registry is empty")
    (output / "workers.json").write_text(json.dumps({"schema": 1, "platform": platform, "workers": workers}, sort_keys=True) + "\n")


def pinned_assets(frozen, platform):
    key = platform.replace("/", "_")
    pattern = re.compile(r"baseline_" + re.escape(key) + r"_([0-9a-f]{64})\.bin")
    assets = {}
    for name in sorted(set(frozen.split(","))):
        match = pattern.fullmatch(name)
        if match:
            assets[name] = match[1]
    return assets


def freeze_pins(path, frozen):
    path = Path(path)
    value = json.loads(path.read_bytes())
    pinned = []
    for platform in ("linux/amd64", "linux/arm64", "darwin/arm64"):
        pinned.extend({"artifact": {"platform": dict(zip(("os", "arch"), platform.split("/"))), "binarySha256": digest},
                       "asset": name, "rollback": True} for name, digest in pinned_assets(frozen, platform).items())
    value["customBaselines"] = pinned
    path.write_text(json.dumps(value, sort_keys=True) + "\n")


def proof_shards(selection, frozen):
    full = {}
    for item in selection["baselines"]:
        platform = item["artifact"]["platform"]
        key = platform["os"] + "/" + platform["arch"]
        versions = full.setdefault(key, set())
        if item["rollback"]:
            versions.add(item["version"])
    sizes = [len(versions) + len(pinned_assets(frozen, platform)) for platform, versions in full.items()]
    return list(range(max([1] + sizes)))


def main(args):
    candidate, previous, platform, frozen, directory, output_list = args
    selection = json.loads(Path(os.environ["MESH_RELEASE_SELECTION"]).read_bytes())
    output = Path(directory)
    output.mkdir(parents=True, exist_ok=True)
    cache = Path(os.environ.get("MESH_BASELINE_CACHE", str(output / "cache")))
    cache.mkdir(parents=True, exist_ok=True)
    baselines = [item for item in selection["baselines"]
                 if item["artifact"]["platform"] == dict(zip(("os", "arch"), platform.split("/"), strict=True))]
    if previous not in [item["version"] for item in baselines] or not baselines:
        raise ValueError("the previous published release must be a required baseline")
    paths = []
    for item in baselines:
        binary = prepare_item(item, output, cache)
        paths.append(str(binary))
        (binary.parent / "source.json").write_text(json.dumps(item, sort_keys=True) + "\n")
    prepare_workers(selection, dict(zip(("os", "arch"), platform.split("/"), strict=True)), output, cache)
    for name, digest in pinned_assets(frozen, platform).items():
        cached = acquire(candidate, name, digest, output, cache)
        binary = output / name
        binary.write_bytes(cached.read_bytes())
        binary.chmod(0o755)
        paths.append(str(binary))
    Path(output_list).write_text("\n".join(sorted(set(paths))) + "\n")


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "--pins":
        freeze_pins(*sys.argv[2:])
    elif len(sys.argv) == 4 and sys.argv[1] == "--shards":
        print(json.dumps(proof_shards(json.loads(Path(sys.argv[2]).read_bytes()), sys.argv[3])))
    else:
        main(sys.argv[1:])
