#!/usr/bin/env python3
import json
from pathlib import Path
import shutil
import sys


def stage(dist, documents, output):
    metadata = json.loads((dist / "mesh-update.json").read_bytes())
    ref = metadata["page"]
    source = dist / ref["name"] if ref["tag"] == metadata["version"] else documents / ref["tag"] / ref["name"]
    shutil.copyfile(source, output)


if __name__ == "__main__":
    stage(*(Path(argument) for argument in sys.argv[1:]))
