#!/usr/bin/env python3
"""Check that native helper failures block the exact publication aggregate."""

import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent.parent
CODE_FILTER = "${{ needs.changes.outputs.code == 'true' && (github.event_name != 'pull_request' || !github.event.pull_request.draft) }}"
DRAFT_FILTER = "github.event_name == 'pull_request' && github.event.pull_request.draft"
FILTERED = {"app-pill", "socket-portability", "auth-cutover", "machine-naming", "helper-recovery"}


def job(source, name):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:|\Z)", source, re.M | re.S)
    assert match, f"missing CI job {name}"
    return match.group(1)


def step(source, name):
    match = re.search(rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - name:|\Z)", source, re.M | re.S)
    assert match, f"missing CI step {name}"
    return match.group(1)


def run_script(source):
    match = re.search(r"^        run: \|\n((?:          .*(?:\n|$))+)", source, re.M)
    assert match, "missing step script"
    return "\n".join(line[10:] for line in match.group(1).splitlines())


def status(command, environment, pipefail=True):
    args = ["bash", "-e"]
    if pipefail:
        args.extend(["-o", "pipefail"])
    return subprocess.run(args + ["-c", command], env=environment, capture_output=True,
                          check=False, timeout=5).returncode


def check_recovery(ci, recovery):
    caller = job(ci, "helper-recovery")
    assert "uses: ./.github/workflows/helper-recovery.yml" in caller
    assert not re.search(r"^    continue-on-error:", caller, re.M)
    for name in FILTERED:
        filtered = job(ci, name)
        assert re.search(r"^    needs: changes$", filtered, re.M)
        assert re.findall(r"^    if: (.+)$", filtered, re.M) == [CODE_FILTER]
    assert re.search(r"^  workflow_call:\s*$", recovery, re.M)
    assert re.search(r"^defaults:\n  run:\n    shell: bash$", recovery, re.M)
    assert set(re.findall(r"^\s+shell: (.+)$", recovery, re.M)) == {"bash"}
    for platform, runner in (("linux/amd64", "ubuntu-24.04"), ("darwin/arm64", "macos-15")):
        assert re.search(rf"platform: {platform}\s+runner: {runner}", recovery)
    assert "fail-fast: false" in recovery and "continue-on-error:" not in recovery
    assert '$(go env GOHOSTOS)/$(go env GOHOSTARCH)' in recovery
    assert "bash scripts/prove-helper-recovery.sh" in recovery and "if: always()" in recovery


def reject_outcomes(command, environment, variable):
    for outcome in ("success", "failure", "cancelled", "skipped", "", "pending"):
        if outcome == environment[variable]:
            continue
        changed = dict(environment, **{variable: outcome})
        assert status(command, changed) != 0, f"aggregate accepts {variable}={outcome!r} with CODE={environment['CODE']}"


def check_outcomes(command, results):
    for code in ("true", "false"):
        for proofs in ("true", "false"):
            check_filtered_outcomes(command, results, code, proofs)
    environment = dict(os.environ, UPDATE_PROOFS_REQUIRED="true", **{name: "success" for name in results})
    for code in ("", "pending", "True", "unknown"):
        assert status(command, dict(environment, CODE=code)) != 0, f"aggregate accepts unknown CODE={code!r}"
    for proofs in ("", "pending", "True", "unknown"):
        assert status(command, dict(environment, CODE="true", UPDATE_PROOFS_REQUIRED=proofs)) != 0, \
            f"aggregate accepts unknown UPDATE_PROOFS_REQUIRED={proofs!r}"


def check_filtered_outcomes(command, results, code, proofs):
    environment = dict(os.environ, CODE=code, UPDATE_PROOFS_REQUIRED=proofs)
    environment.update({name: "skipped" if code == "false" and dependency in FILTERED else "success"
                        for name, dependency in results.items()})
    environment["UPDATE_PROOFS"] = "success" if proofs == "true" else "skipped"
    assert status(command, environment) == 0, f"aggregate rejects valid CODE={code}, proofs={proofs} results"
    for variable in results:
        reject_outcomes(command, environment, variable)


def check_gate(ci, recovery):
    check_recovery(ci, recovery)
    gate = job(ci, "gate")
    assert "name: Complete CI gate" in gate and "if: always()" in gate
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    assert needed
    dependencies = {name.strip() for name in needed.group(1).split(",")}
    assert {"changes", "helper-recovery"} <= dependencies
    results = dict(re.findall(r"^          (\w+): \$\{\{ needs\.([\w-]+)\.result \}\}$", gate, re.M))
    assert set(results.values()) == dependencies and FILTERED <= dependencies
    aggregate = step(gate, "Require every CI job")
    assert "CODE: ${{ needs.changes.outputs.code }}" in aggregate
    readiness = step(gate, "Require a ready PR")
    assert re.findall(r"^        if: (.+)$", readiness, re.M) == [DRAFT_FILTER]
    assert status(run_script(readiness), os.environ) != 0, "draft readiness script succeeds"
    check_outcomes(run_script(aggregate), results)
    publication = job(ci, "publish")
    assert "needs: gate" in publication and "uses: ./.github/workflows/release.yml" in publication
    assert "source_sha: ${{ github.sha }}" in publication
    assert re.search(r"^    if: github.event_name == 'push' && github.ref == 'refs/heads/main'$", publication, re.M)


def without_gate_dependency(ci, dependency):
    gate = job(ci, "gate")
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    assert needed
    dependencies = [name.strip() for name in needed.group(1).split(",")]
    assert dependencies.count(dependency) == 1
    remaining = ", ".join(name for name in dependencies if name != dependency)
    return ci.replace(gate, gate[:needed.start(1)] + remaining + gate[needed.end(1):], 1)


def reject_source(ci, recovery):
    try:
        check_gate(ci, recovery)
    except AssertionError:
        return
    raise AssertionError("disconnected helper publication gate accepted")


def check_mutations(ci, recovery):
    mutations = (
        without_gate_dependency(ci, "helper-recovery"),
        ci.replace(' && $HELPER_RECOVERY == "$expected"', "", 1),
        ci.replace("name: Required native helper recovery", "name: Required native helper recovery\n    if: false", 1),
        ci.replace("CODE: ${{ needs.changes.outputs.code }}", "CODE: false", 1),
        ci.replace("[[ $CHANGES == success ]]", "[[ $CHANGES == success ]] || true", 1),
        ci.replace(DRAFT_FILTER, "false", 1),
    )
    for mutated in mutations:
        assert mutated != ci, "publication negative control did not change CI source"
        reject_source(mutated, recovery)
    unsafe = recovery.replace("defaults:\n  run:\n    shell: bash\n\n", "", 1)
    assert unsafe != recovery, "shell negative control did not change workflow"
    reject_source(ci, unsafe)
    assert status("(exit 71) | true", os.environ, pipefail=False) == 0
    assert status("(exit 71) | true", os.environ) != 0, "piped test failure was ignored"


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    recovery = (ROOT / ".github/workflows/helper-recovery.yml").read_text()
    check_gate(ci, recovery)
    check_mutations(ci, recovery)
    reservation = (ROOT / "scripts/reserve-release.sh").read_text()
    assert '.name == "Complete CI gate"' in reservation
    assert '.status == "completed"' in reservation and '.conclusion == "success"' in reservation
    assert "commits/$source_sha/check-runs" in reservation
    print("PASS native helper publication contract: code, documentation and draft outcomes, rejection controls and exact-source publication")


if __name__ == "__main__":
    main()
