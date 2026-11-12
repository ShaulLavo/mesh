"""Native consumers of actual main-only draft bytes and their real signature."""
import errno
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import threading
import time
from types import SimpleNamespace

from fixtures import fleet_baselines, installed_adoption, installed_chain, installed_fresh, installed_native, installed_cohort, publication_origin, published_releases


def require_main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise RuntimeError("real draft installation requires an approved main-only hosted publication caller")


def stage(root, output, native, version, source):
    require_main()
    if not re.fullmatch(r"v\d+\.\d+\.\d+", version) or not re.fullmatch(r"[0-9a-f]{40}", source):
        raise RuntimeError("publication requires an exact real draft version and source SHA")
    result = subprocess.run(["gh", "release", "view", version, "--repo", "ShaulLavo/mesh", "--json", "isDraft,tagName,assets,url"],
                            capture_output=True, text=True, check=False, timeout=30)
    if result.returncode:
        raise RuntimeError("required main-only signed draft does not exist or is not readable yet")
    release = json.loads(result.stdout)
    if release["isDraft"] is not True or release["tagName"] != version:
        raise RuntimeError("publication consumer requires the reserved unpublished real draft")
    selected = [asset for asset in release["assets"] if asset["name"].endswith(".json") or re.fullmatch(r"mesh_.*\.tar\.gz", asset["name"])]
    if any(Path(asset["name"]).name != asset["name"] for asset in selected):
        raise RuntimeError("real release contains a non-local asset name")
    required = sum(asset["size"] for asset in selected) + (4 << 30)
    free = shutil.disk_usage(root).free
    (output / "publication-storage.json").write_text(json.dumps({"root": str(root), "freeBytes": free, "requiredBytes": required}) + "\n")
    if not selected or free < required:
        raise RuntimeError("native runner lacks space for real draft assets and installed scratch")
    directory = root / "releases" / version
    directory.mkdir(parents=True)
    downloaded = subprocess.run(["gh", "release", "download", version, "--repo", "ShaulLavo/mesh", "--pattern", "*.json", "--pattern", "mesh_*.tar.gz", "--dir", str(directory)],
                                capture_output=True, check=False, timeout=90)
    if downloaded.returncode:
        raise RuntimeError("real signed draft asset acquisition failed")
    return load_draft(root, output, directory, native, version, source, release["url"])


def load_draft(root, output, directory, native, version, source, url):
    manifest = json.loads((directory / "mesh-release.json").read_text())
    metadata = json.loads((directory / "mesh-update.json").read_text())
    if manifest["version"] != version or manifest["commit"] != source or metadata["version"] != version:
        raise RuntimeError("real draft metadata or manifest differs from its exact source/version pin")
    if published_releases.digest((directory / "mesh-release.json").read_bytes()) != metadata["manifest"]:
        raise RuntimeError("real signed metadata does not bind its exact release manifest")
    subjects = [directory / name for name in ("mesh-release.json", "mesh-update.json", "mesh-retention.json", "mesh-workers.json")]
    subjects.extend(directory / item["archive"] for item in manifest["artifacts"])
    verify_signature(root, output, directory / "mesh-update.sig.json", subjects)
    platform = dict(zip(("os", "arch"), native.split("/")))
    artifact = next(item for item in manifest["artifacts"] if item["platform"] == platform)
    binary = root / "real-candidate"
    binary.write_bytes(published_releases.archive_binary((directory / artifact["archive"]).read_bytes(), artifact))
    binary.chmod(0o755)
    environment = os.environ | {"MESH_STATE_DIR": str(root / "inspect-state"), "MESH_CONFIG_DIR": str(root / "inspect-config")}
    build = json.loads(subprocess.check_output([str(binary), "version", "--json"], env=environment, timeout=10))
    if build["modified"] or build["commit"] != source or build["version"] != version or build["digest"] != artifact["binarySha256"] or build["platform"] != platform:
        raise RuntimeError("actual unmodified native draft image differs from its authenticated manifest")
    (output / "real-draft.json").write_text(json.dumps({"url": url, "version": version, "source": source, "build": build,
        "manifest": metadata["manifest"], "metadata": published_releases.digest((directory / "mesh-update.json").read_bytes()),
        "testTrustRoot": False, "firstPublishedUpdater": fleet_baselines.load()["firstPublishedUpdater"]}, indent=2) + "\n")
    return SimpleNamespace(root=root, directory=directory.parent, native=native, builds={version: build | {"path": str(binary)}})


