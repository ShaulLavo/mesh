#!/usr/bin/env python3
"""Hosted publication matrix, bounded child evidence intake and owned cleanup."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import zipfile

from fixtures import draft_intake, installed_artifacts, installed_storage

ROOT = Path(__file__).resolve().parent.parent
RUNNERS = {"linux/amd64": "ubuntu-24.04", "linux/arm64": "ubuntu-24.04-arm", "darwin/arm64": "macos-15"}
MAX_ARCHIVE = 256 << 20
MAX_UNPACKED = 1 << 30
MAX_FILES = 32768


def draft(version, source, destination, intake, run):
    if (os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
            or os.environ.get("GITHUB_REF") != "refs/heads/main"):
        raise ValueError("publication scheduling requires the main-only hosted caller")
    draft_intake.documents(intake, version, source, destination, run)


def inventory(directory, platform, harness):
    result = subprocess.run(["python3", str(ROOT / "scripts/publication-jobs.py"), "inventory", str(directory), platform, harness],
                            capture_output=True, text=True, check=True, timeout=30)
    return json.loads(result.stdout)


def rows(directory, platform, harness):
    tasks = inventory(directory, platform, harness)["tasks"]
    key = platform.replace("/", "-")
    return [{"platform": platform, "key": key, "runner": RUNNERS[platform], "task": task["id"],
             "artifact": "publication-task-" + key + "-" + hashlib.sha256(task["id"].encode()).hexdigest()}
            for task in tasks]


def matrix(directory, harness):
    include = [row for platform in RUNNERS for row in rows(directory, platform, harness)]
    if not include or len(include) > 256:
        raise ValueError("complete publication inventory exceeds the hosted matrix limit")
    return {"include": include}


def artifact_pin(item):
    artifact = {"id": item["id"], "name": item["name"], "size": item["size_in_bytes"], "digest": item.get("digest")}
    installed_artifacts.require_archive_pin(artifact)
    if artifact["size"] > MAX_ARCHIVE:
        raise ValueError("publication child archive exceeds its acquisition bound")
    return artifact


def checked_members(package):
    members, names, size = package.infolist(), set(), 0
    if not members or len(members) > MAX_FILES:
        raise ValueError("publication child archive exceeds its file bound")
    for member in members:
        name = Path(member.filename)
        kind = stat.S_IFMT(member.external_attr >> 16)
        if (name.is_absolute() or not name.parts or ".." in name.parts or "\\" in member.filename
                or member.filename in names or kind not in (0, stat.S_IFREG, stat.S_IFDIR)
                or member.is_dir() != (kind == stat.S_IFDIR) and kind != 0):
            raise ValueError("publication child archive contains a nonlocal, repeated or nonregular member")
        names.add(member.filename)
        size += member.file_size
        if size > MAX_UNPACKED:
            raise ValueError("publication child archive exceeds its extraction bound")
    return members, size


def extract(archive, destination):
    with zipfile.ZipFile(archive) as package:
        members, size = checked_members(package)
        if shutil.disk_usage(destination.parent).free < size + (4 << 30):
            raise RuntimeError("publication child archive lacks measured extraction space")
        destination.mkdir(exist_ok=False)
        for member in members:
            extract_member(package, member, destination)


def extract_member(package, member, destination):
    target = destination / member.filename
    if member.is_dir():
        target.mkdir(parents=True, exist_ok=True)
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    with package.open(member) as source, target.open("xb") as output:
        shutil.copyfileobj(source, output, length=1 << 20)


def fetch(repository, run, directory, platform, harness, destination):
    if run <= 0:
        raise ValueError("publication children require the current positive run ID")
    expected = rows(directory, platform, harness)
    prefix = "publication-task-" + platform.replace("/", "-") + "-"
    # The whole-run listing only proves the exact set of child names; each child's
    # current upload comes from its own name listing, which a shifting page cannot skip.
    listed = installed_artifacts.pages(f"repos/{repository}/actions/runs/{run}/artifacts", "artifacts")
    names = {item["name"] for item in listed if item["name"].startswith(prefix) and not item["expired"]}
    if names != {row["artifact"] for row in expected}:
        raise ValueError("publication children require the exact complete same-run artifact set")
    pinned = {name: artifact_pin(current_child(repository, run, name)) for name in sorted(names)}
    destination.mkdir(exist_ok=False)
    (destination / "artifact-pins.json").write_text(json.dumps({"run": run, "artifacts": pinned}, sort_keys=True) + "\n")
    for row in expected:
        fetch_child(repository, pinned[row["artifact"]], destination / row["artifact"])


def current_child(repository, run, name):
    try:
        item = installed_artifacts.current_artifacts(installed_artifacts.named_artifacts(repository, run, name), run).get(name)
    except RuntimeError as error:
        raise ValueError("publication children require the exact complete same-run artifact set") from error
    if item is None:
        raise ValueError("publication children require the exact complete same-run artifact set")
    return item


def fetch_child(repository, artifact, destination):
    if shutil.disk_usage(destination.parent).free < artifact["size"] + MAX_UNPACKED + (4 << 30):
        raise RuntimeError("publication child acquisition lacks bounded archive and extraction space")
    with tempfile.TemporaryDirectory(prefix="publication-child-", dir=destination.parent) as temporary:
        archive = Path(temporary) / "artifact.zip"
        installed_artifacts.fetch_archive(installed_storage.archive_url(repository, artifact["id"]), archive, artifact)
        extract(archive, destination)


def cleanup(root):
    control, proof = root / "publication-control", root / "publication-proof"
    control.mkdir(exist_ok=True)
    launched = (control / "fixture-launch.json").exists()
    scratch = proof / "scratch-cleanup.json"
    removed = json.loads(scratch.read_bytes()) if scratch.exists() else {}
    absent = removed.get("absent") is True and isinstance(removed.get("root"), str) and not Path(removed["root"]).exists()
    names = ("native-worker-input.noindex", "native-tools.noindex", "publication-tools", "publication-trust", "publication-draft-input")
    allowed = not launched or absent
    decision = {"inputsMayBeRemoved": allowed, "retained": [] if allowed else [str(root / name) for name in names]}
    (control / "tools-cleanup-decision.json").write_text(json.dumps(decision) + "\n")
    if not allowed:
        raise RuntimeError("publication ownership unconfirmed; retain native inputs and helper")
    for name in names:
        path = root / name
        if path.exists():
            shutil.rmtree(path)
        if path.exists():
            raise RuntimeError("publication owned input remains after cleanup")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    acquire = commands.add_parser("draft")
    acquire.add_argument("version")
    acquire.add_argument("source")
    acquire.add_argument("destination", type=Path)
    acquire.add_argument("--intake", type=Path, required=True)
    acquire.add_argument("--run", type=int, required=True)
    plans = commands.add_parser("matrix")
    plans.add_argument("directory", type=Path)
    plans.add_argument("harness")
    intake = commands.add_parser("fetch")
    intake.add_argument("repository")
    intake.add_argument("run", type=int)
    intake.add_argument("directory", type=Path)
    intake.add_argument("platform", choices=tuple(RUNNERS))
    intake.add_argument("harness")
    intake.add_argument("destination", type=Path)
    remove = commands.add_parser("cleanup")
    remove.add_argument("root", type=Path)
    args = parser.parse_args()
    if args.command == "draft":
        draft(args.version, args.source, args.destination, args.intake, args.run)
        return
    if args.command == "matrix":
        print(json.dumps(matrix(args.directory, args.harness), separators=(",", ":")))
        return
    if args.command == "fetch":
        fetch(args.repository, args.run, args.directory, args.platform, args.harness, args.destination)
        return
    cleanup(args.root)


if __name__ == "__main__":
    main()
