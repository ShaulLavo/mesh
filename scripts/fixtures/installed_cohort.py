"""Owned copies and observations using the authoritative published-worker API."""
import hashlib
import errno
import json
import os
import shutil
import subprocess
from pathlib import Path

from fixtures import release_workers, release_worker_groups


def stage(registry, destination, platform):
    from fixtures import noindex_paths
    registry = Path(registry).resolve(strict=True)
    data = json.loads(registry.read_text())
    if data["schema"] != 1 or data["platform"] != platform or not data["workers"]:
        raise RuntimeError("native exact worker registry schema/platform differs")
    destination.mkdir(parents=True)
    noindex_paths.require(destination)
    owned = os.environ.get("MESH_OWNED_WORKER_INPUT")
    if owned and (os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"):
        raise RuntimeError("owned native artifact links require the disposable hosted input directory")
    owned_root = Path(owned).resolve(strict=True) if owned else None
    records = []
    for item in data["workers"]:
        records.append(copy_record(registry, destination, item, owned_root))
    path = destination / "workers.json"
    path.write_text(json.dumps(data | {"workers": records}, sort_keys=True) + "\n")
    return path


def copy_record(registry, destination, item, owned_root=None):
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
        copy_image(source, target, owned_root)
    with target.open("rb") as stream:
        if hashlib.file_digest(stream, "sha256").hexdigest() != digest:
            raise RuntimeError("copied exact archived worker differs from its published identity")
    return item | {"binary": str(target.relative_to(destination))}


def copy_image(source, target, owned_root):
    if owned_root is None:
        shutil.copy2(source, target)
        return
    if not source.is_relative_to(owned_root) or source.stat().st_uid != os.getuid():
        raise RuntimeError("owned worker source is outside the task-owned artifact directory")
    try:
        os.link(source, target)
    except OSError as error:
        if error.errno != errno.EXDEV:
            raise
        shutil.copy2(source, target)


class Cohort:
    def __init__(self, registry, probe, state, config, version, binary, record, group=None, original=None, catalog=None):
        self.registry, self.probe, self.state, self.config = registry, probe, state, config
        self.version, self.binary, self.record = version, binary, record
        self.group, self.original = group, original
        self.catalog = catalog
        if group is not None:
            self.selected = group["members"]
            return
        script = "import json,sys; from fixtures.release_workers import applicable; print(json.dumps(applicable(*sys.argv[1:])))"
        environment = self.environment() | {"PYTHONPATH": str(Path(release_workers.__file__).parent.parent)}
        data = subprocess.check_output([os.sys.executable, "-c", script, str(registry), version, str(binary)], env=environment, timeout=90)
        self.selected = json.loads(data)

    def environment(self):
        environment = os.environ | {"MESH_WORKER_PROBE": str(self.probe), "MESH_STATE_DIR": str(self.state), "MESH_CONFIG_DIR": str(self.config)}
        if self.original is not None:
            environment["MESH_GROUP_ORIGINAL"] = json.dumps(self.original)
        if self.catalog and not self.catalog.closed:
            environment["MESH_CATALOG_IO_TIMELINE"] = str(self.record.with_suffix(".io.jsonl"))
        return environment

    def start(self):
        # The shared producer persists ownership before each worker can start.
        environment = self.environment()
        if self.group is not None:
            path = self.record.with_name("selected-group.json")
            path.write_bytes(release_worker_groups.encode({"group": self.group, "originalBytes": self.original["imageBytes"]}))
            environment["MESH_WORKER_GROUP"] = str(path)
        try:
            subprocess.run([os.sys.executable, str(Path(release_workers.__file__)), "start-published", str(self.state), str(self.config),
                str(self.registry), self.version, str(self.binary), str(self.record)], env=environment,
                capture_output=True, text=True, check=True, timeout=90)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
            self.retain_verification_failure("startup-publication", error)
            self.finish_catalog("startup-publication", error)
            raise
        return self.verify("before-install")

    def verify(self, label):
        environment = self.environment()
        if self.catalog and label == "before-install":
            self.catalog.checkpoint("before-original-verification")
        try:
            result = subprocess.run([os.sys.executable, str(Path(release_workers.__file__)), "verify", str(self.state), str(self.record), label],
                env=environment, capture_output=True, text=True, check=True, timeout=90)
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
            self.retain_verification_failure(label, error)
            self.finish_catalog(label, error)
            raise
        except BaseException as error:
            self.finish_catalog(label, error)
            raise
        self.finish_catalog(label)
        observed = json.loads(result.stdout)
        expected = sorted({item["artifact"]["binarySha256"] for item in self.selected})
        if observed != expected:
            raise RuntimeError("native observed worker digests differ from the exact selected source cohort")
        return observed

    def retain_verification_failure(self, label, error):
        def raw(value):
            return value if isinstance(value, bytes) else (value or "").encode("utf-8")

        def tail(value):
            return {"tail": value[-32768:].decode("utf-8", "replace"), "bytes": len(value), "truncated": len(value) > 32768}

        try:
            stdout, stderr = raw(error.stdout), raw(error.stderr)
            record = json.loads(self.record.read_bytes())
            record["verificationFailure"] = {"label": label, "returncode": getattr(error, "returncode", None),
                                              "stdout": tail(stdout), "stderr": tail(stderr)}
            if isinstance(error, subprocess.TimeoutExpired):
                record["verificationFailure"]["timeoutSeconds"] = error.timeout
            release_workers.save(self.record, record)
            self.record.with_suffix(".failure.stdout").write_bytes(stdout)
            self.record.with_suffix(".failure.stderr").write_bytes(stderr)
        except (OSError, ValueError) as capture_error:
            error.add_note("cohort verification evidence unavailable: " + type(capture_error).__name__)

    def finish_catalog(self, label, primary=None):
        if not self.catalog or label not in ("before-install", "startup-publication"):
            return
        try:
            phase = "after-failed-startup-publication" if label == "startup-publication" else "after-failed-io" if primary else "after-original-verification"
            self.catalog.stop(phase)
        except Exception as error:
            if primary is None:
                raise
            primary.add_note("catalog evidence unavailable: " + type(error).__name__)

    def close(self):
        release_workers.stop(self.record)
