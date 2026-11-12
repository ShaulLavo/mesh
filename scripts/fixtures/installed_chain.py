"""Signed native hops with the exact original-source worker cohort retained."""
import json
import shutil
import time

from fixtures import installed_adoption, installed_native, installed_releases


def prepare(releases, origin, source):
    first, intermediate, final = installed_releases.VERSIONS
    registry = releases.registry
    releases.registry = None
    releases.build(first)
    proof = releases.prove(source, first)
    releases.publish(first, 0, [proof])
    releases.registry = registry
    releases.build(intermediate)
    proof = releases.prove(releases.builds[first]["path"], intermediate)
    releases.publish(intermediate, 1, [proof])
    # These are actual prior pinned source images, not published registry entries.
    releases.retain_custom_source(first)
    releases.build(final)
    proof = releases.prove(releases.builds[intermediate]["path"], final)
    releases.publish(final, 2, [proof], anchors_from=1)
    origin.serve_tree(releases.directory)
    origin.head(releases.directory, first)


def run_case(root, output, host, pair, preparer, origin, releases, previous, fail_final):
    scenario = "chain-rollback" if fail_final else "chain"
    case = installed_adoption.AdoptionCase(root, host, pair, preparer, origin, releases, scenario, previous)
    record = {"scenario": scenario, "passed": False, "supplementalSourceBuilds": True, "hops": []}
    first, intermediate, final = installed_releases.VERSIONS
    origin.head(releases.directory, first)
    try:
        record["initial"] = case.prepare()
        case.seed_prior()
        case.seed()
        start = case.start_adoption(first)
        record["bootstrap"] = case.validate_result(case.await_result(first), first, start["pin"])
        record["cohort"] = case.start_cohort(releases.registry, releases.probe)
        if fail_final:
            case.fault.write_text(json.dumps({"daemon": [final]}))
        origin.head(releases.directory, final)
        # A real daemon restart is the documented startup/wake discovery trigger.
        case.units.stop("daemon")
        case.units.start("daemon")
        await_chain(case, record, intermediate, final, fail_final)
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        finish(case, record, output)


def await_chain(case, record, intermediate, final, fail_final):
    deadline = time.monotonic() + 150
    seen = set()
    while time.monotonic() < deadline:
        journal = read_journal(case)
        version = journal.get("request", {}).get("manifest", {}).get("version")
        phase = journal.get("phase")
        if version == intermediate and phase == "committed" and intermediate not in seen and observe_hop(case, record, intermediate):
            seen.add(intermediate)
        if version == final and phase in ("committed", "rolled_back"):
            validate_terminal(case, record, journal, seen, intermediate, final, fail_final)
            return
        if phase == "rollback_failed":
            raise RuntimeError("native chain rollback failed")
        time.sleep(0.05)
    raise RuntimeError("signed native chain did not finish within its two real updater ticks and activation bound")


def read_journal(case):
    path = case.state / "update/installation.json"
    return json.loads(path.read_text()) if path.exists() else {}


def observe_hop(case, record, version):
    if not case.ready():
        return False
    receipt = json.loads((case.state / "update/helper/installed.json").read_text())
    if receipt["digest"] != case.releases.builds[version]["digest"]:
        return False
    pid = case.units.managers["helper"].manager_pid()
    image = installed_native.native_services.mapped_image(pid, receipt["executable"])
    if image["sha256"] != receipt["digest"]:
        raise RuntimeError("intermediate native helper executes another image")
    case.inspect_retained()
    digests = case.cohort.verify("native-hop-" + version)
    record["hops"].append({"version": version, "helper": image, "workers": digests})
    return True


def validate_terminal(case, record, journal, seen, intermediate, final, fail_final):
    expected_phase = "rolled_back" if fail_final else "committed"
    if journal["phase"] != expected_phase or intermediate not in seen:
        raise RuntimeError("chain did not install and observe its real intermediate or expected rollback")
    case.inspect_retained()
    record["finalWorkers"] = case.cohort.verify("native-final-" + expected_phase)
    running = case.control("host.info")["host"]["build"]
    expected = intermediate if fail_final else final
    if running["digest"] != case.releases.builds[expected]["digest"]:
        raise RuntimeError("chain ended on another executing image")
    case.origin.ledger(case.state, case.root / "chain-ledger.json")
    ledger = json.loads((case.root / "chain-ledger.json").read_text())
    commits = {item["version"]: item for item in ledger["entries"]}
    for version in (intermediate,) if fail_final else (intermediate, final):
        entry = commits.get(version) or {}
        metadata = case.releases.directory / version / "mesh-update.json"
        if entry.get("origin") != "host" or entry.get("metadata") != installed_releases.digest(metadata.read_bytes()):
            raise RuntimeError("native unattended ledger lacks its exact signed metadata pin")
        if entry.get("workersPreserved") != case.expected_workers:
            raise RuntimeError("native unattended ledger lost an exact retained worker")
    record["running"], record["journalPhase"] = running, expected_phase


def finish(case, record, output):
    try:
        case.close()
        record["cleanup"] = {"servicesAbsent": True, "processesAbsent": True}
    except BaseException as error:
        record["passed"] = False
        record["cleanupError"] = str(error)
        raise
    finally:
        installed_native.retain_logs(case.root, output, case.host["name"])
        retain_chain_result(case, output)
        (output / (case.host["name"] + "-native-pair.json")).write_text(json.dumps(record, indent=2) + "\n")


def retain_chain_result(case, output):
    for name in ("cohort.json", "chain-ledger.json"):
        path = case.root / name
        if path.exists():
            shutil.copy2(path, output / (case.host["name"] + "-" + name))
