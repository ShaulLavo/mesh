"""Hosted healthy-child control for the actual read-only EPERM collectors."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from fixtures import release_worker_evidence as evidence
from fixtures import release_worker_snapshot as snapshot


def kernel_access(result):
    output = result.get("output", "")
    denied = ("permission denied", "operation not permitted", "not seeing messages", "insufficient permissions")
    if any(word in output.lower() for word in denied):
        return "permission-gap"
    if result.get("returncode") != 0:
        return "unavailable"
    if not output.strip() or output.strip() == "-- No entries --":
        return "empty"
    return "available"


def verify_snapshot(value):
    if value.get("cleanupConfirmed") is not True:
        raise RuntimeError("snapshot command ownership unconfirmed")
    mounts = value.get("mounts")
    if not isinstance(mounts, list) or len(mounts) != 3:
        raise RuntimeError("control requires actual readable mounts")
    if any(not row.get("mountId") or not row.get("options") or not row.get("filesystem") for row in mounts):
        raise RuntimeError("control mount identities are incomplete")
    if any(not isinstance(value["sysctls"].get(name), str) or not value["sysctls"][name].strip().isdigit()
           for name in snapshot.SYSCTLS):
        raise RuntimeError("control requires readable known sysctls")
    comparisons = value.get("namespacesComparedToLaunch", {})
    if set(comparisons) != {"workerAtObservation", "workerAtFailure", "launcherAtFailure"}:
        raise RuntimeError("control namespace comparison missing")
    if any(set(context) != {"user", "mnt"} for context in comparisons.values()):
        raise RuntimeError("control namespace identities missing")
    if any(row.get("same") is not True for context in comparisons.values() for row in context.values()):
        raise RuntimeError("control namespace identity differs")
    kernel = value.get("kernelJournal", {})
    arguments = kernel.get("arguments", [])
    if not arguments or arguments[0] != "journalctl" or "--since" not in arguments or "--until" not in arguments:
        raise RuntimeError("actual kernel window attempt missing")
    if kernel.get("unavailable") == "startup deadline exhausted":
        raise RuntimeError("kernel window was never attempted")


def capture_child(process, deadline):
    launcher = evidence.launcher_snapshot()
    with evidence.Startup(process.pid) as observed:
        evidence.verify_control_context(observed.before)
        evidence.verify_control_image(observed.image)
        if not observed.before.get("stat", {}).get("startTicks"):
            raise RuntimeError("known child birth identity missing")
        if not all(key in observed.before.get("status", {}) for key in ("Uid", "Gid", "NoNewPrivs", "Seccomp", "TracerPid", "CapEff")):
            raise RuntimeError("known child security status missing")
        context = {"launcherAtLaunch": launcher, "launcherAtFailure": launcher,
                   "workerAtObservation": observed.before, "workerAtFailure": observed.before}
        value = snapshot.capture(context, os.getcwd(), deadline)
        return {"knownChildPid": process.pid, "knownParentPid": os.getpid(), "proc": context, "snapshot": value}


def positive_control(output):
    path = Path(output)
    value = {"passed": False, "cleanupConfirmed": False, "kind": "healthy owned Python child",
             "limits": "Readability and an attempted kernel window; no denied event, hook or syscall coverage."}
    process = None
    try:
        process = subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"],
                                   stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        value.update(knownChildPid=process.pid, knownParentPid=os.getpid())
        value.update(capture_child(process, time.monotonic() + 6))
        value["kernelAccess"] = {name: kernel_access(result) for name, result in value["snapshot"].items()
                                 if name in ("kernelJournal", "kernelDmesg", "hostedSudoKernelJournal")}
        verify_snapshot(value["snapshot"])
        value["passed"] = True
    except BaseException as error:
        value["error"] = type(error).__name__ + ": " + str(error)
        raise
    finally:
        finish_control(process, value, path)
    return value


def finish_control(process, value, path):
    try:
        if process is not None:
            evidence.stop_control(process)
            value["childReturncode"] = process.returncode
        snapshot_clean = value.get("snapshot", {}).get("cleanupConfirmed", process is None)
        value["cleanupConfirmed"] = snapshot_clean and (process is None or process.poll() is not None)
        if not value["cleanupConfirmed"]:
            value["passed"] = False
            raise RuntimeError("positive control cleanup unconfirmed")
    except BaseException as error:
        value.update(passed=False, cleanupConfirmed=False, cleanupError=type(error).__name__ + ": " + str(error))
        raise
    finally:
        path.write_text(json.dumps(value, indent=2) + "\n")
