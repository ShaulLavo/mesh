"""Exact draft snapshots captured by the release writer for read-only consumers."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

from fixtures import paged_listing, release_documents, release_tool, release_worker_index

REPOSITORY = "repos/ShaulLavo/mesh"
ARCHIVES = {"linux/amd64": "mesh_linux_amd64.tar.gz", "linux/arm64": "mesh_linux_arm64.tar.gz",
            "darwin/arm64": "mesh_darwin_arm64.tar.gz"}
NAMES = tuple(ARCHIVES.values()) + ("checksums.txt",)
SIGNED = ("mesh-release.json", "mesh-update.json", "mesh-update-root.json", "mesh-update.sig.json",
          "mesh-retention.json", "mesh-workers.json", "mesh-worker-groups.json")
MAX_TOTAL = 512 << 20
BASELINE = re.compile(r"baseline_(linux_amd64|linux_arm64|darwin_arm64)_([0-9a-f]{64})\.bin")


def baseline_names(frozen):
    names = frozen.split(",") if frozen else []
    if len(set(names)) != len(names) or any(not BASELINE.fullmatch(name) for name in names):
        raise ValueError("draft intake requires the exact frozen custom baseline inventory")
    return sorted(names)


def identity(release, version, source):
    if not re.fullmatch(r"v\d+\.\d+\.\d+", version) or not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("draft intake requires an exact version and source")
    if (release.get("draft") is not True or release.get("tag_name") != version
            or release.get("target_commitish") != source or type(release.get("id")) is not int or release["id"] <= 0):
        raise ValueError("draft intake differs from the reserved source or version")
    if (release.get("repository") != "ShaulLavo/mesh" or type(release.get("intake_run")) is not int or release["intake_run"] <= 0
            or release.get("url") != f'https://api.github.com/{REPOSITORY}/releases/{release["id"]}'
            or not release.get("html_url", "").startswith("https://github.com/ShaulLavo/mesh/releases/")):
        raise ValueError("draft intake differs from its repository, release ID or writer run")


def inventory(release, version, source, signed=False):
    identity(release, version, source)
    all_names = [item["name"] for item in release["assets"]]
    if any(Path(name).name != name or "\\" in name or name == "draft.json" for name in all_names):
        raise ValueError("draft intake asset lacks its local name")
    if len(all_names) != len(set(all_names)):
        raise ValueError("draft intake requires every unique physical asset name")
    frozen = release["frozen_baselines"]
    if not isinstance(frozen, list) or baseline_names(",".join(frozen)) != frozen:
        raise ValueError("draft intake has invalid frozen custom baseline inventory")
    if sorted(item["name"] for item in release["assets"] if BASELINE.fullmatch(item["name"])) != frozen:
        raise ValueError("draft intake differs from the frozen custom baseline inventory")
    required = set(NAMES) | set(frozen)
    if signed:
        required.update(release_documents.PHYSICAL + ("mesh.rb",))
    aliases = [item["name"] for item in release["assets"] if release_documents.RECEIPT.fullmatch(item["name"])] if signed else []
    if signed and len(aliases) > len(release_documents.legacy_versions()) * len(ARCHIVES):
        raise ValueError("draft intake exceeds its supported physical legacy projection")
    selected = [item for item in release["assets"] if item["name"] in required or item["name"] in aliases]
    names = {item["name"] for item in selected}
    if not required <= names or len(selected) != len(names):
        raise ValueError("draft intake requires every unique candidate archive, checksum and required metadata asset")
    for item in selected:
        validate_asset(item)
    if len(selected) > 32768 or sum(item["size"] for item in selected) > MAX_TOTAL:
        raise ValueError("draft intake exceeds its complete acquisition bound")
    if len({item["id"] for item in selected}) != len(selected):
        raise ValueError("draft intake repeats an asset ID")
    return selected


def validate_asset(item):
    if (Path(item["name"]).name != item["name"] or "\\" in item["name"] or item["name"] == "draft.json"
            or type(item.get("id")) is not int or item["id"] <= 0 or type(item.get("size")) is not int
            or not 0 < item["size"] <= 128 << 20 or item.get("state") != "uploaded"
            or not re.fullmatch(r"sha256:[0-9a-f]{64}", item.get("digest") or "")):
        raise ValueError("draft intake asset lacks its local name, uploaded ID, size or SHA256")
    baseline = BASELINE.fullmatch(item["name"])
    if baseline and item["digest"] != "sha256:" + baseline[2]:
        raise ValueError("custom baseline differs from its frozen filename checksum")


def verify(directory, version, source, signed=False, baselines=None, run=None):
    release = json.loads((directory / "draft.json").read_bytes())
    assets = inventory(release, version, source, signed)
    if run is not None and release["intake_run"] != int(run):
        raise ValueError("draft intake differs from its explicit writer run")
    if baselines is not None and release["frozen_baselines"] != baseline_names(baselines):
        raise ValueError("draft intake differs from the requested frozen custom baseline inventory")
    if {path.name for path in directory.iterdir()} != {item["name"] for item in assets} | {"draft.json"}:
        raise ValueError("draft intake differs from its complete asset inventory")
    for item in assets:
        path = directory / item["name"]
        if path.is_symlink() or not path.is_file():
            raise ValueError("draft intake asset is not a regular local file")
        with path.open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if path.stat().st_size != item["size"] or "sha256:" + digest != item["digest"]:
            raise ValueError("draft intake asset differs from its exact uploaded bytes")
    verify_checksums(directory, assets)
    return release


def verify_checksums(directory, assets):
    checksums = {}
    for line in (directory / "checksums.txt").read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (\S+)", line)
        if not match or match[2] in checksums:
            raise ValueError("draft intake has invalid or repeated checksums")
        checksums[match[2]] = match[1]
    for item in assets:
        if item["name"] in ARCHIVES.values() and checksums.get(item["name"]) != item["digest"].removeprefix("sha256:"):
            raise ValueError("draft intake archive differs from its release checksum")


def verify_signed_identity(directory, version, source):
    manifest_bytes = (directory / "mesh-release.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    metadata = json.loads((directory / "mesh-update.json").read_bytes())
    if (manifest["version"] != version or manifest["commit"] != source or metadata["version"] != version
            or metadata["manifest"] != hashlib.sha256(manifest_bytes).hexdigest()):
        raise ValueError("signed draft intake differs from its exact release source, version or manifest")


def same_candidate(candidate, release, version, source):
    original = verify(candidate, version, source)
    before = {item["name"]: (item["id"], item["digest"], item["size"]) for item in inventory(original, version, source)}
    after = {item["name"]: (item["id"], item["digest"], item["size"]) for item in inventory(release, version, source)}
    if original["id"] != release["id"] or original["intake_run"] != release["intake_run"] or before != after:
        raise ValueError("signed draft differs from the actual transition candidate or reserved release ID")


def capture(directory, version, source, signed=False, candidate=None, baselines=""):
    if signed and candidate is None:
        raise ValueError("signed draft capture requires its actual transition candidate")
    release = find_release(version, source, candidate if signed else None)
    release["repository"] = "ShaulLavo/mesh"
    release["intake_run"] = int(os.environ["GITHUB_RUN_ID"])
    release["frozen_baselines"] = baseline_names(baselines)
    identity(release, version, source)
    release["assets"] = paged_listing.gh_listing(REPOSITORY + f'/releases/{release["id"]}/assets')
    assets = inventory(release, version, source, signed)
    if signed:
        same_candidate(candidate, release, version, source)
    release["assets"] = assets
    directory.mkdir(exist_ok=False)
    for item in assets:
        acquire_asset(directory, item, candidate if signed else None)
    (directory / "draft.json").write_text(json.dumps(release, sort_keys=True) + "\n")
    return verify(directory, version, source, signed)


def find_release(version, source, candidate):
    if candidate is not None:
        original = verify(candidate, version, source, run=int(os.environ["GITHUB_RUN_ID"]))
        return json.loads(subprocess.check_output(["gh", "api", REPOSITORY + f'/releases/{original["id"]}'], timeout=120))
    # The tag endpoint is for published releases; finding the initial draft needs push access.
    matches = [item for item in paged_listing.gh_listing(REPOSITORY + "/releases") if item["tag_name"] == version]
    if len(matches) != 1:
        raise ValueError("reserved draft is missing or unavailable to the release writer")
    return matches[0]


def acquire_asset(directory, item, candidate=None):
    cached = candidate / item["name"] if candidate is not None else None
    if cached is not None and cached.is_file():
        shutil.copyfile(cached, directory / item["name"])
        return
    with (directory / item["name"]).open("xb") as output:
        subprocess.run(["gh", "api", REPOSITORY + f'/releases/assets/{item["id"]}',
                        "-H", "Accept: application/octet-stream"], stdout=output, check=True, timeout=120)


def documents(directory, version, source, destination, run):
    release = verify(directory, version, source, signed=True, run=run)
    release_tool.materialize(directory, release, destination, destination.with_name(destination.name + ".logical-inventory.json"))
    verify_signed_identity(destination, version, source)
    manifest = json.loads((destination / "mesh-release.json").read_bytes())
    index = release_worker_index.validate_index(destination / "mesh-worker-groups.json", manifest)
    aliases = release_documents.selection_projection(destination, index)
    physical = {item["name"] for item in release["assets"] if release_documents.RECEIPT.fullmatch(item["name"])}
    if physical != set(aliases):
        raise ValueError("actual draft differs from the complete supported legacy projection")
    return release


def compare_live(directory, version, source, run):
    recorded = verify(directory, version, source, signed=True, run=run)
    endpoint = REPOSITORY + f'/releases/{recorded["id"]}'
    live = json.loads(subprocess.check_output(["gh", "api", endpoint], timeout=120))
    live.update(repository="ShaulLavo/mesh", intake_run=int(run), frozen_baselines=recorded["frozen_baselines"])
    identity(live, version, source)
    if live["id"] != recorded["id"]:
        raise ValueError("live draft has another release ID")
    live["assets"] = paged_listing.gh_listing(endpoint + "/assets")
    keys = ("id", "name", "size", "state", "digest")
    before = sorted(tuple(item[key] for key in keys) for item in recorded["assets"])
    after = sorted(tuple(item[key] for key in keys) for item in live["assets"])
    if before != after:
        raise ValueError("live draft differs from the actual signed snapshot physical inventory")
    return live


def native(directory, version, source, platform, report, run=None):
    verify(directory, version, source, run=run)
    value = json.loads(report.read_bytes())
    build = value["build"]
    expected = dict(zip(("os", "arch"), platform.split("/"), strict=True))
    if (platform not in ARCHIVES or build.get("commit") != source or build.get("version") != version
            or build.get("platform") != expected or build.get("modified") is not False):
        raise ValueError("native candidate differs from the reserved source, version or platform")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("capture", "verify", "native", "documents"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("--signed", action="store_true")
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--baselines")
    parser.add_argument("--run", type=int)
    parser.add_argument("--platform", choices=ARCHIVES)
    parser.add_argument("--report", type=Path)
    parser.add_argument("--destination", type=Path)
    args = parser.parse_args()
    if args.operation == "documents":
        if args.destination is None or args.run is None:
            parser.error("documents requires --destination and --run")
        documents(args.directory, args.version, args.source, args.destination, args.run)
        return
    if args.operation == "native":
        if args.platform is None or args.report is None:
            parser.error("native requires --platform and --report")
        native(args.directory, args.version, args.source, args.platform, args.report, args.run)
        return
    if args.operation == "capture":
        capture(args.directory, args.version, args.source, args.signed, args.candidate, args.baselines or "")
        return
    verify(args.directory, args.version, args.source, args.signed, args.baselines, args.run)
