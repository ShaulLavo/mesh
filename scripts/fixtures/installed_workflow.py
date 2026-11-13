"""Small hosted prepared-proof selection, artifact intake and cleanup controls."""
import argparse
import json
import os
from pathlib import Path
import shutil
import tarfile

from fixtures import installed_artifacts, installed_budgets, installed_clock, installed_storage

PROFILES = ("none", "first", "final")
MODES = ("chain", "waypoint")


def physical_key(platform, mode, role, profile=None):
    key = installed_budgets.PLATFORMS[platform][0] + "-" + mode + "-" + role
    return key + "-" + profile if profile else key


def physical_plans(index, diagnostic):
    values, paths = {}, {}
    for platform in installed_budgets.PLATFORMS:
        for mode in MODES:
            add_physical_plans(values, paths, index, diagnostic, platform, mode)
    return {"minutes": values, "plans": paths}


def add_physical_plans(values, paths, index, diagnostic, platform, mode):
    host = installed_budgets.PLATFORMS[platform][1]
    for role, profile in (("prepare", None), ("consume", "none"), ("consume", "first"), ("consume", "final")):
        key = physical_key(platform, mode, role, profile)
        if diagnostic:
            values[key], paths[key] = 30, ""
            continue
        context = {"platform": platform, "host": host, "mode": mode, "preparedRole": role,
                   "grouped": True, "scenario": None, "catalogEvidence": False}
        path, plan = installed_budgets.select_plan(index, key, context)
        validate_profile(plan.workload, role, profile)
        values[key], paths[key] = installed_budgets.workflow_minutes(plan), str(path.relative_to(installed_budgets.ROOT))


def validate_profile(workload, role, profile):
    if role == "prepare":
        return
    selected = workload.get("installedSlice", {}).get("select", [])
    if len(selected) != 1 or selected[0].get("profile") != "chain-" + profile:
        raise ValueError("reviewed prepared plan selects another profile")


def unsplit_plans(args):
    if args.diagnostic or args.publication:
        return {}
    profiles = installed_budgets.required_profiles(False, True, args.adoption_scenario, args.catalog)
    if args.cohort:
        profiles += [installed_budgets.selection(platform, "oldest", catalog=args.catalog and platform.startswith("linux/"))
                     for platform in installed_budgets.PLATFORMS]
        profiles.append(installed_budgets.selection("linux/amd64", "order"))
    return {key: installed_budgets.workflow_minutes(installed_budgets.select_plan(args.index, key, context)[1])
            for key, context in profiles}


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
    matches = [item for item in artifacts if item["name"] == name and not item["expired"]]
    if len(matches) != 1:
        raise ValueError("prepared intake requires exactly one live same-run artifact: " + name)
    item = matches[0]
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


def write_plans_output(value):
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        for key, data in value.items():
            output.write(key + "=" + json.dumps(data, separators=(",", ":")) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    plans = commands.add_parser("plans")
    plans.add_argument("--index", type=Path, default=installed_budgets.INDEX)
    plans.add_argument("--diagnostic", action="store_true")
    unsplit = commands.add_parser("unsplit")
    unsplit.add_argument("--index", type=Path, default=installed_budgets.INDEX)
    unsplit.add_argument("--diagnostic", action="store_true")
    unsplit.add_argument("--publication", action="store_true")
    unsplit.add_argument("--cohort", action="store_true")
    unsplit.add_argument("--catalog", action="store_true")
    unsplit.add_argument("--adoption-scenario", choices=installed_budgets.SCENARIOS)
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
    elif args.command == "plans":
        write_plans_output(physical_plans(args.index, args.diagnostic))
    elif args.command == "unsplit":
        print(json.dumps(unsplit_plans(args), separators=(",", ":")))
    elif args.command == "fetch":
        fetch(args.repository, args.run, args.name, args.destination, args.output)
    elif args.command == "pack":
        pack(args.bundle, args.transfer)
    elif args.command == "cleanup":
        cleanup(args.root, args.bundle)


if __name__ == "__main__":
    main()
