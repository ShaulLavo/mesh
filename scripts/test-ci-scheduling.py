#!/usr/bin/env python3
"""Execute the ordinary test verdicts and reject scheduling regressions."""

import io
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
READY = "${{ !inputs.client_interrupt && (github.event_name != 'pull_request' || !github.event.pull_request.draft) }}"
ALWAYS_READY = "${{ always() && !inputs.client_interrupt && (github.event_name != 'pull_request' || !github.event.pull_request.draft) }}"
REQUIRED = {"changes", "app-pill", "test", "socket-portability", "auth-cutover", "machine-naming",
            "helper-recovery", "gates", "vulnerability", "packaging"}
# Only trusted events reach the owner's Mac runner: main pushes, manual runs and
# pull requests from branches of this repository. Fork pull requests stay hosted.
TRUSTED = ("matrix.runner == 'macos-15' && (github.event_name != 'pull_request' || "
           "github.event.pull_request.head.repo.full_name == github.repository)")
MAC_ROUTE = "${{ " + TRUSTED + " && fromJSON('[\"self-hosted\",\"macOS\",\"ARM64\",\"mesh-mac\"]') || matrix.runner }}"
# The Mac runner's start hook waits up to this long in the Mac FIFO.
MAC_FIFO_MINUTES = 45
MAC_JOBS = {"ci.yml": ("socket-portability", "auth-cutover", "machine-naming"),
            "helper-recovery.yml": ("native-recovery",), "update-proofs.yml": ("prepare", "prove")}
HOSTED_ONLY = ("installed-catchup.yml", "installed-prepared.yml", "release.yml")
MAC_GO_CACHE = "${{ runner.environment == 'github-hosted' }}"


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


def publication_allowed(condition, environment, ancestors):
    # GitHub inserts success() unless the expression has a status function.
    if not re.search(r"\b(?:always|cancelled|failure|success)\(\)", condition):
        if any(result != "success" for result in ancestors):
            return False
    expression = condition.removeprefix("${{").removesuffix("}}")
    for original, replacement in (
        ("cancelled()", '( "$CANCELLED" == true )'),
        ("needs.gate.result", '"$GATE"'),
        ("github.event_name", '"$EVENT"'),
        ("github.ref", '"$REF"'),
    ):
        expression = expression.replace(original, replacement)
    return STATUS("[[ " + expression + " ]]", dict(os.environ, **environment)) == 0


def check_publication_schedule(ci):
    publication = JOB(ci, "publish")
    assert re.findall(r"^    needs: (.+)$", publication, re.M) == ["gate"]
    conditions = re.findall(r"^    if: (.+)$", publication, re.M)
    assert conditions == [CONTRACT["PUBLICATION_FILTER"]]
    condition = conditions[0]
    main = dict(CANCELLED="false", GATE="success", EVENT="push", REF="refs/heads/main")
    for ancestors in (("success",), ("success", "skipped", "skipped")):
        assert publication_allowed(condition, main, ancestors), ancestors
    for draft in ("true", "false"):
        environment = dict(main, EVENT="pull_request", DRAFT=draft)
        assert not publication_allowed(condition, environment, ("success", "skipped")), draft
    for variable, rejected in (
        ("GATE", ("failure", "cancelled", "skipped", "pending", "")),
        ("CANCELLED", ("true",)),
        ("EVENT", ("pull_request", "workflow_dispatch", "pull_request_target")),
        ("REF", ("refs/heads/topic", "refs/pull/1/merge", "refs/tags/v0.1.400")),
    ):
        for value in rejected:
            assert not publication_allowed(condition, dict(main, **{variable: value}), ("success", "skipped")), (variable, value)
    legacy = "github.event_name == 'push' && github.ref == 'refs/heads/main'"
    assert publication_allowed(legacy, main, ("success",))
    assert not publication_allowed(legacy, main, ("success", "skipped", "skipped"))


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
    check_publication_schedule(ci)


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
        ci.replace("timeout-minutes: 20\n", "timeout-minutes: 21\n", 1),
        ci.replace("          cache: true", "          cache: false"),
        ci.replace("go test -race ./...", "go test -race ./internal/cli", 1),
        ci.replace("        run: ./scripts/verify.sh", "        run: ./scripts/verify.sh\n      - run: ./scripts/verify.sh", 1),
        ci.replace("        run: ./scripts/verify.sh", "        if: false\n        run: ./scripts/verify.sh", 1),
        ci.replace("!cancelled() && ", "", 1),
        ci.replace("needs.gate.result == 'success' && ", "", 1),
        ci.replace("!cancelled()", "true", 1),
        ci.replace("!cancelled()", "always()", 1),
        ci.replace("github.event_name == 'push'", "true", 1),
        ci.replace("github.ref == 'refs/heads/main'", "true", 1),
    )
    for mutated in mutations:
        reject_mutation(ci, recovery, mutated)


