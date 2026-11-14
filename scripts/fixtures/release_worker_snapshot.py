"""Cheap, bounded, read-only Linux context collected only after startup EPERM."""
from datetime import datetime, timezone
import os
from pathlib import Path
import select
import subprocess
import time

from fixtures import release_worker_evidence as evidence


OUTPUT_LIMIT = 16384
SYSCTLS = ("kernel/apparmor_restrict_unprivileged_userns", "kernel/unprivileged_userns_clone",
           "user/max_user_namespaces", "user/max_mnt_namespaces", "user/max_pid_namespaces")


def command(arguments, deadline):
    end = min(deadline, time.monotonic() + 0.4)
    if end <= time.monotonic():
        return {"unavailable": "startup deadline exhausted", "arguments": arguments,
                "cleanup": {"reaped": True, "stdoutClosed": True}}
    process = None
    try:
        process = subprocess.Popen(arguments, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        read_end = end - min(0.1, max(0, end - time.monotonic()) / 4)
        result = read_command(process, arguments, read_end)
    except Exception as error:
        result = dict(evidence.unavailable(error), arguments=arguments)
    finally:
        cleanup = close_command(process, end)
    result["cleanup"] = cleanup
    if process is not None:
        result["returncode"] = process.returncode
    return result


def close_command(process, deadline):
    if process is None:
        return {"reaped": True, "stdoutClosed": True}
    result = {"pid": process.pid, "reaped": False, "stdoutClosed": False}
    try:
        terminate_command(process)
        process.wait(timeout=max(0, deadline - time.monotonic()))
        result["reaped"] = True
    except Exception as error:
        result["error"] = evidence.unavailable(error)
    finally:
        close_stdout(process, result)
    return result


def terminate_command(process):
    if process.poll() is not None:
        return
    try:
        process.kill()
    except ProcessLookupError:
        pass


def close_stdout(process, result):
    try:
        process.stdout.close()
        result["stdoutClosed"] = True
    except Exception as error:
        result["stdoutError"] = evidence.unavailable(error)


def command_clean(result):
    cleanup = result.get("cleanup", {})
    return cleanup.get("reaped") is True and cleanup.get("stdoutClosed") is True


def read_command(process, arguments, end):
    output = bytearray()
    eof = False
    while time.monotonic() < end and len(output) < OUTPUT_LIMIT:
        ready, _, _ = select.select([process.stdout], [], [], max(0, end - time.monotonic()))
        if not ready:
            break
        block = os.read(process.stdout.fileno(), min(4096, OUTPUT_LIMIT - len(output)))
        if not block:
            eof = True
            break
        output.extend(block)
    return {"arguments": arguments, "output": output.decode(errors="replace"), "bytes": len(output),
            "byteLimitReached": len(output) == OUTPUT_LIMIT, "incomplete": not eof, "returncode": process.poll()}


def read_file(path, maximum=8192):
    with Path(path).open("rb") as stream:
        data = stream.read(maximum + 1)
    if len(data) > maximum:
        raise ValueError("EPERM snapshot file exceeds byte bound")
    return data.decode(errors="replace")


def mount_for(path, lines):
    actual = os.path.realpath(path)
    rows = [mount_fields(line) for line in lines]
    matches = [row for row in rows if row[4] == "/" or actual == row[4] or actual.startswith(row[4].rstrip("/") + "/")]
    fields = max(matches, key=lambda row: len(row[4]))
    separator = fields.index("-")
    return {"requestedPath": str(path), "actualPath": actual, "mountId": fields[0], "mountPoint": fields[4],
            "options": fields[5], "filesystem": fields[separator + 1], "superOptions": fields[separator + 3]}


def mount_fields(line):
    fields = line.split()
    fields[4] = fields[4].replace(r"\040", " ").replace(r"\011", "\t").replace(r"\012", "\n").replace(r"\134", "\\")
    return fields


def mounts(working_directory):
    lines = read_file("/proc/self/mountinfo", 131072).splitlines()
    return [mount_for(path, lines) for path in ("/bin", "/bin/sh", working_directory)]


def namespace_comparison(failure):
    original = failure.get("launcherAtLaunch", {}).get("namespaces", {})
    contexts = ("workerAtObservation", "workerAtFailure", "launcherAtFailure")
    return {name: compare_namespaces(original, failure.get(name, {}).get("namespaces", {})) for name in contexts}


def compare_namespaces(original, actual):
    return {name: {"original": original.get(name), "observed": actual.get(name),
                   "same": original[name] == actual[name] if name in original and name in actual else None}
            for name in ("user", "mnt")}


def capture(failure, working_directory, deadline):
    until = time.time()
    since = until - 60
    result = {"schema": 1, "windowUnixSeconds": {"since": since, "until": until},
              "sysctls": {name: evidence.attempt(read_file, "/proc/sys/" + name, 256) for name in SYSCTLS},
              "mounts": evidence.attempt(mounts, working_directory),
              "mountsNamespace": failure.get("launcherAtFailure", {}).get("namespaces", {}).get("mnt"),
              "namespacesComparedToLaunch": namespace_comparison(failure),
              "shellChild": {"unavailable": "pre-exec child PID was not published; worker shellPid is zero"}}
    args = ["journalctl", "-k", "--since", "@" + str(since), "--until", "@" + str(until),
            "--no-pager", "-n", "128", "-o", "short-iso"]
    result["kernelJournal"] = command(args, deadline)
    if not command_clean(result["kernelJournal"]):
        return result | {"cleanupConfirmed": False}
    if not readable(result["kernelJournal"]):
        result["kernelDmesg"] = command(["dmesg", "--time-format", "iso", "--since", iso(since), "--until", iso(until)], deadline)
    if "kernelDmesg" in result and not command_clean(result["kernelDmesg"]):
        return result | {"cleanupConfirmed": False}
    needs_access = not readable(result["kernelJournal"]) and not readable(result.get("kernelDmesg", {}))
    if needs_access and os.environ.get("MESH_WORKER_EPERM_SUDO") == "1" and os.environ.get("GITHUB_ACTIONS") == "true":
        result["hostedSudoKernelJournal"] = command(["sudo", "-n"] + args, deadline)
    result["cleanupConfirmed"] = all(command_clean(value) for name, value in result.items() if name in
                                     ("kernelJournal", "kernelDmesg", "hostedSudoKernelJournal"))
    return result


def readable(result):
    return result.get("returncode") == 0 and result.get("bytes", 0) > 0


def iso(value):
    return datetime.fromtimestamp(value, timezone.utc).isoformat()
