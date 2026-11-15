#!/usr/bin/env python3
"""Pack the explicit validated logical closure and declare fixed physical uploads."""
import argparse
import json
from pathlib import Path
import subprocess

from fixtures import draft_intake, release_documents, release_tool


def prepare(candidate, version, source, dist, proofs, history_root, history, output, run):
    release = draft_intake.verify(candidate, version, source, run=run)
    output.mkdir(exist_ok=False)
    logical = output / "logical"
    members = release_documents.stage(dist, proofs, logical)
    manifest = json.loads((logical / "mesh-release.json").read_bytes())
    aliases = release_documents.legacy_projection(manifest, logical, history_root, json.loads(history.read_bytes()))
    member_path, binding_path = output / "members.json", output / "binding.json"
    member_path.write_text(json.dumps(members) + "\n")
    binding_path.write_text(json.dumps(release_tool.binding(release)) + "\n")
    tool = release_tool.configured()
    result = json.loads(subprocess.check_output([str(tool), "pack-documents", str(logical), str(member_path),
        str(binding_path), str(dist / release_documents.BUNDLE)], timeout=180))
    if result.get("binding") != release_tool.binding(release):
        raise ValueError("packed bundle differs from the exact reserved draft identity")
    (output / "pack-inventory.json").write_text(json.dumps(result, sort_keys=True) + "\n")
    paths = [str(dist / name) for name in release_documents.PHYSICAL] + [str(proofs / name) for name in aliases]
    (output / "uploads.json").write_text(json.dumps(paths) + "\n")
    (output / "legacy.json").write_text(json.dumps(aliases) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", type=Path)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("dist", type=Path)
    parser.add_argument("proofs", type=Path)
    parser.add_argument("history_root", type=Path)
    parser.add_argument("history", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("run", type=int)
    args = parser.parse_args()
    prepare(args.candidate, args.version, args.source, args.dist, args.proofs, args.history_root, args.history, args.output, args.run)


if __name__ == "__main__":
    main()