def check_mac_job(source, name):
    job = JOB(source, name)
    assert re.findall(r"^    runs-on: (.+)$", job, re.M) == [MAC_ROUTE], f"{name} must use the guarded Mac route"
    # The persistent Mac keeps Go and its module cache; restoring setup-go's
    # cache there re-downloads about 1 GB, fails over the existing files, and
    # its saves push the repository past GitHub's cache limit.
    caches = re.findall(r"uses: actions/setup-go@.*\n(?:        .*\n)*?          cache: (.+)\n", job)
    assert caches == [MAC_GO_CACHE] * len(re.findall(r"uses: actions/setup-go@", job)), \
        f"{name} must cache setup-go only on GitHub-hosted runners"
    timeouts = re.findall(r"^    timeout-minutes: (.+)$", job, re.M)
    if not timeouts:
        return
    match = re.fullmatch(r"\$\{\{ " + re.escape(TRUSTED) + r" && (\d+) \|\| (\d+) \}\}", timeouts[0])
    assert match, f"{name} must bound the Mac and hosted legs separately"
    mac, hosted = (int(value) for value in match.groups())
    assert mac == hosted + MAC_FIFO_MINUTES, f"{name} Mac hang cap must add the FIFO wait to the hosted bound"


def check_mac_routing(workflows):
    for name, jobs in MAC_JOBS.items():
        for job in jobs:
            check_mac_job(workflows[name], job)
    for name, source in workflows.items():
        # Signature verification's --deny-self-hosted-runners is not a route.
        routes = [line for line in source.splitlines() if "runs-on:" in line and "self-hosted" in line]
        assert all(line == "    runs-on: " + MAC_ROUTE for line in routes), f"{name} has an unguarded self-hosted route"
        expected = 0 if name in HOSTED_ONLY else len(MAC_JOBS.get(name, ()))
        assert len(routes) == expected, f"{name} must route exactly its reviewed Darwin legs to the Mac"


def reject_mac_routing(workflows):
    route = "github.event.pull_request.head.repo.full_name == github.repository"
    mutations = (
        ("ci.yml", lambda text: text.replace(" || " + route, "", 1)),
        ("ci.yml", lambda text: text.replace("\"mesh-mac\"", "\"other\"", 1)),
        ("ci.yml", lambda text: text.replace("&& 55 || 10", "&& 10 || 10", 1)),
        ("helper-recovery.yml", lambda text: text.replace("cache: " + MAC_GO_CACHE, "cache: true", 1)),
        ("update-proofs.yml", lambda text: text.replace("    runs-on: " + MAC_ROUTE, "    runs-on: ${{ matrix.runner }}", 1)),
        ("release.yml", lambda text: text.replace("    runs-on: ${{ matrix.runner }}", "    runs-on: " + MAC_ROUTE, 1)),
    )
    for name, mutate in mutations:
        mutated = dict(workflows, **{name: mutate(workflows[name])})
        assert mutated[name] != workflows[name], f"Mac routing mutation did not apply to {name}"
        try:
            check_mac_routing(mutated)
        except AssertionError:
            continue
        raise AssertionError(f"Mac routing accepted a mutation of {name}")


MAC_TURN_ENV = {"GH_TOKEN": "${{ github.token }}", "EVENT": "${{ github.event_name }}",
                "DRAFT": "${{ github.event.pull_request.draft }}",
                "HEAD_REPOSITORY": "${{ github.event.pull_request.head.repo.full_name }}",
                "REPOSITORY": "${{ github.repository }}"}