def verify_signature(root, output, envelope, subjects):
    verifier = root / "real-release-verifier"
    subprocess.run(["go", "build", "-o", str(verifier), "./cmd/mesh-release-sign"], check=True, timeout=180)
    cache = os.environ.get("MESH_PUBLICATION_TRUST_CACHE", str(root / "production-trust"))
    environment = os.environ | {"MESH_RELEASE_TRUST_CACHE": cache}
    with (output / "real-signature-verification.log").open("wb") as log:
        refresh_trust(verifier, environment, log)
        subprocess.run([str(verifier), "verify", str(envelope), *map(str, subjects)], env=environment,
                       stdout=log, stderr=subprocess.STDOUT, check=True, timeout=60)


def refresh_trust(verifier, environment, log):
    for _pass in range(4):
        result = subprocess.run([str(verifier), "refresh", environment["MESH_RELEASE_TRUST_CACHE"]], env=environment,
                                stdout=log, stderr=subprocess.STDOUT, check=False, timeout=60)
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
        deadline = time.monotonic() + 90
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
        self.inspect_retained()
        helper = self.validate_helper(version)
        if self.prior and installed_chain.read_journal(self)["settings"] != self.prior["settings"]:
            raise RuntimeError("real draft adoption changed its genuine prior installation settings")
        return {"running": running, "helper": helper, "phase": result["details"]["phase"], "confirmed": result.get("confirmed"),
                "worker": self.worker, "shell": self.shell, "client": self.terminal.pid, "listener": self.listener,
                "route": self.route_observer.snapshot()}

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


def prove(root, output, native, version, source, preparer, registry=None, probe=None):
    releases = stage(root, output, native, version, source)
    inspector = root / "publication-reader"
    subprocess.run(["go", "test", "-c", "-tags", "mesh_installed_fixture", "-o", str(inspector), "./internal/update"], check=True, timeout=180)
    origin = publication_origin.Origin(root, releases.directory, inspector)
    try:
        origin.start()
        origin.head(releases.directory, version)
        run_pairs(root, output, native, version, preparer, releases, origin)
        run_fresh(root, output, version, releases, origin)
        run_first_published(root, output, version, releases, origin, registry, probe)
    finally:
        (output / "publication-transport-cleanup.json").write_text(json.dumps(origin.close(), indent=2) + "\n")


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
    record = {"passed": False, "realDraft": version, "adoptionSeededPolicy": False}
    try:
        record["initial"] = case.prepare()
        case.seed()
        case.snapshots = case.retained()
        record["restart"] = case.restart_control()
        case.inspect_retained()
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        installed_fresh.finish(case, output, record)


def run_first_published(root, output, version, releases, origin, registry, probe):
    first = fleet_baselines.load()["firstPublishedUpdater"]
    record = {"firstPublishedUpdater": first, "available": first is not None, "realDraft": version}
    (output / "first-published-updater.json").write_text(json.dumps(record, indent=2) + "\n")
    if first is None:
        return
    if registry is None or probe is None:
        raise RuntimeError("first published updater route requires the authoritative native worker registry and observer")
    platform = dict(zip(("os", "arch"), releases.native.split("/")))
    baseline = fleet_baselines.acquire(root / "public", first, platform)
    environment = os.environ | {"MESH_STATE_DIR": str(root / "baseline-inspect-state"), "MESH_CONFIG_DIR": str(root / "baseline-inspect-config")}
    build = json.loads(subprocess.check_output([str(baseline["executable"]), "version", "--json"], env=environment, timeout=10))
    if build["modified"] or build["digest"] != baseline["artifact"]["binarySha256"] or build["version"] != first:
        raise RuntimeError("original published updater image differs from its published baseline")
    releases.builds[first] = build | {"path": str(baseline["executable"])}
    owned_registry = installed_cohort.stage(registry, root / "published-workers", platform)
    for rollback in (False, True):
        published_route(root, output, first, version, releases, origin, owned_registry, probe, rollback)


