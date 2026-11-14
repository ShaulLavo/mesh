#!/usr/bin/env python3
"""Package the three already-built native candidates for the frozen reader gate."""
import json
from pathlib import Path
import sys
import tarfile


def prepare(native, policy, dist):
    dist.mkdir(parents=True, exist_ok=True)
    for platform in ("linux-amd64", "linux-arm64", "darwin-arm64"):
        binary = native / ("update-native-" + platform) / "tools" / "candidate"
        binary.chmod(0o755)
        with tarfile.open(dist / ("mesh_" + platform.replace("-", "_") + ".tar.gz"), "w:gz") as archive:
            archive.add(binary, arcname="mesh")
    value = json.loads((policy / "input.json").read_bytes())
    print(value["version"])


if __name__ == "__main__":
    prepare(*(Path(argument) for argument in sys.argv[1:]))
