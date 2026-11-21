#!/usr/bin/env python3
"""Check that native helper failures block the exact publication aggregate."""

import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent.parent
DRAFT_FILTER = "github.event_name == 'pull_request' && github.event.pull_request.draft"
SCOPED = {
    "CODE": ("race", "integration", "native"),
    "BROWSER": ("app-pill",),
    "PACKAGING": (),
    "UPDATE_PROOFS_REQUIRED": ("update-proofs", "installed-catchup"),
}
PUBLICATION_FILTER = "${{ !cancelled() && needs.gate.result == 'success' && github.event_name == 'push' && github.ref == 'refs/heads/main' }}"


def job(source, name):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:|\Z)", source, re.M | re.S)
    assert match, f"missing CI job {name}"
    return match.group(1)


def step(source, name):
    match = re.search(rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - name:|\Z)", source, re.M | re.S)
    assert match, f"missing CI step {name}"
    return match.group(1)


def run_script(source):
    match = re.search(r"^        run: \|\n((?:(?:          .*)?(?:\n|$))+)", source, re.M)
    assert match, "missing step script"
    return "\n".join(line[10:] for line in match.group(1).splitlines())


def status(command, environment, pipefail=True):
    args = ["bash", "-e"]
    if pipefail:
        args.extend(["-o", "pipefail"])
    return subprocess.run(args + ["-c", command], env=environment, capture_output=True,
                          check=False, timeout=5).returncode


def check_recovery(ci):
    native = job(ci, "native")
    assert "needs: changes" in native and "if: needs.changes.outputs.code == 'true'" in native
    assert re.search(r"^    defaults:\n      run:\n        shell: bash$", native, re.M)
    for platform, runner in (("linux/amd64", "ubuntu-24.04"), ("darwin/arm64", "macos-15")):
        assert re.search(rf"platform: {platform}\s+runner: {runner}\b", native)
    assert "fail-fast: false" in native and "continue-on-error:" not in native
    assert '$(go env GOHOSTOS)/$(go env GOHOSTARCH)' in native
    recovery = step(native, "Recover historical helper and run published bridge chain")
    assert "if: matrix.platform != 'linux/arm64'" in recovery
    assert 'bash scripts/prove-helper-recovery.sh "$RUNNER_TEMP/helper-recovery"' in run_script(recovery)


def reject_outcomes(command, environment, variable):
    for outcome in ("success", "failure", "cancelled", "skipped", "", "pending"):
        if outcome == environment[variable]:
            continue
        changed = dict(environment, **{variable: outcome})
        assert status(command, changed) != 0, f"aggregate accepts {variable}={outcome!r}"


def check_outcomes(command, results):
    for flags in ((False, False, False, False), (True, False, False, False), (True, True, False, False),
                  (True, False, True, True), (True, True, True, True)):
        decisions = dict(zip(SCOPED, flags, strict=True))
        required = {"changes", "quality"}
        for flag, dependencies in SCOPED.items():
            if decisions[flag]:
                required.update(dependencies)
        environment = dict(os.environ)
        environment.update({name: str(value).lower() for name, value in decisions.items()})
        environment.update({name: "success" if dependency in required else "skipped"
                            for name, dependency in results.items()})
        environment["SNAPSHOT"] = "success" if decisions["PACKAGING"] else "skipped"
        environment["SNAPSHOT_CONTRACT"] = environment["SNAPSHOT"]
        assert status(command, environment) == 0, f"aggregate rejects valid decisions {decisions}"
        for variable in tuple(results) + ("SNAPSHOT", "SNAPSHOT_CONTRACT"):
            reject_outcomes(command, environment, variable)
        for variable in SCOPED:
            assert status(command, dict(environment, **{variable: ""})) != 0
            assert status(command, dict(environment, **{variable: "unknown"})) != 0


