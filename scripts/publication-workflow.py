#!/usr/bin/env python3
"""Hosted publication matrix, bounded child evidence intake and owned cleanup."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

from fixtures import installed_artifacts, installed_storage

ROOT = Path(__file__).resolve().parent.parent
RUNNERS = {"linux/amd64": "ubuntu-24.04", "linux/arm64": "ubuntu-24.04-arm", "darwin/arm64": "macos-15"}
MAX_ARCHIVE = 256 << 20
MAX_UNPACKED = 1 << 30
MAX_FILES = 32768


def draft(version, source, destination):
    if (os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
            or os.environ.get("GITHUB_REF") != "refs/heads/main"):
        raise ValueError("publication scheduling requires the main-only hosted caller")
    if not re.fullmatch(r"v\d+\.\d+\.\d+", version) or not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("publication scheduling requires an exact version and source")
    result = subprocess.run(["gh", "release", "view", version, "--repo", "ShaulLavo/mesh", "--json", "isDraft,tagName,assets"],
                            capture_output=True, text=True, check=True, timeout=30)
    release = json.loads(result.stdout)
    if release["isDraft"] is not True or release["tagName"] != version:
        raise ValueError("publication scheduling requires the actual reserved draft")
    assets = [item for item in release["assets"] if item["name"].endswith(".json")]
    names = {item["name"] for item in assets}
    size = sum(item["size"] for item in assets)
    if (not assets or len(assets) > MAX_FILES or len(names) != len(assets) or size > MAX_ARCHIVE
            or any(Path(item["name"]).name != item["name"] or type(item["size"]) is not int or item["size"] <= 0 for item in assets)):
        raise ValueError("draft JSON inventory is nonlocal, repeated or exceeds its acquisition bound")
    destination.mkdir(exist_ok=False)
    if shutil.disk_usage(destination).free < size + (4 << 30):
        raise RuntimeError("publication scheduling lacks space for draft JSON and aggregation")
    for item in assets:
        download_draft_asset(item, destination / item["name"])
    if {path.name for path in destination.iterdir()} != names or any((destination / item["name"]).stat().st_size != item["size"] for item in assets):
        raise ValueError("draft JSON acquisition differs from its complete asset inventory")
    manifest = json.loads((destination / "mesh-release.json").read_bytes())
    if manifest["version"] != version or manifest["commit"] != source:
        raise ValueError("draft inventory differs from its exact release source and version")


def download_draft_asset(item, destination):
    if type(item.get("id")) is not int or item["id"] <= 0:
        raise ValueError("draft asset lacks its exact positive ID")
    request = urllib.request.Request(f'https://api.github.com/repos/ShaulLavo/mesh/releases/assets/{item["id"]}',
        headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"], "Accept": "application/octet-stream"})
    try:
        response = urllib.request.build_opener(installed_storage.NoRedirect).open(request, timeout=30)
    except urllib.error.HTTPError as error:
        location = error.headers.get("Location", "")
        error.close()
        if error.code != 302 or urllib.parse.urlsplit(location).scheme != "https":
            raise RuntimeError("draft asset API did not supply an HTTPS download") from None
        # The redirected asset request carries no GitHub authorization header.
        response = urllib.request.urlopen(location, timeout=30)
    with response, destination.open("xb") as target:
        size, _digest = installed_artifacts.copy_archive(response, target, item["size"])
    if size != item["size"]:
        raise ValueError("draft JSON acquisition differs from its pinned size")


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
    artifacts = installed_artifacts.pages(f"repos/{repository}/actions/runs/{run}/artifacts", "artifacts")
    selected = [item for item in artifacts if item["name"].startswith(prefix)]
    if (len(selected) != len(expected) or {item["name"] for item in selected} != {row["artifact"] for row in expected}
            or any(item["expired"] for item in selected)):
        raise ValueError("publication children require the exact complete same-run artifact set")
    pinned = {item["name"]: artifact_pin(item) for item in selected}
    destination.mkdir(exist_ok=False)
    (destination / "artifact-pins.json").write_text(json.dumps({"run": run, "artifacts": pinned}, sort_keys=True) + "\n")
    for row in expected:
        fetch_child(repository, pinned[row["artifact"]], destination / row["artifact"])


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
    names = ("native-worker-input.noindex", "native-tools.noindex", "publication-trust")
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
        draft(args.version, args.source, args.destination)
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
