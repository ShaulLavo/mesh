#!/usr/bin/env python3
"""Execute the ordinary test verdicts and reject scheduling regressions."""

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
STATUS = CONTRACT["status"]
CODE = "needs.changes.outputs.code == 'true'"
MAIN_PUSH = "github.event_name == 'push' && github.ref == 'refs/heads/main'"
MAC_ROUTE = "${{ matrix.runner == 'macos-15' && " + MAIN_PUSH + " && fromJSON('[\"self-hosted\",\"macOS\",\"ARM64\",\"mesh-mac\"]') || matrix.runner }}"
MAC_FIFO_MINUTES = 45
MAC_JOBS = {"ci.yml": ("native",)}
MAC_GO_CACHE = "${{ runner.environment == 'github-hosted' }}"
INSTALLED_TIMEOUT = "needs.changes.outputs.installed == 'true' && 80 || "


def check_phase(ci, name, command):
    phase = JOB(ci, name)
    assert re.findall(r"^    if: (.+)$", phase, re.M) == [CODE]
    assert re.findall(r"^    timeout-minutes: (.+)$", phase, re.M) == ["20"]
    assert re.findall(r"^    runs-on: (.+)$", phase, re.M) == ["ubuntu-24.04"]
    assert re.findall(r"^    needs: (.+)$", phase, re.M) == ["changes"], "test phases must start independently"
    assert not re.search(r"^        if:|continue-on-error:|strategy:", phase, re.M)
    assert len(re.findall(r"uses: actions/setup-go@", phase)) == 1
    assert "          go-version-file: go.mod\n          cache: true\n" in phase
    assert len(re.findall(rf"^\s+(?:- )?run: {re.escape(command)}$", ci, re.M)) == 1
    assert f"        run: {command}\n" in phase
    return phase


def check_release_cadence(ci, release):
    assert "\n  publish:\n" not in ci and "uses: ./.github/workflows/release.yml" not in ci, \
        "CI must not trigger release publication on every push to main"
    triggers = release[:release.index("\nconcurrency:\n")]
    assert "\n  workflow_dispatch:\n" in triggers, \
        "release.yml must trigger on workflow_dispatch"
    assert "\n  schedule:" not in triggers and "cron:" not in triggers, \
        "release.yml must not run on a schedule; releases happen only when the owner asks"
    assert "\n  push:" not in triggers and "\n  workflow_call:" not in triggers, \
        "release.yml must not trigger on push or workflow_call"
    assert "\nconcurrency:\n  group: mesh-stable-publication\n  queue: max\n" in release, \
        "release.yml must queue publications so a second manual release cannot cancel a queued one"
    reserve = JOB(release, "reserve")
    assert re.findall(r"^    if: (.+)$", reserve, re.M) == ["github.ref == 'refs/heads/main'"], \
        "release reservation must run only on refs/heads/main"
    assert "ref: ${{ github.sha }}" in reserve, \
        "release reservation must execute from the trusted workflow revision, not an unverified input ref"
    assert "WORKFLOW_SHA: ${{ github.sha }}" in reserve
    for name in ("publish", "publication-native", "finalize"):
        assert "github.ref == 'refs/heads/main'" in JOB(release, name), \
            f"{name} must remain restricted to refs/heads/main"


def check_schedule(ci, release):
    race = check_phase(ci, "race", "go test -race ./...")
    integration = check_phase(ci, "integration", "./scripts/verify.sh")
    assert "      - name: Require a tidy module graph\n" in race
    assert "          go mod tidy\n          git diff --exit-code -- go.mod go.sum\n" in race
    assert "          python3 scripts/test-private-boundary.py\n          scripts/check-private-hosting-boundary.sh\n" in race
    assert "      - name: Install zsh\n" + APT_ACTION + "        with:\n          packages: zsh\n" in integration
    assert re.findall(r"^          (MESH_INTEGRATION_\w+): (.+)$", integration, re.M) == [
        ("MESH_INTEGRATION_TIMEOUT", "60s")]
    assert "python3 -B scripts/test-ci-scheduling.py" in JOB(ci, "quality")
    assert "mac-turn" not in ci and "wait-for-main-ci.py" not in ci
    check_release_cadence(ci, release)


