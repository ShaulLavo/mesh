"""One manual hosted run of the unchanged, pinned C4 catalog diagnostic."""
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
SOURCE = Path(os.environ.get("GITHUB_WORKSPACE", ".")) / "source"
CHECKPOINT = "b0002608614ad41bf92dd477b2ec961a791fa420"
C4 = "c0c899eea10a9c4f5916b0b629b736455d1d5a92"


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def command(arguments, log, environment=None, timeout=120):
    with log.open("xb") as output:
        task = subprocess.Popen(arguments, cwd=SOURCE, env=environment, stdout=output,
                                stderr=subprocess.STDOUT, start_new_session=True)
        try:
            result = task.wait(timeout=timeout)
        finally:
            stop_task(task)
    if result:
        raise subprocess.CalledProcessError(result, arguments)


def stop_task(task):
    if task.poll() is not None:
        return
    os.killpg(task.pid, signal.SIGTERM)
    try:
        task.wait(timeout=60)
    except subprocess.TimeoutExpired:
        os.killpg(task.pid, signal.SIGKILL)
        task.wait(timeout=10)


def source_context(control):
    def git(*arguments):
        return subprocess.check_output(["git", *arguments], cwd=SOURCE, text=True, timeout=10).strip()
    if git("rev-parse", "HEAD") != CHECKPOINT or git("status", "--porcelain"):
        raise RuntimeError("hosted source differs from its clean reviewed checkpoint")
    if git("rev-parse", "--is-shallow-repository") != "false" or git("branch", "--show-current"):
        raise RuntimeError("hosted source requires detached complete history")
    subprocess.run(["git", "merge-base", "--is-ancestor", C4, CHECKPOINT], cwd=SOURCE, check=True, timeout=10)
    environment = dict(os.environ)
    for key in ("GOOS", "GOARCH", "GOROOT"):
        environment.pop(key, None)
    environment["GOTOOLCHAIN"] = "local"
    goroot = subprocess.check_output(["go", "env", "GOROOT"], env=environment, text=True, timeout=10).strip()
    environment.update(GOROOT=goroot, GOTOOLCHAIN="local", PATH=str(Path(goroot) / "bin") + os.pathsep + environment["PATH"])
    actual = subprocess.check_output(["go", "env", "GOROOT", "GOHOSTOS", "GOHOSTARCH"], env=environment, text=True, timeout=10).splitlines()
    if actual != [goroot, "linux", "amd64"] or not Path(goroot).is_absolute():
        raise RuntimeError("selected compiler root or host platform differs")
    write(control / "source-compiler.json", {"checkpoint": CHECKPOINT, "c4": C4, "fullHistory": True,
          "detached": True, "goroot": goroot, "compilerHost": actual[1:]})
    return environment


def acquire(control, payload, environment):
    from fixtures import installed_artifacts, installed_storage
    pin = json.loads((HERE / "producer-pin.json").read_text())
    live = installed_artifacts.verify_set(os.environ["GITHUB_REPOSITORY"], pin["run"], pin["source"], "update-native")
    write(control / "producer-live.json", live)
    if live != pin:
        raise RuntimeError("live complete native producer set differs from the retained pin")
    shutil.copyfile(HERE / "producer-pin.json", control / "producer-pin.json")
    inputs = payload / "input.noindex"
    inputs.mkdir()
    installed_storage.preflight(os.environ["GITHUB_REPOSITORY"], control / "producer-pin.json", "linux-amd64",
                                inputs, control / "storage-preflight.json")
    installed_artifacts.download(os.environ["GITHUB_REPOSITORY"], control / "producer-pin.json", "linux-amd64",
                                 inputs, control / "download.json")
    installed_artifacts.verify_native(inputs, pin["source"], "linux/amd64")
    for name in ("preparation.json", "candidate-reader.json"):
        shutil.copyfile(inputs / "tools" / name, control / name)
    if shutil.disk_usage(payload).free < 4 << 30:
        raise RuntimeError("hosted diagnostic requires its original 4 GiB post-acquisition floor")
    (inputs / "tools/worker-probe").chmod(0o755)
    command(["go", "build", "-o", str(payload / "native-helper"), "./scripts/fixtures/native-helper"],
            control / "native-helper-build.log", environment, timeout=180)
    return inputs


