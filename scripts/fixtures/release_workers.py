"""Exact archived worker cohorts shared by release proofs and installed catch-up."""
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import sqlite3
import struct
import subprocess
import sys
import time


def version(value):
    parts = value.removeprefix("v").split(".")
    if len(parts) != 3 or any(not part.isdecimal() for part in parts):
        raise ValueError("worker applicability requires an actual stable source version")
    return tuple(map(int, parts))


def applicable(registry_path, source_version, source_binary):
    registry_path = Path(registry_path)
    registry = json.loads(registry_path.read_bytes())
    if registry["schema"] != 1:
        raise ValueError("unsupported exact worker registry")
    selected = []
    for item in registry["workers"]:
        if version(item["version"]) > version(source_version):
            continue
        binary = (registry_path.parent / item["binary"]).resolve()
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        if digest != item["artifact"]["binarySha256"]:
            raise ValueError("retained worker executable changed")
        selected.append(dict(item, binary=str(binary)))
    source_binary = Path(source_binary).resolve()
    source_digest = hashlib.sha256(source_binary.read_bytes()).hexdigest()
    if not any(item["artifact"]["binarySha256"] == source_digest for item in selected):
        build = json.loads(subprocess.check_output([str(source_binary), "version", "--json"], timeout=10))
        selected.append({"version": source_version, "protocol": build["workerProtocol"], "binary": str(source_binary),
                         "artifact": {"binarySha256": source_digest, "platform": registry["platform"]}})
    if not selected or len(selected) > 4096:
        raise ValueError("exact worker cohort exceeds its existing count bound")
    return selected


def read_exact(stream, count):
    data = bytearray()
    while len(data) < count:
        block = stream.recv(count - len(data))
        if not block:
            raise RuntimeError("worker closed its protocol stream")
        data.extend(block)
    return bytes(data)


def frame(stream, kind, payload):
    stream.sendall(bytes([kind]) + struct.pack(">I", len(payload)) + payload)


def control(stream, value):
    frame(stream, 1, json.dumps(value).encode())


def session_io(endpoint, session, token):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(6)
        connection.connect(str(endpoint))
        control(connection, {"type": "session.attach", "sessionId": session, "cols": 80, "rows": 24})
        command = f"printf '\\n%s\\n' '{token}'\n".encode()
        frame(connection, 3, session.encode().ljust(8, b"\0") + command)
        await_token(connection, token)
        control(connection, {"type": "session.detach", "sessionId": session})


def await_token(connection, token):
    output = bytearray()
    for _ in range(256):
        header = read_exact(connection, 5)
        length = struct.unpack(">I", header[1:])[0]
        if length > 4 << 20:
            raise ValueError("worker frame exceeds the existing protocol bound")
        payload = read_exact(connection, length)
        if header[0] == 1:
            check_control(payload)
        if header[0] == 2:
            output.extend(payload[16:])
        if token.encode() in output.replace(b"\r", b"").splitlines():
            return
    raise RuntimeError("retained worker did not exchange its cohort token")


def check_control(payload):
    value = json.loads(payload)
    if value["type"] == "error":
        raise RuntimeError(f"daemon or worker refused cohort I/O: {value}")


def process_info(pid):
    output = subprocess.check_output(["ps", "-p", str(pid), "-o", "ppid=,rss="], timeout=5).split()
    if len(output) != 2:
        raise RuntimeError("retained worker or shell disappeared")
    return int(output[0]), int(output[1])


def save(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, sort_keys=True) + "\n")
    temporary.replace(path)


def reserve_worker(binary, state, index, environment):
    help_text = subprocess.check_output([str(binary), "session-worker", "--help"], env=environment, timeout=10, stderr=subprocess.STDOUT).decode()
    if "--instance" not in help_text:
        return {"id": "W" + format(index, "03X")}
    probe = environment.get("MESH_WORKER_PROBE")
    if not probe:
        raise ValueError("reserved worker startup requires the maintained reservation exporter")
    output = state / ("cohort-reservation-" + str(index) + ".json")
    child = dict(environment, MESH_COHORT_RESERVE_ROOT=str(state / "s"), MESH_COHORT_RESERVATION=str(output))
    result = subprocess.run([probe, "-test.v", "-test.run=^TestReserveRetainedWorkerCohort$"], env=child,
                            capture_output=True, timeout=30, check=True)
    if b"--- PASS: TestReserveRetainedWorkerCohort" not in result.stdout:
        raise RuntimeError("maintained worker reservation exporter did not run")
    return json.loads(output.read_bytes())


def worker_arguments(binary, reservation, directory):
    args = [str(binary), "session-worker", "--id", reservation["id"], "--dir", str(directory), "--cols", "80", "--rows", "24"]
    if "instance" in reservation:
        args.extend(["--instance", reservation["instance"], "--boot", reservation["bootId"], "--purpose", reservation["purpose"]])
    return args + ["--", "/bin/sh"]


