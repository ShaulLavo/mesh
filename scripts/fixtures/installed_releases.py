"""Unpublished source builds with genuine signatures and measured transitions."""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
import tarfile
from pathlib import Path

from fixtures import installed_origin, native_services, installed_clock
from fixtures import release_workers, release_worker_groups

REPO = Path(__file__).resolve().parents[2]
PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/arm64")
VERSIONS = ("v0.1.9001", "v0.1.9002", "v0.1.9003")


def write_json(path, value):
    data = json.dumps(value, separators=(",", ":")).encode()
    path.write_bytes(data)
    return data


def digest(data):
    return hashlib.sha256(data).hexdigest()


def future_manifest_overlay(root):
    source = REPO / "internal/release/release.go"
    original = source.read_text()
    guard = "if m.Schema != ManifestSchema {"
    if original.count(guard) != 1:
        raise RuntimeError("future-format fixture requires exactly one manifest schema guard")
    path = Path(root) / "future-release.go"
    path.write_text(original.replace(guard, "if m.Schema != ManifestSchema && m.Schema != 2 {", 1))
    return {str(source): str(path)}


def source_build(source, environment, native):
    source = Path(source)
    result = subprocess.run([str(source), "version", "--json"], env=environment,
                            capture_output=True, text=True, check=True, timeout=10)
    build = json.loads(result.stdout)
    with source.open("rb") as stream:
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
    if build["digest"] != actual or build["modified"] or build["platform"] != dict(zip(("os", "arch"), native.split("/"))):
        raise RuntimeError("transition source report differs from its actual unmodified native image")
    return build


def rollback_context(manifest, source):
    compatibility = manifest["compatibility"]
    if source["stateVersion"] > compatibility["stateReadMax"] or source["workerProtocol"] > compatibility["workerMax"]:
        raise RuntimeError("transition source exceeds the actual candidate context capabilities")
    return manifest | {"compatibility": compatibility | {
        "stateReadMin": min(compatibility["stateReadMin"], source["stateVersion"]),
        "workerMin": min(compatibility["workerMin"], source["workerProtocol"]), "transitions": []}}


