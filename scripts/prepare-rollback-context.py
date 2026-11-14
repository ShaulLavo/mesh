#!/usr/bin/env python3
"""Describe actual prepared artifacts for an unpublished rollback cache context."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tarfile

PLATFORMS = ({"os": "darwin", "arch": "arm64"}, {"os": "linux", "arch": "amd64"}, {"os": "linux", "arch": "arm64"})


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def archive_binary_digest(archive):
    with tarfile.open(archive, "r:gz") as package:
        member = package.getmember("mesh")
        if not member.isfile():
            raise ValueError("prepared archive does not contain a regular candidate")
        with package.extractfile(member) as stream:
            return hashlib.file_digest(stream, "sha256").hexdigest()


def record(tools, baseline_list, supplied_archive=None):
    binary = tools / "candidate"
    build = json.loads(subprocess.check_output([str(binary), "version", "--json"], timeout=10))
    if build["modified"] or build["digest"] != digest(binary) or build["platform"] not in PLATFORMS:
        raise ValueError("rollback artifact must be the actual clean native candidate")
    name = "mesh_" + build["platform"]["os"] + "_" + build["platform"]["arch"] + ".tar.gz"
    archive = supplied_archive or tools / name
    if supplied_archive is None:
        with tarfile.open(archive, "w:gz") as package:
            package.add(binary, arcname="mesh")
    if archive_binary_digest(archive) != build["digest"]:
        raise ValueError("prepared archive differs from the native candidate")
    sources = []
    for line in baseline_list.read_text().splitlines():
        source = Path(line)
        if not source.is_absolute():
            source = baseline_list.parent / source
        report = json.loads(subprocess.check_output([str(source), "version", "--json"], timeout=10))
        if report["platform"] != build["platform"] or report["digest"] != digest(source):
            raise ValueError("rollback source report differs from its actual native image")
        sources.append(report)
    if not sources:
        raise ValueError("rollback context requires actual prepared source reports")
    value = {"schema": 1, "build": build, "artifact": {"platform": build["platform"], "archive": name,
             "sha256": digest(archive), "binarySha256": build["digest"]}, "sources": sources}
    (tools / "rollback-artifact.json").write_text(json.dumps(value, sort_keys=True) + "\n")
    return value


def assemble(directory, output):
    records = [json.loads(path.read_bytes()) for path in sorted(directory.glob("*/rollback-artifact.json"))]
    if len(records) != 3:
        raise ValueError("rollback context requires all three actual native artifact records")
    first = records[0]["build"]
    platforms = []
    for item in records:
        build = item["build"]
        if item["schema"] != 1 or build["modified"] or item["artifact"]["binarySha256"] != build["digest"]:
            raise ValueError("invalid actual native artifact binding")
        if any(build[field] != first[field] for field in ("version", "commit", "stateVersion", "workerProtocol")):
            raise ValueError("native candidates disagree on rollback context identity")
        if item["artifact"]["platform"] != build["platform"] or build["platform"] in platforms:
            raise ValueError("duplicate or mismatched native rollback platform")
        platforms.append(build["platform"])
    if any(platform not in platforms for platform in PLATFORMS):
        raise ValueError("rollback context lacks a supported native platform")
    state_min = min(source["stateVersion"] for item in records for source in item["sources"])
    worker_min = min(source["workerProtocol"] for item in records for source in item["sources"])
    if state_min <= 0 or state_min > first["stateVersion"] or worker_min <= 0 or worker_min > first["workerProtocol"]:
        raise ValueError("actual source reports exceed the candidate rollback context range")
    value = {"schema": 1, "version": first["version"], "commit": first["commit"],
             "artifacts": [item["artifact"] for item in records], "compatibility": {
                 "stateReadMin": state_min,
                 "stateReadMax": first["stateVersion"], "stateWrite": first["stateVersion"],
                 "workerMin": worker_min,
                 "workerMax": first["workerProtocol"], "workerWrite": first["workerProtocol"],
                 "journalVersion": 1, "transitions": []}}
    output.write_text(json.dumps(value, sort_keys=True) + "\n")
    return value


if __name__ == "__main__":
    if sys.argv[1] == "record":
        record(Path(sys.argv[2]), Path(sys.argv[3]), Path(sys.argv[4]) if len(sys.argv) == 5 else None)
    elif sys.argv[1] == "assemble":
        assemble(Path(sys.argv[2]), Path(sys.argv[3]))
    else:
        raise SystemExit("expected record or assemble")
