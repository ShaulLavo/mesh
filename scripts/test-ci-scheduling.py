#!/usr/bin/env python3
"""Execute the ordinary test verdicts and reject scheduling regressions."""

import io
import itertools
import os
from pathlib import Path
import re
import runpy
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
APT_ACTION = "        uses: ./.github/actions/apt-packages\n"
# Stand-ins for sudo and apt-get. apt-get logs each call and behaves like an
# archive: update writes lists, an online install writes a .deb, and an
# offline (--no-download) install succeeds only when the .deb is cached.
FAKE_SUDO = '#!/bin/sh\nexec "$@"\n'
FAKE_APT = """#!/bin/sh
printf '%s\\n' "$*" >> "$APT_LOG"
lists=$(printf '%s\\n' "$@" | sed -n 's/^Dir::State::Lists=//p')
archives=$(printf '%s\\n' "$@" | sed -n 's/^Dir::Cache::Archives=//p')
case " $* " in
  *" update "*) touch "$lists/archive_Packages"; exit 0 ;;
  *" --no-download "*) [ -f "$archives/zsh.deb" ] && [ -z "$APT_OFFLINE_SHORT" ]; exit $? ;;
  *) touch "$archives/zsh.deb" ;;
esac
"""
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
# The shared Mac runners hold the Mac FIFO together, up to three jobs at once,
# and skip the machine-wide cleanup the exclusive runner does. Only legs that
# keep every file, process and port in their own job may use them.
MAC_SHARED_ROUTE = MAC_ROUTE.replace('"mesh-mac"', '"mesh-mac-shared"')
MAC_SHARED_JOBS = {("update-proofs.yml", "prepare"), ("update-proofs.yml", "prove")}
# CI callers whose Mac legs all run on the shared runners.
MAC_TURN_FREE = {"update-proofs"}
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
    assert "      - name: Install zsh\n" + APT_ACTION + "        with:\n          packages: zsh\n" in integration
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
        ci.replace("'success' && github.event_name == 'push'", "'success' && true", 1),
        ci.replace("'push' && github.ref == 'refs/heads/main' }}", "'push' && true }}", 1),
    )
    for mutated in mutations:
        reject_mutation(ci, recovery, mutated)


def check_apt_workflows(workflows, action):
    """Every hosted apt install goes through the cached action, which tries
    the cache offline before it ever contacts the archive."""
    for name, source in workflows.items():
        assert not re.search(r"apt-get|apt install|--with-deps|install-deps(?! --dry-run)", source), \
            f"{name} installs from the live Ubuntu archive; use ./.github/actions/apt-packages"
    pill = JOB(workflows["ci.yml"], "app-pill")
    assert "playwright install-deps --dry-run chromium webkit\n" in pill
    assert '[ "${arguments[*]:0:3}" != "install -s --no-install-recommends" ]' in pill
    assert ("        timeout-minutes: 25\n" + APT_ACTION
            + "        with:\n          packages: ${{ steps.browser-packages.outputs.packages }}\n"
            "          options: --no-install-recommends\n") in pill
    assert "        run: pnpm --dir web/app-pill exec playwright install chromium webkit\n" in pill
    assert pill.index("install-deps --dry-run") < pill.index(APT_ACTION) < pill.index("playwright install chromium")
    diagnostic = JOB(workflows["ci.yml"], "client-interrupt-diagnostic")
    assert APT_ACTION + "        with:\n          packages: strace\n" in diagnostic
    order = [action.index(text) for text in (
        "uses: actions/cache/restore@", "install.sh", "uses: actions/cache/save@")]
    assert order == sorted(order), "the cache must be restored before and saved after the install"
    assert "      if: steps.restore.outputs.cache-hit != 'true' && steps.install.outputs.source == 'network'\n" in action
    assert re.search(r"key: \$\{\{ steps\.identity\.outputs\.key \}\}", action)
    assert 'key=$prefix${ImageVersion:-unknown}' in action and "${ImageOS:-unknown}" in action
    # A new runner image falls back to the newest cache for the same request.
    prefix = re.search(r'^        prefix="(.+)"$', action, re.M)
    assert prefix and "ImageVersion" not in prefix.group(1) and "$request" in prefix.group(1), \
        "the restore prefix must name the request but not the runner image version"
    assert "        restore-keys: ${{ steps.identity.outputs.prefix }}\n" in action


