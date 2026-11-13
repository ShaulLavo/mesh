"""Build and exercise only owned synthetic processes through the heavy runner."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time

import capture


def command(args, cwd, log, timeout=90):
    environment = dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN="local", PYTHONDONTWRITEBYTECODE="1")
    result = subprocess.run(args, cwd=cwd, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    log.write_bytes(result.stdout)
    if result.returncode:
        raise RuntimeError(f"synthetic step failed with status {result.returncode}, see {log.name}")


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def clean_tasks(report):
    remaining = []
    for trace in report.rglob("*.jsonl"):
        for line in trace.read_text().splitlines():
            row = json.loads(line)
            pid = row.get("root") if row.get("event") == "armed" else row.get("tid")
            if pid and Path(f"/proc/{pid}").exists():
                remaining.append(pid)
    return sorted(set(remaining))


def controls(root, scratch, report):
    here = root / "scripts/diagnostics/childstage"
    observer, worker = scratch / "childstage", scratch / "worker-control"
    go_root = Path.home() / ".local/share/mise/installs/go"
    modern, historical = go_root / "1.27.4/bin/go", go_root / "1.27.0/bin/go"
    if not modern.is_file() or not historical.is_file():
        raise RuntimeError("required installed Go compilers are absent")
    command([str(modern), "build", "-o", str(observer), "./scripts/diagnostics/childstage"], root, report / "observer-build.log")
    (scratch / "go.mod").write_text("module mesh-childstage-control\n\ngo 1.27.0\n\nrequire github.com/charmbracelet/x/xpty v0.1.4\nrequire golang.org/x/sys v0.47.0\n")
    shutil.copyfile(root / "go.sum", scratch / "go.sum")
    command([str(historical), "build", "-mod=mod", "-o", str(worker),
             str(root / "scripts/diagnostics/childstage-control/main.go")], scratch, report / "control-build.log")
    command([str(historical), "version", "-m", str(worker)], scratch, report / "control-build-info.txt")
    (report / "binaries.json").write_text(json.dumps({"observerSha256": digest(observer), "controlSha256": digest(worker),
        "controlBytes": worker.stat().st_size, "observerBytes": observer.stat().st_size}) + "\n")
    for mode in ("parent", "parent-thread", "child", "terminal", "clone3"):
        code = capture.ptrace(observer, report, mode, [sys.executable, str(here / "launcher.py"), str(worker), mode,
                                                    str(report / f"{mode}.control.json")], 25)
        if code:
            raise RuntimeError(f"synthetic {mode} failed with status {code}")
    command([sys.executable, str(here / "check-controls.py"), str(report), str(worker)], root, report / "controls.json")
    capture.lifecycle_controls("ptrace", observer, worker, report)
    capture.death_controls(observer, report)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, terminate)
    args.report.mkdir(parents=True, exist_ok=False)
    root = Path(__file__).resolve().parents[3]
    started = time.time()
    inventory = {"startedUtc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(started)),
                 "existingReport": False, "resourcesCreated": ["scratch directory", "two compiled binaries",
                 "finite compiler processes", "observer and synthetic Python/Go descendants", "small report files"],
                 "sharedCachesPreserved": True, "hostedWork": False, "meshExecution": False}
    (args.report / "inventory.json").write_text(json.dumps(inventory, indent=2) + "\n")
    scratch_path = None
    try:
        with tempfile.TemporaryDirectory(prefix="mesh-childstage-controls-", dir="/work/tmp") as scratch:
            scratch_path = scratch
            controls(root, Path(scratch), args.report)
    finally:
        cleanup = {"scratchAbsent": scratch_path is None or not Path(scratch_path).exists(),
                   "remainingOwnedTasks": clean_tasks(args.report), "elapsedSeconds": time.time() - started}
        (args.report / "cleanup.json").write_text(json.dumps(cleanup, indent=2) + "\n")
        if cleanup["remainingOwnedTasks"] or not cleanup["scratchAbsent"]:
            raise RuntimeError("synthetic control cleanup incomplete")


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    main()