def start_one(state, config, item, index, boot, host, record, path):
    binary = Path(item["binary"])
    binary.chmod(0o755)
    environment = dict(os.environ, MESH_STATE_DIR=str(state), MESH_CONFIG_DIR=str(config), MESH_HOST_ID=host)
    reservation = reserve_worker(binary, state, index, environment)
    session = reservation["id"]
    directory = state / "s" / session
    if "instance" not in reservation:
        directory.mkdir(mode=0o700)
    with (directory / "worker.log").open("wb") as log:
        process = subprocess.Popen(worker_arguments(binary, reservation, directory), env=environment, stdout=log, stderr=log)
    row = {"session": session, "pid": process.pid, "shellPid": 0, "digest": item["artifact"]["binarySha256"],
           "protocol": item["protocol"], "version": item["version"]}
    record["workers"].append(row)
    save(path, record)
    wait_startup(directory, process, item, record, row, path)
    meta = json.loads((directory / "meta.json").read_bytes())
    row["shellPid"] = meta["pid"]
    save(path, record)
    session_io(directory / "sock", session, "COHORT_INITIAL_" + session)
    if process_info(row["shellPid"])[0] != row["pid"]:
        raise RuntimeError("worker cohort shell belongs to another process")


def wait_startup(directory, process, item, record, row, path):
    from fixtures import release_worker_evidence
    with release_worker_evidence.Startup(process.pid) as observed:
        try:
            wait_worker(directory, process)
        except BaseException:
            release_worker_evidence.publish(record, row, observed, item, directory, path, save)
            raise


def wait_worker(directory, process):
    from fixtures import release_worker_evidence
    for _ in range(120):
        if process.poll() is not None:
            log = release_worker_evidence.log_tail(directory / "worker.log")
            raise RuntimeError(f"retained worker exited: {directory}; {log}")
        if (directory / "sock").is_socket() and (directory / "meta.json").is_file():
            return
        time.sleep(0.05)
    raise RuntimeError("retained worker did not start within the existing proof bound")


def start(state, config, selected, path, publish=False):
    from fixtures import release_worker_evidence
    state, config, path = Path(state), Path(config), Path(path)
    primary = next((state / "s").glob("*/meta.json"))
    boot = json.loads(primary.read_bytes()).get("bootId", "")
    with closing(sqlite3.connect(state / "mesh.db")) as database:
        host = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 1").fetchone()[0]
    record = {"schema": 1, "workers": [], "observations": [],
              "startupObserverControl": release_worker_evidence.positive_control()}
    save(path, record)
    try:
        for index, item in enumerate(selected):
            start_one(state, config, item, index, boot, host, record, path)
        if publish:
            record["initialPublication"] = wait_publication(state, host, record["workers"])
            save(path, record)
    except BaseException:
        stop(path)
        raise
    return record


def wait_publication(state, host, workers):
    expected = {worker["session"] for worker in workers}
    original = os.environ.get("MESH_GROUP_ORIGINAL")
    if original:
        expected.add(json.loads(original)["session"])
    visible = set()
    # The installed caller's existing 90-second startup subprocess owns this
    # blocking watch too. Receiving a frame never renews that absolute bound.
    with socket.socket(socket.AF_UNIX) as connection:
        connection.connect(str(state / "daemon.sock"))
        control(connection, {"type": "state.watch", "requestId": "cohort-publication", "watch": {"topics": ["host", "sessions"]}})
        initial = publication_message(connection)
        if initial["type"] != "state.snapshot" or initial["stateSnapshot"]["host"]["id"] != host:
            raise RuntimeError("cohort publication snapshot has another type or host")
        for row in initial["stateSnapshot"].get("sessions", []):
            published_session(row, host, expected, visible)
        while expected - visible:
            publication_event(publication_message(connection), host, expected, visible)
    return {"method": "state.watch", "host": host, "sessions": sorted(visible)}


def publication_message(connection):
    from fixtures.installed_catalog_evidence import receive_control
    value = receive_control(connection)
    if value.get("more") or value.get("page", 0) or value["type"] in ("error", "state.resync"):
        raise RuntimeError("cohort publication watch is incomplete or refused: " + value["type"])
    return value


def published_session(row, host, expected, visible):
    if row["id"] not in expected:
        return
    if row["hostId"] != host or row["state"] not in ("running", "detached"):
        raise RuntimeError("cohort publication has wrong host or inactive session " + row["id"])
    visible.add(row["id"])