class Releases:
    def __init__(self, root, native, output, *, manifest_schemas=(1,), clock_plan=None):
        if manifest_schemas not in ((1,), (1, 2)):
            raise ValueError("unsupported fixture exporter manifest capability")
        self.manifest_schemas = manifest_schemas
        self.clock_plan, self.clock = clock_plan, installed_clock.current()
        self.root, self.native, self.output = Path(root), native, Path(output)
        self.directory = self.root / "releases"
        self.directory.mkdir()
        self.signer = None
        self.signer_log = None
        self.signer_started = None
        self.closed = False
        self.transition_clean = True
        self.registry, self.probe = None, None
        self.builds, self.manifests, self.metadata, self.startup = {}, {}, {}, {}

    def environment(self):
        return os.environ | {"TMPDIR": str(self.root), "TMP": str(self.root), "TEMP": str(self.root), "MESH_SHORT_TMP": str(self.root),
            "MESH_STATE_DIR": str(self.root / "build-state"), "MESH_CONFIG_DIR": str(self.root / "build-config"),
            "MESH_UPDATE_CACHE_DIR": str(self.root / "build-cache")}

    def start_signer(self):
        if self.signer_started is not None:
            raise RuntimeError("installed fixture exporter was already launched")
        if self.clock_plan is None or self.clock is None or self.clock.plan is not self.clock_plan:
            raise RuntimeError("executable fixture exporter requires an active explicit clock plan")
        self.clock_plan.validate()
        self.prepare_signer()
        with installed_clock.operation("signer-start"):
            self.launch_signer()

    @installed_clock.timed("signer-build")
    def prepare_signer(self):
        flags = self.exporter_overlay()
        self.signer_log = (self.root / "signer.log").open("wb")
        binary = self.root / "signer-test"
        with (self.root / "signer-build.log").open("wb") as output:
            subprocess.run(["go", "test", "-c", "-tags", "mesh_installed_fixture"] + flags + ["-o", str(binary), "./internal/update"],
                cwd=REPO, env=self.environment(), stdout=output, stderr=subprocess.STDOUT, check=True, timeout=installed_clock.timeout(180))
        environment = self.environment() | {"MESH_INSTALLED_FIXTURE_DIR": str(self.root)}
        subprocess.run([str(binary), "-test.run=^TestInstalledEmptyLedgerBoundary$"], env=environment,
                       capture_output=True, check=True, timeout=installed_clock.timeout(15))

    def launch_signer(self):
        self.signer_started = time.monotonic()
        self.signer_started_ns = time.monotonic_ns()
        remaining_ns = self.clock.deadline_ns - self.signer_started_ns
        if remaining_ns <= 0:
            self.clock.check()
        environment = self.environment() | {"MESH_INSTALLED_FIXTURE_DIR": str(self.root)}
        self.record_ownership(signer_absent=False)
        self.signer = subprocess.Popen([str(self.root / "signer-test"), "-test.run", "^TestInstalledFixtureExport$", "-test.timeout=" + str(remaining_ns) + "ns"], cwd=REPO, env=environment,
            stdout=self.signer_log, stderr=subprocess.STDOUT)
        native_services.wait_for(self.signer_ready, "installed signature exporter did not start")

    def exporter_overlay(self):
        replacements = future_manifest_overlay(self.root) if self.manifest_schemas == (1, 2) else {}
        overlay = self.root / "exporter-overlay.json"
        data = write_json(overlay, {"Replace": replacements})
        source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=REPO, text=True).strip()
        write_json(self.output / "fixture-exporter.json", {"observer": "fixture-exporter",
            "manifestSchemas": self.manifest_schemas, "harnessSource": source, "overlaySha256": digest(data),
            "manifestSourceSha256": digest((REPO / "internal/release/release.go").read_bytes()),
            "overlaySourceSha256": {name: digest(Path(path).read_bytes()) for name, path in replacements.items()}})
        return ["-overlay", str(overlay)] if replacements else []

    def signer_ready(self):
        if self.signer.poll() is not None:
            raise RuntimeError("installed signature exporter exited: " + (self.root / "signer.log").read_text()[-8192:])
        return (self.root / "control.sock").exists()

    def check_budget(self, phase=None):
        if self.clock is None:
            raise RuntimeError("installed fixture requires an active clock")
        if self.clock.failures:
            raise installed_clock.ClockExpired("installed fixture stopped after its first failure")
        self.clock.check()
        if self.signer_started is None or self.signer.poll() is not None:
            raise RuntimeError("installed fixture signer lifetime ended; no clock was reset")

    @installed_clock.timed("build", lambda self, version, epoch=1: version + ":epoch=" + str(epoch))
    def build(self, version, epoch=1):
        overlay = self.overlay(epoch)
        artifacts = []
        for platform in PLATFORMS:
            artifacts.append(self.build_platform(version, platform, overlay))
        native = next(row for row in artifacts if row["platform"] == dict(zip(("os", "arch"), self.native.split("/"))))
        binary = self.root / "build" / version / self.native.replace("/", "-") / "mesh"
        result = subprocess.run([str(binary), "version", "--json"], env=self.environment(), capture_output=True, text=True, check=True, timeout=10)
        build = json.loads(result.stdout)
        if build["modified"] or build["version"] != version or build["digest"] != native["binarySha256"]:
            raise RuntimeError("supplemental source build identity differs from its compiled image")
        self.builds[version] = build | {"path": str(binary)}
        self.manifests[version] = {"schema": 1, "version": version, "commit": build["commit"], "artifacts": artifacts,
            "compatibility": {"stateReadMin": build["stateVersion"], "stateReadMax": build["stateVersion"],
                "stateWrite": build["stateVersion"], "workerMin": 1, "workerMax": build["workerProtocol"],
                "workerWrite": build["workerProtocol"], "journalVersion": 1, "transitions": []}}
        self.build_startup(version, overlay)
        return binary

    def build_startup(self, version, overlay):
        directory = self.root / "startup" / version
        directory.mkdir(parents=True)
        subprocess.run([sys.executable, str(REPO / "scripts/fixtures/release_transition.py"), "startup-overlay", str(REPO), str(directory)],
            cwd=REPO, env=self.environment(), check=True, timeout=10)
        combined = json.loads(overlay.read_text())["Replace"]
        combined.update(json.loads((directory / "overlay.json").read_text())["Replace"])
        write_json(directory / "combined.json", {"Replace": combined})
        image = directory / "mesh"
        with (directory / "build.log").open("wb") as output:
            subprocess.run(["go", "build", "-trimpath", "-tags", "mesh_installed_fixture", "-overlay", str(directory / "combined.json"),
                "-ldflags=-X github.com/shaul/mesh/internal/release.Version=" + version, "-o", str(image), "./cmd/mesh"],
                cwd=REPO, env=self.environment() | {"CGO_ENABLED": "0"}, stdout=output, stderr=subprocess.STDOUT, check=True, timeout=180)
        self.startup[version] = image
        if not (self.root / "boundary").exists():
            subprocess.run(["go", "build", "-trimpath", "-o", str(self.root / "boundary"), "./internal/storage/cmd/boundary"],
                cwd=REPO, env=self.environment(), capture_output=True, check=True, timeout=120)

    def overlay(self, epoch):
        replacements = {str(REPO / "internal/release/trust" / name): str(self.root / name)
                        for name in ("bootstrap-root.json", "trusted-root.json", "provenance.json")}
        if epoch != 1:
            source = REPO / "internal/update/host_reader_types.go"
            path = self.root / "reader-types.go"
            text = source.read_text()
            updated = text.replace("uint32 = 1", "uint32 = 2", 1)
            if updated == text:
                raise RuntimeError("epoch fixture did not change compiled reader")
            path.write_text(updated)
            replacements[str(source)] = str(path)
            replacements.update(future_manifest_overlay(self.root))
        path = self.root / ("overlay-" + str(epoch) + ".json")
        write_json(path, {"Replace": replacements})
        return path

    @installed_clock.timed("custom-source", lambda self, version: version)
    def retain_custom_source(self, version):
        selected = release_workers.applicable(self.registry, version, self.builds[version]["path"])
        records = [item | {"binary": str(Path(item["binary"]).relative_to(self.root))} for item in selected]
        path = self.root / "fixture-workers.json"
        write_json(path, {"schema": 1, "platform": dict(zip(("os", "arch"), self.native.split("/"))), "workers": records})
        self.registry = path

    def build_platform(self, version, platform, overlay):
        system, architecture = platform.split("/")
        binary = self.root / "build" / version / platform.replace("/", "-") / "mesh"
        binary.parent.mkdir(parents=True, exist_ok=True)
        environment = self.environment() | {"GOOS": system, "GOARCH": architecture, "CGO_ENABLED": "0"}
        log = self.root / ("build-" + version + "-" + system + "-" + architecture + ".log")
        with log.open("wb") as output:
            subprocess.run(["go", "build", "-trimpath", "-tags", "mesh_installed_fixture", "-overlay", str(overlay),
                "-ldflags=-X github.com/shaul/mesh/internal/release.Version=" + version, "-o", str(binary), "./cmd/mesh"],
                env=environment, cwd=REPO, stdout=output, stderr=subprocess.STDOUT, check=True, timeout=180)
        directory = self.directory / version
        directory.mkdir(exist_ok=True)
        name = "mesh_" + system + "_" + architecture + ".tar.gz"
        with tarfile.open(directory / name, "w:gz") as archive:
            archive.add(binary, arcname="mesh", recursive=False)
        return {"platform": {"os": system, "arch": architecture}, "archive": name,
            "sha256": digest((directory / name).read_bytes()), "binarySha256": digest(binary.read_bytes())}

    @installed_clock.timed("proof", lambda self, source, target: Path(source).parent.name + ":" + target)
    def prove(self, source, target):
        original = source_build(source, self.environment(), self.native)
        directory = self.root / "proofs" / (Path(source).parent.name + "-" + target + "-" + original["digest"][:8])
        directory.mkdir(parents=True)
        context_manifest = directory / "candidate-context.json"
        write_json(context_manifest, rollback_context(self.manifests[target], original))
        with (directory / "proof.log").open("wb") as output:
            environment = self.environment() | {"MESH_STARTUP_FAULT_BINARY": str(self.startup[target]), "MESH_BOUNDARY_TOOL": str(self.root / "boundary"),
                "MESH_ROLLBACK_MANIFEST": str(context_manifest)}
            if self.registry:
                environment.update(MESH_WORKER_REGISTRY=str(self.registry), MESH_WORKER_PROBE=str(self.probe))
            self.run_transition(source, target, directory, environment, output)
        receipts = [path for path in directory.glob("*.json") if release_worker_groups.RECEIPT.fullmatch(path.name)]
        rollbacks = list((directory / "rollback").glob("*.rollback.json"))
        if len(receipts) != 1 or len(rollbacks) != 1:
            raise RuntimeError("native full transition did not produce one forward and rollback proof")
        receipt, rollback = receipts[0], rollbacks[0]
        proof = json.loads(receipt.read_text())
        manifest = self.manifests[target]
        manifest["compatibility"]["stateReadMin"] = min(manifest["compatibility"]["stateReadMin"], proof["stateReadMin"])
        manifest["compatibility"]["transitions"].append({key: proof[key] for key in ("platform", "fromDigest", "toDigest")} | {"proof": receipt.stem})
        shutil.copy2(receipt, self.directory / target / receipt.name)
        shutil.copy2(rollback, self.directory / target / rollback.name)
        for path in directory.glob("*.worker-groups.json"):
            shutil.copy2(path, self.directory / target / path.name)
        for path in (directory / "groups" / "leaves").glob("*.worker-group.json"):
            shutil.copy2(path, self.directory / target / path.name)
        return {"forward": receipt.stem, "rollback": rollback.name.removesuffix(".rollback.json")}

    @installed_clock.timed("native-transition", lambda self, source, target, *_args: Path(source).parent.name + ":" + target)
    def run_transition(self, source, target, directory, environment, output):
        command = ["bash", str(REPO / "scripts/prove-release-transition.sh"), str(source), self.builds[target]["path"], self.native, str(directory)]
        deadline_ns = installed_clock.parent_deadline(600 if self.registry else 180)
        environment = environment | self.transition_clock(directory, deadline_ns)
        self.transition_clean = False
        self.record_ownership()
        process = subprocess.Popen(command, cwd=REPO, env=environment, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        ownership = None
        try:
            ownership = installed_clock.NativeOwnership(process, self.root, directory, deadline_ns)
            wait_transition(process, command, deadline_ns, ownership)
            self.transition_clean = True
        except BaseException as error:
            installed_clock.failure(error)
            stop_transition(process, ownership=ownership)
            self.transition_clean = True
            raise
        finally:
            if ownership is not None:
                ownership.save()
            self.record_ownership()
            self.retain_transition_clock(directory)

    def record_ownership(self, signer_absent=None):
        absent = self.signer is None or self.signer.poll() is not None
        write_json(self.output / "fixture-ownership.json", {"root": str(self.root), "signerAbsent": absent if signer_absent is None else signer_absent,
                   "transitionsAbsent": self.transition_clean})

    def transition_clock(self, directory, deadline_ns):
        if self.clock is None:
            return {}
        path = directory / "fixture-clock-handoff.json"
        write_json(path, {"deadline_ns": deadline_ns, "cleanup_deadline_ns": min(self.clock.deadline_ns + self.clock.plan.emergency_ns, self.clock.job_deadline_ns),
                          "prefix": self.clock.active_key, "mode": self.clock.plan.mode,
                          "guards": {row.key: {"limit_ns": row.limit_ns, "workload": row.workload} for row in self.clock.plan.guards}})
        return {"MESH_INSTALLED_CLOCK_HANDOFF": str(path)}

    def retain_transition_clock(self, directory):
        path = directory / "fixture-group-clock.json"
        if self.clock and path.exists():
            value = json.loads(path.read_text())
            self.clock.samples.extend(value["samples"])
            self.clock.expected_samples.update(value.get("expected_sample_keys", []))

    @installed_clock.timed("publish", lambda self, version, *_args, **_kwargs: version)
    def publish(self, version, ordinal, rollbacks, since=(), epoch=1, reader_epoch=1, anchors_from=0, entries=None):
        directory = self.directory / version
        manifest = write_json(directory / "mesh-release.json", self.manifests[version])
        page = {"schema": 1, "epoch": epoch, "page": 0, "entries": [], "back": []}
        for previous, metadata in self.metadata.items():
            page["entries"].append({"ordinal": metadata["position"]["ordinal"], "version": previous,
                "manifest": metadata["manifest"], "metadata": digest((self.directory / previous / "mesh-update.json").read_bytes()),
                "anchorsFrom": metadata["direct"]["anchorsFrom"], "publishedAt": metadata["publishedAt"]})
        if entries is not None:
            page["entries"] = entries
        page_data = json.dumps(page, separators=(",", ":")).encode()
        page_name = digest(page_data) + ".anchors.json"
        (directory / page_name).write_bytes(page_data)
        position = {"anchor": True, "ordinal": ordinal}
        metadata = {"schema": 1, "epoch": epoch, "trust": 1, "version": version, "manifest": digest(manifest),
            "publishedAt": "2026-11-01T00:00:00Z", "position": position,
            "direct": {"anchorsFrom": anchors_from, "since": list(since)}, "rollback": rollbacks,
            "page": {"tag": version, "name": page_name, "sha256": digest(page_data)},
            "updater": {"schema": 1, "epoch": reader_epoch, "position": position if reader_epoch == epoch else {"anchor": True, "ordinal": 0}}}
        write_json(directory / "mesh-update.json", metadata)
        subjects = [directory / "mesh-update.json"]
        subjects.extend(sorted(directory.glob("mesh_*.tar.gz")))
        installed_origin.sign(self.root, subjects, directory / "mesh-update.sig.json")
        self.metadata[version] = metadata
        write_json(directory / "mesh-update-root.json", {"format": 1, "epoch": epoch,
            "head": {"tag": version, "name": "mesh-update.json", "sha256": digest((directory / "mesh-update.json").read_bytes())}})

    def close(self):
        if self.closed:
            return
        installed_clock.cleanup()
        if self.clock:
            self.clock.switch("completion")
        if self.clock:
            self.clock.provenance["supplementalBuilds"] = self.builds
            exporter = self.output / "fixture-exporter.json"
            if exporter.exists():
                self.clock.provenance["exporter"] = json.loads(exporter.read_text())
        if self.signer and self.signer.poll() is None:
            try:
                installed_origin.sign(self.root, [], stop=True)
                self.signer.wait(timeout=installed_clock.timeout(10))
            except (OSError, RuntimeError, subprocess.TimeoutExpired):
                self.signer.kill()
                self.signer.wait(timeout=installed_clock.timeout(10))
        if self.signer_log:
            self.signer_log.close()
        self.record_ownership()
        self.closed = True
        if self.signer_started is not None:
            write_json(self.output / "signer-lifetime.json", {"seconds": time.monotonic() - self.signer_started,
                "start_ns": self.signer_started_ns, "end_ns": time.monotonic_ns(),
                "commandDeadlineNs": self.clock.deadline_ns, "mode": self.clock_plan.mode, "reaped": self.signer.poll() is not None})


def stop_transition(process, roots=(), *, ownership=None):
    installed_clock.stop_group(process, installed_clock.parent_deadline(20), roots, ownership=ownership)


def wait_transition(process, command, deadline_ns, ownership):
    while process.poll() is None:
        ownership.observe(deadline_ns)
        try:
            process.wait(timeout=installed_clock.remaining_until(deadline_ns, 0.1))
        except subprocess.TimeoutExpired:
            installed_clock.remaining_until(deadline_ns, 600)
    code = process.returncode
    if code:
        raise subprocess.CalledProcessError(code, command)
    if installed_clock.group_alive(process.pid):
        raise RuntimeError("native transition leader exited before its owned descendants")
    ownership.verify(deadline_ns)
