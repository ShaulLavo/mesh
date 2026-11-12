#!/usr/bin/env python3
"""Execute the ordinary test verdicts and reject scheduling regressions."""

import itertools
import os
from pathlib import Path
import re
import runpy

ROOT = Path(__file__).resolve().parents[1]
CONTRACT = runpy.run_path(str(ROOT / "scripts/test-helper-recovery-gate.py"))
JOB = CONTRACT["job"]
STEP = CONTRACT["step"]
RUN_SCRIPT = CONTRACT["run_script"]
STATUS = CONTRACT["status"]
READY = "${{ (github.event_name != 'pull_request' || !github.event.pull_request.draft) }}"
ALWAYS_READY = "${{ always() && (github.event_name != 'pull_request' || !github.event.pull_request.draft) }}"
REQUIRED = {"changes", "app-pill", "test", "socket-portability", "auth-cutover", "machine-naming",
            "helper-recovery", "gates", "vulnerability", "packaging"}


def dependencies(source):
    match = re.search(r"^    needs: \[(.+)\]$", source, re.M)
    assert match, "missing aggregate dependencies"
    return [name.strip() for name in match.group(1).split(",")]


def check_phase(ci, name, command):
    phase = JOB(ci, name)
    assert re.findall(r"^    if: (.+)$", phase, re.M) == [READY]
    assert re.findall(r"^    timeout-minutes: (.+)$", phase, re.M) == ["20"]
    assert re.findall(r"^    runs-on: (.+)$", phase, re.M) == ["ubuntu-24.04"]
    assert not re.search(r"^    needs:", phase, re.M), "test phases must start independently"
    assert not re.search(r"^        if:|continue-on-error:|strategy:", phase, re.M)
    assert len(re.findall(r"uses: actions/setup-go@", phase)) == 1
    assert "          go-version-file: go.mod\n          cache: true\n" in phase
    assert len(re.findall(rf"^\s+(?:- )?run: {re.escape(command)}$", ci, re.M)) == 1
    assert f"        run: {command}\n" in phase
    return phase


def check_test_gate(ci):
    gate = JOB(ci, "test")
    assert "    name: Race tests and integrations\n" in gate
    assert re.findall(r"^    if: (.+)$", gate, re.M) == [ALWAYS_READY]
    assert sorted(dependencies(gate)) == ["integration", "race"]
    assert "continue-on-error:" not in gate
    assert "        run: python3 -B scripts/test-ci-scheduling.py\n" in gate
    verdict = STEP(gate, "Require races and integrations")
    results = dict(re.findall(r"^          (\w+): \$\{\{ needs\.([\w-]+)\.result \}\}$", verdict, re.M))
    assert results == {"RACE": "race", "INTEGRATION": "integration"}
    assert not re.search(r"^        if:", verdict, re.M)
    command = RUN_SCRIPT(verdict)
    for race, integration in itertools.product(("success", "failure", "cancelled", "skipped", "", "pending"), repeat=2):
        environment = dict(os.environ, RACE=race, INTEGRATION=integration)
        assert (STATUS(command, environment) == 0) == (race == integration == "success"), (race, integration)


def check_schedule(ci, recovery):
    race = check_phase(ci, "race", "go test -race ./...")
    integration = check_phase(ci, "integration", "./scripts/verify.sh")
    assert "      - name: Require a tidy module graph\n" in race
    assert "          go mod tidy\n          git diff --exit-code -- go.mod go.sum\n" in race
    assert "          python3 scripts/test-private-boundary.py\n          scripts/check-private-hosting-boundary.sh\n" in race
    assert "      - run: sudo apt-get update && sudo apt-get install -y zsh\n" in integration
    assert re.findall(r"^          (MESH_INTEGRATION_\w+): (.+)$", integration, re.M) == [
        ("MESH_INTEGRATION_TIMEOUT", "60s")]
    check_test_gate(ci)
    assert REQUIRED <= set(dependencies(JOB(ci, "gate")))
    CONTRACT["check_gate"](ci, recovery)


def reject_mutation(ci, recovery, mutated):
    assert mutated != ci, "negative control did not change the workflow"
    try:
        check_schedule(mutated, recovery)
    except AssertionError:
        return
    raise AssertionError("unsafe test schedule accepted")


def check_mutations(ci, recovery):
    mutations = (
        ci.replace("  integration:\n", "  integration:\n    needs: race\n", 1),
        ci.replace("needs: [race, integration]", "needs: [race]", 1),
        ci.replace(ALWAYS_READY, READY, 1),
        ci.replace("$RACE == success && ", "", 1),
        ci.replace(" && $INTEGRATION == success", "", 1),
        ci.replace("[[ $RACE == success && $INTEGRATION == success ]]",
                   "[[ $RACE == success && $INTEGRATION == success ]] || true", 1),
        ci.replace("$TEST == success && ", "", 1),
        CONTRACT["without_gate_dependency"](ci, "test"),
        ci.replace("timeout-minutes: 20", "timeout-minutes: 21", 1),
        ci.replace("          cache: true", "          cache: false"),
        ci.replace("go test -race ./...", "go test -race ./internal/cli", 1),
        ci.replace("        run: ./scripts/verify.sh", "        run: ./scripts/verify.sh\n      - run: ./scripts/verify.sh", 1),
        ci.replace("        run: ./scripts/verify.sh", "        if: false\n        run: ./scripts/verify.sh", 1),
    )
    for mutated in mutations:
        reject_mutation(ci, recovery, mutated)


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    recovery = (ROOT / ".github/workflows/helper-recovery.yml").read_text()
    check_schedule(ci, recovery)
    check_mutations(ci, recovery)
    CONTRACT["check_mutations"](ci, recovery)
    print("PASS independent race and integration jobs, unchanged budgets and caches, both blocking aggregates and mutation controls")


if __name__ == "__main__":
    main()
