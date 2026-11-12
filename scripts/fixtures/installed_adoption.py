"""Real-manager adoption and rollback consumers of the delivered product CLI."""
import hashlib
import json
import shutil
import subprocess
import time

from fixtures import fleet_baselines, installed_native, installed_origin, installed_releases


class AdoptionCase(installed_native.Installation):
    def __init__(self, root, host, pair, preparer, origin, releases, scenario, previous=None):
        self.scenario, self.origin, self.releases = scenario, origin, releases
        self.desired_host, self.desired_pair, self.previous = host, pair, previous
        named = host | {"name": host["name"] + "-" + scenario, "helperRunning": False if previous else host["helperRunning"]}
        installed_pair = pair if previous is None else pair | {"daemon": previous}
        super().__init__(root, named, installed_pair, preparer)
        self.fault = self.root / "fault.json"
        self.fault.write_text("{}")
        self.environment.update(origin.environment() | {"MESH_INSTALLED_FAULT": str(self.fault),
            "MESH_UPDATE_CACHE_DIR": str(self.root / "cache")})
        self.snapshots = {}
        self.prior = None

    def seed_prior(self):
        if self.previous is None:
            return
        self.origin.seed(self.state, self.desired_host["daemon"])
        self.prior = json.loads((self.state / "update/installation.json").read_text())
        if self.prior["phase"] != "committed" or self.prior["request"]["manifest"]["version"] != self.desired_host["daemon"]:
            raise RuntimeError("baseline lacks its actual committed published prior transaction")
        self.pair = self.desired_pair
        self.host["helperRunning"] = self.desired_host["helperRunning"]
        self.units.managers["daemon"].pid = self.units.managers["daemon"].manager_pid()
        if self.host["helperRunning"]:
            self.units.start("helper")

    def retained(self):
        paths = [self.state / "identity.key", self.state / "authorized_keys"]
        paths.extend(self.home.glob("Library/LaunchAgents/*mesh.plist"))
        paths.extend((self.home / ".config/systemd/user").glob("mesh.service"))
        paths.extend((self.root / "config").glob("*.json"))
        return {str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in paths if path.is_file()}

    def inspect_retained(self):
        if self.retained() != self.snapshots:
            raise RuntimeError("adoption changed preserved identity, device grants, configuration or daemon definition")
        if installed_native.peer_pid(self.state / "s" / self.session / "sock") != self.worker:
            raise RuntimeError("adoption replaced retained worker")
        meta = json.loads((self.state / "s" / self.session / "meta.json").read_text())
        if meta["pid"] != self.shell:
            raise RuntimeError("adoption replaced retained shell")
        self.open_stream()
        self.wait_attachment_resume()
        self.io()

    def start_adoption(self, version):
        side = self.releases.builds[version]["path"]
        metadata = self.releases.directory / version / "mesh-update.json"
        pin = installed_releases.digest(metadata.read_bytes())
        host = self.control("host.info")["host"]["id"]
        input_data = {"schema": 1, "id": "a" * 32, "pin": pin, "version": version, "expected": host,
            "policy": {"schema": 1, "revision": 1, "mode": "automatic"}}
        self.snapshots = self.retained()
        if self.scenario == "rollback":
            self.fault.write_text(json.dumps({"daemon": [version]}))
        if self.scenario == "helper":
            self.fault.write_text(json.dumps({"helper": [version]}))
        result = subprocess.run([side, "update", "adopt", "start", "--local", "--release", version, "--json"],
            input=json.dumps(input_data), env=self.environment, capture_output=True, text=True, timeout=90, check=False)
        (self.root / "adopt-cli.log").write_text(result.stdout + result.stderr)
        # The product may return a durable attention state after a failed native
        # promotion. Only the actual journal/envelope and retained processes decide.
        return {"pin": pin, "exit": result.returncode}

    def await_result(self, version):
        deadline = time.monotonic() + 90
        last = {}
        while time.monotonic() < deadline:
            last = self.adoption_status()
            phase = last.get("details", {}).get("phase")
            if self.scenario == "helper" and last.get("details", {}).get("problem"):
                return last
            if phase in ("adopted", "rolled-back", "attention"):
                return last
            time.sleep(0.05)
        raise RuntimeError("real helper did not finish adoption in its existing 90 s fixture bound: " + json.dumps(last))

    def adoption_status(self):
        path = self.state / "updates/host-adoption.json"
        return json.loads(path.read_text()) if path.exists() else {}

    def validate_result(self, result, version, pin):
        phase = result.get("details", {}).get("phase")
        if result.get("pin") != pin:
            raise RuntimeError("canonical adoption metadata pin differs")
        if self.scenario == "adopt" and (phase != "adopted" or not result.get("confirmed")):
            raise RuntimeError("adoption did not confirm: " + json.dumps(result))
        if self.scenario == "rollback" and phase != "rolled-back":
            raise RuntimeError("failed native activation did not roll back: " + json.dumps(result))
        if self.scenario == "helper" and (phase != "recorded" or not result.get("details", {}).get("problem") or result.get("confirmed")):
            raise RuntimeError("failed native helper start did not hold adoption: " + json.dumps(result))
        running = self.control("host.info")["host"]["build"]
        expected = self.releases.builds[version] if self.scenario == "adopt" else self.pair["daemon"]["artifact"]
        digest = expected["digest"] if self.scenario == "adopt" else expected["binarySha256"]
        if running["digest"] != digest:
            raise RuntimeError("native daemon image differs after adoption/failure")
        self.inspect_retained()
        helper = self.validate_helper(version)
        if self.prior:
            journal = json.loads((self.state / "update/installation.json").read_text())
            if journal["settings"] != self.prior["settings"]:
                raise RuntimeError("adoption changed actual prior journal/service/cache settings")
        return {"phase": phase, "confirmed": result.get("confirmed", False), "running": running,
            "worker": self.worker, "shell": self.shell, "listener": self.listener, "client": self.terminal.pid, "helper": helper}

    def validate_helper(self, version):
        receipt = json.loads((self.state / "update/helper/installed.json").read_text())
        expected = self.pair["helper"]["artifact"]["binarySha256"] if self.scenario == "helper" else self.releases.builds[version]["digest"]
        if receipt["digest"] != expected:
            raise RuntimeError("actual helper receipt differs after promotion/restoration")
        if self.scenario == "helper" and not self.host["helperRunning"]:
            if self.units.managers["helper"].manager_pid() != 0:
                raise RuntimeError("failed promotion did not restore the stopped helper disposition")
            return {"stopped": True, "digest": expected}
        manager = self.units.managers["helper"]
        pid = manager.manager_pid()
        image = installed_native.native_services.mapped_image(pid, receipt["executable"])
        if image["sha256"] != expected or manager.manager_pid() != pid:
            raise RuntimeError("actual helper mapped image differs after promotion/restoration")
        return image


