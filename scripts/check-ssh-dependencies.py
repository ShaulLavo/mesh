#!/usr/bin/env python3
"""Compare the local SSH dependencies with pinned upstream archives plus patches."""

import json
from pathlib import Path
import subprocess
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parent.parent
THIRD_PARTY = ROOT / "third_party"


def go_metadata(*arguments):
    result = subprocess.run(
        ["go", *arguments], cwd=ROOT, check=True, capture_output=True, text=True
    )
    return json.loads(result.stdout)


def files(directory):
    result = {}
    for path in directory.rglob("*"):
        if path.is_symlink():
            raise ValueError(f"unexpected symlink: {path}")
        if path.is_file():
            result[path.relative_to(directory)] = path.read_bytes()
    return result


def check(dependency):
    local = THIRD_PARTY / dependency["directory"]
    installed = go_metadata("list", "-m", "-json", dependency["module"])
    if installed["Version"] != dependency["version"] or Path(installed["Dir"]).resolve() != local:
        raise ValueError(f"go.mod does not use the pinned local {dependency['module']}")
    source = go_metadata("mod", "download", "-json", f"{dependency['module']}@{dependency['version']}")
    if source["Sum"] != dependency["sum"]:
        raise ValueError(f"upstream checksum changed for {dependency['module']}")
    with tempfile.TemporaryDirectory(prefix="mesh-ssh-deps-") as temporary:
        with zipfile.ZipFile(source["Zip"]) as archive:
            archive.extractall(temporary)
        expected = Path(temporary) / f"{dependency['module']}@{dependency['version']}"
        subprocess.run(
            ["patch", "--batch", "--forward", "-s", "-p1", "-i", str(THIRD_PARTY / dependency["patch"])],
            cwd=expected, check=True,
        )
        expected_files, local_files = files(expected), files(local)
    differences = sorted(
        str(name) for name in expected_files.keys() | local_files.keys()
        if expected_files.get(name) != local_files.get(name)
    )
    if differences:
        raise ValueError(f"unrecorded changes in {local}: {', '.join(differences)}")
    print(f"PASS: {dependency['module']} {dependency['version']} matches upstream plus patch")


if __name__ == "__main__":
    for dependency in json.loads((THIRD_PARTY / "ssh-dependencies.json").read_text()):
        check(dependency)
