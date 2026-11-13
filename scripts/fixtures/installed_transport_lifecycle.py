"""Bounded diagnostic coverage of the actual post-adoption original attachment."""
import json
import ipaddress
import os
from pathlib import Path
import shutil
import stat
import time

from fixtures import installed_clock

FLAG = "MESH_TRANSPORT_LIFECYCLE"
MARKER = {"schema": 1, "diagnosticOnly": True, "payloads": False}
MAXIMUM_FILE = 2 << 20
MAXIMUM_TOTAL = 64 << 20
MAXIMUM_PROCESSES = 32
REQUIRED = ("request.begin", "upgrade.done", "handshake.begin", "authorize.allow", "handshake.done",
            "acceptance.done", "stream.begin", "attach.read", "attached.write")


class CoverageIncomplete(RuntimeError):
    pass


def enabled():
    return os.environ.get(FLAG) == "1"


def configure(case):
    if not enabled():
        return
    clock = installed_clock.current()
    if clock is None or clock.plan.mode != "diagnostic" or clock.plan.workload.get("transportLifecycle") != MARKER:
        raise CoverageIncomplete("transport lifecycle observer requires its marked diagnostic clock")
    directory = case.root / "transport-lifecycle"
    directory.mkdir(mode=0o700)
    case.environment["MESH_TRANSPORT_LIFECYCLE_DIR"] = str(directory)


def capture_original(case):
    if not enabled():
        return
    from fixtures import installed_native
    image = installed_native.native_services.mapped_image(case.terminal.pid, case.binary)
    if image["sha256"] != case.pair["daemon"]["artifact"]["binarySha256"]:
        raise CoverageIncomplete("diagnostic original client differs from the exact published source image")
    case.transport_original = {"client": image, "worker": case.worker, "shell": case.shell,
                               "session": case.session, "processStamps": case.process_stamps.copy()}


def read_process(path):
    if not stat.S_ISREG(path.lstat().st_mode) or path.stat().st_size > MAXIMUM_FILE:
        raise CoverageIncomplete("transport lifecycle file is not a bounded regular file")
    data = path.read_bytes()
    lines = data.splitlines()
    if not data.endswith(b"\n") or not lines or len(lines) > 4096 or any(len(line) > 1024 for line in lines):
        raise CoverageIncomplete("transport lifecycle records are incomplete or oversized")
    rows = [json.loads(line) for line in lines]
    ordered = sorted(rows, key=lambda row: row["sequence"])
    if any(row.get("schema") != 1 or row.get("dropped") != 0 for row in rows):
        raise CoverageIncomplete("transport lifecycle observer lost event coverage")
    if [row["sequence"] for row in ordered] != list(range(1, len(rows) + 1)) or ordered[0]["stage"] != "process.begin":
        raise CoverageIncomplete("transport lifecycle observer sequence is incomplete")
    return rows


def records(case):
    return {path.name: read_process(path) for path in bounded_paths(case)}


def bounded_paths(case):
    directory = case.root / "transport-lifecycle"
    paths = sorted(directory.glob("lifecycle-*.jsonl"))
    if not paths or len(paths) > MAXIMUM_PROCESSES or sum(path.stat().st_size for path in paths) > MAXIMUM_TOTAL:
        raise CoverageIncomplete("transport lifecycle process/file coverage is absent or exceeds its bound")
    if any(not stat.S_ISREG(path.lstat().st_mode) or path.stat().st_size > MAXIMUM_FILE for path in paths):
        raise CoverageIncomplete("transport lifecycle retention refused an unbounded or nonregular file")
    return paths


def covered_connection(rows, session):
    attached = [row for row in rows if row["stage"] == "attached.write" and row.get("session") == session]
    for row in attached:
        connection = row["connection"]
        events = [event for event in rows if event.get("connection") == connection]
        stages = {event["stage"] for event in events if event.get("error", "ok") == "ok"}
        if set(REQUIRED).issubset(stages) and ordered_attachment(events, session, row["sequence"]):
            return {"connection": connection, "remote": row.get("remote"), "local": row.get("local"), "stages": sorted(stages)}
    raise CoverageIncomplete("original post-adoption attachment lacks positive current-daemon transport coverage")


def ordered_attachment(events, session, attached_sequence):
    cursor = 0
    for stage in REQUIRED:
        matching = [event["sequence"] for event in events if event["stage"] == stage
                    and cursor < event["sequence"] <= attached_sequence
                    and event.get("error", "ok") == "ok"
                    and (stage not in ("attach.read", "attached.write") or event.get("session") == session)]
        if not matching:
            return False
        cursor = min(matching)
    return True


