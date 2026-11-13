"""One diagnostic-only replay of the retained Linux group on a disposable job."""
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import zipfile

import backend

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
sys.path.insert(0, str(ROOT / "scripts"))


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def command(argv, log, cwd=ROOT, environment=None, timeout=120):
    with log.open("xb") as stream:
        subprocess.run(argv, cwd=cwd, env=environment, stdout=stream, stderr=subprocess.STDOUT, check=True, timeout=timeout)


def acquire(scratch, evidence, pins):
    from fixtures import installed_artifacts, installed_storage
    complete = installed_artifacts.verify_set("ShaulLavo/mesh", pins["producerRun"], pins["nativeSource"], pins["artifactPrefix"])
    selected = complete["artifacts"]["linux-amd64"]
    if complete["runHead"] != pins["caller"] or selected != {"id": pins["artifactId"], "name": pins["artifactName"],
        "size": pins["archiveBytes"], "digest": "sha256:" + pins["archiveSha256"]}:
        raise RuntimeError("live producer set differs from the C4 pin")
    (evidence / "producer-pin.json").write_text(json.dumps(complete, indent=2) + "\n")
    meta = installed_artifacts.api(f'repos/ShaulLavo/mesh/actions/artifacts/{pins["artifactId"]}')
    expected = (pins["artifactName"], pins["archiveBytes"], "sha256:" + pins["archiveSha256"], pins["caller"])
    actual = (meta["name"], meta["size_in_bytes"], meta["digest"], meta["workflow_run"]["head_sha"])
    if actual != expected or meta["expired"] or meta["workflow_run"]["id"] != pins["producerRun"]:
        raise RuntimeError("producer artifact no longer matches its exact live pin")
    (evidence / "artifact.json").write_text(json.dumps(meta, indent=2) + "\n")
    free = shutil.disk_usage(scratch).free
    if free < pins["archiveBytes"] + pins["maxExpandedBytes"] + (1 << 30):
        raise RuntimeError("execution host lacks space for the bounded packet")
    (evidence / "storage.json").write_text(json.dumps({"path": str(scratch), "freeBytes": free}) + "\n")
    archive = scratch / "native.zip"
    url = installed_storage.archive_url("ShaulLavo/mesh", pins["artifactId"])
    installed_artifacts.fetch_archive(url, archive, {"id": pins["artifactId"], "size": pins["archiveBytes"], "digest": expected[2]})
    inputs = scratch / "inputs"
    inputs.mkdir()
    with zipfile.ZipFile(archive) as package:
        members = package.infolist()
        if len(members) > pins["maxEntries"] or sum(item.file_size for item in members) > pins["maxExpandedBytes"]:
            raise RuntimeError("native archive exceeds the retained entry or byte bound")
        for item in members:
            check_member(item)
        package.extractall(inputs)
    return inputs


def check_member(item):
    from fixtures import installed_artifacts
    installed_artifacts.require_local_member(item)
    mode = item.external_attr >> 16
    if stat.S_IFMT(mode) not in (0, stat.S_IFDIR, stat.S_IFREG):
        raise RuntimeError("native archive contains a nonregular input")


