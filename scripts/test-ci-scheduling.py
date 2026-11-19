#!/usr/bin/env python3
"""Execute the ordinary test verdicts and reject scheduling regressions."""

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
        ci.replace("timeout-minutes: 20", "timeout-minutes: 21", 1),
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


def check_apt_workflows(workflows, action):
    """Every hosted apt install goes through the cached action, which tries
    the cache offline before it ever contacts the archive."""
    for name, source in workflows.items():
        assert not re.search(r"apt-get|apt install|--with-deps|install-deps(?! --dry-run)", source), \
            f"{name} installs from the live Ubuntu archive; use ./.github/actions/apt-packages"
    pill = JOB(workflows["ci.yml"], "app-pill")
    assert "playwright install-deps --dry-run chromium webkit\n" in pill
    assert '[ "${arguments[*]:0:3}" != "install -s --no-install-recommends" ]' in pill
    assert ("      - name: Install confirmation browser system packages\n        timeout-minutes: 5\n" + APT_ACTION
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
        # A warm cache installs with apt's downloads disabled and never
        # refreshes the lists: the archive is not contacted at all.
        calls, output = run_apt_install(cache, bin_dir, log)
        assert len(calls) == 1 and calls[0].startswith("install -y --no-download "), calls
        assert "Acquire::http::Proxy=http://127.0.0.1:9" in calls[0] and calls[0].endswith(" zsh"), calls
        assert f"Dir::Cache::Archives={cache}/archives" in calls[0], calls
        assert output == "source=cache\n", output
        # A cache that does not cover the request falls back to the archive.
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


def main():
    ci = (ROOT / ".github/workflows/ci.yml").read_text()
    recovery = (ROOT / ".github/workflows/helper-recovery.yml").read_text()
    check_schedule(ci, recovery)
    check_mutations(ci, recovery)
    CONTRACT["check_mutations"](ci, recovery)
    workflows = {path.name: path.read_text() for path in sorted((ROOT / ".github/workflows").glob("*.yml"))}
    check_mac_routing(workflows)
    reject_mac_routing(workflows)
    action = (ROOT / ".github/actions/apt-packages/action.yml").read_text()
    check_apt_workflows(workflows, action)
    reject_apt_mutations(workflows, action)
    check_apt_install((ROOT / ".github/actions/apt-packages/install.sh").read_text())
    print("PASS independent tests, blocking aggregates, main publication after skipped ancestors, failure/cancel/PR refusal, guarded Mac routing, offline apt cache and mutation controls")


if __name__ == "__main__":
    main()