WAIT_SCRIPT = ROOT / "scripts/wait-for-main-ci.py"


def mac_users(ci):
    # Jobs with a Mac leg of their own, and callers of workflows that have one.
    callers = [name for name, workflow in re.findall(r"^  ([\w-]+):\n(?:    .*\n|\n)*?    uses: \./\.github/workflows/([\w-]+\.yml)$", ci, re.M)
               if workflow in MAC_JOBS]
    return set(MAC_JOBS["ci.yml"]) | set(callers)


def check_mac_turn(ci):
    turn = JOB(ci, "mac-turn")
    assert re.findall(r"^    if: (.+)$", turn, re.M) == ["${{ !inputs.client_interrupt }}"]
    assert re.findall(r"^    runs-on: (.+)$", turn, re.M) == ["ubuntu-24.04"], "the Mac turn must wait on a hosted runner"
    assert not re.search(r"^    needs:|continue-on-error:", turn, re.M), "the Mac turn must start at once and block"
    assert re.findall(r"^    timeout-minutes: (\d+)$", turn, re.M) == ["200"], "the hang cap must exceed the script deadline"
    assert "      actions: read\n" in turn
    assert "        run: python3 -B scripts/wait-for-main-ci.py\n" in turn
    assert dict(re.findall(r"^          (\w+): (\$\{\{ .+ \}\})$", turn, re.M)) == MAC_TURN_ENV
    users = mac_users(ci)
    assert users == {"socket-portability", "auth-cutover", "machine-naming", "helper-recovery", "update-proofs"}, users
    for name in users:
        needs = re.findall(r"^    needs: (.+)$", JOB(ci, name), re.M)
        assert needs == ["[changes, mac-turn]"], f"{name} must wait for its Mac turn"


class FakeGitHub:
    """One answer per poll: False is idle, True a queued main run, a string that
    run's status, an exception an API failure. Like GitHub between job phases,
    a status-filtered list misses the active run."""

    def __init__(self, answers):
        self.answers = list(answers)
        self.endpoints = []

    def __call__(self, endpoint):
        self.endpoints.append(endpoint)
        answer = self.answers[0] if len(self.answers) == 1 else self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        runs = [{"id": 3, "status": "completed"}]
        if answer and "status=" not in endpoint:
            runs.insert(0, {"id": 7, "status": "queued" if answer is True else answer})
        return {"workflow_runs": runs}


def run_wait(wait, environment, answers, minutes=180):
    github, clock, sleeps = FakeGitHub(answers), [0.0], []

    def sleep(seconds):
        sleeps.append(seconds)
        clock[0] += seconds

    out = io.StringIO()
    status = wait(dict(environment, DEADLINE_MINUTES=str(minutes)), api=github, sleep=sleep, clock=lambda: clock[0], out=out)
    return status, github.endpoints, sleeps, out.getvalue()