def prepare_group(inputs, evidence, pins):
    from fixtures import installed_artifacts
    paths = {"candidate-reader.json": inputs / "tools/candidate-reader.json", "preparation.json": inputs / "tools/preparation.json",
             "worker-registry.json": inputs / "retained/workers.json"}
    for name, path in paths.items():
        if path.stat().st_size > 1 << 20:
            raise RuntimeError("pinned packet record exceeds its bound: " + name)
        shutil.copyfile(path, evidence / name)
    installed_artifacts.verify_native(inputs, pins["nativeSource"], "linux/amd64")
    selection = HERE / "driver-selection.json"
    if digest(selection) != pins["selectionSha256"]:
        raise RuntimeError("frozen diagnostic group selection changed")
    descriptor = json.loads(selection.read_bytes())
    registry = json.loads(paths["worker-registry.json"].read_bytes())["workers"]
    by_version = {row["version"]: row for row in registry}
    if len(by_version) != len(registry):
        raise RuntimeError("producer registry has duplicate versions")
    for row in descriptor["group"]["members"]:
        expected = by_version[row["version"]]
        if any(row[key] != expected[key] for key in ("version", "protocol", "commit", "artifact", "binary")):
            raise RuntimeError("diagnostic group member differs from the actual registry")
        path = inputs / "retained" / row["binary"]
        verify_image(path, row["artifact"]["binarySha256"], row["imageBytes"])
        row["binary"] = str(path)
    source = by_version[pins["sourceVersion"]]
    old = inputs / "retained" / source["binary"]
    verify_image(old, pins["sourceDigest"], descriptor["originalBytes"])
    actual_bytes = descriptor["originalBytes"] + sum(row["imageBytes"] for row in descriptor["group"]["members"])
    if descriptor["group"]["index"] != 1 or len(descriptor["group"]["members"]) != 31 or actual_bytes != 934547328:
        raise RuntimeError("diagnostic altered the original group or image bound")
    failing = descriptor["group"]["members"][15]
    if failing["version"] != "v0.1.225" or failing["artifact"]["binarySha256"] != pins["workerDigest"]:
        raise RuntimeError("original W00F image differs from the failed image")
    (evidence / "selected-group.json").write_text(json.dumps(descriptor, indent=2) + "\n")
    return old


def verify_image(path, expected, size):
    if path.stat().st_size != size or digest(path) != expected:
        raise RuntimeError("diagnostic executable differs from its selected image")
    path.chmod(0o755)


def build(scratch, evidence):
    observer, control = scratch / "observer", scratch / "control"
    environment = os.environ | {"CGO_ENABLED": "0", "GOTOOLCHAIN": "local"}
    if subprocess.check_output(["go", "version"], text=True).split()[2] != "go1.27.4":
        raise RuntimeError("diagnostic observer requires explicit Go 1.27.4")
    if subprocess.check_output([os.environ["HISTORICAL_GO"], "version"], text=True).split()[2] != "go1.27.0":
        raise RuntimeError("historical controls require explicit Go 1.27.0")
    command(["go", "build", "-o", str(observer), "./scripts/diagnostics/childstage"], evidence / "observer-build.log", environment=environment)
    module = scratch / "control-module"
    module.mkdir()
    (module / "go.mod").write_text("module childstage-control\n\ngo 1.27.0\n\nrequire github.com/charmbracelet/x/xpty v0.1.4\nrequire golang.org/x/sys v0.47.0\n")
    shutil.copyfile(ROOT / "go.sum", module / "go.sum")
    command([os.environ["HISTORICAL_GO"], "build", "-mod=mod", "-o", str(control), str(HERE.parent / "childstage-control/main.go")],
            evidence / "control-build.log", module, environment)
    command([os.environ["HISTORICAL_GO"], "version", "-m", str(control)], evidence / "control-build-info.txt", module, environment)
    return observer, control


def execute(scratch, evidence, inputs, old, observer, control):
    import importlib.util
    spec = importlib.util.spec_from_file_location("prepare", HERE / "prepare-prologue.py")
    prepare = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(prepare)
    script = scratch / "diagnostic.sh"
    script.write_text(prepare.prepare(ROOT, observer, control, evidence / "observer"))
    (evidence / "prologue-sha256.txt").write_text(digest(script) + "\n")
    for name in ("candidate", "worker-probe", "boundary"):
        (inputs / "tools" / name).chmod(0o755)
    environment = os.environ | {"PYTHONDONTWRITEBYTECODE": "1", "MESH_TRANSITION_PROOF_MODE": "forward-only",
        "MESH_WORKER_REGISTRY": str(inputs / "retained/workers.json"), "MESH_WORKER_GROUP": str(evidence / "selected-group.json"),
        "MESH_WORKER_PROBE": str(inputs / "tools/worker-probe"), "MESH_BOUNDARY_TOOL": str(inputs / "tools/boundary"), "MESH_SHORT_TMP": str(scratch)}
    environment.pop("GH_TOKEN", None)
    environment.pop("GITHUB_TOKEN", None)
    with (evidence / "diagnostic.log").open("xb") as log:
        command = ["timeout", "--kill-after=15s", "600s", "bash", str(script), str(old),
                   str(inputs / "tools/candidate"), "linux/amd64", str(evidence / "proof")]
        return execute_guarded(command, environment, log, evidence)


