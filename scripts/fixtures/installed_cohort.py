"""Owned copies and observations using the authoritative published-worker API."""
import hashlib
import json
import os
import shutil
import subprocess
from pathlib import Path

from fixtures import release_workers


def stage(registry, destination, platform):
    registry = Path(registry).resolve(strict=True)
    data = json.loads(registry.read_text())
    if data["schema"] != 1 or data["platform"] != platform or not data["workers"]:
        raise RuntimeError("native exact worker registry schema/platform differs")
    destination.mkdir(parents=True)
    records = []
    for item in data["workers"]:
        records.append(copy_record(registry, destination, item))
    path = destination / "workers.json"
    path.write_text(json.dumps(data | {"workers": records}, sort_keys=True) + "\n")
    return path


def copy_record(registry, destination, item):
    relative = Path(item["binary"])
    if relative.is_absolute() or ".." in relative.parts:
        raise RuntimeError("worker registry path is not relative beneath its parent")
    source = (registry.parent / relative).resolve(strict=True)
    if not source.is_relative_to(registry.parent.resolve()):
        raise RuntimeError("worker registry symlink escapes its parent")
    digest = item["artifact"]["binarySha256"]
    target = destination / digest / "mesh"
    target.parent.mkdir(exist_ok=True)
    if not target.exists():
        shutil.copy2(source, target)
    with target.open("rb") as stream:
        if hashlib.file_digest(stream, "sha256").hexdigest() != digest:
            raise RuntimeError("copied exact archived worker differs from its published identity")
    return item | {"binary": str(target.relative_to(destination))}


class Cohort:
    def __init__(self, registry, probe, state, config, version, binary, record):
        self.registry, self.probe, self.state, self.config = registry, probe, state, config
        self.version, self.binary, self.record = version, binary, record
        self.selected = release_workers.applicable(registry, version, binary)

    def start(self):
        # The shared producer persists ownership before each worker can start.
        environment = os.environ | {"MESH_WORKER_PROBE": str(self.probe)}
        subprocess.run([os.sys.executable, str(Path(release_workers.__file__)), "start", str(self.state), str(self.config),
            str(self.registry), self.version, str(self.binary), str(self.record)], env=environment,
            capture_output=True, text=True, check=True, timeout=90)
        return self.verify("before-install")

    def verify(self, label):
        environment = os.environ | {"MESH_WORKER_PROBE": str(self.probe)}
        result = subprocess.run([os.sys.executable, str(Path(release_workers.__file__)), "verify", str(self.state), str(self.record), label],
            env=environment, capture_output=True, text=True, check=True, timeout=90)
        observed = json.loads(result.stdout)
        expected = sorted({item["artifact"]["binarySha256"] for item in self.selected})
        if observed != expected:
            raise RuntimeError("native observed worker digests differ from the exact selected source cohort")
        return observed

    def close(self):
        release_workers.stop(self.record)
