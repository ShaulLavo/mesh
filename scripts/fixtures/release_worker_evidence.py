"""Bounded Linux startup context without tracing or changing worker parentage."""
import hashlib
import itertools
import os
from pathlib import Path
import subprocess
import sys
import time


STATUS_FIELDS = {"Uid", "Gid", "Groups", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb",
                 "NoNewPrivs", "Seccomp", "Seccomp_filters", "TracerPid", "NSpid", "NStgid"}
NAMESPACES = ("user", "pid", "mnt", "cgroup")
IMAGE_LIMIT = 256 << 20


def unavailable(error):
    return {"unavailable": type(error).__name__, "errno": getattr(error, "errno", None)}


def read_at(root, name, maximum=8192):
    with os.fdopen(os.open(name, os.O_RDONLY | os.O_CLOEXEC, dir_fd=root), "rb") as stream:
        data = stream.read(maximum + 1)
    if len(data) > maximum:
        raise ValueError("startup context exceeds its byte bound")
    return data.decode(errors="replace")


def process_stat(root):
    fields = read_at(root, "stat").rsplit(") ", 1)[1].split()
    return dict(zip(("state", "parentPid", "group", "session", "terminal", "foregroundGroup", "startTicks"),
                    (fields[0], *map(int, fields[1:6]), int(fields[19]))))


def status(root):
    lines = read_at(root, "status").splitlines()
    return {name: value.strip() for name, _, value in (line.partition(":") for line in lines) if name in STATUS_FIELDS}


def terminals(root):
    descriptor = os.open("fd", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC, dir_fd=root)
    try:
        return terminal_entries(root, descriptor)
    finally:
        os.close(descriptor)


def terminal_entries(root, descriptor):
    result = []
    with os.scandir(descriptor) as entries:
        names = [entry.name for entry in itertools.islice(entries, 129)]
    for name in names[:128]:
        append_terminal(root, name, result)
    return {"entries": result, "descriptorLimitReached": len(names) == 129}


def append_terminal(root, name, result):
    link = os.readlink("fd/" + name, dir_fd=root)
    if not link.startswith("/dev/pts/") and link != "/dev/ptmx":
        return
    info = os.stat("fd/" + name, dir_fd=root)
    result.append({"fd": int(name), "terminal": link, "device": info.st_rdev,
                   "uid": info.st_uid, "gid": info.st_gid, "mode": info.st_mode})


def snapshot(root):
    result = {"monotonicNs": time.monotonic_ns()}
    readers = {"stat": process_stat, "status": status, "terminals": terminals,
               "cgroup": lambda fd: read_at(fd, "cgroup"), "label": lambda fd: read_at(fd, "attr/current", 4096),
               "uidMap": lambda fd: read_at(fd, "uid_map"), "gidMap": lambda fd: read_at(fd, "gid_map"),
               "setgroups": lambda fd: read_at(fd, "setgroups", 128), "namespaces": namespaces,
               "cwd": lambda fd: os.readlink("cwd", dir_fd=fd)}
    for name, reader in readers.items():
        result[name] = attempt(reader, root)
    return result


def namespaces(root):
    return {name: os.readlink("ns/" + name, dir_fd=root) for name in NAMESPACES}


def attempt(operation, *args):
    try:
        return operation(*args)
    except (OSError, ValueError, IndexError) as error:
        return unavailable(error)


def image_digest(descriptor):
    info = os.fstat(descriptor)
    if info.st_size > IMAGE_LIMIT:
        raise ValueError("mapped worker image exceeds the diagnostic byte bound")
    digest, size = hashlib.sha256(), 0
    while data := os.read(descriptor, 1 << 20):
        size += len(data)
        if size > IMAGE_LIMIT:
            raise ValueError("mapped worker image grew beyond the diagnostic byte bound")
        digest.update(data)
    return {"sha256": digest.hexdigest(), "bytes": size, "device": info.st_dev, "inode": info.st_ino,
            "uid": info.st_uid, "gid": info.st_gid, "mode": info.st_mode}


