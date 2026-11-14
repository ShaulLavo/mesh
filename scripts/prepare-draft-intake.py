#!/usr/bin/env python3
"""Carry exact reserved draft archives from the release writer to read-only proofs."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

REPOSITORY = "repos/ShaulLavo/mesh"
ARCHIVES = {"linux/amd64": "mesh_linux_amd64.tar.gz", "linux/arm64": "mesh_linux_arm64.tar.gz",
            "darwin/arm64": "mesh_darwin_arm64.tar.gz"}
NAMES = tuple(ARCHIVES.values()) + ("checksums.txt",)


def inventory(release, version, source):
    if not re.fullmatch(r"v\d+\.\d+\.\d+", version) or not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("draft intake requires an exact version and source")
    if (release.get("draft") is not True or release.get("tag_name") != version
            or release.get("target_commitish") != source or type(release.get("id")) is not int or release["id"] <= 0):
        raise ValueError("draft intake differs from the reserved source or version")
    selected = [item for item in release["assets"] if item["name"] in NAMES]
    if len(selected) != len(NAMES) or {item["name"] for item in selected} != set(NAMES):
        raise ValueError("draft intake requires every unique candidate archive and checksum asset")
    for item in selected:
        if (type(item.get("id")) is not int or item["id"] <= 0 or type(item.get("size")) is not int
                or not 0 < item["size"] <= 128 << 20 or item.get("state") != "uploaded"
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", item.get("digest") or "")):
            raise ValueError("draft intake asset lacks its uploaded ID, size or SHA256")
    if len({item["id"] for item in selected}) != len(selected):
        raise ValueError("draft intake repeats an asset ID")
    return selected


def verify(directory, version, source):
    release = json.loads((directory / "draft.json").read_bytes())
    assets = inventory(release, version, source)
    for item in assets:
        path = directory / item["name"]
        with path.open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if path.stat().st_size != item["size"] or "sha256:" + digest != item["digest"]:
            raise ValueError("draft intake asset differs from its exact uploaded bytes")
    checksums = {}
    for line in (directory / "checksums.txt").read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (\S+)", line)
        if not match or match[2] in checksums:
            raise ValueError("draft intake has invalid or repeated checksums")
        checksums[match[2]] = match[1]
    for item in assets:
        if item["name"] in ARCHIVES.values() and checksums.get(item["name"]) != item["digest"].removeprefix("sha256:"):
            raise ValueError("draft intake archive differs from its release checksum")
    return release


def capture(directory, version, source):
    # The tag endpoint is for published releases; listing drafts needs push access.
    pages = json.loads(subprocess.check_output(
        ["gh", "api", "--paginate", "--slurp", REPOSITORY + "/releases?per_page=100"], timeout=120))
    matches = [item for page in pages for item in page if item["tag_name"] == version]
    if len(matches) != 1:
        raise ValueError("reserved draft is missing or unavailable to the release writer")
    release = matches[0]
    assets = inventory(release, version, source)
    directory.mkdir(exist_ok=False)
    for item in assets:
        with (directory / item["name"]).open("xb") as output:
            subprocess.run(["gh", "api", REPOSITORY + f'/releases/assets/{item["id"]}',
                            "-H", "Accept: application/octet-stream"], stdout=output, check=True, timeout=120)
    (directory / "draft.json").write_text(json.dumps(release, sort_keys=True) + "\n")
    return verify(directory, version, source)


def native(directory, version, source, platform, report):
    verify(directory, version, source)
    value = json.loads(report.read_bytes())
    build = value["build"]
    expected = dict(zip(("os", "arch"), platform.split("/"), strict=True))
    if (platform not in ARCHIVES or build.get("commit") != source or build.get("version") != version
            or build.get("platform") != expected or build.get("modified") is not False):
        raise ValueError("native candidate differs from the reserved source, version or platform")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("capture", "verify", "native"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("--platform", choices=ARCHIVES)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    if args.operation == "native":
        if args.platform is None or args.report is None:
            parser.error("native requires --platform and --report")
        native(args.directory, args.version, args.source, args.platform, args.report)
        return
    operation = capture if args.operation == "capture" else verify
    operation(args.directory, args.version, args.source)


if __name__ == "__main__":
    main()