def published_route(root, output, first, target, releases, origin, registry, probe, rollback):
    name = "published-rollback" if rollback else "published"
    case = installed_fresh.FreshCase(root, origin, releases, version=first, publication=True, name=name)
    fault = PortFault(case, target)
    record = {"passed": False, "firstPublishedUpdater": first, "realDraft": target, "rollback": rollback, "hops": []}
    origin.head(releases.directory, first)
    try:
        record["initial"] = case.prepare()
        case.seed()
        record["cohort"] = case.start_cohort(registry, probe)
        policy = subprocess.run([str(case.binary), "update", "policy", "set", "--expected-revision", "0", "--json"],
                                env=case.environment, capture_output=True, text=True, check=True, timeout=15)
        record["explicitPolicy"] = json.loads(policy.stdout)
        if record["explicitPolicy"].get("mode") != "automatic" or record["explicitPolicy"].get("revision") != 1:
            raise RuntimeError("original published updater did not accept its explicit automatic policy")
        case.snapshots = case.retained()
        origin.head(releases.directory, target)
        plan = read_route_plan(case, target)
        if plan["next"]["version"] == target:
            raise RuntimeError("signed publication route is direct; the required intermediate/waypoint proof is unavailable")
        record["expectedHops"] = [plan["next"]]
        record["authenticatedTarget"] = plan["target"]
        record["routePlans"] = [plan]
        if rollback:
            fault.start()
        case.units.stop("daemon")
        case.units.start("daemon")
        await_published_route(case, record, first, target, fault, rollback)
        if rollback:
            previous = installed_chain.read_journal(case)["request"]["current"]["version"]
            installed_chain.verify_failed_restart(case, record, previous, target)
            verify_restarted_published_helper(case, record, previous)
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        finish_fault_case(case, record, output, fault)


def finish_fault_case(case, record, output, fault):
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


def await_published_route(case, record, first, target, fault, rollback):
    if not record.get("expectedHops") or not record.get("authenticatedTarget"):
        raise RuntimeError("original published route lacks its authenticated intended intermediate/waypoint")
    deadline, seen = time.monotonic() + 150, set()
    while time.monotonic() < deadline:
        observe_published_hop(case, record, first, target, seen)
        ledger = installed_chain.read_ledger(case)
        journal = installed_chain.read_journal(case)
        terminal = journal.get("request", {}).get("manifest", {}).get("version") == target and journal.get("phase") == ("rolled_back" if rollback else "committed")
        if terminal and not ledger["state"].get("owned"):
            validate_published_route(case, record, journal, ledger, target, fault, rollback, seen)
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
        case.inspect_retained()
        if image["sha256"] != expected["binary"]:
            raise RuntimeError("published intermediate helper differs from its authenticated release")
        record["hops"].append({"expected": expected, "build": build, "helper": image, "workers": case.cohort.verify("real-published-hop-" + build["version"])})
        seen.add(build["version"])
        case.releases.builds[build["version"]] = build
        extend_expected_route(case, record, target)
    finally:
        os.kill(pid, signal.SIGCONT)