class Startup:
    def __init__(self, pid):
        self.pid, self.root, self.image = pid, None, None
        self.before = {"unavailable": "non-Linux startup"}
        if sys.platform != "linux":
            return
        try:
            self.root = os.open(f"/proc/{pid}", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            # Keep the mapped file available after Go reaps its failed child and the worker exits.
            self.image = os.open("exe", os.O_RDONLY | os.O_CLOEXEC, dir_fd=self.root)
            self.before = snapshot(self.root)
        except OSError as error:
            self.before = unavailable(error)

    def close(self):
        for descriptor in (self.image, self.root):
            if descriptor is not None:
                os.close(descriptor)
        self.image, self.root = None, None

    def failure(self, selected, log):
        mapped = attempt(image_digest, self.image) if self.image is not None else {"unavailable": "mapped image was not opened"}
        result = {"schema": 1, "pid": self.pid, "selectedArtifact": selected, "workerAtObservation": self.before,
                  "workerAtFailure": snapshot(self.root) if self.root is not None else self.before,
                  "mappedImage": mapped, "workerLogTail": log, "failingCall": "unavailable",
                  "callEvidence": "Go reports errno without the failing pre-exec syscall; no syscall tracer was installed"}
        with Startup(os.getpid()) as launcher:
            result["launcherAtFailure"] = launcher.before
        return result

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


def launcher_snapshot():
    root = os.open("/proc/self", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        return snapshot(root)
    finally:
        os.close(root)


def stop_control(process):
    if process.poll() is None:
        process.kill()
    process.communicate(timeout=3)


def positive_control():
    if sys.platform != "linux":
        return {"supported": False, "reason": "Linux procfs observer"}
    process = subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"],
                               stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        with Startup(process.pid) as observed:
            verify_control_context(observed.before)
            verify_control_image(observed.image)
            process.communicate(b"x", timeout=3)
            verify_exited_image(observed.image, process.returncode)
        return {"supported": True, "passed": True, "launcherPid": os.getpid(), "childPid": process.pid,
                "parentageAndCredentials": True, "cgroupAndNamespaces": True, "mappedImageRetainedAfterExit": True}
    finally:
        stop_control(process)


def verify_control_context(context):
    stat, credentials = context.get("stat", {}), context.get("status", {})
    expected = (os.getpid(), os.getsid(0), os.getpgrp(), [str(os.geteuid())], [str(os.getegid())])
    actual = (stat.get("parentPid"), stat.get("session"), stat.get("group"),
              credentials.get("Uid", "").split()[1:2], credentials.get("Gid", "").split()[1:2])
    if actual != expected or sorted(map(int, credentials.get("Groups", "").split())) != sorted(os.getgroups()):
        raise RuntimeError("startup observer control lacks its actual child context")
    with Startup(os.getpid()) as launcher:
        expected = {name: launcher.before.get(name) for name in ("namespaces", "cgroup")}
        actual = {name: context.get(name) for name in expected}
        if actual != expected or any(isinstance(value, dict) and "unavailable" in value for value in actual.values()):
            raise RuntimeError("startup observer control changed its launcher isolation context")


def verify_control_image(descriptor):
    expected, actual = os.stat(sys.executable), os.fstat(descriptor)
    if (expected.st_dev, expected.st_ino) != (actual.st_dev, actual.st_ino):
        raise RuntimeError("startup observer control opened another process image")


def verify_exited_image(descriptor, code):
    mapped = image_digest(descriptor)
    if not mapped["bytes"] or code != 0:
        raise RuntimeError("startup observer control did not retain the exited child's image")


def log_tail(path):
    with Path(path).open("rb") as stream:
        stream.seek(max(0, os.fstat(stream.fileno()).st_size - 4096))
        return stream.read(4096).decode(errors="replace")


def publish(record, row, observed, item, directory, path, save):
    try:
        selected = {"version": item["version"], "sha256": item["artifact"]["binarySha256"]}
        row["startupFailure"] = observed.failure(selected, attempt(log_tail, directory / "worker.log"))
        if "startupLaunchContext" in row:
            row["startupFailure"]["launcherAtLaunch"] = row["startupLaunchContext"]
        save(path, record)
    except Exception as error:
        print("retained worker startup evidence unavailable: " + type(error).__name__, file=sys.stderr)
