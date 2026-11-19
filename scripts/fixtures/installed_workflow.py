"""Small hosted prepared-proof selection, artifact intake and cleanup controls."""
import argparse
import json
import os
from pathlib import Path
import shutil
import tarfile

from fixtures import fleet_baselines, installed_artifacts, installed_clock, installed_storage

PROFILES = ("none", "first", "final")
MODES = ("chain", "waypoint")
PLATFORMS = {"linux/amd64": ("linux-amd64", "pc"), "linux/arm64": ("linux-arm64", "pi"),
             "darwin/arm64": ("darwin-arm64", "mac")}
RUNNERS = {"linux/amd64": "ubuntu-24.04", "linux/arm64": "ubuntu-24.04-arm", "darwin/arm64": "macos-15"}


def case_matrix(platform, mode):
    if platform not in RUNNERS or mode not in MODES:
        raise ValueError("prepared workflow needs one supported native platform and mode")
    key, host = PLATFORMS[platform]
    hosts = [row for row in fleet_baselines.load()["hosts"] if row["name"] == host and row["platform"] == platform]
    if len(hosts) != 1:
        raise ValueError("prepared workflow host mapping differs from the actual native inventory")
    return {"include": [{"platform": platform, "key": key, "host": host, "runner": RUNNERS[platform], "mode": mode}]}


def workflow_case(platform, mode):
    return {"case": case_matrix(platform, mode), "profiles": list(PROFILES)}


def selection(bundle, profile):
    from fixtures import installed_prepared, installed_slices
    descriptor, _identity = installed_prepared.read_descriptor(bundle)
    groups = descriptor["groups"]
    indices = [group["index"] for group in groups]
    if indices != list(range(len(groups))):
        raise ValueError("prepared authoritative partition indices are not contiguous")
    value = {"schema": 1, "select": [{"profile": "chain-" + profile, "group_indices": indices}],
             "readers": indices if descriptor["mode"] == "waypoint" and profile == "none" else []}
    return installed_slices.parse_slice(value)


def fetch(repository, run, name, destination, output):
    output.mkdir(parents=True, exist_ok=True)
    artifacts = installed_artifacts.pages(f"repos/{repository}/actions/runs/{run}/artifacts", "artifacts")
    try:
        item = installed_artifacts.current_artifacts(artifacts, int(run)).get(name)
    except RuntimeError as error:
        raise ValueError("prepared intake requires exactly one live same-run artifact: " + name) from error
    if item is None:
        raise ValueError("prepared intake requires exactly one live same-run artifact: " + name)
    artifact = {"id": item["id"], "name": name, "size": item["size_in_bytes"], "digest": item.get("digest")}
    installed_artifacts.require_archive_pin(artifact)
    pin = output / "prepared-pin.json"
    pin.write_text(json.dumps({"run": run, "artifacts": {"prepared": artifact}}) + "\n")
    transfer = destination.parent / "prepared-transfer.noindex"
    installed_storage.preflight(repository, pin, "prepared", transfer, output / "prepared-storage.json")
    installed_artifacts.download(repository, pin, "prepared", transfer, output / "prepared-download.json")
    if {path.name for path in transfer.iterdir()} != {"bundle.tar"}:
        raise ValueError("prepared artifact must contain exactly one bundle.tar payload")
    unpack(transfer / "bundle.tar", destination)


def pack(bundle, transfer):
    from fixtures import installed_prepared
    descriptor, _identity = installed_prepared.read_descriptor(bundle)
    installed_prepared.validate_files(bundle, descriptor)
    names = ["prepared.json"] + list(descriptor["files"])
    required = sum(installed_prepared.local_file(bundle, name).stat().st_size for name in names) + (4 << 30)
    if shutil.disk_usage(transfer.parent).free < required:
        raise RuntimeError("prepared transfer lacks space for its measured tar payload")
    transfer.mkdir(exist_ok=False)
    with tarfile.open(transfer / "bundle.tar", "x:") as archive:
        for name in names:
            archive.add(installed_prepared.local_file(bundle, name), arcname=name, recursive=False)


def unpack(archive_path, destination):
    from fixtures import installed_prepared, noindex_paths
    destination.mkdir(exist_ok=False)
    noindex_paths.require(destination)
    with tarfile.open(archive_path, "r:") as archive:
        members = checked_members(archive)
        required = sum(member.size for member in members) + (4 << 30)
        if shutil.disk_usage(destination).free < required:
            raise RuntimeError("prepared transfer lacks measured extraction space")
        for member in members:
            extract_member(archive, member, destination)
    descriptor, _identity = installed_prepared.read_descriptor(destination)
    if {member.name for member in members} != {"prepared.json"} | set(descriptor["files"]):
        raise ValueError("prepared tar inventory differs from its descriptor")
    installed_prepared.validate_files(destination, descriptor)


def checked_members(archive):
    members, names, size = [], set(), 0
    for member in archive:
        name = Path(member.name)
        if not member.isfile() or name.is_absolute() or not name.parts or ".." in name.parts or member.name in names:
            raise ValueError("prepared tar contains a nonlocal, repeated or nonregular member")
        size += member.size
        names.add(member.name)
        members.append(member)
        if len(members) > 32769 or size > 256 << 30:
            raise ValueError("prepared tar exceeds its file or extraction bound")
    return members


def extract_member(archive, member, destination):
    target = destination / member.name
    target.parent.mkdir(parents=True, exist_ok=True)
    with archive.extractfile(member) as source, target.open("xb") as output:
        shutil.copyfileobj(source, output, length=1 << 20)
    target.chmod(member.mode & 0o777)


def cleanup(root, bundle=False):
    installed_clock.cleanup_inputs(root)
    names = ("native-worker-input.noindex", "native-tools.noindex") + (("prepared.noindex", "prepared-transfer.noindex") if bundle else ())
    for name in names:
        path = root / name
        if path.exists():
            shutil.rmtree(path)
        if path.exists():
            raise RuntimeError("owned payload remains after cleanup: " + str(path))


def write_case_output(value):
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for key, data in value.items():
            output.write(key + "=" + json.dumps(data, separators=(",", ":")) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    case = commands.add_parser("case")
    case.add_argument("--platform", choices=tuple(RUNNERS), required=True)
    case.add_argument("--mode", choices=MODES, required=True)
    select = commands.add_parser("select")
    select.add_argument("bundle", type=Path)
    select.add_argument("profile", choices=PROFILES)
    select.add_argument("output", type=Path)
    package = commands.add_parser("pack")
    package.add_argument("bundle", type=Path)
    package.add_argument("transfer", type=Path)
    intake = commands.add_parser("fetch")
    intake.add_argument("repository")
    intake.add_argument("run", type=int)
    intake.add_argument("name")
    intake.add_argument("destination", type=Path)
    intake.add_argument("output", type=Path)
    remove = commands.add_parser("cleanup")
    remove.add_argument("root", type=Path)
    remove.add_argument("--bundle", action="store_true")
    args = parser.parse_args()
    if args.command == "select":
        args.output.write_text(json.dumps(selection(args.bundle, args.profile)) + "\n")
    elif args.command == "case":
        write_case_output(workflow_case(args.platform, args.mode))
    elif args.command == "fetch":
        fetch(args.repository, args.run, args.name, args.destination, args.output)
    elif args.command == "pack":
        pack(args.bundle, args.transfer)
    elif args.command == "cleanup":
        cleanup(args.root, args.bundle)


if __name__ == "__main__":
    main()
