#!/usr/bin/env python3
"""Check that native helper failures block the exact publication aggregate."""

import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent.parent


def job(source, name):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:|\Z)", source, re.M | re.S)
    if not match:
        raise AssertionError(f"missing CI job {name}")
    return match.group(1)


def check_gate(ci, recovery):
    caller = job(ci, "helper-recovery")
    assert "uses: ./.github/workflows/helper-recovery.yml" in caller
    assert not re.search(r"^    (if|continue-on-error):", caller, re.M)
    assert re.search(r"^  workflow_call:\s*$", recovery, re.M)
    for platform, runner in (("linux/amd64", "ubuntu-24.04"), ("darwin/arm64", "macos-15")):
        assert re.search(rf"platform: {platform}\s+runner: {runner}", recovery)
    assert "fail-fast: false" in recovery
    assert "continue-on-error:" not in recovery
    assert '$(go env GOHOSTOS)/$(go env GOHOSTARCH)' in recovery
    assert "bash scripts/prove-helper-recovery.sh" in recovery
    assert "if: always()" in recovery
    gate = job(ci, "gate")
    assert "name: Complete CI gate" in gate and "if: always()" in gate
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    assert needed
    dependencies = [name.strip() for name in needed.group(1).split(",")]
    assert "helper-recovery" in dependencies
    results = dict(re.findall(r"^          (\w+): \$\{\{ needs\.([\w-]+)\.result \}\}$", gate, re.M))
    assert set(results.values()) == set(dependencies)
    script = re.search(r"run: \|\n((?:          .*(?:\n|$))+)", gate)
    assert script
    command = "\n".join(line[10:] for line in script.group(1).splitlines())
    environment = dict(os.environ, **{name: "success" for name in results})
    assert subprocess.run(["bash", "-c", command], env=environment, check=False).returncode == 0
    for name in results:
        for outcome in ("failure", "cancelled", "skipped", "", "pending"):
            result = subprocess.run(["bash", "-c", command], env=dict(environment, **{name: outcome}), check=False)
            assert result.returncode != 0, f"aggregate accepts {name}={outcome!r}"
    publication = job(ci, "publish")
    assert "needs: gate" in publication
    assert "uses: ./.github/workflows/release.yml" in publication
    assert "source_sha: ${{ github.sha }}" in publication


def without_gate_dependency(ci, dependency):
    gate = job(ci, "gate")
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    assert needed
    dependencies = [name.strip() for name in needed.group(1).split(",")]
    assert dependencies.count(dependency) == 1
    remaining = ", ".join(name for name in dependencies if name != dependency)
    mutated_gate = gate[:needed.start(1)] + remaining + gate[needed.end(1):]
    return ci.replace(gate, mutated_gate, 1)


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    recovery = (ROOT / ".github/workflows/helper-recovery.yml").read_text()
    check_gate(ci, recovery)
    # These mutations preserve a superficially present helper job while cutting
    # the dependency or allowing a failing/skipped native aggregate to publish.
    for mutated in (
        without_gate_dependency(ci, "helper-recovery"),
        ci.replace("$HELPER_RECOVERY == success && ", ""),
        ci.replace("name: Required native helper recovery", "name: Required native helper recovery\n    if: false"),
    ):
        assert mutated != ci, "publication negative control did not change CI source"
        try:
            check_gate(mutated, recovery)
        except AssertionError:
            continue
        raise AssertionError("disconnected helper publication gate accepted")
    reservation = (ROOT / "scripts/reserve-release.sh").read_text()
    assert '.name == "Complete CI gate"' in reservation
    assert '.status == "completed"' in reservation and '.conclusion == "success"' in reservation
    assert "commits/$source_sha/check-runs" in reservation
    print("PASS native Linux/Darwin helper gate, all terminal-state controls and exact-source publication")


if __name__ == "__main__":
    main()
