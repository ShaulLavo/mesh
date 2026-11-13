"""Render the independent, diagnostic-only AMD64 chain-final review vehicle."""
import argparse
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HEADER = """name: Retained attachment transport diagnosis

on:
  workflow_dispatch:
    inputs:
      source_sha:
        description: Exact independently reviewed observer source commit
        required: true
        type: string
      producer_run:
        description: Existing complete reviewed original-worker preparation run
        required: true
        type: string
      producer_source:
        description: Exact original preparation source SHA
        required: true
        type: string
      producer_prefix:
        description: Exact original preparation artifact prefix
        required: true
        type: string

permissions:
  contents: read
  actions: read

env:
  MESH_HARNESS_SOURCE: ${{ inputs.source_sha }}
  MESH_PREPARATION_SOURCE: ${{ inputs.producer_source }}
  PYTHONDONTWRITEBYTECODE: '1'
  PYTHONPATH: ${{ github.workspace }}/scripts

concurrency:
  group: retained-attachment-transport-${{ github.run_id }}
  cancel-in-progress: false

defaults:
  run:
    shell: bash

jobs:
"""


def render():
    source = (ROOT / ".github/workflows/installed-prepared.yml").read_text()
    jobs = source.split("\njobs:\n", 1)[1].split("\n  aggregate:\n", 1)[0]
    replacements = {
        "inputs.harness_source": "inputs.source_sha",
        "inputs.worker_artifact_run": "fromJSON(inputs.producer_run)",
        "inputs.worker_artifact_source": "inputs.producer_source",
        "inputs.worker_artifact_prefix": "inputs.producer_prefix",
        "inputs.calibration_only": "true",
        "inputs.native_platform": "'linux/amd64'",
        "inputs.mode": "'chain'",
        "profile: [none, first, final]": "profile: [final]",
        "  consume:\n": "  consume:\n    env:\n      MESH_TRANSPORT_LIFECYCLE: '1'\n",
        '          python3 scripts/fixtures/installed_workflow.py select "$RUNNER_TEMP/prepared.noindex" "$PROFILE" "$RUNNER_TEMP/cohort-control/installed-slice.json"': '''          python3 -c 'import os; from pathlib import Path; from fixtures.installed_prepared import read_descriptor; d,_=read_descriptor(Path(os.environ["RUNNER_TEMP"])/"prepared.noindex"); assert d["native"]=="linux/amd64" and d["mode"]=="chain" and d["pair"]["daemon"]["version"]=="v0.1.249"'
          python3 scripts/fixtures/installed_workflow.py select "$RUNNER_TEMP/prepared.noindex" "$PROFILE" "$RUNNER_TEMP/cohort-control/installed-slice.json"''',
        "      - name: Require independently reviewed physical job plans or explicit measurement": """      - name: Require exact independently reviewed source pin
        run: |
          set -euo pipefail
          [[ $MESH_HARNESS_SOURCE =~ ^[0-9a-f]{40}$ ]]
          test "$(git rev-parse HEAD)" = "$MESH_HARNESS_SOURCE"
      - name: Require independently reviewed physical job plans or explicit measurement""",
    }
    for before, after in replacements.items():
        if before not in jobs:
            raise ValueError("prepared workflow changed before diagnostic vehicle composition: " + before)
        jobs = jobs.replace(before, after)
    return HEADER + jobs


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.write_text(render())