def prove(case, bootstrap, boundary="post-adoption before selected cohort"):
    if not enabled():
        return None
    original = case.transport_original
    if case.terminal.pid != original["client"]["pid"] or case.terminal.poll() is not None:
        raise CoverageIncomplete("original diagnostic client exited or was replaced before the workload gate")
    from fixtures import installed_native
    daemon = installed_native.peer_pid(case.state / "daemon.sock")
    images = getattr(case, "transport_images", {})
    if daemon not in images:
        images[daemon] = installed_native.native_services.mapped_image(daemon, case.binary)
        case.transport_images = images
    image = images[daemon]
    if image["sha256"] != bootstrap["running"]["digest"]:
        raise CoverageIncomplete("coverage gate daemon differs from the actual adopted image")
    deadline = time.monotonic() + installed_clock.timeout(1)
    observed = await_original(case, daemon, bootstrap, deadline)
    if (observed["remote"], observed["local"]) not in original_sockets(case.terminal.pid) or case.terminal.poll() is not None:
        raise CoverageIncomplete("covered session does not belong to the live original client's TCP socket")
    result = MARKER | {"status": "covered", "original": original, "daemon": image,
                       "running": {key: bootstrap["running"][key] for key in ("version", "commit", "digest")},
                       "observed": observed, "clientTcpTupleMatched": True,
                       "boundary": boundary}
    checkpoints = getattr(case, "transport_checkpoints", [])
    if len(checkpoints) >= 16:
        raise CoverageIncomplete("transport lifecycle inspection checkpoint bound exceeded")
    checkpoints.append(result)
    case.transport_checkpoints = checkpoints
    (case.root / "transport-coverage.json").write_text(json.dumps({"schema": 1, "checkpoints": checkpoints}) + "\n")
    return result


def checkpoint(case, version, boundary):
    if enabled():
        prove(case, {"running": case.releases.builds[version]}, boundary)


def read_bounded(path, maximum):
    with Path(path).open("rb") as stream:
        data = stream.read(maximum + 1)
    if len(data) > maximum:
        raise CoverageIncomplete("original-client socket observation exceeded its read bound")
    return data


def original_sockets(pid):
    root = Path("/proc") / str(pid)
    descriptors = list((root / "fd").iterdir())
    if root.stat().st_uid != os.getuid() or len(descriptors) > 256:
        raise CoverageIncomplete("original-client socket ownership or descriptor bound differs")
    inodes = {target[8:-1] for target in (os.readlink(path) for path in descriptors) if target.startswith("socket:[")}
    rows = read_bounded(root / "net/tcp", 256 << 10).splitlines()[1:]
    rows += read_bounded(root / "net/tcp6", 256 << 10).splitlines()[1:]
    return {(tcp_endpoint(fields[1]), tcp_endpoint(fields[2])) for fields in (row.decode().split() for row in rows)
            if len(fields) >= 10 and fields[9] in inodes and fields[3] == "01"}


def tcp_endpoint(value):
    address, port = value.split(":")
    packed = b"".join(bytes.fromhex(address[index:index + 8])[::-1] for index in range(0, len(address), 8))
    ip = ipaddress.ip_address(packed)
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped:
        ip = ip.ipv4_mapped
    host = "[" + str(ip) + "]" if isinstance(ip, ipaddress.IPv6Address) else str(ip)
    return host + ":" + str(int(port, 16))


def await_original(case, daemon, bootstrap, deadline):
    while True:
        try:
            return matching_original(records(case), daemon, bootstrap["running"], case.session)
        except (CoverageIncomplete, ValueError, KeyError) as error:
            last = error
        if time.monotonic() >= deadline:
            raise last
        time.sleep(min(0.01, installed_clock.timeout(0.01)))


def matching_original(processes, daemon, running, session):
    for filename, rows in processes.items():
        identity = rows[0].get("identity", {})
        if identity != {"pid": daemon, "digest": running["digest"], "version": running["version"], "commit": running["commit"]}:
            continue
        connection = covered_connection(rows, session)
        if not connection["remote"] or not connection["local"]:
            raise CoverageIncomplete("actual original attachment lacks its numeric TCP tuple")
        return {"file": filename, "identity": identity} | connection
    raise CoverageIncomplete("original attachment has no event file for the mapped current daemon")


def retain(case, output, name, primary=None):
    if not enabled():
        return None
    result = MARKER | {"status": "incomplete", "accepted": False}
    try:
        paths = bounded_paths(case)
        destination = output / (name + "-transport-lifecycle")
        destination.mkdir(exist_ok=True)
        for path in paths:
            shutil.copyfile(path, destination / path.name)
        processes = records(case)
        if any(rows[-1]["stage"] != "process.end" for rows in processes.values()):
            raise CoverageIncomplete("transport lifecycle process shutdown coverage is incomplete")
        coverage = json.loads((case.root / "transport-coverage.json").read_text())
        result.update(status="covered", coverage=coverage, processFiles=sorted(processes))
    except Exception as error:
        result["errorType"] = type(error).__name__
    try:
        (output / (name + "-transport-coverage.json")).write_text(json.dumps(result) + "\n")
    except OSError:
        result.update(status="incomplete", errorType="RetentionFailure")
    return result


def finish(case, record, output):
    coverage = retain(case, output, case.host["name"])
    if coverage is None:
        return
    record["transportLifecycle"] = coverage
    if coverage["status"] != "covered":
        record["passed"] = False


def sampled_failure(error):
    sampled = getattr(error, "mapped_image", None)
    if not isinstance(sampled, dict):
        return None
    return {side: {key: row[key] for key in ("device", "inode") if type(row.get(key)) is int}
            for side, row in sampled.items() if side in ("expected", "mapped") and isinstance(row, dict)}
