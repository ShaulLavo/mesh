#!/usr/bin/env python3
"""Recheck the real signed native closure and post-sign installed completion."""
import argparse
import json
import os
from pathlib import Path
import re
import tempfile

from fixtures import draft_intake, installed_publication, release_documents, release_worker_completion, release_worker_index
from fixtures import release_publication_platform


def verify(version, source, completions, snapshot, run, *, platform_plan=None, plan_sha256=None, harness=None, attempts=None):
    parent = Path(os.environ.get("RUNNER_TEMP", "/tmp"))
    with tempfile.TemporaryDirectory(prefix="worker-finalize-", dir=parent) as temporary:
        root = Path(temporary) / "logical"
        snapshot = Path(snapshot)
        recorded = draft_intake.documents(snapshot, version, source, root, int(run))
        manifest = json.loads((root / "mesh-release.json").read_bytes())
        if manifest["version"] != version or manifest["commit"] != source:
            raise ValueError("worker finalization has another reserved manifest context")
        subjects = [root / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh-workers.json", "mesh-worker-groups.json")]
        subjects.extend(snapshot / item["archive"] for item in manifest["artifacts"])
        subjects.append(snapshot / release_documents.BUNDLE)
        installed_publication.verify_signature(root, root, root / "mesh-update.sig.json", subjects)
        index = release_worker_index.validate_index(root / "mesh-worker-groups.json", manifest)
        context = release_worker_completion.release_context(root)
        if platform_plan is not None:
            plan = release_publication_platform.load_plan(platform_plan, plan_sha256)
            release_publication_platform.validate_platforms(Path(completions), plan, plan_sha256, attempts,
                                                            root, recorded, harness, context, index)
        else:
            validate_legacy(completions, context, index)
        draft_intake.compare_live(snapshot, version, source, int(run))


def validate_legacy(completions, context, index):
    workflow = Path(".github/workflows/release.yml").read_text()
    pins = re.findall(r"uses: ShaulLavo/mesh/.github/workflows/installed-catchup.yml@([0-9a-f]{40})", workflow)
    if len(pins) != 1:
        raise ValueError("finalization cannot identify its immutable installed harness")
    release_worker_completion.validate(Path(completions), context, pins[0], index["selection"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("source")
    parser.add_argument("completions", type=Path)
    parser.add_argument("snapshot", type=Path)
    parser.add_argument("run", type=int)
    parser.add_argument("--platform-plan", type=Path)
    parser.add_argument("--plan-sha256")
    parser.add_argument("--harness")
    parser.add_argument("--attempts", type=json.loads, help="exact producing attempt per native platform, from the evidence job handoff")
    args = parser.parse_args()
    reduced = (args.platform_plan, args.plan_sha256, args.harness, args.attempts)
    if any(value is not None for value in reduced):
        if any(value is None for value in reduced) or not re.fullmatch(r"[0-9a-f]{40}", args.harness):
            parser.error("reduced publication requires its plan, signer digest, exact harness and producer attempts")
    verify(args.version, args.source, args.completions, args.snapshot, args.run, platform_plan=args.platform_plan,
           plan_sha256=args.plan_sha256, harness=args.harness, attempts=args.attempts)


if __name__ == "__main__":
    main()
