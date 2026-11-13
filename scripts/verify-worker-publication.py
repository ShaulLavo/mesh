#!/usr/bin/env python3
"""Recheck the real signed native closure and post-sign installed completion."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

from fixtures import installed_publication, release_worker_completion, release_worker_index


def verify(version, source, completions):
    parent = Path(os.environ.get("RUNNER_TEMP", "/tmp"))
    with tempfile.TemporaryDirectory(prefix="worker-finalize-", dir=parent) as temporary:
        root = Path(temporary)
        subprocess.run(["gh", "release", "download", version, "--repo", "ShaulLavo/mesh", "--pattern", "*.json", "--dir", str(root)], check=True, timeout=90)
        manifest = json.loads((root / "mesh-release.json").read_bytes())
        if manifest["version"] != version or manifest["commit"] != source:
            raise ValueError("worker finalization has another reserved manifest context")
        subjects = [root / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh-workers.json", "mesh-worker-groups.json")]
        installed_publication.verify_signature(root, root, root / "mesh-update.sig.json", subjects)
        index = release_worker_index.validate_index(root / "mesh-worker-groups.json", manifest)
        workflow = Path(".github/workflows/release.yml").read_text()
        pins = re.findall(r"uses: ShaulLavo/mesh/.github/workflows/installed-catchup.yml@([0-9a-f]{40})", workflow)
        if len(pins) != 1:
            raise ValueError("finalization cannot identify its immutable installed harness")
        release_worker_completion.validate(Path(completions), release_worker_completion.release_context(root), pins[0], index["selection"])


if __name__ == "__main__":
    verify(*sys.argv[1:])
