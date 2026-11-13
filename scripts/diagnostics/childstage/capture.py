"""Diagnostic cohort-start wrapper. It never runs a native acceptance gate."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time

import backend


HERE = Path(__file__).resolve().parent


def ptrace(observer, output, name, argv, seconds):
    read, write = os.pipe()
    ready_read, ready_write = os.pipe()
    try:
        return guarded_ptrace(observer, output, name, argv, seconds, (read, write, ready_read, ready_write))
    finally:
        for descriptor in (read, write, ready_read, ready_write):
            os.close(descriptor)


def guarded_ptrace(observer, output, name, argv, seconds, pipes):
    read, write, ready_read, ready_write = pipes
    with (output / f"{name}.log").open("xb") as log:
        command = [str(observer), "-output", str(output / f"{name}.jsonl"), "-seconds", str(seconds), "--"] + argv
        child = backend.GuardedTarget([sys.executable, str(HERE / "barrier.py"), str(read), str(ready_write)] + command,
                                      (read, ready_write), log)
        context = {"controller": os.getpid(), "guardian": child.guardian.pid, "observer": child.pid,
                   "ancestry": "controller -> guardian -> ptrace observer -> Python launcher -> Go worker",
                   "ptraceChangesTimingAndSecurityContext": True}
        context_path = output / f"{name}.context.json"
        context_path.write_text(json.dumps(context) + "\n")
        try:
            backend.wait_barrier(ready_read)
            os.write(write, b"x")
            return child.wait(timeout=seconds)
        finally:
            child.close()
            context["guardianReaped"] = True
            context_path.write_text(json.dumps(context) + "\n")


def observe(selected, observer, output, name, argv, seconds, after_exit=None):
    if selected == "bpftrace":
        return backend.KernelRun(output, name).run(argv, seconds, after_exit)
    code = ptrace(observer, output, name, argv, seconds)
    if after_exit:
        after_exit()
    return code


def controls(selected, args, output):
    directory = output / "controls"
    directory.mkdir()
    for mode in ("parent", "parent-thread", "child", "terminal", "clone3"):
        argv = [sys.executable, str(HERE / "launcher.py"), str(args.control), mode, str(directory / f"{mode}.control.json")]
        if observe(selected, args.observer, directory, mode, argv, 25):
            raise RuntimeError("synthetic observer control failed; cohort is prohibited")
    with (directory / "controls.json").open("xb") as stream:
        subprocess.run([sys.executable, str(HERE / "check-controls.py"), str(directory), str(args.control)],
                       stdout=stream, stderr=subprocess.STDOUT, check=True, timeout=10)
    if selected == "ptrace":
        lifecycle_controls(selected, args.observer, args.control, directory)
        death_controls(args.observer, directory)
    return json.loads((directory / "controls.json").read_bytes())


def lifecycle_controls(selected, observer, worker, output):
    spec = importlib.util.spec_from_file_location("lifecycle_controls", HERE / "lifecycle-controls.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    def control(directory, name, argv, seconds):
        return observe(selected, observer, directory, name, argv, seconds)
    return module.exercise(output / "lifecycle", worker, control)


def death_controls(observer, output):
    import test_diagnostic
    results = {}
    for victim in ("controller", "observer"):
        directory = output / f"{victim}-death"
        directory.mkdir()
        results[victim] = test_diagnostic.exercise_ptrace_death(directory, observer, victim)
    (output / "death-controls.json").write_text(json.dumps(results, indent=2) + "\n")
    return results


def stop_cohort(tool, cohort, output):
    if not cohort.is_file():
        return
    if cohort.stat().st_size > 16 << 20:
        raise RuntimeError("partial cohort exceeds diagnostic evidence bound")
    shutil.copyfile(cohort, output / "cohort.json")
    with (output / "cohort-stop.log").open("xb") as log:
        subprocess.run([sys.executable, str(tool), "stop", str(cohort)], stdout=log, stderr=subprocess.STDOUT,
                       timeout=15, check=True)


def summarize(output):
    rows = [json.loads(line) for line in (output / "capture.jsonl").read_text().splitlines()]
    if not rows or rows[-1] != {"event": "teardown", "remainingTasks": 0, "observerError": ""}:
        raise RuntimeError("capture cleanup or observer coverage is unconfirmed")
    cohort = json.loads((output / "cohort.json").read_bytes()) if (output / "cohort.json").is_file() else {}
    results = []
    for worker in cohort.get("workers", []):
        failure = worker.get("startupFailure")
        if failure:
            bindings = json.loads((output / "selected-images.json").read_bytes()) if (output / "selected-images.json").exists() else {}
            results.append(worker_calls(worker, failure, rows, bindings))
    result = {"diagnosticOnly": True, "nativeAcceptance": False, "workers": results,
              "actualErrno1Captured": any(row["startupCalls"] for row in results),
              "oldCauseProved": False, "traceTeardown": rows[-1] if rows else None}
    (output / "capture-summary.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def worker_calls(worker, failure, rows, bindings=None):
    image = failure.get("mappedImage", {})
    execs = [row for row in rows if row.get("event") == "exec" and row["tid"] == worker["pid"]]
    if len(execs) != 1:
        raise RuntimeError("selected worker lacks an unambiguous exec identity")
    identity = execs[0]["image"]
    calls = [row for row in rows if row.get("event") == "return" and row["result"] == -1
             and row["owner"] == worker["pid"] and row.get("image") == identity]
    actual = bound_image(worker, image, calls, identity, bindings or {})
    verify_start_ticks(failure, execs[0])
    startup = [row for row in calls if startup_call(row, worker["pid"], rows)]
    return {"worker": worker["pid"], "selectedArtifact": failure["selectedArtifact"], "mappedImage": image,
            "observerMappedImage": actual, "workerExec": execs[0], "calls": calls, "startupCalls": startup,
            "attribution": "startup candidates require a refused process clone or owned pre-exec child; historical cause remains unproved"}


def startup_call(row, worker, rows):
    if row.get("preExec"):
        return any(created.get("event") == "created" and created["tid"] == row["tid"]
                   and created["creatorTgid"] == worker and created["preExec"] for created in rows)
    if row.get("tgid") != worker:
        return False
    call = row.get("call", {})
    if call.get("number") == 56:
        return bool(call["args"][0] & 0xFF == signal.SIGCHLD) and parent_pty(row)
    if call.get("number") == 435:
        words = call.get("clone3", [])
        return len(words) >= 5 and words[4] == signal.SIGCHLD and not words[0] & 0x10000 and parent_pty(row)
    return False


def parent_pty(row):
    terminals = row.get("context", {}).get("terminals", [])
    names = {terminal.get("terminal", "") for terminal in terminals}
    return bool(names & {"/dev/ptmx", "/dev/pts/ptmx"}) and any(name.startswith("/dev/pts/") and name != "/dev/pts/ptmx" for name in names)


def bound_image(worker, original, calls, identity, bindings):
    observed = [row["mappedImage"] for row in calls if row.get("mappedImage")]
    if original.get("sha256"):
        observed.append(original)
    if identity in bindings:
        observed.append(bindings[identity])
    if not observed:
        raise RuntimeError("selected worker has no mapped-image byte binding")
    for image in observed:
        actual = f"{image.get('device')}:{image.get('inode')}:{image.get('bytes')}"
        if image.get("sha256") != worker["digest"] or actual != identity:
            raise RuntimeError("partial cohort lost selected/mapped image binding")
    return observed[0]


def verify_start_ticks(failure, executed):
    before = failure.get("workerAtObservation", {}).get("stat", {})
    expected = before.get("startTicks")
    context = executed.get("context", {})
    actual = context.get("stat", [None])[-1]
    if "startBootNs" in executed:
        actual = executed["startBootNs"] * os.sysconf("SC_CLK_TCK") // 1_000_000_000
    if expected is not None and str(actual) != str(expected):
        raise RuntimeError("worker exec and held proc identity have different start times")


def selected_images(output):
    descriptor = Path(os.environ["MESH_WORKER_GROUP"])
    if descriptor.stat().st_size > 1 << 20:
        raise RuntimeError("selected group descriptor exceeds its document bound")
    members = json.loads(descriptor.read_bytes())["group"]["members"]
    if not 1 <= len(members) <= 31:
        raise RuntimeError("diagnostic requires one original bounded group")
    result = {}
    for member in members:
        st = Path(member["binary"]).stat()
        identity = f"{st.st_dev}:{st.st_ino}:{st.st_size}"
        result[identity] = {"device": st.st_dev, "inode": st.st_ino, "bytes": st.st_size,
                            "sha256": member["artifact"]["binarySha256"],
                            "byteVerifier": "unchanged selected_group verification inside the observed launcher"}
    (output / "selected-images.json").write_text(json.dumps(result, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--authorized-disposable-diagnostic", action="store_true", required=True)
    parser.add_argument("--observer", type=Path, required=True)
    parser.add_argument("--control", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cohort-tool", type=Path, required=True)
    parser.add_argument("values", nargs=6, metavar="COHORT_ARGUMENT")
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, terminate)
    backend.require_disposable()
    if Path(args.values[-1]).exists() or Path(args.values[-1]).is_symlink():
        raise RuntimeError("diagnostic refuses pre-existing cohort state")
    args.output.mkdir(parents=True, exist_ok=False)
    started = time.monotonic()
    (args.output / "inventory.json").write_text(json.dumps({"preexistingCohort": Path(args.values[-1]).exists(),
        "newResources": ["observer hooks", "owned synthetic controls", "owned cohort descendants", "small evidence files"],
        "observerSha256": hashlib.sha256(args.observer.read_bytes()).hexdigest(),
        "controlSha256": hashlib.sha256(args.control.read_bytes()).hexdigest(), "deadlineSeconds": 600,
        "productDeadlinesUnchanged": True, "uid": os.geteuid(), "gid": os.getegid()}) + "\n")
    selected = None
    try:
        capabilities = backend.capabilities(args.output)
        selected = "bpftrace" if capabilities["compilePassed"] else "ptrace"
        passed = controls(selected, args, args.output)
        (args.output / "control-gate.json").write_text(json.dumps({"backend": selected, "passed": passed}) + "\n")
        selected_images(args.output)
        argv = [sys.executable, str(args.cohort_tool), "start"] + args.values
        def callback():
            stop_cohort(args.cohort_tool, Path(args.values[-1]), args.output)

        code = observe(selected, args.observer, args.output, "capture", argv, 600, callback)
        result = summarize(args.output)
        if not result["actualErrno1Captured"]:
            raise RuntimeError("no actual failing call captured; stop without another cohort")
        return code
    finally:
        cohort = Path(args.values[-1])
        if cohort.is_file() and not (args.output / "cohort.json").exists():
            stop_cohort(args.cohort_tool, cohort, args.output)
        (args.output / "driver-finish.json").write_text(json.dumps({"backend": selected, "elapsedSeconds": time.monotonic() - started,
            "diagnosticOnly": True, "nativeAcceptance": False, "noRetry": True}) + "\n")


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    raise SystemExit(main())