def validate_published_route(case, record, journal, ledger, target, fault, rollback, seen):
    verify_observed_route(record, seen)
    case.inspect_retained()
    build = case.control("host.info")["host"]["build"]
    expected = journal["request"]["current"]["digest"] if rollback else case.releases.builds[target]["digest"]
    if build["digest"] != expected or rollback and (not fault.bound or not fault.released or fault.error):
        raise RuntimeError("real published route did not restore or activate its exact native image")
    helper = case.mapped_helper()
    expected_helper = expected if rollback else record["authenticatedTarget"]["binary"]
    if helper["sha256"] != expected_helper:
        raise RuntimeError("final published helper differs from the authenticated expected release image")
    if rollback:
        verify_published_failure(case, journal, ledger, target)
    if not rollback:
        pin = published_releases.digest((case.releases.directory / target / "mesh-update.json").read_bytes())
        committed = [entry for entry in ledger["entries"] if entry["version"] == target and entry["metadata"] == pin and entry["workersPreserved"] == case.expected_workers]
        if not committed:
            raise RuntimeError("real published updater lacks its exact pinned preserved-worker commit")
    record["final"] = {"build": build, "ledger": ledger, "route": case.route_observer.snapshot(),
                       "workers": case.cohort.verify("real-published-final"), "helper": helper}


def read_route_plan(case, target):
    result = subprocess.run([str(case.binary), "update", "--local", "--check", "--version", target, "--json"],
                            env=case.environment, capture_output=True, text=True, check=True, timeout=30)
    plan = json.loads(result.stdout)
    current = case.control("host.info")["host"]["build"]
    metadata = case.releases.directory / target / "mesh-update.json"
    if plan["currentDigest"] != current["digest"] or not plan.get("next"):
        raise RuntimeError("read-only authenticated route plan does not match the executing published source")
    if plan["target"]["binary"] != case.releases.builds[target]["digest"] or plan["target"]["metadata"] != published_releases.digest(metadata.read_bytes()):
        raise RuntimeError("authenticated route target differs from the actual signed draft")
    return plan


def next_expected_hop(record, version):
    index = len(record["hops"])
    if index >= len(record["expectedHops"]) or record["expectedHops"][index]["version"] != version:
        raise RuntimeError("published route executed an unintended or unplanned intermediate")
    return record["expectedHops"][index]


def authenticate_hop(case, build, expected):
    prefix = "/ShaulLavo/mesh/releases/download/" + expected["version"] + "/"
    manifest = case.origin.asset(prefix + "mesh-release.json").read_bytes()
    metadata = case.origin.asset(prefix + "mesh-update.json").read_bytes()
    if published_releases.digest(manifest) != expected["manifest"] or published_releases.digest(metadata) != expected["metadata"]:
        raise RuntimeError("observed intermediate bytes differ from the authenticated route plan")
    value = json.loads(manifest)
    platform = dict(zip(("os", "arch"), case.releases.native.split("/")))
    artifacts = [item for item in value["artifacts"] if item["platform"] == platform]
    if len(artifacts) != 1 or artifacts[0]["binarySha256"] != expected["binary"]:
        raise RuntimeError("authenticated intermediate manifest names another native image")
    if build["version"] != expected["version"] or build["digest"] != expected["binary"] or build["commit"] != value["commit"] or build["modified"] or build["platform"] != platform:
        raise RuntimeError("executing intermediate differs from its authenticated release identity")


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
    expected, observed = record.get("expectedHops", []), record.get("hops", [])
    if not expected or len(observed) != len(expected) or seen != {item["version"] for item in expected}:
        raise RuntimeError("published terminal state skipped its required signed intermediate/waypoint observations")
    for wanted, actual in zip(expected, observed, strict=True):
        if actual["expected"] != wanted or actual["build"]["digest"] != wanted["binary"] or actual["helper"]["sha256"] != wanted["binary"]:
            raise RuntimeError("published hop or helper does not match its authenticated expected image")


def verify_published_failure(case, journal, ledger, target):
    edge = {"from": journal["request"]["current"]["digest"], "to": case.releases.builds[target]["digest"],
            "platform": journal["request"]["current"]["platform"]}
    if not any(item["edge"] == edge and item["version"] == target and item["phase"] == "rolled_back" for item in ledger["failed"]):
        raise RuntimeError("published rollback lacks its exact durable failed edge")