def execute(control, evidence, payload, inputs, environment):
    environment = environment | {"MESH_OWNED_WORKER_INPUT": str(inputs)}
    environment.pop("GH_TOKEN", None)
    environment.pop("GITHUB_TOKEN", None)
    shutil.copyfile(HERE / "amd64-cohort.json", control / "amd64-cohort.json")
    arguments = [sys.executable, "scripts/diagnose-installed-catalog.py", "--source-checkpoint", CHECKPOINT,
                 "--registry", str(inputs / "retained/workers.json"), "--probe", str(inputs / "tools/worker-probe"),
                 "--native-helper", str(payload / "native-helper"), "--c4-cohort", str(control / "amd64-cohort.json"),
                 "--output", str(evidence)]
    write(control / "invocation.json", arguments)
    command(arguments, control / "diagnostic.log", environment, timeout=900)


def cleanup(control, evidence, payload):
    attempted = evidence.exists()
    scratch = evidence / "scratch-cleanup.json"
    clean = not attempted or scratch.exists() and json.loads(scratch.read_text()).get("absent") is True
    if attempted:
        after = evidence / "scoped-services-after.json"
        clean = clean and after.exists() and all(not row["exists"] for row in json.loads(after.read_text()).values())
    record = {"fixtureCleanupConfirmed": clean, "paths": {}}
    if clean:
        short_record = control / "short-root.json"
        short = (Path(json.loads(short_record.read_text())["root"]),) if short_record.exists() else ()
        record["paths"] = remove_payloads((payload, SOURCE) + short)
        (control / "retain-payloads").unlink(missing_ok=True)
    else:
        (control / "retain-payloads").write_text("Parent owns retained payloads until native cleanup is confirmed.\n")
        record["retained"] = [str(payload), str(SOURCE)]
    write(control / "outer-cleanup.json", record)
    if not clean:
        raise RuntimeError("native cleanup unconfirmed; preserve named payloads for parent")


def remove_payloads(paths):
    record = {}
    for path in paths:
        if path.exists():
            shutil.rmtree(path)
        record[str(path)] = {"absent": not path.exists()}
    return record


def interrupted(*_arguments):
    raise InterruptedError("focused hosted diagnostic interrupted")


def finish(control, evidence, payload, primary):
    try:
        cleanup(control, evidence, payload)
    except BaseException as error:
        (control / "retain-payloads").write_text("Parent owns payloads after outer cleanup error.\n")
        if primary is None:
            raise
        primary.add_note("outer cleanup: " + str(error))


def main():
    if platform.system() != "Linux" or platform.machine() != "x86_64" or os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise RuntimeError("vehicle requires a disposable GitHub-hosted Linux AMD64 runner")
    control = Path(os.environ["RUNNER_TEMP"]) / "catalog-control"
    evidence = Path(os.environ["RUNNER_TEMP"]) / "catalog-evidence"
    payload = Path(os.environ["RUNNER_TEMP"]) / "catalog-payload"
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    primary = None
    (control / "retain-payloads").write_text("Diagnostic active; parent owns payloads until cleanup is confirmed.\n")
    try:
        payload.mkdir(exist_ok=False)
        filesystem = subprocess.check_output(["findmnt", "-n", "-o", "FSTYPE", "-T", "/tmp"], text=True, timeout=10).strip()
        if filesystem in ("tmpfs", "ramfs"):
            raise RuntimeError("fixture short scratch must be disk-backed")
        short = Path(tempfile.mkdtemp(prefix="cg-", dir="/tmp"))
        write(control / "short-root.json", {"root": str(short)})
        write(control / "resources-before.json", {"source": str(SOURCE), "vehicle": str(HERE.parents[2]),
              "payload": str(payload), "evidence": str(evidence), "compilerOwner": (control / "compiler-owner").read_text().strip(),
              "created": ["AMD64 archive/extraction", "native helper", "Go scratch and caches", "driver-owned native fixture"],
              "filesystem": subprocess.check_output(["df", "-Pk", str(payload), "/tmp"], text=True, timeout=10)})
        environment = source_context(control)
        sys.path.insert(0, str(SOURCE / "scripts"))
        for name in ("go-cache", "go-mod", "go-tmp"):
            (payload / name).mkdir()
        environment.update(GOCACHE=str(payload / "go-cache"), GOMODCACHE=str(payload / "go-mod"),
                           GOTMPDIR=str(payload / "go-tmp"), PYTHONPATH=str(SOURCE / "scripts"), MESH_SHORT_TMP=str(short))
        inputs = acquire(control, payload, environment)
        execute(control, evidence, payload, inputs, environment)
    except BaseException as error:
        primary = error
        raise
    finally:
        finish(control, evidence, payload, primary)


if __name__ == "__main__":
    main()