def execute_guarded(command, environment, log, evidence):
    read, write = os.pipe()
    ready_read, ready_write = os.pipe()
    task = None
    try:
        task = backend.GuardedTarget([sys.executable, str(HERE / "barrier.py"), str(read), str(ready_write)] + command,
                                     (read, ready_write), log, environment=environment, new_session=True)
        (evidence / "outer-guardian.json").write_text(json.dumps({"guardian": task.guardian.pid, "root": task.pid,
            "rootStartsNewSession": True, "controllerDeathCleanup": True}) + "\n")
        backend.wait_barrier(ready_read)
        os.write(write, b"x")
        return task.wait(timeout=630)
    finally:
        close_guarded(task, (read, write, ready_read, ready_write))


def close_guarded(task, descriptors):
    try:
        if task is not None:
            task.close()
    finally:
        for descriptor in descriptors:
            os.close(descriptor)


def main():
    backend.require_disposable()
    evidence = Path(os.environ["CHILDSTAGE_EVIDENCE"])
    evidence.mkdir(parents=True, exist_ok=False)
    pins = json.loads((HERE / "driver-pins.json").read_bytes())
    validate_source(evidence, pins)
    scratch = Path(tempfile.mkdtemp(prefix="cs-", dir=os.environ["RUNNER_TEMP"]))
    owner = backend.KernelRun(evidence, "outer")
    previous = backend.subreaper()
    try:
        (evidence / "inventory.json").write_text(json.dumps({"scratch": str(scratch), "diagnosticOnly": True,
            "nativeAcceptance": False, "resources": ["packet", "compiled tools", "synthetic controls", "original prologue", "owned hooks and cohort"]}) + "\n")
        observer, control = build(scratch, evidence)
        inputs = acquire(scratch, evidence, pins)
        old = prepare_group(inputs, evidence, pins)
        return execute(scratch, evidence, inputs, old, observer, control)
    finally:
        cleanup_error = None
        try:
            remaining = owner.cleanup()
        except BaseException as error:
            cleanup_error = error
            remaining = owner.cleanup_remaining
        if not remaining and cleanup_error is None:
            shutil.rmtree(scratch)
        (evidence / "outer-cleanup.json").write_text(json.dumps({"remainingOwnedTasks": remaining, "scratchAbsent": not scratch.exists(),
            "cleanupError": str(cleanup_error) if cleanup_error else ""}) + "\n")
        backend.subreaper(previous)
        if cleanup_error:
            raise cleanup_error
        if remaining:
            raise RuntimeError("diagnostic outer task cleanup incomplete")


def validate_source(evidence, pins):
    expected = os.environ.get("REVIEWED_DIAGNOSTIC_SOURCE", "")
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    if not re.fullmatch("[0-9a-f]{40}", expected) or actual != expected:
        raise RuntimeError("entire diagnostic source and vehicle must match the fresh review pin")
    if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=all"], cwd=ROOT):
        raise RuntimeError("diagnostic checkout is not clean")
    value = {"driverSource": actual, "reviewedObserverSource": expected, "producerSource": pins["nativeSource"],
             "sourcePinIncludesVehicle": True}
    (evidence / "source-pin.json").write_text(json.dumps(value, indent=2) + "\n")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    raise SystemExit(main())