def check_gate(ci):
    check_recovery(ci)
    gate = job(ci, "gate")
    assert "name: Complete CI gate" in gate and "if: ${{ always() && !inputs.client_interrupt }}" in gate
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    dependencies = {name.strip() for name in needed.group(1).split(",")}
    assert dependencies == {"changes", "quality"} | set().union(*SCOPED.values())
    results = dict(re.findall(r"^          (\w+): \$\{\{ needs\.([\w-]+)\.result \}\}$", gate, re.M))
    assert set(results.values()) == dependencies
    aggregate = step(gate, "Require every CI job")
    assert "if: ${{ success() && !cancelled() }}" in aggregate
    for check in SCOPED:
        output = {"UPDATE_PROOFS_REQUIRED": "update_proofs"}.get(check, check.lower())
        assert f"{check}: ${{{{ needs.changes.outputs.{output} }}}}" in aggregate
    assert "SNAPSHOT: ${{ needs.quality.outputs.snapshot }}" in aggregate
    assert "SNAPSHOT_CONTRACT: ${{ needs.quality.outputs.snapshot_contract }}" in aggregate
    readiness = step(gate, "Require a ready PR")
    assert re.findall(r"^        if: (.+)$", readiness, re.M) == [DRAFT_FILTER]
    assert status(run_script(readiness), os.environ) != 0, "draft readiness script succeeds"
    check_outcomes(run_script(aggregate), results)
    publication = job(ci, "publish")
    assert "needs: gate" in publication and "uses: ./.github/workflows/release.yml" in publication
    assert "source_sha: ${{ github.sha }}" in publication
    assert re.findall(r"^    if: (.+)$", publication, re.M) == [PUBLICATION_FILTER]
    check_cask_status(gate)


def check_cask_status(gate):
    report = step(gate, "Report the real CI verdict on the tested commit")
    assert "always() && github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/automation/cask-updates'" in report
    assert "HEAD: ${{ github.sha }}" in report and "RESULT: ${{ steps.verdict.outcome }}" in report
    assert "statuses: write" in gate
    prefix = 'gh() { printf "%s\\n" "$@"; }\n'
    for result, state in (("success", "success"), ("failure", "failure"), ("skipped", "failure"), ("cancelled", "failure")):
        sha = "a" * 40
        environment = dict(os.environ, RESULT=result, GH_REPO="ShaulLavo/mesh", HEAD=sha, RUN_URL="test-run")
        args = subprocess.check_output(["bash", "-e", "-c", prefix + run_script(report)],
                                       env=environment, text=True, timeout=5).splitlines()
        assert f"repos/ShaulLavo/mesh/statuses/{sha}" in args
        assert f"state={state}" in args and "context=Complete CI gate" in args


def without_gate_dependency(ci, dependency):
    gate = job(ci, "gate")
    needed = re.search(r"^    needs: \[(.+)\]$", gate, re.M)
    assert needed
    dependencies = [name.strip() for name in needed.group(1).split(",")]
    assert dependencies.count(dependency) == 1
    remaining = ", ".join(name for name in dependencies if name != dependency)
    return ci.replace(gate, gate[:needed.start(1)] + remaining + gate[needed.end(1):], 1)


def check_mutations(ci):
    mutations = (
        without_gate_dependency(ci, "native"),
        ci.replace('          require "$CODE" native "$NATIVE"\n', "", 1),
        ci.replace("          bash scripts/prove-helper-recovery.sh", "          echo skipped instead of", 1),
        ci.replace("CODE: ${{ needs.changes.outputs.code }}", "CODE: false", 1),
        ci.replace('          require true changes "$CHANGES"\n', "", 1),
        ci.replace(DRAFT_FILTER, "false", 1),
        ci.replace("HEAD: ${{ github.sha }}", "HEAD: ${{ github.event.pull_request.head.sha }}", 1),
    )
    for mutated in mutations:
        assert mutated != ci, "publication negative control did not change CI source"
        try:
            check_gate(mutated)
        except AssertionError:
            continue
        raise AssertionError("disconnected helper publication gate accepted")
    assert status("(exit 71) | true", os.environ, pipefail=False) == 0
    assert status("(exit 71) | true", os.environ) != 0, "piped test failure was ignored"


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    check_gate(ci)
    check_mutations(ci)
    reservation = (ROOT / "scripts/reserve-release.sh").read_text()
    assert '.name == "Complete CI gate"' in reservation
    assert '.status == "completed"' in reservation and '.conclusion == "success"' in reservation
    assert "commits/$source_sha/check-runs" in reservation
    print("PASS native helper publication contract: code, documentation and draft outcomes, rejection controls and exact-source publication")


if __name__ == "__main__":
    main()
