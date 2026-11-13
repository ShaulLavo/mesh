"""One disposable Linux C4 group-2 observation, never release acceptance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess

from fixtures import fleet_baselines, installed_adoption, installed_catalog_evidence, installed_cohort
from fixtures import installed_native, installed_origin, installed_paths, installed_releases, native_services, release_worker_groups

C4 = "c0c899eea10a9c4f5916b0b629b736455d1d5a92"
C4_RECORD = "e55620b6729be489c697bec1030ca354f23dea11f33c05efa283d20a66aad086"
BASELINE = "940d638ea8ea5be29f17c1ab311aedd4039be2f6a221bd1f28a4ae2f1aee9eda"
REPO = Path(__file__).resolve().parents[1]


def require_source(checkpoint):
    def git(*arguments):
        return subprocess.check_output(["git", *arguments], cwd=REPO, text=True, timeout=10).strip()
    if git("rev-parse", "--is-shallow-repository") != "false":
        raise RuntimeError("diagnostic source requires complete Git history")
    if git("rev-parse", "HEAD") != checkpoint or git("status", "--porcelain"):
        raise RuntimeError("diagnostic source differs from its clean reviewed checkpoint")
    subprocess.run(["git", "merge-base", "--is-ancestor", C4, checkpoint], cwd=REPO, check=True, timeout=10)
    expected = os.environ.get("GOROOT")
    if not expected or not (Path(expected) / "bin/go").is_file():
        raise RuntimeError("diagnostic compiler requires an explicit installed GOROOT")
    actual = subprocess.check_output(["go", "env", "GOROOT"], text=True, timeout=10).strip()
    if Path(actual).resolve() != Path(expected).resolve():
        raise RuntimeError("diagnostic go command and explicit GOROOT differ")


def exact_group(registry, binary, c4_record):
    raw = Path(c4_record).read_bytes()
    if hashlib.sha256(raw).hexdigest() != C4_RECORD:
        raise RuntimeError("diagnostic C4 failure record differs")
    prior = json.loads(raw)
    groups = installed_adoption.installed_groups(registry, "v0.1.249", binary, binary)
    if len(groups) != 3:
        raise RuntimeError("diagnostic complete registry no longer partitions as C4")
    group = groups[2]
    selected = [(item["version"], item["artifact"]["binarySha256"], item["protocol"]) for item in group["members"]]
    expected = [(item["version"], item["digest"], item["protocol"]) for item in prior["workers"]]
    if selected != expected or len(selected) != 9 or prior["original"]["digest"] != BASELINE:
        raise RuntimeError("diagnostic must retain all nine actual C4 group-2 workers and baseline")
    return group


def observe_case(root, output, host, pair, previous, preparer, origin, releases, group):
    (output / "case-attempted.json").write_text('{"nativeSetupMayStart":true}\n')
    case = installed_adoption.AdoptionCase(root, host, pair, preparer, origin, releases, "adopt", previous)
    case.worker_group = group
    record = {"host": host["name"], "scenario": "adopt", "passed": False, "diagnosticOnly": True,
              "boundary": "original before-install verification only", "group": release_worker_groups.public_group(group)}
    primary = None
    try:
        record["initial"] = case.prepare()
        case.seed_prior()
        record["postSeedDaemon"] = installed_catalog_evidence.daemon_identity(case)
        case.arm_catalog()
        case.seed()
        case.prove_catalog()
        record["observedCohort"] = case.start_cohort(releases.registry, releases.probe)
        case.route_observer.check()
        record["diagnosticOutcome"] = "original I/O returned; no installation or acceptance tested"
    except BaseException as error:
        primary = error
        record["error"] = str(error)
        raise
    finally:
        finish((lambda: installed_adoption.finish_case(case, record, output),), primary)


def run(arguments, root, output):
    host = next(host for host in fleet_baselines.load()["hosts"] if host["name"] == "pc")
    pair = fleet_baselines.acquire_pair(root / "public", host)
    if pair["daemon"]["artifact"]["binarySha256"] != BASELINE:
        raise RuntimeError("diagnostic published baseline differs from C4")
    previous = fleet_baselines.acquire(root / "public", "v0.1.248", {"os": "linux", "arch": "amd64"})
    releases = installed_releases.Releases(root, "linux/amd64", output)
    releases.registry = installed_cohort.stage(arguments.registry, root / "workers", {"os": "linux", "arch": "amd64"})
    releases.probe = arguments.probe.resolve(strict=True)
    group = exact_group(releases.registry, pair["daemon"]["executable"], arguments.c4_cohort)
    (output / "selected-group.json").write_bytes(release_worker_groups.encode(release_worker_groups.public_group(group)))
    primary = None
    try:
        releases.start_signer()
        origin = installed_origin.Origin(root)
        origin.serve_tree(root / "public")
        releases.check_budget()
        observe_case(root, output, host, pair, previous, arguments.native_helper.resolve(strict=True), origin, releases, group)
        releases.check_budget()
    except BaseException as error:
        primary = error
        raise
    finally:
        finish((releases.close, lambda: installed_adoption.retain_producer_logs(root, output)), primary)


def finish(actions, primary):
    try:
        installed_native.cleanup_all(actions)
    except BaseException as error:
        if primary is None:
            raise
        primary.add_note("diagnostic cleanup unconfirmed: " + type(error).__name__ + "; retain scoped evidence and scratch")


def interrupted(*_arguments):
    raise InterruptedError("focused diagnostic interrupted")


def cleanup(root, output):
    pairs = list(output.glob("*-native-pair.json"))
    clean = bool(pairs) or not (output / "case-attempted.json").exists()
    clean = clean and all(json.loads(path.read_text()).get("cleanup") == {"servicesAbsent": True, "processesAbsent": True} for path in pairs)
    if not clean:
        raise RuntimeError("diagnostic host cleanup unconfirmed; retain named scratch for parent")
    shutil.rmtree(root)
    (output / "scratch-cleanup.json").write_text(json.dumps({"root": str(root), "absent": not root.exists()}) + "\n")


def scoped_services(root):
    manager = native_services.Manager(root)
    result = {}
    for label in ("mesh", "mesh-update-helper"):
        manager.label = label
        result[label] = {"exists": manager.exists()}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("registry", "probe", "native-helper", "c4-cohort", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--source-checkpoint", required=True)
    arguments = parser.parse_args()
    if platform.system() != "Linux" or platform.machine() != "x86_64" or os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise RuntimeError("focused diagnostic requires its disposable GitHub-hosted Linux AMD64 runner")
    require_source(arguments.source_checkpoint)
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    os.environ["MESH_CATALOG_EVIDENCE"] = "1"
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    root = installed_paths.create()
    (output / "resources-before.json").write_text(json.dumps({"scratch": str(root), "preExistingInputs": [str(arguments.registry), str(arguments.probe), str(arguments.native_helper)],
        "createdResources": ["pc-adopt daemon/helper units", "original worker/shell/listener/client", "nine cohort workers/shells", "fixture signer/control socket", "task downloads and metadata"],
        "cleanupOwner": "parent owns any unconfirmed cleanup; named scratch retained on failure"}) + "\n")
    primary = None
    try:
        services = scoped_services(root)
        (output / "scoped-services-before.json").write_text(json.dumps(services) + "\n")
        if any(row["exists"] for row in services.values()):
            raise RuntimeError("diagnostic refuses pre-existing intended native unit names")
        free = shutil.disk_usage(root).free
        if free < 4 << 30:
            raise RuntimeError("focused diagnostic requires 4 GiB free before acquisition")
        run(arguments, root, output)
    except BaseException as error:
        primary = error
        raise
    finally:
        finish((lambda: cleanup(root, output), lambda: (output / "scoped-services-after.json").write_text(json.dumps(scoped_services(root)) + "\n")), primary)


if __name__ == "__main__":
    main()