def check_mutations(ci, release):
    ci_mutations = (
        ci.replace("  integration:\n    needs: changes", "  integration:\n    needs: race", 1),
        ci.replace("          cache: true", "          cache: false"),
        ci.replace("go test -race ./...", "go test -race ./internal/cli", 1),
        ci.replace("        run: ./scripts/verify.sh", "        run: ./scripts/verify.sh\n      - run: ./scripts/verify.sh", 1),
        ci.replace("        run: ./scripts/verify.sh", "        if: false\n        run: ./scripts/verify.sh", 1),
        ci + "\n  publish:\n    uses: ./.github/workflows/release.yml\n",
    )
    for mutated in ci_mutations:
        assert mutated != ci, "negative control did not change CI workflow"
        try:
            check_schedule(mutated, release)
        except AssertionError:
            continue
        raise AssertionError("unsafe CI schedule accepted")
    release_mutations = (
        release.replace("  workflow_dispatch:\n", "  push:\n    branches: [main]\n  workflow_dispatch:\n", 1),
        release.replace("  workflow_dispatch:\n", "  workflow_call:\n  workflow_dispatch:\n", 1),
        release.replace("  workflow_dispatch:\n", "  schedule:\n    - cron: '0 2,10,18 * * *'\n  workflow_dispatch:\n", 1),
        release.replace("  queue: max\n", "  cancel-in-progress: false\n", 1),
        release.replace("    if: github.ref == 'refs/heads/main'\n", "", 1),
        release.replace("          ref: ${{ github.sha }}\n", "          ref: ${{ inputs.source_sha }}\n", 1),
    )
    for mutated in release_mutations:
        assert mutated != release, "negative control did not change release workflow"
        try:
            check_schedule(ci, mutated)
        except AssertionError:
            continue
        raise AssertionError("unsafe release cadence accepted")


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


def check_mac_job(source, name, route, guard):
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
    prefix = r"\$\{\{ " + re.escape("matrix.runner == 'macos-15' && " + guard)
    hosted_timeout = r"(\d+)"
    if name == "native":
        hosted_timeout = r"\(" + re.escape(INSTALLED_TIMEOUT) + hosted_timeout + r"\)"
    match = re.fullmatch(prefix + r" && (\d+) \|\| " + hosted_timeout + r" \}\}", timeouts[0])
    assert match, f"{name} must preserve its hosted and self-hosted hang caps"
    assert match, f"{name} must bound the Mac and hosted legs separately"
    mac, hosted = (int(value) for value in match.groups())
    assert mac == hosted + MAC_FIFO_MINUTES, f"{name} Mac hang cap must add the FIFO wait to the hosted bound"


def check_mac_routing(workflows):
    for name, jobs in MAC_JOBS.items():
        for job in jobs:
            check_mac_job(workflows[name], job, MAC_ROUTE, MAIN_PUSH)
    for job in ("prepare", "prove"):
        section = JOB(workflows["update-proofs.yml"], job)
        assert re.findall(r"^    runs-on: (.+)$", section, re.M) == ["${{ matrix.runner }}"], \
            f"update-proofs {job} must use GitHub-hosted capacity"
    for name, source in workflows.items():
        routes = [line for line in source.splitlines() if "runs-on:" in line and "self-hosted" in line]
        assert all(line == "    runs-on: " + MAC_ROUTE for line in routes), \
            f"{name} has an unguarded self-hosted route"
        expected = len(MAC_JOBS.get(name, ()))
        assert len(routes) == expected, f"{name} must route exactly its reviewed Darwin legs to the Mac"


