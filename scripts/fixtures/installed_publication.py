"""Native consumers of actual main-only draft bytes and their real signature."""
import errno
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from types import SimpleNamespace

from fixtures import draft_intake, installed_clock, fleet_baselines, installed_adoption, installed_chain, installed_fresh, installed_native, installed_cohort, publication_origin, published_releases, release_worker_index, release_worker_completion, release_tool, release_documents


def require_main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise RuntimeError("real draft installation requires an approved main-only hosted publication caller")


def stage(root, output, native, version, source):
    require_main()
    directory = Path(os.environ["MESH_PUBLICATION_DRAFT"])
    release = draft_intake.verify(directory, version, source, signed=True, run=int(os.environ["MESH_PUBLICATION_RUN"]))
    if directory.name != version:
        raise ValueError("publication intake directory must name its exact draft version")
    required = 4 << 30
    free = shutil.disk_usage(root).free
    (output / "publication-storage.json").write_text(json.dumps({"root": str(root), "freeBytes": free, "requiredBytes": required}) + "\n")
    if free < required:
        raise RuntimeError("native runner lacks space for real draft assets and installed scratch")
    (output / "draft-intake.json").write_text(json.dumps(release, sort_keys=True) + "\n")
    logical = root / "logical" / version
    logical.parent.mkdir()
    draft_intake.documents(directory, version, source, logical, int(os.environ["MESH_PUBLICATION_RUN"]))
    return load_draft(root, output, logical, native, version, source, release["html_url"], directory)


