#!/usr/bin/env python3
"""Recheck the real signed native closure and post-sign installed completion."""
import json
import os
from pathlib import Path
import re
import sys
import tempfile

from fixtures import draft_intake, installed_publication, release_documents, release_worker_completion, release_worker_index


def verify(version, source, completions, snapshot, run):
    parent = Path(os.environ.get("RUNNER_TEMP", "/tmp"))
    with tempfile.TemporaryDirectory(prefix="worker-finalize-", dir=parent) as temporary:
        root = Path(temporary) / "logical"
        snapshot = Path(snapshot)
        draft_intake.documents(snapshot, version, source, root, int(run))
        manifest = json.loads((root / "mesh-release.json").read_bytes())
        if manifest["version"] != version or manifest["commit"] != source:
            raise ValueError("worker finalization has another reserved manifest context")
        subjects = [root / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh-workers.json", "mesh-worker-groups.json")]
        subjects.extend(snapshot / item["archive"] for item in manifest["artifacts"])
        subjects.append(snapshot / release_documents.BUNDLE)
        installed_publication.verify_signature(root, root, root / "mesh-update.sig.json", subjects)
        index = release_worker_index.validate_index(root / "mesh-worker-groups.json", manifest)
        workflow = Path(".github/workflows/release.yml").read_text()
        pins = re.findall(r"uses: ShaulLavo/mesh/.github/workflows/installed-catchup.yml@([0-9a-f]{40})", workflow)
        if len(pins) != 1:
            raise ValueError("finalization cannot identify its immutable installed harness")
        release_worker_completion.validate(Path(completions), release_worker_completion.release_context(root), pins[0], index["selection"])
        draft_intake.compare_live(snapshot, version, source, int(run))


if __name__ == "__main__":
    verify(*sys.argv[1:])