def run_apt_install(cache, bin_dir, log, **env):
    environment = dict(os.environ, PATH=f"{bin_dir}:{os.environ['PATH']}", APT_LOG=str(log),
                       GITHUB_OUTPUT=str(log.with_suffix(".out")), **env)
    log.write_text("")
    log.with_suffix(".out").write_text("")
    subprocess.run([str(ROOT / ".github/actions/apt-packages/install.sh"), str(cache), "zsh"],
                   env=environment, check=True, stdout=subprocess.DEVNULL)
    return log.read_text().splitlines(), log.with_suffix(".out").read_text()


def check_apt_install(script):
    with tempfile.TemporaryDirectory(prefix="mesh-apt-") as scratch:
        root = Path(scratch)
        bin_dir = root / "bin"
        bin_dir.mkdir()
        for name, body in (("sudo", FAKE_SUDO), ("apt-get", FAKE_APT)):
            (bin_dir / name).write_text(body)
            (bin_dir / name).chmod(0o755)
        cache, log = root / "cache", root / "apt.log"
        calls, output = run_apt_install(cache, bin_dir, log)
        assert [call.split()[0] for call in calls] == ["update", "install"], calls
        assert "--no-download" not in calls[1] and "APT::Keep-Downloaded-Packages=true" in calls[1]
        assert output == "source=network\n", output
        assert (cache / "lists/archive_Packages").exists() and (cache / "archives/zsh.deb").exists()
        # A warm cache, whether saved on this runner image or restored through
        # restore-keys from an older one, installs with apt's downloads
        # disabled and never refreshes the lists: no archive contact at all.
        calls, output = run_apt_install(cache, bin_dir, log)
        assert len(calls) == 1 and calls[0].startswith("install -y --no-download "), calls
        assert "Acquire::http::Proxy=http://127.0.0.1:9" in calls[0] and calls[0].endswith(" zsh"), calls
        assert f"Dir::Cache::Archives={cache}/archives" in calls[0], calls
        assert output == "source=cache\n", output
        # An older image's cache that no longer covers the request (its .deb
        # versions are gone from the lists) falls back to the archive and
        # reports network, which saves a fresh cache under the exact key.
        calls, output = run_apt_install(cache, bin_dir, log, APT_OFFLINE_SHORT="1")
        assert [call.split()[0] for call in calls] == ["install", "update", "install"], calls
        assert output == "source=network\n", output
    assert "--no-download" in script


def reject_apt_mutations(workflows, action):
    ci = workflows["ci.yml"]
    mutations = (
        ("ci.yml", ci.replace("      - name: Install zsh\n" + APT_ACTION + "        with:\n          packages: zsh\n",
                              "      - run: sudo apt-get update && sudo apt-get install -y zsh\n", 1)),
        ("ci.yml", ci.replace("exec playwright install chromium webkit", "exec playwright install --with-deps chromium webkit", 1)),
        ("ci.yml", ci.replace("          options: --no-install-recommends\n", "", 1)),
        ("action", action.replace("steps.install.outputs.source == 'network'", "always()", 1)),
        ("action", action.replace("        restore-keys: ${{ steps.identity.outputs.prefix }}\n", "", 1)),
        ("action", action.replace('-$request-"', '-$request-${ImageVersion:-unknown}-"', 1)),
    )
    for target, mutated in mutations:
        assert mutated != (action if target == "action" else ci), f"apt mutation did not apply to {target}"
        try:
            if target == "action":
                check_apt_workflows(workflows, mutated)
            else:
                check_apt_workflows(dict(workflows, **{target: mutated}), action)
        except AssertionError:
            continue
        raise AssertionError(f"live-archive apt install accepted in {target}")


def check_mac_job(source, name, route=MAC_ROUTE):
    job = JOB(source, name)
    assert re.findall(r"^    runs-on: (.+)$", job, re.M) == [route], f"{name} must use its reviewed guarded Mac route"
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
            check_mac_job(workflows[name], job, MAC_SHARED_ROUTE if (name, job) in MAC_SHARED_JOBS else MAC_ROUTE)
    for name, source in workflows.items():
        # Signature verification's --deny-self-hosted-runners is not a route.
        routes = [line for line in source.splitlines() if "runs-on:" in line and "self-hosted" in line]
        assert all(line in ("    runs-on: " + MAC_ROUTE, "    runs-on: " + MAC_SHARED_ROUTE) for line in routes), \
            f"{name} has an unguarded self-hosted route"
        expected = 0 if name in HOSTED_ONLY else len(MAC_JOBS.get(name, ()))
        assert len(routes) == expected, f"{name} must route exactly its reviewed Darwin legs to the Mac"