def load_draft(root, output, directory, native, version, source, url, physical):
    manifest = json.loads((directory / "mesh-release.json").read_text())
    metadata = json.loads((directory / "mesh-update.json").read_text())
    if manifest["version"] != version or manifest["commit"] != source or metadata["version"] != version:
        raise RuntimeError("real draft metadata or manifest differs from its exact source/version pin")
    if published_releases.digest((directory / "mesh-release.json").read_bytes()) != metadata["manifest"]:
        raise RuntimeError("real signed metadata does not bind its exact release manifest")
    subjects = [directory / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh-workers.json", "mesh-worker-groups.json")]
    subjects.extend(physical / item["archive"] for item in manifest["artifacts"])
    subjects.append(physical / release_documents.BUNDLE)
    verify_signature(root, output, directory / "mesh-update.sig.json", subjects)
    release_worker_index.validate_index(directory / "mesh-worker-groups.json", manifest)
    platform = dict(zip(("os", "arch"), native.split("/")))
    artifact = next(item for item in manifest["artifacts"] if item["platform"] == platform)
    binary = root / "real-candidate"
    binary.write_bytes(published_releases.archive_binary((physical / artifact["archive"]).read_bytes(), artifact))
    binary.chmod(0o755)
    environment = os.environ | {"MESH_STATE_DIR": str(root / "inspect-state"), "MESH_CONFIG_DIR": str(root / "inspect-config")}
    build = json.loads(subprocess.check_output([str(binary), "version", "--json"], env=environment, timeout=installed_clock.timeout()))
    if build["modified"] or build["commit"] != source or build["version"] != version or build["digest"] != artifact["binarySha256"] or build["platform"] != platform:
        raise RuntimeError("actual unmodified native draft image differs from its authenticated manifest")
    harness = os.environ["MESH_HARNESS_SOURCE"]
    release_worker_completion.validate_harness(json.loads((output / "harness-source.json").read_bytes()), harness)
    context = release_worker_completion.release_context(directory)
    verified = {"url": url, "version": version, "source": source, "build": build,
        "manifest": metadata["manifest"], "metadata": published_releases.digest((directory / "mesh-update.json").read_bytes()),
        "workerGroups": published_releases.digest((directory / "mesh-worker-groups.json").read_bytes()),
        "installedHarness": harness, "testTrustRoot": False, "firstPublishedUpdater": fleet_baselines.load()["firstPublishedUpdater"],
        "routeContract": context["routeContract"]}
    release_worker_completion.validate_draft(verified, context, native, harness)
    (output / "real-draft.json").write_text(json.dumps(verified, indent=2) + "\n")
    return SimpleNamespace(root=root, directory=directory.parent, physical_directory=physical.parent, native=native, verification=verified,
                           builds={version: build | {"path": str(binary)}})


def verify_signature(root, output, envelope, subjects):
    verifier = release_tool.configured()
    cache = os.environ.get("MESH_PUBLICATION_TRUST_CACHE", str(root / "production-trust"))
    environment = os.environ | {"MESH_RELEASE_TRUST_CACHE": cache}
    with (output / "real-signature-verification.log").open("wb") as log:
        subprocess.run([str(verifier), "verify", str(envelope), *map(str, subjects)], env=environment,
                       stdout=log, stderr=subprocess.STDOUT, check=True, timeout=installed_clock.timeout())


def refresh_trust(verifier, environment, log):
    for _pass in range(4):
        result = subprocess.run([str(verifier), "refresh", environment["MESH_RELEASE_TRUST_CACHE"]], env=environment,
                                stdout=log, stderr=subprocess.STDOUT, check=False, timeout=installed_clock.timeout())
        if result.returncode != 75:
            break
    if result.returncode:
        raise RuntimeError("production signature trust refresh did not finish its bounded passes")


class PortFault:
    def __init__(self, case, version):
        self.case, self.version = case, version
        self.stopping, self.thread, self.socket = threading.Event(), None, None
        self.bound, self.released, self.error = False, False, ""

    def start(self):
        self.thread = threading.Thread(target=self.run, name="owned-activation-port-fault")
        self.thread.start()

    def run(self):
        try:
            self.observe()
        except Exception as error:
            self.error = str(error)
        finally:
            if self.socket:
                self.socket.close()

    def observe(self):
        deadline = time.monotonic() + installed_clock.timeout()
        while not self.stopping.is_set() and time.monotonic() < deadline:
            journal = installed_chain.read_journal(self.case)
            if self.bound and journal.get("phase") == "rolling_back":
                self.released = True
                return
            self.try_bind(journal)
            self.stopping.wait(0.005)

    def try_bind(self, journal):
        version = journal.get("request", {}).get("manifest", {}).get("version")
        if self.bound or version != self.version or journal.get("phase") not in ("activating", "validating"):
            return
        connection = socket.socket()
        connection.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            connection.bind(("127.0.0.1", self.case.port))
            connection.listen(5)
        except OSError as error:
            connection.close()
            if error.errno != errno.EADDRINUSE:
                raise
            return
        self.socket, self.bound = connection, True

    def close(self):
        self.stopping.set()
        if self.thread:
            self.thread.join(timeout=2)
        if self.thread and self.thread.is_alive():
            raise RuntimeError("owned activation fault observer did not stop")


class DraftCase(installed_adoption.AdoptionCase):
    def validate_result(self, result, version, pin):
        # Production artifacts have no unpublished first-start marker hook.
        if self.scenario == "rollback" and result.get("details", {}).get("phase") != "rolled-back":
            raise RuntimeError("actual draft activation did not roll back after the owned native port fault")
        successful = self.scenario != "rollback"
        if result.get("pin") != pin or successful and (not result.get("confirmed") or result.get("details", {}).get("phase") != "adopted"):
            raise RuntimeError("actual draft adoption lacks its real pinned confirmation")
        running = self.control("host.info")["host"]["build"]
        expected = self.releases.builds[version]["digest"] if successful else self.pair["daemon"]["artifact"]["binarySha256"]
        if running["digest"] != expected:
            raise RuntimeError("real draft success/rollback executes another image")
        milestone = result.get("adopted") or {}
        if successful and (milestone.get("workersPreserved") != self.expected_workers or milestone.get("metadata") != pin):
            raise RuntimeError("real draft confirmation lost its original worker")
        attachment = self.inspect_retained()
        helper = self.validate_helper(version)
        if self.prior and installed_chain.read_journal(self)["settings"] != self.prior["settings"]:
            raise RuntimeError("real draft adoption changed its genuine prior installation settings")
        return {"running": running, "helper": helper, "phase": result["details"]["phase"], "confirmed": result.get("confirmed"),
                "worker": self.worker, "shell": self.shell, "client": self.terminal.pid, "listener": self.listener,
                "route": self.route_observer.snapshot(), "attachment": attachment}

    def interrupt_real_helper(self, version):
        if self.adoption_status().get("confirmed"):
            raise RuntimeError("draft adoption completed before helper interruption; no interruption proof recorded")
        receipt = json.loads((self.state / "update/helper/installed.json").read_text())
        pid = self.units.managers["helper"].manager_pid()
        image = installed_native.native_services.mapped_image(pid, receipt["executable"])
        if image["sha256"] != self.releases.builds[version]["digest"] or receipt["digest"] != image["sha256"]:
            raise RuntimeError("draft replacement helper has an unverified native image")
        os.kill(pid, signal.SIGKILL)
        return {"killedNativePID": pid, "mappedImage": image, "confirmedBeforeInterruption": False}


def prove(root, output, native, version, source, preparer, registry=None, probe=None, *, task_id=None):
    releases = stage(root, output, native, version, source)
    directory = releases.directory / version
    inventory = release_worker_completion.task_inventory(directory, native, os.environ["MESH_HARNESS_SOURCE"]) if task_id is not None else None
    task = release_worker_completion.select_task(inventory, task_id) if inventory else None
    inspector = release_tool.configured().parent / "publication-reader"
    if not inspector.is_file():
        raise ValueError("publication requires the once-built reviewed H inspector")
    origin = publication_origin.Origin(root, releases.physical_directory, inspector)
    try:
        origin.start()
        origin.head(releases.directory, version)
        if task is None:
            run_pairs(root, output, native, version, preparer, releases, origin)
            run_fresh(root, output, version, releases, origin)
            run_first_published(root, output, version, releases, origin, registry, probe)
        else:
            run_task(root, output, version, preparer, releases, origin, registry, probe, task)
    finally:
        (output / "publication-transport-cleanup.json").write_text(json.dumps(origin.close(), indent=2) + "\n")
    if task is None:
        release_worker_completion.record(root, output, native, version, source)
        return
    context = release_worker_completion.release_context(directory)
    selection = json.loads((directory / "mesh-worker-groups.json").read_bytes())["selection"]
    release_worker_completion.record_task(output, inventory, task, context, selection)


def run_task(root, output, version, preparer, releases, origin, registry, probe, task):
    contract = releases.verification["routeContract"]
    (output / "first-published-updater.json").write_text(json.dumps(contract, indent=2) + "\n")
    if task.get("case") == "fresh-managed-native-pair.json":
        run_fresh(root, output, version, releases, origin)
        return
    if "case" not in task:
        run_first_published(root, output, version, releases, origin, registry, probe, task=task)
        return
    matches = [(host, scenario) for host in fleet_baselines.load()["hosts"] for scenario in
               ("publication", "rollback", "publication-interrupt") if host["platform"] == releases.native
               and host["name"] + "-" + scenario + "-native-pair.json" == task["case"]]
    if len(matches) != 1:
        raise ValueError("publication task does not select one exact native legacy case")
    host, scenario = matches[0]
    pair = fleet_baselines.acquire_pair(root / "public", host)
    prior_version = "v0.1." + str(int(host["daemon"].split(".")[-1]) - 1)
    previous = fleet_baselines.acquire(root / "public", prior_version, pair["daemon"]["artifact"]["platform"])
    run_pair(root, output, host, pair, preparer, releases, origin, previous, version, scenario)


def run_pairs(root, output, native, version, preparer, releases, origin):
    hosts = [host for host in fleet_baselines.load()["hosts"] if host["platform"] == native]
    for host in hosts:
        pair = fleet_baselines.acquire_pair(root / "public", host)
        prior_version = "v0.1." + str(int(host["daemon"].split(".")[-1]) - 1)
        previous = fleet_baselines.acquire(root / "public", prior_version, pair["daemon"]["artifact"]["platform"])
        for scenario in ("publication", "rollback", "publication-interrupt"):
            run_pair(root, output, host, pair, preparer, releases, origin, previous, version, scenario)


def run_pair(root, output, host, pair, preparer, releases, origin, previous, version, scenario):
    case = DraftCase(root, host, pair, preparer, origin, releases, scenario, previous)
    fault = PortFault(case, version)
    record = {"passed": False, "realDraft": version, "testTrustRoot": False, "scenario": scenario}
    try:
        record["initial"] = case.prepare()
        case.seed_prior()
        case.seed()
        original = {"version": host["daemon"], "platform": pair["daemon"]["artifact"]["platform"],
                    "digest": pair["daemon"]["artifact"]["binarySha256"], "protocol": pair["daemon"]["manifest"]["compatibility"]["workerWrite"]}
        record["publication"] = release_worker_completion.publication_case(case, releases, scenario, original)
        if scenario == "rollback":
            fault.start()
        started = case.start_adoption(version)
        if scenario == "publication-interrupt":
            record["interruption"] = case.interrupt_real_helper(version)
        record["result"] = case.validate_result(case.await_result(version), version, started["pin"])
        if scenario == "rollback" and (not fault.bound or not fault.released or fault.error):
            raise RuntimeError("real draft native activation fault did not reach and release its actual rollback boundary")
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        finish_fault_case(case, record, output, fault)


def run_fresh(root, output, version, releases, origin):
    case = installed_fresh.FreshCase(root, origin, releases, version=version, publication=True)
    record = {"passed": False, "realDraft": version, "testTrustRoot": False, "scenario": "fresh", "adoptionSeededPolicy": False}
    try:
        record["initial"] = case.prepare()
        case.seed()
        original = release_worker_completion.source_identity(releases.verification["build"])
        record["publication"] = release_worker_completion.publication_case(case, releases, "fresh", original)
        record["baselineInitialization"] = case.initialize_preserved_baseline()
        case.snapshots = case.retained()
        record["restart"] = case.restart_control()
        case.inspect_retained()
        record["running"] = case.control("host.info")["host"]["build"]
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        installed_fresh.finish(case, output, record)


def run_first_published(root, output, version, releases, origin, registry, probe, *, task=None):
    contract = releases.verification["routeContract"]
    first = contract["first"]
    (output / "first-published-updater.json").write_text(json.dumps(contract, indent=2) + "\n")
    if first is None:
        return
    if registry is None or probe is None:
        raise RuntimeError("first published updater route requires the authoritative native worker registry and observer")
    platform = dict(zip(("os", "arch"), releases.native.split("/")))
    baseline = fleet_baselines.acquire(root / "public", first, platform)
    environment = os.environ | {"MESH_STATE_DIR": str(root / "baseline-inspect-state"), "MESH_CONFIG_DIR": str(root / "baseline-inspect-config")}
    build = json.loads(subprocess.check_output([str(baseline["executable"]), "version", "--json"], env=environment, timeout=installed_clock.timeout()))
    if build["modified"] or build["digest"] != baseline["artifact"]["binarySha256"] or build["version"] != first:
        raise RuntimeError("original published updater image differs from its published baseline")
    releases.builds[first] = build | {"path": str(baseline["executable"])}
    owned_registry = installed_cohort.stage(registry, root / "published-workers", platform)
    groups = installed_adoption.installed_groups(owned_registry, first, baseline["executable"], baseline["executable"])
    if task is not None:
        selected = [group for group in groups if group["index"] == task["group"]["index"]]
        if len(selected) != 1 or release_worker_completion.groups.public_group(selected[0]) != task["group"]:
            raise ValueError("native publication group differs from its signed inventory")
        run_published_groups(root, output, first, version, releases, origin, owned_registry, probe,
                             task["rollback"], selected, task["scenario"], task["via"], complete=False)
        return
    for name, rollback, via in release_worker_completion.route_scenarios(contract):
        run_published_groups(root, output, first, version, releases, origin, owned_registry, probe, rollback, groups, name, via)


def run_published_groups(root, output, first, target, releases, origin, registry, probe, rollback, groups, name, via, *, complete=True):
    for group in groups:
        directory = output / "groups" / name / str(group["index"])
        directory.mkdir(parents=True)
        case_root = published_route(root, directory, first, target, releases, origin, registry, probe, rollback, group, name, via)
        records = list(directory.glob("*-native-pair.json"))
        if len(records) != 1:
            raise RuntimeError("published group completion is missing")
        result = json.loads(records[0].read_bytes())
        required = release_worker_completion.route_labels(result)
        installed_adoption.validate_installed_group(directory, group, required,
            {"digest": releases.builds[first]["digest"], "protocol": releases.builds[first]["workerProtocol"]})
        shutil.rmtree(case_root)
    if complete:
        release_worker_completion.route_companion(output, releases.verification["routeContract"], name, rollback, via, groups)


def published_route(root, output, first, target, releases, origin, registry, probe, rollback, group, name, via):
    case = installed_fresh.FreshCase(root, origin, releases, version=first, publication=True, name=name)
    case.worker_group = group
    fault = PortFault(case, target)
    record = {"passed": False, "firstPublishedUpdater": first, "realDraft": target, "testTrustRoot": False,
              "rollback": rollback, "via": via, "routeContract": releases.verification["routeContract"], "updates": []}
    origin.head(releases.directory, first)
    failure = None
    try:
        record["initial"] = case.prepare()
        case.seed()
        original = release_worker_completion.source_identity(releases.builds[first])
        record["publication"] = release_worker_completion.publication_case(case, releases, name, original)
        record["baselineInitialization"] = case.initialize_preserved_baseline()
        record["cohort"] = case.start_cohort(registry, probe)
        policy = subprocess.run([str(case.binary), "update", "policy", "set", "--expected-revision", "0", "--json"],
                                env=case.environment, capture_output=True, text=True, check=True, timeout=installed_clock.timeout())
        record["explicitPolicy"] = json.loads(policy.stdout)
        if record["explicitPolicy"].get("mode") != "automatic" or record["explicitPolicy"].get("revision") != 1:
            raise RuntimeError("original published updater did not accept its explicit automatic policy")
        case.snapshots = case.retained()
        deadline = time.monotonic() + installed_clock.timeout()
        if via is not None:
            record["updates"].append(run_published_update(case, via, fault, False, deadline))
        record["updates"].append(run_published_update(case, target, fault, rollback, deadline))
        if rollback:
            previous = installed_chain.read_journal(case)["request"]["current"]["version"]
            installed_chain.verify_failed_restart(case, record["updates"][-1], previous, target)
            verify_restarted_published_helper(case, record["updates"][-1], previous)
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        failure = error
        try:
            retain_publication_failure(case, record, output, error)
        except BaseException as retention_error:
            error.add_note("publication failure retention failed: " + str(retention_error)[:512])
        raise
    finally:
        finish_fault_case(case, record, output, fault, failure)
    return case.root


def run_published_update(case, target, fault, rollback, deadline):
    case.origin.head(case.releases.directory, target)
    plan = read_route_plan(case, target)
    source = case.control("host.info")["host"]["build"]
    direct = plan["next"]["version"] == target
    record = {"sourceBuild": source, "rollback": rollback, "routeKind": "direct" if direct else "multihop",
              "expectedHops": [] if direct else [plan["next"]], "hops": [],
              "authenticatedTarget": plan["target"], "routePlans": [plan]}
    if rollback:
        fault.start()
    case.units.stop("daemon")
    case.units.start("daemon")
    try:
        await_published_route(case, record, source["version"], target, fault, rollback, deadline=deadline)
    except BaseException:
        case.published_failed_update = record
        raise
    return record


def retain_publication_failure(case, record, output, primary):
    evidence = record["failureState"] = {"lastHelperSample": getattr(case, "helper_sample", None),
        "verifiedHelper": getattr(case, "helper_verified", None), "files": {}}
    record["failedUpdate"] = getattr(case, "published_failed_update", None)
    for relative in ("update/installation.json", "updates/host-update.json", "update/helper/installed.json",
                     "update/helper/promotion.json", "update/helper/activation-pending"):
        destination = output / (case.host["name"] + "-failure-" + relative.replace("/", "-"))
        try:
            evidence["files"][relative] = installed_adoption.retain_failure_state_file(case.state / relative, destination)
        except BaseException as error:
            evidence["files"][relative] = {"retentionError": str(error)[:512]}
            primary.add_note("publication failure retention: " + str(error)[:512])
    try:
        pid = case.units.managers["helper"].manager_pid()
        evidence["managerPID"] = pid
        retain_helper_mapping(pid, evidence, output, case.host["name"])
        receipt = json.loads((case.state / "update/helper/installed.json").read_bytes())
        evidence["mappedHelper"] = installed_native.native_services.mapped_image(pid, receipt["executable"])
    except BaseException as error:
        evidence["mappingError"] = str(error)[:512]
        evidence["mapping"] = getattr(error, "mapped_image", None)
        primary.add_note("publication failure helper observation: " + str(error)[:512])


def retain_helper_mapping(pid, evidence, output, name):
    if sys.platform == "darwin":
        result = installed_native.native_services.command(["/usr/sbin/lsof", "-a", "-p", str(pid), "-d", "txt", "-F", "finD"], check=False)
        evidence["nativeMapping"] = {"stdout": result.stdout[:65536], "stderr": result.stderr[:4096], "returncode": result.returncode,
            "truncated": len(result.stdout) > 65536 or len(result.stderr) > 4096}
        return
    path = Path(f"/proc/{pid}/exe")
    with path.open("rb") as stream:
        status = os.fstat(stream.fileno())
        if status.st_size > 128 << 20:
            raise RuntimeError("failing helper image exceeds the 128 MiB observation bound")
        evidence["nativeMapping"] = {"path": os.readlink(path), "device": status.st_dev, "inode": status.st_ino,
            "bytes": status.st_size, "sha256": hashlib.file_digest(stream, "sha256").hexdigest()}
        if status.st_size > 1 << 20:
            return
        stream.seek(0)
        destination = output / (name + "-failure-helper-image")
        destination.write_bytes(stream.read((1 << 20) + 1))
        evidence["nativeMapping"]["retainedImage"] = destination.name


def finish_fault_case(case, record, output, fault, primary=None):
    try:
        close_fault_case(case, record, output, fault)
    except BaseException as error:
        if primary is None:
            raise
        primary.add_note("publication cleanup failed: " + str(error))
        raise primary from error


def close_fault_case(case, record, output, fault):
    try:
        fault.close()
    except BaseException as error:
        record["passed"], record["faultCleanupError"] = False, str(error)
        raise
    finally:
        installed_adoption.finish_case(case, record, output)


def verify_restarted_published_helper(case, record, previous):
    helper = case.mapped_helper()
    if helper["sha256"] != case.releases.builds[previous]["digest"]:
        raise RuntimeError("restarted rollback helper differs from its authenticated restored release")
    record["failedRestart"]["helper"] = helper


def await_published_route(case, record, first, target, fault, rollback, *, deadline=None):
    if "expectedHops" not in record or not record.get("authenticatedTarget"):
        raise RuntimeError("original published route lacks its authenticated intended route")
    deadline, seen = deadline if deadline is not None else time.monotonic() + installed_clock.timeout(), set()
    while time.monotonic() < deadline:
        observe_published_hop(case, record, first, target, seen)
        ledger = installed_chain.read_ledger(case)
        journal = installed_chain.read_journal(case)
        terminal = journal.get("request", {}).get("manifest", {}).get("version") == target and journal.get("phase") == ("rolled_back" if rollback else "committed")
        if terminal and not ledger["state"].get("owned"):
            validate_published_route(case, record, journal, ledger, target, fault, rollback, seen, deadline=deadline)
            return
        time.sleep(0.05)
    raise RuntimeError("original published updater did not finish its real retained route within the existing chain bound")


def observe_published_hop(case, record, first, target, seen):
    if not case.ready():
        return
    build = case.control("host.info")["host"]["build"]
    if build["version"] in (first, target) or build["version"] in seen:
        return
    expected = next_expected_hop(record, build["version"])
    authenticate_hop(case, build, expected)
    receipt = json.loads((case.state / "update/helper/installed.json").read_text())
    if receipt["digest"] != build["digest"]:
        return
    pid = case.units.managers["helper"].manager_pid()
    try:
        image = installed_native.native_services.mapped_image(pid, receipt["executable"])
    except (OSError, RuntimeError):
        return
    os.kill(pid, signal.SIGSTOP)
    try:
        case.releases.builds[build["version"]] = build
        case.inspect_retained()
        if image["sha256"] != expected["binary"]:
            raise RuntimeError("published intermediate helper differs from its authenticated release")
        if case.units.managers["helper"].manager_pid() != pid or json.loads((case.state / "update/helper/installed.json").read_bytes()) != receipt:
            return
        case.helper_verified = {"receipt": receipt, "image": image}
        record["hops"].append({"expected": expected, "build": build, "helper": image,
                               "workers": case.cohort.verify(release_worker_completion.hop_label(record, build["version"]))})
        seen.add(build["version"])
        extend_expected_route(case, record, target)
    finally:
        os.kill(pid, signal.SIGCONT)


def validate_published_route(case, record, journal, ledger, target, fault, rollback, seen, *, deadline=None):
    verify_observed_route(record, seen)
    expected = journal["request"]["current"]["digest"] if rollback else record["authenticatedTarget"]["binary"]
    expected_helper = expected if rollback else record["authenticatedTarget"]["binary"]
    helper = case.mapped_helper(expected=expected_helper, deadline=deadline)
    if helper["sha256"] != expected_helper:
        raise RuntimeError("final published helper differs from the authenticated expected release image")
    build = case.control("host.info")["host"]["build"]
    if build["digest"] != expected or rollback and (not fault.bound or not fault.released or fault.error):
        raise RuntimeError("real published route did not restore or activate its exact native image")
    register_running_build(case, record, build, rollback)
    case.inspect_retained()
    if rollback:
        verify_published_failure(case, journal, ledger, target)
        record["activationFault"] = {"bound": fault.bound, "released": fault.released, "error": fault.error}
    if not rollback:
        pin = record["authenticatedTarget"]["metadata"]
        committed = [entry for entry in ledger["entries"] if entry["version"] == target and entry["metadata"] == pin and entry["workersPreserved"] == case.expected_workers]
        if not committed:
            raise RuntimeError("real published updater lacks its exact pinned preserved-worker commit")
    record["final"] = {"build": build, "ledger": ledger, "journal": journal, "route": case.route_observer.snapshot(),
                       "workers": case.cohort.verify(release_worker_completion.final_label(record)), "helper": helper}


def register_running_build(case, record, build, rollback):
    # inspect_retained checks the running daemon against releases.builds. A
    # published via head (an earlier real release) is known only through its
    # authenticated route plan, so it is registered here, after authentication
    # and before inspection. A restored daemon must already be a verified build.
    # A version recorded independently keeps that record: the daemon's own
    # report must match it rather than replace it.
    known = case.releases.builds.get(build["version"])
    if rollback:
        if known is None or known["digest"] != build["digest"]:
            raise RuntimeError("restored published daemon is not a previously verified release")
        return
    authenticate_hop(case, build, record["authenticatedTarget"])
    if known is None:
        case.releases.builds[build["version"]] = build
        return
    if known["digest"] != build["digest"]:
        raise RuntimeError("published daemon differs from its independently recorded release")


def read_route_plan(case, target):
    result = subprocess.run([str(case.binary), "update", "--local", "--check", "--version", target, "--json"],
                            env=case.environment, capture_output=True, text=True, check=True, timeout=installed_clock.timeout())
    plan = json.loads(result.stdout)
    current = case.control("host.info")["host"]["build"]
    manifest = case.origin.asset("/ShaulLavo/mesh/releases/download/" + target + "/mesh-release.json").read_bytes()
    if plan["currentDigest"] != current["digest"] or not plan.get("next"):
        raise RuntimeError("read-only authenticated route plan does not match the executing published source")
    if (plan["target"]["version"] != target or plan["target"]["metadata"] != logical_digest(case.releases, target, "mesh-update.json")
            or plan["target"]["manifest"] != logical_digest(case.releases, target, "mesh-release.json")
            or published_releases.digest(manifest) != plan["target"]["manifest"]):
        raise RuntimeError("authenticated route target differs from the actual signed draft")
    platform = dict(zip(("os", "arch"), case.releases.native.split("/")))
    artifacts = [item for item in json.loads(manifest)["artifacts"] if item["platform"] == platform]
    if len(artifacts) != 1 or artifacts[0]["binarySha256"] != plan["target"]["binary"]:
        raise RuntimeError("authenticated route target names another native image")
    return plan


def next_expected_hop(record, version):
    index = len(record["hops"])
    if index >= len(record["expectedHops"]) or record["expectedHops"][index]["version"] != version:
        raise RuntimeError("published route executed an unintended or unplanned intermediate")
    return record["expectedHops"][index]


def authenticate_hop(case, build, expected):
    manifest = case.origin.asset("/ShaulLavo/mesh/releases/download/" + expected["version"] + "/mesh-release.json").read_bytes()
    if (published_releases.digest(manifest) != expected["manifest"]
            or logical_digest(case.releases, expected["version"], "mesh-release.json") != expected["manifest"]
            or logical_digest(case.releases, expected["version"], "mesh-update.json") != expected["metadata"]):
        raise RuntimeError("observed intermediate bytes differ from the authenticated route plan")
    value = json.loads(manifest)
    platform = dict(zip(("os", "arch"), case.releases.native.split("/")))
    artifacts = [item for item in value["artifacts"] if item["platform"] == platform]
    if len(artifacts) != 1 or artifacts[0]["binarySha256"] != expected["binary"]:
        raise RuntimeError("authenticated intermediate manifest names another native image")
    if build["version"] != expected["version"] or build["digest"] != expected["binary"] or build["commit"] != value["commit"] or build["modified"] or build["platform"] != platform:
        raise RuntimeError("executing intermediate differs from its authenticated release identity")


def logical_digest(releases, version, name):
    local = releases.directory / version / name
    if local.is_file():
        return published_releases.digest(local.read_bytes())
    directory = releases.directory / releases.verification["version"]
    history = json.loads((directory / "mesh-retention.json").read_bytes())
    matches = [asset["sha256"] for item in history if item["tag"] == version for asset in item["assets"] if asset["name"] == name]
    if len(matches) != 1:
        raise ValueError("published route requires its exact authenticated logical retention reference")
    return matches[0]


def extend_expected_route(case, record, target):
    plan = read_route_plan(case, target)
    if release_identity(plan["target"]) != release_identity(record["authenticatedTarget"]):
        raise RuntimeError("authenticated final target changed between published hops")
    record["routePlans"].append(plan)
    if plan["next"]["version"] != target:
        record["expectedHops"].append(plan["next"])


def release_identity(value):
    return {key: value[key] for key in ("version", "metadata", "manifest", "binary")}


def verify_observed_route(record, seen):
    try:
        release_worker_completion.validate_observed_update(record, seen)
    except ValueError as error:
        raise RuntimeError(str(error)) from error


def verify_published_failure(case, journal, ledger, target):
    edge = {"from": journal["request"]["current"]["digest"], "to": case.releases.builds[target]["digest"],
            "platform": journal["request"]["current"]["platform"]}
    if not any(item["edge"] == edge and item["version"] == target and item["phase"] == "rolled_back" for item in ledger["failed"]):
        raise RuntimeError("published rollback lacks its exact durable failed edge")
