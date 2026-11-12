"""Unpublished source builds with genuine signatures and measured transitions."""
import hashlib
import json
import os
import shutil
import subprocess
import tarfile
from pathlib import Path

from fixtures import installed_origin, native_services

REPO = Path(__file__).resolve().parents[2]
PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/arm64")
VERSIONS = ("v0.1.9001", "v0.1.9002", "v0.1.9003")


def write_json(path, value):
    data = json.dumps(value, separators=(",", ":")).encode()
    path.write_bytes(data)
    return data


def digest(data):
    return hashlib.sha256(data).hexdigest()


class Releases:
    def __init__(self, root, native, output):
        self.root, self.native, self.output = Path(root), native, Path(output)
        self.directory = self.root / "releases"
        self.directory.mkdir()
        self.signer = None
        self.signer_log = None
        self.builds, self.manifests, self.metadata = {}, {}, {}

    def environment(self):
        return os.environ | {"TMPDIR": str(self.root), "TMP": str(self.root), "TEMP": str(self.root), "MESH_SHORT_TMP": str(self.root)}

    def start_signer(self):
        self.signer_log = (self.root / "signer.log").open("wb")
        binary = self.root / "signer-test"
        with (self.root / "signer-build.log").open("wb") as output:
            subprocess.run(["go", "test", "-c", "-tags", "mesh_installed_fixture", "-o", str(binary), "./internal/update"],
                cwd=REPO, env=self.environment(), stdout=output, stderr=subprocess.STDOUT, check=True, timeout=180)
        environment = self.environment() | {"MESH_INSTALLED_FIXTURE_DIR": str(self.root)}
        self.signer = subprocess.Popen([str(binary), "-test.run", "^TestInstalledFixtureExport$", "-test.timeout=600s"], cwd=REPO, env=environment,
            stdout=self.signer_log, stderr=subprocess.STDOUT)
        native_services.wait_for(self.signer_ready, "installed signature exporter did not start")

    def signer_ready(self):
        if self.signer.poll() is not None:
            raise RuntimeError("installed signature exporter exited: " + (self.root / "signer.log").read_text()[-8192:])
        return (self.root / "control.sock").exists()

    def build(self, version, epoch=1):
        overlay = self.overlay(epoch)
        artifacts = []
        for platform in PLATFORMS:
            artifacts.append(self.build_platform(version, platform, overlay))
        native = next(row for row in artifacts if row["platform"] == dict(zip(("os", "arch"), self.native.split("/"))))
        binary = self.root / "build" / version / self.native.replace("/", "-") / "mesh"
        result = subprocess.run([str(binary), "version", "--json"], capture_output=True, text=True, check=True, timeout=10)
        build = json.loads(result.stdout)
        if build["modified"] or build["version"] != version or build["digest"] != native["binarySha256"]:
            raise RuntimeError("supplemental source build identity differs from its compiled image")
        self.builds[version] = build | {"path": str(binary)}
        self.manifests[version] = {"schema": 1, "version": version, "commit": build["commit"], "artifacts": artifacts,
            "compatibility": {"stateReadMin": build["stateVersion"], "stateReadMax": build["stateVersion"],
                "stateWrite": build["stateVersion"], "workerMin": 1, "workerMax": build["workerProtocol"],
                "workerWrite": build["workerProtocol"], "journalVersion": 1, "transitions": []}}
        return binary

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
        path = self.root / ("overlay-" + str(epoch) + ".json")
        write_json(path, {"Replace": replacements})
        return path

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

    def prove(self, source, target):
        directory = self.root / "proofs" / (Path(source).parent.name + "-" + target + "-" + digest(Path(source).read_bytes())[:8])
        directory.mkdir(parents=True)
        with (directory / "proof.log").open("wb") as output:
            subprocess.run(["bash", str(REPO / "scripts/prove-release-transition.sh"), str(source), self.builds[target]["path"],
                self.native, str(directory)], cwd=REPO, env=self.environment(), stdout=output, stderr=subprocess.STDOUT, check=True, timeout=180)
        receipts = list(directory.glob("*.json"))
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
        return {"forward": receipt.stem, "rollback": rollback.name.removesuffix(".rollback.json")}

    def publish(self, version, ordinal, rollbacks, since=(), epoch=1, reader_epoch=1):
        directory = self.directory / version
        manifest = write_json(directory / "mesh-release.json", self.manifests[version])
        page = {"schema": 1, "epoch": epoch, "page": 0, "entries": [], "back": []}
        for previous, metadata in self.metadata.items():
            page["entries"].append({"ordinal": metadata["position"]["ordinal"], "version": previous,
                "manifest": metadata["manifest"], "metadata": digest((self.directory / previous / "mesh-update.json").read_bytes()),
                "anchorsFrom": metadata["direct"]["anchorsFrom"], "publishedAt": metadata["publishedAt"]})
        page_data = json.dumps(page, separators=(",", ":")).encode()
        page_name = digest(page_data) + ".anchors.json"
        (directory / page_name).write_bytes(page_data)
        position = {"anchor": True, "ordinal": ordinal}
        metadata = {"schema": 1, "epoch": epoch, "trust": 1, "version": version, "manifest": digest(manifest),
            "publishedAt": "2026-11-01T00:00:00Z", "position": position,
            "direct": {"anchorsFrom": 0, "since": list(since)}, "rollback": rollbacks,
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
        if self.signer and self.signer.poll() is None:
            try:
                installed_origin.sign(self.root, [], stop=True)
                self.signer.wait(timeout=10)
            except (OSError, RuntimeError, subprocess.TimeoutExpired):
                self.signer.kill()
                self.signer.wait(timeout=10)
        if self.signer_log:
            self.signer_log.close()