def reject_mac_routing(workflows):
    route = "github.event.pull_request.head.repo.full_name == github.repository"
    mutations = (
        ("ci.yml", lambda text: text.replace(" || " + route, "", 1)),
        ("ci.yml", lambda text: text.replace("\"mesh-mac\"", "\"other\"", 1)),
        ("ci.yml", lambda text: text.replace("&& 55 || 10", "&& 10 || 10", 1)),
        ("helper-recovery.yml", lambda text: text.replace("cache: " + MAC_GO_CACHE, "cache: true", 1)),
        ("update-proofs.yml", lambda text: text.replace("    runs-on: " + MAC_SHARED_ROUTE, "    runs-on: ${{ matrix.runner }}", 1)),
        ("release.yml", lambda text: text.replace("    runs-on: ${{ matrix.runner }}", "    runs-on: " + MAC_ROUTE, 1)),
        # A leg with machine-wide fixtures must never run beside other Mac jobs.
        ("ci.yml", lambda text: text.replace("    runs-on: " + MAC_ROUTE, "    runs-on: " + MAC_SHARED_ROUTE, 1)),
        ("helper-recovery.yml", lambda text: text.replace("    runs-on: " + MAC_ROUTE, "    runs-on: " + MAC_SHARED_ROUTE, 1)),
        ("release.yml", lambda text: text.replace("    runs-on: ${{ matrix.runner }}", "    runs-on: " + MAC_SHARED_ROUTE, 1)),
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
        # Exclusive mesh-mac legs wait so main goes first; update proofs use only the
        # shared runners, whose three slots leave room for main, so they start at once.
        if name in MAC_TURN_FREE:
            assert needs == ["changes"], f"{name} must not hold its shared Mac legs behind main CI"
        else:
            assert needs == ["[changes, mac-turn]"], f"{name} must wait for its Mac turn"


class FakeGitHub:
    def __init__(self, answers):
        self.answers = list(answers)
        self.endpoints = []

    def __call__(self, endpoint):
        self.endpoints.append(endpoint)
        answer = self.answers[0] if len(self.answers) == 1 else self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return {"workflow_runs": [{"id": 7, "status": "queued"}] if answer else []}


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
    ACTIVE = wait["ACTIVE"]
    pr = {"EVENT": "pull_request", "DRAFT": "false", "HEAD_REPOSITORY": "ShaulLavo/mesh", "REPOSITORY": "ShaulLavo/mesh"}
    for environment in (dict(pr, EVENT="push"), dict(pr, EVENT="workflow_dispatch"),
                        dict(pr, HEAD_REPOSITORY="someone/mesh"), dict(pr, DRAFT="true")):
        status, endpoints, sleeps, _ = run_wait(wait["wait"], environment, [True])
        assert (status, endpoints, sleeps) == (0, [], []), environment
    status, endpoints, sleeps, _ = run_wait(wait["wait"], pr, [False])
    assert status == 0 and sleeps == [] and len(endpoints) == len(ACTIVE)
    assert set(ACTIVE) >= {"queued", "in_progress", "waiting", "requested", "pending"}
    for endpoint, active in zip(endpoints, ACTIVE):
        assert endpoint == f"repos/ShaulLavo/mesh/actions/workflows/ci.yml/runs?branch=main&status={active}&per_page=20", endpoint
    # Busy for two polls of every status, then idle: the PR waits, then runs.
    busy = [True] * len(ACTIVE) * 2 + [False]
    status, _, sleeps, out = run_wait(wait["wait"], pr, busy)
    assert status == 0 and len(sleeps) == 2 and "waiting for main CI: 7 (queued)" in out
    # An unreadable API counts as busy, never as idle.
    status, _, sleeps, _ = run_wait(wait["wait"], pr, [RuntimeError("HTTP 502")] + [False] * len(ACTIVE))
    assert status == 0 and len(sleeps) == 1
    # Past the hang cap a PR proceeds with a warning.
    status, _, sleeps, out = run_wait(wait["wait"], pr, [True], minutes=3)
    assert status == 0 and len(sleeps) == 3 and "::warning::" in out


def reject_mac_turn(ci, script):
    mutations = (
        (ci.replace("  socket-portability:\n    needs: [changes, mac-turn]", "  socket-portability:\n    needs: changes", 1), script),
        (ci.replace("  helper-recovery:\n    needs: [changes, mac-turn]", "  helper-recovery:\n    needs: changes", 1), script),
        # Holding the shared-runner proofs behind main again wastes the shared slots.
        (ci.replace("its Mac turn.\n    needs: changes\n", "its Mac turn.\n    needs: [changes, mac-turn]\n", 1), script),
        (ci.replace("    timeout-minutes: 200\n", "    timeout-minutes: 20\n", 1), script),
        (ci.replace("    name: Wait for main CI before Mac legs\n", "    name: Wait for main CI before Mac legs\n    continue-on-error: true\n", 1), script),
        (ci, script.replace('return environment.get("HEAD_REPOSITORY") == environment.get("REPOSITORY")', "return False", 1)),
        (ci, script.replace("branch=main&", "branch=topic&", 1)),
        (ci, script.replace('runs = [f"unknown ({error})"]', "runs = []", 1)),
        (ci, script.replace('ACTIVE = ("queued", "in_progress", ', 'ACTIVE = (', 1)),
    )
    for mutated, mutated_script in mutations:
        assert (mutated, mutated_script) != (ci, script), "Mac turn mutation did not apply"
        try:
            check_mac_turn(mutated)
            check_wait_script(mutated_script)
        except AssertionError:
            continue
        raise AssertionError("an unsafe Mac turn was accepted")


def actions_value(expression, context):
    # GitHub's && and || return an operand, like Python's and/or.
    python = expression.replace("&&", " and ").replace("||", " or ")
    python = re.sub(r"!(?!=)", " not ", python)
    python = re.sub(r"\bgithub\.([\w.]+)", lambda match: repr(context.get(match.group(1))), python)
    assert re.fullmatch(r"[\s\w'.=!()\-/]*", python), python
    return eval(python, {"__builtins__": {}})  # noqa: S307 - workflow expression from the repository


def concurrency(ci, context):
    block = re.search(r"^concurrency:\n  group: (.+)\n  cancel-in-progress: (.+)\n", ci, re.M)
    assert block, "CI must declare one top-level concurrency group"
    group = re.sub(r"\$\{\{ (.+?) \}\}", lambda match: str(actions_value(match.group(1), context)), block.group(1))
    cancel = actions_value(re.fullmatch(r"\$\{\{ (.+) \}\}", block.group(2)).group(1), context)
    return group, bool(cancel)


def check_concurrency(ci):
    def run(event, ref, run_id, number=None):
        return {"workflow": "CI", "event_name": event, "ref": ref, "run_id": run_id, "event.pull_request.number": number}

    first, second = run("push", "refs/heads/main", 101), run("push", "refs/heads/main", 102)
    main = concurrency(ci, first)
    # Main pushes share one group and never cancel a running publish run, so
    # GitHub keeps the running one and replaces only a waiting one.
    assert main == concurrency(ci, second) and main[1] is False, main
    pr = concurrency(ci, run("pull_request", "refs/pull/7/merge", 103, 7))
    assert pr == concurrency(ci, run("pull_request", "refs/pull/7/merge", 104, 7)) and pr[1] is True, pr
    assert pr[0] != concurrency(ci, run("pull_request", "refs/pull/8/merge", 105, 8))[0]
    manual = {concurrency(ci, run("workflow_dispatch", "refs/heads/main", run_id)) for run_id in (106, 107)}
    assert len(manual) == 2 and all(cancel is False for _, cancel in manual), manual
    assert main[0] not in {pr[0]} | {group for group, _ in manual}


def reject_concurrency(ci):
    old_group = "  group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}\n"
    new_group = re.search(r"^  group: .+\n", ci, re.M).group(0)
    mutations = (
        ci.replace(new_group, old_group, 1),
        ci.replace("  cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
                   "  cancel-in-progress: ${{ github.event_name == 'pull_request' || github.ref == 'refs/heads/main' }}", 1),
        ci.replace("github.event_name == 'push' && github.ref == 'refs/heads/main' && 'main'",
                   "github.ref == 'refs/heads/main' && 'main'", 1),
    )
    for mutated in mutations:
        assert mutated != ci, "concurrency mutation did not apply"
        try:
            check_concurrency(mutated)
        except AssertionError:
            continue
        raise AssertionError("an unsafe CI concurrency was accepted")


PROVE_MATRIX_STEP = ("          printf 'prove=%s\\n' \"$(python3 scripts/prepare-release-baselines.py "
                     "--prove-matrix \"$shards\" \"$DRAFT\")\" >> \"$GITHUB_OUTPUT\"\n")


def check_darwin_smoke(workflows):
    """A PR proves one Darwin shard; the release path still proves all of them."""
    proofs, release = workflows["update-proofs.yml"], workflows["release.yml"]
    policy = JOB(proofs, "policy")
    assert "      prove: ${{ steps.select.outputs.prove }}\n" in policy
    assert PROVE_MATRIX_STEP in policy and "          DRAFT: ${{ inputs.draft }}\n" in policy, "the proof matrix must follow the draft input"
    prove = JOB(proofs, "prove")
    assert "      matrix: ${{ fromJSON(needs.policy.outputs.prove) }}\n" in prove
    assert re.findall(r"^    name: (.+)$", prove, re.M) == ["prove (${{ matrix.platform }}, ${{ matrix.shard }})"]
    # The reader requires the complete proof matrix, so it runs on the release path only.
    assert re.findall(r"^    if: (.+)$", JOB(proofs, "reader"), re.M) == ["inputs.draft"], "the reader must run only for a release"
    caller = JOB(release, "prove-transition")
    assert "    uses: ./.github/workflows/update-proofs.yml\n" in caller and "      draft: true\n" in caller, \
        "the release path must prove every Darwin shard"
    assert not re.search(r"^      draft:", JOB(workflows["ci.yml"], "update-proofs"), re.M), "PR proofs must not claim the release matrix"


def reject_darwin_smoke(workflows):
    mutations = (
        ("release.yml", lambda text: text.replace("      draft: true\n", "      draft: false\n", 1)),
        ("update-proofs.yml", lambda text: text.replace("one Darwin shard.\n    if: inputs.draft\n", "one Darwin shard.\n", 1)),
        ("update-proofs.yml", lambda text: text.replace('"$shards" "$DRAFT"', '"$shards" false', 1)),
        ("update-proofs.yml", lambda text: text.replace("fromJSON(needs.policy.outputs.prove)", "fromJSON(needs.policy.outputs.shards)", 1)),
    )
    for name, mutate in mutations:
        mutated = dict(workflows, **{name: mutate(workflows[name])})
        assert mutated[name] != workflows[name], f"Darwin smoke mutation did not apply to {name}"
        try:
            check_darwin_smoke(mutated)
        except AssertionError:
            continue
        raise AssertionError(f"Darwin smoke wiring accepted a mutation of {name}")


# macOS mktemp replaces only trailing X's: /tmp/ms-XXXXXX.noindex is one fixed
# path, so parallel Mac jobs collide on it and a leftover blocks every later job.
MKTEMP_TEMPLATE = re.compile(r"\bmktemp\b[^\n|;)]*?([^\s\"'()]*X{3,}[^\s\"'()]*)")


def mktemp_sources():
    paths = sorted((ROOT / ".github").rglob("*.yml")) + sorted((ROOT / "scripts").rglob("*.sh")) + sorted((ROOT / "integration").rglob("*.sh"))
    return {str(path.relative_to(ROOT)): path.read_text() for path in paths}


def check_mktemp_templates(sources):
    found = 0
    for name, text in sources.items():
        for template in MKTEMP_TEMPLATE.findall(text):
            found += 1
            assert re.search(r"X{3,}$", template), f"{name}: mktemp template {template} must end in its X's; macOS fills only trailing X's"
    assert found, "no mktemp templates found; the scan is broken"


def reject_mktemp_templates(sources):
    name = ".github/workflows/update-proofs.yml"
    mutated = sources[name].replace('$(mktemp -u /tmp/ms-XXXXXX).noindex && mkdir -m 700 "$scratch"', "$(mktemp -d /tmp/ms-XXXXXX.noindex)", 1)
    assert mutated != sources[name], "mktemp mutation did not apply"
    try:
        check_mktemp_templates(dict(sources, **{name: mutated}))
    except AssertionError:
        return
    raise AssertionError("a mktemp template with a suffix after its X's was accepted")


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    check_concurrency(ci)
    reject_concurrency(ci)
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
    check_darwin_smoke(workflows)
    reject_darwin_smoke(workflows)
    sources = mktemp_sources()
    check_mktemp_templates(sources)
    reject_mktemp_templates(sources)
    action = (ROOT / ".github/actions/apt-packages/action.yml").read_text()
    check_apt_workflows(workflows, action)
    reject_apt_mutations(workflows, action)
    check_apt_install((ROOT / ".github/actions/apt-packages/install.sh").read_text())
    print("PASS independent tests, blocking aggregates, main publication after skipped ancestors, failure/cancel/PR refusal, guarded exclusive and shared Mac routing, one Darwin proof shard on PRs and all on releases, PR Mac legs behind main CI, offline apt cache, one never-cancelled main CI group and mutation controls")


if __name__ == "__main__":
    main()