def run_case(root, output, host, pair, preparer, origin, releases, scenario, previous=None):
    case = AdoptionCase(root, host, pair, preparer, origin, releases, scenario, previous)
    record = {"host": host["name"], "scenario": scenario, "passed": False, "supplementalSourceCandidate": True}
    try:
        record["initial"] = case.prepare()
        case.seed_prior()
        if case.prior:
            record["genuinePrior"] = {"version": host["daemon"], "phase": case.prior["phase"], "settings": case.prior["settings"]}
        case.seed()
        target = installed_releases.VERSIONS[0]
        started = case.start_adoption(target)
        record["start"] = started
        record["result"] = case.validate_result(case.await_result(target), target, started["pin"])
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        finish_case(case, record, output)


def finish_case(case, record, output):
    try:
        case.close()
        record["cleanup"] = {"servicesAbsent": True, "processesAbsent": True}
    except BaseException as error:
        record["passed"] = False
        record["cleanupError"] = str(error)
        raise
    finally:
        installed_native.retain_logs(case.root, output, case.host["name"])
        log = case.root / "adopt-cli.log"
        if log.exists():
            installed_native.copy_log_tail(log, output / (case.host["name"] + "-adopt-cli.log"))
        (output / (case.host["name"] + "-native-pair.json")).write_text(json.dumps(record, indent=2) + "\n")


def run(root, output, native, preparer, prepare_only=False):
    space = shutil.disk_usage(root)
    (output / "storage-preflight.json").write_text(json.dumps({"root": str(root), "freeBytes": space.free, "requiredFreeBytes": 4 << 30}) + "\n")
    if space.free < 4 << 30:
        raise RuntimeError("native signed fixture needs 4 GiB free before source builds and acquisition")
    hosts = [host for host in fleet_baselines.load()["hosts"] if host["platform"] == native]
    releases = installed_releases.Releases(root, native, output)
    try:
        # Install the signer teardown before it can create its server or process.
        releases.start_signer()
        run_prepared(root, output, native, preparer, hosts, releases, prepare_only)
    finally:
        releases.close()
        retain_producer_logs(root, output)


def run_prepared(root, output, native, preparer, hosts, releases, prepare_only=False):
    origin = installed_origin.Origin(root)
    target = installed_releases.VERSIONS[0]
    releases.build(target)
    pairs, previous, proofs, seen = {}, {}, [], set()
    for host in hosts:
        pair = fleet_baselines.acquire_pair(root / "public", host)
        pairs[host["name"]] = pair
        prior_version = "v0.1." + str(int(host["daemon"].split(".")[-1]) - 1)
        previous[host["name"]] = fleet_baselines.acquire(root / "public", prior_version,
            dict(zip(("os", "arch"), native.split("/"))))
        source = pair["daemon"]["artifact"]["binarySha256"]
        if source not in seen:
            proofs.append(releases.prove(pair["daemon"]["executable"], target))
            seen.add(source)
    releases.publish(target, 0, proofs)
    origin.serve_tree(releases.directory)
    origin.serve_tree(root / "public")
    origin.head(releases.directory, target)
    if prepare_only:
        (output / "prepared-releases.json").write_text(json.dumps({"supplementalSourceBuilds": releases.builds,
            "metadata": releases.metadata, "genuineFullProofs": proofs}, indent=2) + "\n")
        return
    for host in hosts:
        for scenario in ("adopt", "rollback", "helper"):
            run_case(root, output, host, pairs[host["name"]], preparer, origin, releases, scenario, previous[host["name"]])


def retain_producer_logs(root, output):
    for path in root.rglob("*.log"):
        label = "producer-" + str(path.relative_to(root)).replace("/", "-")
        installed_native.copy_log_tail(path, output / label)
