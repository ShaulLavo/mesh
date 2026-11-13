"""Fresh managed preparation and a revision-zero automatic native update."""
import json
import shutil
import socket
import subprocess
import sys
import time

from fixtures import fleet_baselines, installed_adoption, installed_chain, installed_native, installed_releases


def prove(root, output, native):
    space = shutil.disk_usage(root)
    (output / "storage-preflight.json").write_text(json.dumps({"root": str(root), "freeBytes": space.free, "requiredFreeBytes": 4 << 30}) + "\n")
    if space.free < 4 << 30:
        raise RuntimeError("fresh installed fixture needs 4 GiB free before builds and acquisition")
    releases = installed_releases.Releases(root, native, output)
    try:
        releases.start_signer()
        from fixtures import installed_origin
        run(root, output, native, releases, installed_origin.Origin(root))
    finally:
        releases.close()
        installed_adoption.retain_producer_logs(root, output)


class FreshCase(installed_adoption.AdoptionCase):
    def __init__(self, root, origin, releases, version=None, publication=False, name="fresh"):
        first = version or installed_releases.VERSIONS[0]
        self.publication = publication
        self.first_start_verified = False
        build = releases.builds[first]
        image = {"executable": build["path"], "artifact": {"binarySha256": build["digest"]}}
        host = {"name": name, "daemon": first, "helper": first, "helperRunning": True}
        super().__init__(root, host, {"daemon": image, "helper": image}, None, origin, releases, "managed")

    def prepare(self):
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        directory = self.home / ("Library/LaunchAgents" if sys.platform == "darwin" else ".config/systemd/user")
        directory.mkdir(parents=True)
        daemon = self.units.managers["daemon"]
        suffix = ".plist" if sys.platform == "darwin" else ".service"
        daemon_path = directory / (daemon.label + suffix)
        self.render_daemon(daemon_path)
        self.units.register("daemon", daemon_path)
        helper = self.units.managers["helper"]
        self.units.reserve_fresh_helper(directory / (helper.label + suffix))
        flags = ["--service-kind", "launchd", "--domain", helper.domain] if sys.platform == "darwin" else ["--service-kind", "systemd"]
        self.install_flags = flags
        self.production_install_command("prepare-updates", flags)
        self.helper = json.loads((self.state / "update/helper/installed.json").read_text())
        self.require_fresh_state()
        prepared = self.mapped_helper()
        self.production_install_command("installation-check", flags)
        if self.mapped_helper()["pid"] != prepared["pid"]:
            raise RuntimeError("fresh installation check changed its verified helper PID")
        self.units.start("daemon")
        installed_native.native_services.wait_for(self.ready, "fresh managed daemon did not become ready")
        host = self.control("host.info")["host"]
        self.require_automatic_zero(host)
        self.require_fresh_state()
        started = self.first_start(daemon)
        self.first_start_verified = True
        return {"helperBeforeDaemon": prepared, "firstNativeStart": started, "effectiveUpdate": host["hostUpdate"]}

    def first_start(self, daemon):
        if self.publication:
            pid = daemon.manager_pid()
            image = installed_native.native_services.mapped_image(pid, self.binary)
            return {"pid": pid, "mappedImage": image, "policyAbsentBeforeActivation": True, "observer": "native manager and filesystem"}
        started = json.loads((self.root / ("start." + self.host["daemon"] + ".json")).read_text())
        if started["policy"] is not None or started["pid"] != daemon.manager_pid():
            raise RuntimeError("fresh first native daemon start had a pre-seeded host policy")
        return started

    def production_install_command(self, operation, flags):
        result = subprocess.run([str(self.binary), "daemon", operation, *flags], env=self.environment,
                                capture_output=True, text=True, check=False, timeout=75)
        (self.root / (operation + ".log")).write_text(result.stdout + result.stderr)
        if result.returncode:
            raise RuntimeError("fresh production " + operation + " failed: " + result.stderr[-4096:])

    def initialize_preserved_baseline(self):
        if not self.first_start_verified:
            raise RuntimeError("baseline initialization requires the actual absent-policy first-start assertions first")
        before = self.retained()
        helper = self.mapped_helper()
        self.production_install_command("installation-check", self.install_flags)
        after = self.retained()
        installed_adoption.validate_initialized_baseline(before, after, str(self.root / "config/domains.json"))
        self.require_fresh_state()
        self.require_automatic_zero(self.control("host.info")["host"])
        if self.mapped_helper()["pid"] != helper["pid"]:
            raise RuntimeError("baseline initialization changed the verified helper PID")
        return {"command": "daemon installation-check", "before": before, "after": after,
                "firstStartAssertionsAlreadyVerified": True}

    def require_fresh_state(self):
        paths = (self.state / "updates/host-policy.json", self.state / "updates/host-adoption.json", self.state / "update/installation.json")
        if any(path.exists() for path in paths):
            raise RuntimeError("fresh first-start fixture acquired a policy, adoption envelope or installation journal")

    def require_automatic_zero(self, host):
        summary = host.get("hostUpdate") or {}
        if summary.get("mode") != "automatic" or summary.get("revision") != 0 or summary.get("installation") != "managed":
            raise RuntimeError("actual managed daemon did not report its revision-zero automatic default")

    def mapped_helper(self):
        receipt = json.loads((self.state / "update/helper/installed.json").read_text())
        manager = self.units.managers["helper"]
        pid = manager.manager_pid()
        image = installed_native.native_services.mapped_image(pid, receipt["executable"])
        if image["sha256"] != receipt["digest"] or manager.manager_pid() != pid:
            raise RuntimeError("fresh native helper image/PID differs from its actual receipt")
        return image