def check_wait_script(source):
    wait = {"__name__": "wait_for_main_ci"}
    exec(compile(source, str(WAIT_SCRIPT), "exec"), wait)  # noqa: S102 - the repository's own script under test
    run = wait["wait"]
    pr = {"EVENT": "pull_request", "DRAFT": "false", "HEAD_REPOSITORY": "ShaulLavo/mesh", "REPOSITORY": "ShaulLavo/mesh"}
    for environment in (dict(pr, EVENT="push"), dict(pr, EVENT="workflow_dispatch"),
                        dict(pr, HEAD_REPOSITORY="someone/mesh"), dict(pr, DRAFT="true")):
        status, endpoints, sleeps, _ = run_wait(run, environment, [True])
        assert (status, endpoints, sleeps) == (0, [], []), environment
    # Idle main must hold for two polls a minute apart, read unfiltered.
    status, endpoints, sleeps, out = run_wait(run, pr, [False])
    assert (status, sleeps) == (0, [60]) and "3 (completed)" not in out
    assert endpoints == ["repos/ShaulLavo/mesh/actions/workflows/ci.yml/runs?branch=main&per_page=30"] * 2, endpoints
    # Busy for two polls, then idle: the PR waits, then runs.
    status, _, sleeps, out = run_wait(run, pr, [True, True, False])
    assert status == 0 and len(sleeps) == 3 and "waiting for main CI: 7 (queued)" in out
    # A run a status-filtered list would miss still holds the PR.
    for active in ("in_progress", "queued", "waiting", "requested", "pending", "action_required"):
        status, _, sleeps, out = run_wait(run, pr, [active, False])
        assert status == 0 and len(sleeps) == 2 and f"7 ({active})" in out, active
    # One idle poll between busy ones is not enough.
    status, _, sleeps, _ = run_wait(run, pr, [False, True, False])
    assert status == 0 and len(sleeps) == 3
    # An unreadable API counts as busy, never as idle, and resets the idle count.
    status, _, sleeps, _ = run_wait(run, pr, [RuntimeError("HTTP 502"), False])
    assert status == 0 and len(sleeps) == 2
    status, _, sleeps, _ = run_wait(run, pr, [False, RuntimeError("HTTP 502"), False])
    assert status == 0 and len(sleeps) == 3
    # Past the hang cap the job fails, so the PR's Mac legs skip, never run.
    status, _, sleeps, out = run_wait(run, pr, [True], minutes=3)
    assert status != 0 and len(sleeps) == 3, (status, sleeps)
    assert "::error::Mac turn: main CI still active after 3 minutes; rerun this job when main is idle" in out, out
    assert "no main CI run is queued or running" not in out
    status, _, _, out = run_wait(run, pr, [RuntimeError("HTTP 502")], minutes=3)
    assert status != 0 and "::error::" in out


def reject_mac_turn(ci, script):
    mutations = (
        (ci.replace("  socket-portability:\n    needs: [changes, mac-turn]", "  socket-portability:\n    needs: changes", 1), script),
        (ci.replace("    name: Required native update proofs\n    needs: [changes, mac-turn]",
                    "    name: Required native update proofs\n    needs: changes", 1), script),
        (ci.replace("    timeout-minutes: 200\n", "    timeout-minutes: 20\n", 1), script),
        (ci.replace("    name: Wait for main CI before Mac legs\n", "    name: Wait for main CI before Mac legs\n    continue-on-error: true\n", 1), script),
        (ci, script.replace('return environment.get("HEAD_REPOSITORY") == environment.get("REPOSITORY")', "return False", 1)),
        (ci, script.replace("branch=main&", "branch=topic&", 1)),
        (ci, script.replace('return [f"unknown ({error})"]', "return []", 1)),
        (ci, script.replace("IDLE_POLLS = 2", "IDLE_POLLS = 1", 1)),
        (ci, script.replace("            return 1\n", "            return 0\n", 1)),
        (ci, script.replace("idle = 0 if runs else idle + 1", "idle = idle if runs else idle + 1", 1)),
        (ci, script.replace("runs?branch=main&per_page=", "runs?branch=main&status=in_progress&per_page=", 1)),
        (ci, script.replace('if run["status"] != "completed"', 'if run["status"] == "in_progress"', 1)),
    )
    for mutated, mutated_script in mutations:
        assert (mutated, mutated_script) != (ci, script), "Mac turn mutation did not apply"
        try:
            check_mac_turn(mutated)
            check_wait_script(mutated_script)
        except AssertionError:
            continue
        raise AssertionError("an unsafe Mac turn was accepted")


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    script = WAIT_SCRIPT.read_text()
    check_mac_turn(ci)
    check_wait_script(script)
    reject_mac_turn(ci, script)
    recovery = (ROOT / ".github/workflows/helper-recovery.yml").read_text()
    check_schedule(ci, recovery)
    check_mutations(ci, recovery)
    CONTRACT["check_mutations"](ci, recovery)
    workflows = {path.name: path.read_text() for path in sorted((ROOT / ".github/workflows").glob("*.yml"))}
    check_mac_routing(workflows)
    reject_mac_routing(workflows)
    print("PASS independent tests, blocking aggregates, main publication after skipped ancestors, failure/cancel/PR refusal, guarded Mac routing, PR Mac legs behind main CI and mutation controls")


if __name__ == "__main__":
    main()