def reject_mac_routing(workflows):
    mutations = (
        ("ci.yml", lambda text: text.replace(MAC_ROUTE, MAC_ROUTE.replace(MAIN_PUSH, "true"), 1)),
        ("ci.yml", lambda text: text.replace('"mesh-mac"', '"other"', 1)),
        ("ci.yml", lambda text: text.replace("&& 85 ||", "&& 40 ||", 1)),
        ("ci.yml", lambda text: text.replace(INSTALLED_TIMEOUT, "", 1)),
        ("ci.yml", lambda text: text.replace("cache: " + MAC_GO_CACHE, "cache: true", 1)),
        ("update-proofs.yml", lambda text: text.replace("    runs-on: ${{ matrix.runner }}", "    runs-on: " + MAC_ROUTE, 1)),
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


PROVE_MATRIX_STEP = '          matrix=$(python3 scripts/prepare-release-baselines.py --prove-matrix "$shards" "$DRAFT")\n'


def check_release_proof_matrix(workflows):
    """The release path still proves every selected shard, including Darwin."""
    proofs, release = workflows["update-proofs.yml"], workflows["release.yml"]
    policy = JOB(proofs, "policy")
    assert "      prove: ${{ steps.select.outputs.prove }}\n" in policy
    assert PROVE_MATRIX_STEP in policy and "          DRAFT: ${{ inputs.draft }}\n" in policy, "the proof matrix must follow the draft input"
    prove = JOB(proofs, "prove")
    assert "      matrix: ${{ fromJSON(needs.policy.outputs.prove) }}\n" in prove
    assert re.findall(r"^    name: (.+)$", prove, re.M) == ["prove (${{ matrix.platform }}, ${{ matrix.shard }})"]
    reader = STEP(JOB(proofs, "reader"), "Decode generated assets with the unchanged published executable").split("\n      - ", 1)[0]
    assert "if: steps.proof-uploads.outcome == 'success' && inputs.draft\n" in reader, "require the release batches before the reader"
    caller = JOB(release, "prove-transition")
    assert "    uses: ./.github/workflows/update-proofs.yml\n" in caller and "      draft: true\n" in caller, \
        "the release path must require its complete proof batches"
    assert "uses: ./.github/workflows/update-proofs.yml" not in workflows["ci.yml"], "PRs must not run the release proof matrix"


def reject_release_proof_mutations(workflows):
    mutations = (
        ("release.yml", lambda text: text.replace("      draft: true\n", "      draft: false\n", 1)),
        ("update-proofs.yml", lambda text: text.replace("name: Decode generated assets with the unchanged published executable\n        if: steps.proof-uploads.outcome == 'success' && inputs.draft", "name: Decode generated assets with the unchanged published executable", 1)),
        ("update-proofs.yml", lambda text: text.replace('"$shards" "$DRAFT"', '"$shards" false', 1)),
        ("update-proofs.yml", lambda text: text.replace("fromJSON(needs.policy.outputs.prove)", "fromJSON(needs.policy.outputs.shards)", 1)),
    )
    for name, mutate in mutations:
        mutated = dict(workflows, **{name: mutate(workflows[name])})
        assert mutated[name] != workflows[name], f"Darwin smoke mutation did not apply to {name}"
        try:
            check_release_proof_matrix(mutated)
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
    workflows = {path.name: path.read_text() for path in sorted((ROOT / ".github/workflows").glob("*.yml"))}
    ci, release = workflows["ci.yml"], workflows["release.yml"]
    check_concurrency(ci)
    reject_concurrency(ci)
    check_schedule(ci, release)
    check_mutations(ci, release)
    check_mac_routing(workflows)
    reject_mac_routing(workflows)
    check_release_proof_matrix(workflows)
    reject_release_proof_mutations(workflows)
    sources = mktemp_sources()
    check_mktemp_templates(sources)
    reject_mktemp_templates(sources)
    action = (ROOT / ".github/actions/apt-packages/action.yml").read_text()
    check_apt_workflows(workflows, action)
    reject_apt_mutations(workflows, action)
    check_apt_install((ROOT / ".github/actions/apt-packages/install.sh").read_text())
    print("PASS independent tests, blocking aggregates, decoupled batched release cadence, failure/cancel/PR refusal, guarded exclusive and shared Mac routing, all Darwin proof shards on release, hosted PR Mac legs, offline apt cache, one never-cancelled main CI group and mutation controls")


if __name__ == "__main__":
    main()