def run(root, output, native, releases, origin):
    first, target = installed_releases.VERSIONS[:2]
    host = next(host for host in fleet_baselines.load()["hosts"] if host["platform"] == native)
    source = fleet_baselines.acquire(root / "public", host["daemon"], dict(zip(("os", "arch"), native.split("/"))))
    releases.build(first)
    releases.publish(first, 0, [releases.prove(source["executable"], first)])
    releases.build(target)
    releases.publish(target, 1, [releases.prove(releases.builds[first]["path"], target)])
    origin.serve_tree(releases.directory)
    origin.head(releases.directory, first)
    case = FreshCase(root, origin, releases)
    record = {"passed": False, "supplementalSourceBuilds": True, "adoptionSeededPolicy": False}
    try:
        record["initial"] = case.prepare()
        case.seed()
        record["baselineInitialization"] = case.initialize_preserved_baseline()
        case.snapshots = case.retained()
        collect_diagnostics(case, output, "before-target-exposure")
        origin.head(releases.directory, target)
        case.units.stop("daemon")
        case.units.start("daemon")
        collect_diagnostics(case, output, "after-restart")
        record["automatic"] = await_commit(case, target)
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        finish(case, output, record)


def await_commit(case, target):
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        ledger = installed_chain.read_ledger(case)
        entries = [entry for entry in ledger["entries"] if entry["version"] == target]
        helper = installed_chain.ready_helper(case, target) if entries and not ledger["state"].get("owned") else None
        if helper is not None:
            return validate_commit(case, target, entries[0], helper)
        time.sleep(0.05)
    raise RuntimeError("fresh managed daemon did not finish its first revision-zero automatic hop")


def validate_commit(case, target, entry, helper):
    host = case.control("host.info")["host"]
    case.require_automatic_zero(host)
    case.inspect_retained()
    pin = installed_releases.digest((case.releases.directory / target / "mesh-update.json").read_bytes())
    if entry["origin"] != "host" or entry["metadata"] != pin or entry["workersPreserved"] != 1 or host["build"]["digest"] != case.releases.builds[target]["digest"]:
        raise RuntimeError("fresh automatic commit differs from its actual signed release and retained worker")
    if (case.state / "updates/host-policy.json").exists() or (case.state / "updates/host-adoption.json").exists():
        raise RuntimeError("fresh automatic update acquired an explicit policy or adoption envelope")
    return {"entry": entry, "effectiveUpdate": host["hostUpdate"], "helper": helper,
            "client": case.terminal.pid, "worker": case.worker, "shell": case.shell, "listener": case.listener}


def finish(case, output, record):
    try:
        collect_diagnostics(case, output)
    except Exception as error:
        record["diagnosticError"] = str(error)
    try:
        case.close()
        record["cleanup"] = {"servicesAbsent": True, "processesAbsent": True}
    except BaseException as error:
        record["passed"] = False
        record["cleanupError"] = str(error)
        raise
    finally:
        installed_native.retain_logs(case.root, output, "fresh-managed")
        retain_install_logs(case, output)
        (output / "fresh-managed-native-pair.json").write_text(json.dumps(record, indent=2) + "\n")


def retain_install_logs(case, output):
    for name in ("prepare-updates", "installation-check"):
        path = case.root / (name + ".log")
        if path.exists():
            installed_native.copy_log_tail(path, output / ("fresh-" + name + ".log"))


def collect_diagnostics(case, output, label="final"):
    (output / ("fresh-" + label + "-preserved-before.json")).write_text(json.dumps(case.snapshots, indent=2) + "\n")
    (output / ("fresh-" + label + "-preserved-after.json")).write_text(json.dumps(case.retained(), indent=2) + "\n")
    ledger = installed_chain.read_ledger(case)
    (output / ("fresh-" + label + "-ledger.json")).write_text(json.dumps(ledger, indent=2) + "\n")
    journal = installed_chain.read_journal(case)
    (output / ("fresh-" + label + "-journal.json")).write_text(json.dumps(journal, indent=2) + "\n")
    if case.ready():
        (output / ("fresh-" + label + "-host.json")).write_text(json.dumps(case.control("host.info"), indent=2) + "\n")
    for name in ("host-update", "host-discovery", "host-status", "host-policy"):
        path = case.state / "updates" / (name + ".json")
        if path.exists():
            (output / ("fresh-" + label + "-" + name + ".json")).write_bytes(path.read_bytes())
    receipt = case.state / "update/helper/installed.json"
    if receipt.exists():
        (output / ("fresh-" + label + "-helper.json")).write_bytes(receipt.read_bytes())
    manager = case.units.managers["helper"]
    if manager.system == "Linux":
        status = manager.systemctl("status", manager.label + ".service", "--no-pager", check=False)
        (output / ("fresh-" + label + "-helper-status.log")).write_text((status.stdout + status.stderr)[-16384:])