def publication_event(value, host, expected, visible):
    if value["type"] == "state.current":
        return
    if value["type"] != "state.event":
        raise RuntimeError("unexpected cohort publication message: " + value["type"])
    event = value["stateEvent"]
    if event["kind"] in ("session.added", "session.changed"):
        published_session(event["payload"]["session"], host, expected, visible)
    if event["kind"] == "session.removed" and event["payload"]["sessionId"] in expected:
        raise RuntimeError("cohort session removed before publication completed")


def verify(state, path, label, daemon=True):
    state, path = Path(state), Path(path)
    record = json.loads(path.read_bytes())
    original = json.loads(os.environ["MESH_GROUP_ORIGINAL"]) if os.environ.get("MESH_GROUP_ORIGINAL") else None
    record["original"] = original
    save(path, record)
    observer_seconds = verify_images(state, path)
    rss = 0
    for worker in record["workers"]:
        rss += verify_one(state, worker, label, daemon)
    identities = [{key: row[key] for key in ("session", "pid", "shellPid", "digest", "protocol", "version")} for row in record["workers"]]
    original_rss = process_info(original["pid"])[1] + process_info(original["shellPid"])[1] if original else 0
    record["observations"].append({"label": label, "workers": len(record["workers"]), "identities": identities, "original": original,
                                   "processes": 2 * len(record["workers"]), "rssKiB": rss,
                                   "originalRssKiB": original_rss,
                                   "observerSeconds": observer_seconds})
    save(path, record)
    return sorted({worker["digest"] for worker in record["workers"]})


def verify_images(state, path):
    probe = os.environ.get("MESH_WORKER_PROBE")
    if not probe:
        raise ValueError("worker proof requires the actual process-image observer")
    environment = dict(os.environ, MESH_COHORT_STATE=str(state), MESH_COHORT_RECORD=str(path))
    started = time.monotonic()
    result = subprocess.run([probe, "-test.v", "-test.run=^TestRetainedWorkerCohortIdentity$"],
                            env=environment, capture_output=True, timeout=30, check=False)
    elapsed = time.monotonic() - started
    if result.returncode:
        output = (result.stdout[-4096:] + b"\n" + result.stderr[-4096:]).decode(errors="replace")
        raise RuntimeError(f"exact running cohort image observer exited {result.returncode} after {elapsed:.3f}s: {output}")
    if b"--- PASS: TestRetainedWorkerCohortIdentity" not in result.stdout:
        raise RuntimeError("exact running cohort image observer did not run")
    return elapsed


def verify_one(state, worker, label, daemon):
    _, worker_rss = process_info(worker["pid"])
    parent, shell_rss = process_info(worker["shellPid"])
    meta = json.loads((state / "s" / worker["session"] / "meta.json").read_bytes())
    if parent != worker["pid"] or meta["pid"] != worker["shellPid"]:
        raise RuntimeError("retained cohort worker or shell identity changed")
    endpoint = state / "daemon.sock" if daemon else state / "s" / worker["session"] / "sock"
    from fixtures import installed_catalog_evidence
    installed_catalog_evidence.io_boundary(worker["session"], "begin")
    try:
        session_io(endpoint, worker["session"], "COHORT_" + label.replace("-", "_") + "_" + worker["session"])
    except BaseException:
        installed_catalog_evidence.io_boundary(worker["session"], "failed")
        raise
    installed_catalog_evidence.io_boundary(worker["session"], "returned")
    return worker_rss + shell_rss


def stop(path):
    path = Path(path)
    if not path.exists():
        return
    record = json.loads(path.read_bytes())
    for worker in record["workers"]:
        stop_one(worker)


def stop_one(worker):
    for pid in (worker["shellPid"], worker["pid"]):
        if not pid:
            continue
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    for _ in range(120):
        if not worker_running(worker) and not process_running(worker["shellPid"]):
            return
        time.sleep(0.025)
    raise RuntimeError("cohort process survived teardown")


def worker_running(worker):
    return process_running(worker["pid"])


def process_running(pid):
    if not pid:
        return False
    result = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, timeout=5, check=False)
    return result.returncode == 0 and not result.stdout.strip().startswith(b"Z")


def main(arguments):
    operation, *values = arguments
    if operation in ("start", "start-published"):
        state, config, registry, source_version, source_binary, output = values
        from fixtures import release_worker_groups
        descriptor = os.environ.get("MESH_WORKER_GROUP")
        selected = release_worker_groups.selected_group(descriptor) if descriptor else applicable(registry, source_version, source_binary)
        start(state, config, selected, output, publish=operation == "start-published")
        return
    if operation == "verify":
        digests = verify(*values)
        print(json.dumps(digests))
        return
    if operation == "stop":
        stop(values[0])
        return
    raise ValueError("unknown worker cohort operation")


if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    main(sys.argv[1:])
