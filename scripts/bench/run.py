#!/usr/bin/env python3
"""Linux scratch-daemon benchmark. Python's standard library is the only dependency."""
import argparse
import base64
import contextlib
import json
import os
from pathlib import Path
import platform
import signal
import shutil
import socket
import statistics
import struct
import subprocess
import tempfile
import threading
import time
import uuid

from receipt import inspect_binary, verify_receipt

HERE = Path(__file__).resolve().parent


def receive_exact(conn, size):
    result = bytearray()
    while len(result) < size:
        data = conn.recv(size - len(result))
        if not data:
            raise EOFError("benchmark connection closed before response")
        result.extend(data)
    return bytes(result)


def read_frame(conn):
    kind, size = struct.unpack(">BI", receive_exact(conn, 5))
    if size > 4 << 20:
        raise ValueError("benchmark response exceeds protocol limit")
    return kind, receive_exact(conn, size)


def write_frame(conn, kind, payload):
    conn.sendall(struct.pack(">BI", kind, len(payload)) + payload)


def control(conn, request):
    write_frame(conn, 1, json.dumps(request, separators=(",", ":")).encode())


def dial(state):
    conn = socket.socket(socket.AF_UNIX)
    conn.settimeout(20)
    try:
        conn.connect(str(state / "daemon.sock"))
        return conn
    except BaseException:
        conn.close()
        raise


def rpc(state, request):
    # Creation is idempotent by request ID across connections.
    request = {"requestId": uuid.uuid4().hex, **request}
    with dial(state) as conn:
        control(conn, request)
        kind, payload = read_frame(conn)
    if kind != 1:
        raise ValueError("expected control response")
    response = json.loads(payload)
    if response.get("type") == "error":
        raise RuntimeError(response)
    return response, len(payload)


def proc_sample(pid):
    fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
    return {"ticks": int(fields[11]) + int(fields[12]),
            "rss_bytes": int(fields[21]) * os.sysconf("SC_PAGE_SIZE"),
            "ppid": int(fields[1]), "start_ticks": int(fields[19])}


class WALCounter:
    """Count committed WAL frames and transactions, including WAL salt resets."""
    def __init__(self, database):
        self.wal = Path(str(database) + "-wal")
        self.shm = Path(str(database) + "-shm")
        self.previous = None
        self.frames = 0
        self.commits = 0
        self.resets = 0
        self.poll()
        self.frames = self.commits = self.resets = 0

    def poll(self):
        try:
            with self.wal.open("rb") as wal:
                header = wal.read(32)
                index = self.shm.read_bytes()[:48]
                if len(header) != 32 or len(index) != 48:
                    return
                frame_count = struct.unpack_from("=I", index, 16)[0]
                salt = header[16:24]
                # WAL-index salts must describe the header read above. A writer
                # crossing a reset during this observation is retried next poll.
                if index[32:40] != salt:
                    return
                page_size = struct.unpack_from(">I", header, 8)[0]
                if page_size == 1:
                    page_size = 65536
                start = 0
                if self.previous and self.previous[0] == salt:
                    start = self.previous[1]
                elif self.previous:
                    self.resets += 1
                if frame_count < start:
                    raise RuntimeError("WAL rewind without salt reset")
                commits = 0
                for frame in range(start, frame_count):
                    wal.seek(32 + frame * (24 + page_size))
                    frame_header = wal.read(24)
                    if len(frame_header) != 24 or frame_header[8:16] != salt:
                        return
                    commits += struct.unpack_from(">I", frame_header, 4)[0] != 0
                self.frames += frame_count - start
                self.commits += commits
                self.previous = salt, frame_count
        except FileNotFoundError:
            return


def timed_command(binary, args, env, root):
    with tempfile.TemporaryFile(dir=root) as output:
        started = time.perf_counter()
        process = subprocess.Popen([str(binary), *args], env=env, stdout=output,
                                   stderr=output, start_new_session=True)
        expired = threading.Event()

        def timeout():
            expired.set()
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)

        watchdog = threading.Timer(20, timeout)
        watchdog.start()
        try:
            _, status, usage = os.wait4(process.pid, 0)
            process.returncode = os.waitstatus_to_exitcode(status)
        except BaseException:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise
        finally:
            watchdog.cancel()
            watchdog.join()
        elapsed = time.perf_counter() - started
        output.seek(0)
        content = output.read()
        if expired.is_set() or process.returncode:
            raise RuntimeError(f"command {args} failed: {content.decode(errors='replace')}")
        return {"wall_ms": elapsed * 1000,
                "cpu_ms": (usage.ru_utime + usage.ru_stime) * 1000,
                "peak_rss_bytes": usage.ru_maxrss * 1024,
                "output_bytes": len(content)}


def aggregate(samples):
    return {"samples": samples,
            "median": {key: statistics.median(row[key] for row in samples)
                       for key in samples[0]}}


def own_worker(state, sid):
    meta = json.loads((state / "s" / sid / "meta.json").read_text())
    worker = proc_sample(meta["pid"])["ppid"]
    command = Path(f"/proc/{worker}/cmdline").read_bytes().split(b"\0")
    if b"session-worker" not in command or str(state / "s" / sid).encode() not in command:
        raise RuntimeError("worker ownership does not match scratch session")
    return worker


def wait_until(predicate, timeout=20):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            value = predicate()
            if value:
                return value
        except (FileNotFoundError, ConnectionError, EOFError):
            pass
        time.sleep(0.05)
    raise TimeoutError("scratch daemon or workload did not become ready")


def measure_attach(state, sid):
    started = time.perf_counter()
    conn = dial(state)
    try:
        control(conn, {"type": "session.attach", "sessionId": sid,
                       "requestId": "bench", "cols": 120, "rows": 40})
        kind, payload = read_frame(conn)
        response = json.loads(payload)
        if kind != 1 or response.get("type") != "session.attached":
            raise RuntimeError(response)
        attached = (time.perf_counter() - started) * 1000
        if response.get("snapshot"):
            kind, _ = read_frame(conn)
            if kind != 4:
                raise RuntimeError("attach did not deliver the promised snapshot")
        return conn, {"attached_ms": attached,
                      "first_paint_ms": (time.perf_counter() - started) * 1000}
    except BaseException:
        conn.close()
        raise


def detach(conn, sid):
    control(conn, {"type": "session.detach", "sessionId": sid})
    conn.close()


def throughput(state, sid, size):
    conn, _ = measure_attach(state, sid)
    try:
        started = time.perf_counter()
        write_frame(conn, 3, sid.encode().ljust(8, b"\0") + f"burst {size}\n".encode())
        count = 0
        tail = b""
        expected_seq = None
        while True:
            kind, payload = read_frame(conn)
            if kind != 2:
                raise RuntimeError("throughput ended before completion marker")
            seq = struct.unpack_from(">Q", payload, 8)[0]
            if expected_seq is not None and seq != expected_seq:
                raise RuntimeError("throughput dropped terminal bytes")
            expected_seq = seq + len(payload) - 16
            data = payload[16:]
            count += len(data)
            tail = (tail + data)[-64:]
            if b"\r\nBENCH_DONE\r\n" in tail:
                break
        if count != size + len(b"\r\nBENCH_DONE\r\n"):
            raise RuntimeError(f"throughput byte mismatch: {count}")
        elapsed = time.perf_counter() - started
        return {"requested_bytes": size, "received_bytes": count,
                "wall_ms": elapsed * 1000, "bytes_per_second": size / elapsed}
    finally:
        detach(conn, sid)


def process_alive(pid, start):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return fields[0] != "Z" and int(fields[19]) == start
    except (FileNotFoundError, ProcessLookupError):
        return False


def owned_workers(state, binary):
    found = {}
    session_root = (state / "s").resolve()
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            command = (entry / "cmdline").read_bytes().split(b"\0")
            if os.readlink(entry / "exe") != str(binary):
                continue
            if len(command) < 6 or command[1] != b"session-worker":
                continue
            directory = Path(os.fsdecode(command[command.index(b"--dir") + 1])).resolve()
            if directory.parent != session_root:
                continue
            start = proc_sample(int(entry.name))["start_ticks"]
            if process_alive(int(entry.name), start):
                found[int(entry.name)] = (start, directory)
        except (FileNotFoundError, ProcessLookupError, PermissionError, ValueError, IndexError):
            continue
    return found


def stop_worker(pid, start, directory, state, binary, sig):
    # Pin the PID while rechecking executable, session path and start identity.
    try:
        with contextlib.ExitStack() as stack:
            descriptor = os.pidfd_open(pid)
            stack.callback(os.close, descriptor)
            if owned_workers(state, binary).get(pid) == (start, directory):
                signal.pidfd_send_signal(descriptor, sig)
    except (FileNotFoundError, ProcessLookupError):
        pass


def cleanup(state, sessions, daemon, binary):
    del sessions  # /proc also finds workers whose create reply or metadata was lost.
    failures = []
    previous = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        # Settle the coordinator first so it cannot launch during the ownership scan.
        daemon.terminate()
        try:
            daemon.wait(timeout=2)
        except subprocess.TimeoutExpired:
            daemon.kill()
            daemon.wait(timeout=2)
        workers = owned_workers(state, binary)
        for pid, (start, directory) in workers.items():
            try:
                if not process_alive(pid, start):
                    continue
                with socket.socket(socket.AF_UNIX) as conn:
                    conn.settimeout(0.5)
                    conn.connect(str(directory / "sock"))
                    control(conn, {"type": "session.kill", "sessionId": directory.name,
                                   "requestId": uuid.uuid4().hex})
            except (OSError, EOFError, RuntimeError, KeyboardInterrupt):
                # A dead socket is expected after a partial launch; identity fallback follows.
                continue
        for sig, seconds in ((None, 2), (signal.SIGTERM, 0.2), (signal.SIGKILL, 2)):
            for pid, (start, directory) in workers.items():
                if sig is None:
                    continue
                try:
                    stop_worker(pid, start, directory, state, binary, sig)
                except (OSError, RuntimeError, KeyboardInterrupt) as error:
                    failures.append(str(error))
            end = time.monotonic() + seconds
            while time.monotonic() < end and any(process_alive(pid, start) for pid, (start, _) in workers.items()):
                time.sleep(0.05)
        remaining = owned_workers(state, binary)
        if remaining:
            failures.append(f"owned workers still running: {list(remaining)}")
        if failures:
            raise RuntimeError("scratch cleanup incomplete: " + "; ".join(failures))
    finally:
        # Always reap our daemon, even if a worker inspection or control operation failed.
        if daemon.poll() is None:
            daemon.kill()
            daemon.wait(timeout=2)
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def root_processes(root):
    found = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            cwd = Path(os.readlink(entry / "cwd"))
            if cwd.is_relative_to(root) and process_alive(int(entry.name), proc_sample(int(entry.name))["start_ticks"]):
                found.append(int(entry.name))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    return found


@contextlib.contextmanager
def scratch_case(parent, binary):
    root = Path(tempfile.mkdtemp(prefix="mesh-m5-", dir=parent))
    try:
        yield root
    finally:
        if owned_workers(root / "state", binary) or root_processes(root):
            raise RuntimeError(f"scratch root retained while owned processes remain: {root}")
        shutil.rmtree(root)



@contextlib.contextmanager
def defer_launch_interrupts():
    pending = []
    previous = {sig: signal.signal(sig, lambda *_: pending.append(True))
                for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        yield
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    if pending:
        raise KeyboardInterrupt("benchmark interrupted during owned launch")


def run_case(args, count, root):
    state = root / "state"
    state.mkdir()
    home = root / "home"
    home.mkdir()
    config = root / "config"
    config.mkdir()
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "SHELL": "/bin/sh",
           "MESH_STATE_DIR": str(state), "MESH_CONFIG_DIR": str(config),
           "TERM": "xterm-256color", "LANG": "C.UTF-8", "NO_COLOR": "1"}
    if args.profile_dir:
        env["MESH_BENCH_PROFILE_DIR"] = str(args.profile_dir.resolve())
    # Allocate a fresh port; the daemon binds its own tailnet address. SSH,
    # HTTPS and serving remain disabled, and control probes use the Unix socket.
    with socket.socket() as reservation:
        reservation.bind(("0.0.0.0", 0))
        port = reservation.getsockname()[1]
    with (root / "daemon.log").open("wb") as log:
        sessions = []
        daemon = None
        try:
            # Record the launched PID before delivering a cancellation to the owner.
            with defer_launch_interrupts():
                daemon = subprocess.Popen([str(args.binary), "daemon", "--tailnet-port", str(port),
                                           "--app-data-root", str(root / "apps")],
                                          env=env, stdout=log, stderr=log)
            wait_until(lambda: rpc(state, {"type": "host.info"})[0])
            for _ in range(count):
                command = [os.sys.executable, str(HERE / "workload.py")]
                response, _ = rpc(state, {"type": "session.create", "command": command,
                                          "cwd": str(root), "cols": 120, "rows": 40,
                                          "term": "xterm-256color"})
                sid = response["sessionId"]
                sessions.append(sid)
                wait_until(lambda: b"BENCH_READY" in base64.b64decode(
                    rpc(state, {"type": "session.logs", "sessionId": sid, "tail": 4096})[0].get("output", "")))
            workers = [own_worker(state, sid) for sid in sessions]
            time.sleep(args.settle_seconds)
            wal = WALCounter(state / "mesh.db")
            before = proc_sample(daemon.pid)
            rss = []
            worker_rss = {pid: [] for pid in workers}
            start = time.monotonic()
            while time.monotonic() - start < args.idle_seconds:
                rss.append(proc_sample(daemon.pid)["rss_bytes"])
                for pid in workers:
                    worker_rss[pid].append(proc_sample(pid)["rss_bytes"])
                wal.poll()
                time.sleep(0.2)
            wal.poll()
            after = proc_sample(daemon.pid)
            duration = time.monotonic() - start
            listed = []
            for _ in range(args.repeats):
                then = time.perf_counter()
                response, size = rpc(state, {"type": "session.list", **args.list_fields})
                if len(response.get("sessions", [])) != count:
                    raise RuntimeError(f"catalog session count mismatch: expected {count} ({sessions}), response={response!r}")
                listed.append({"wall_ms": (time.perf_counter() - then) * 1000,
                               "json_bytes": size, "framed_bytes": size + 5})
            result = {"sessions": count, "scratch_tailnet_port": port,
                      "idle": {"window_seconds": duration,
                               "daemon_cpu_percent_one_core": (after["ticks"] - before["ticks"]) / os.sysconf("SC_CLK_TCK") / duration * 100,
                               "daemon_rss_median_bytes": statistics.median(rss),
                               "daemon_rss_peak_bytes": max(rss),
                               "worker_rss_median_bytes": [statistics.median(worker_rss[pid]) for pid in workers]},
                      "sqlite": {"commits": wal.commits, "wal_frames": wal.frames,
                                 "wal_resets": wal.resets,
                                 "commits_per_minute": wal.commits * 60 / duration,
                                 "wal_frames_per_minute": wal.frames * 60 / duration},
                      "session_list": aggregate(listed),
                      "mesh_ls": aggregate([timed_command(args.binary, ["ls"], env, root) for _ in range(args.repeats)]),
                      "version": aggregate([timed_command(args.binary, ["--version"], env, root) for _ in range(args.repeats)])}
            if sessions:
                attached = []
                for _ in range(args.repeats):
                    conn, sample = measure_attach(state, sessions[0])
                    detach(conn, sessions[0])
                    attached.append(sample)
                result["attach"] = aggregate(attached)
                result["throughput"] = throughput(state, sessions[0], args.throughput_bytes)
            if args.profile_dir:
                # Workers can exit through os.Exit; wait for the timed profiler
                # to close its files before lifecycle cleanup ends the process.
                profiles = [("daemon", daemon.pid), *(("session-worker", pid) for pid in workers)]
                wait_until(lambda: all((args.profile_dir / f"{role}-{pid}.done").exists()
                                       for role, pid in profiles), timeout=10)
            return result
        except BaseException:
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.with_suffix(".failure.log").write_bytes((root / "daemon.log").read_bytes())
            raise
        finally:
            # A partially failed creation can leave a worker without a response.
            # Enumerate only the session directories under our own fresh root.
            sessions = sorted(set(sessions) | {p.name for p in (state / "s").glob("*/meta.json") for p in [p.parent]})
            if daemon is not None:
                cleanup(state, sessions, daemon, args.binary)


def summary(result):
    metadata = result["metadata"]
    lines = [f"# Mesh baseline — {metadata['host']} ({metadata['arch']})", "",
             f"Embedded VCS revision: `{metadata['embedded_vcs_revision']}`. Go: `{metadata['go_version']}`.", "",
             f"Production inputs SHA256: `{metadata['build_receipt']['production_source']['sha256']}`. Equivalent to VCS revision: `{metadata['build_receipt']['production_equivalent_to_vcs_revision']}`.", "",
             "Warm cache, scratch Unix transport, one-core CPU percentage. WAL rates are short-window extrapolations.", "",
             "| Sessions | Idle CPU % | Daemon RSS MiB | WAL commits/min | WAL frames/min | List JSON bytes | List ms | ls ms | version ms | Worker RSS MiB | Attach paint ms | Throughput MiB/s |",
             "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for row in result["cases"]:
        idle = row["idle"]
        workers = idle["worker_rss_median_bytes"]
        attach = f"{row["attach"]["median"]["first_paint_ms"]:.2f}" if "attach" in row else "—"
        rate = f"{row["throughput"]["bytes_per_second"]/2**20:.2f}" if "throughput" in row else "—"
        lines.append(f"| {row['sessions']} | {idle['daemon_cpu_percent_one_core']:.3f} | {idle['daemon_rss_median_bytes']/2**20:.2f} | {row['sqlite']['commits_per_minute']:.1f} | {row['sqlite']['wal_frames_per_minute']:.1f} | {row['session_list']['median']['json_bytes']:.0f} | {row['session_list']['median']['wall_ms']:.2f} | {row['mesh_ls']['median']['wall_ms']:.2f} | {row['version']['median']['wall_ms']:.2f} | {statistics.median(workers)/2**20 if workers else 0:.2f} | {attach} | {rate} |")
    lines += ["", "| Sessions | ls CPU ms | ls peak RSS MiB | version CPU ms | version peak RSS MiB |",
              "| ---: | ---: | ---: | ---: | ---: |"]
    for row in result["cases"]:
        ls, version = row["mesh_ls"]["median"], row["version"]["median"]
        lines.append(f"| {row['sessions']} | {ls['cpu_ms']:.2f} | {ls['peak_rss_bytes']/2**20:.2f} | {version['cpu_ms']:.2f} | {version['peak_rss_bytes']/2**20:.2f} |")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--commit")
    parser.add_argument("--go-version")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, default=Path("/work/tmp"))
    parser.add_argument("--sessions", nargs="+", type=int, default=[0, 5, 20])
    parser.add_argument("--idle-seconds", type=float, default=30)
    parser.add_argument("--settle-seconds", type=float, default=3)
    parser.add_argument("--repeats", type=int, default=5)
    parser.add_argument("--throughput-bytes", type=int, default=1 << 20)
    parser.add_argument("--list-fields", type=json.loads, default={})
    parser.add_argument("--profile-dir", type=Path)
    args = parser.parse_args()
    if min(args.sessions) < 0 or max(args.sessions) > 20 or args.idle_seconds < 1 or args.repeats < 1 or args.settle_seconds < 0 or args.throughput_bytes < 1:
        parser.error("use 0–20 sessions, a positive window/repeats/throughput, and nonnegative settlement")
    args.binary = args.binary.resolve(strict=True)
    args.scratch_parent = args.scratch_parent.resolve(strict=True)
    if inspect_binary(args.binary)["build_info"].splitlines()[0] != "path\tgithub.com/shaul/mesh/cmd/mesh":
        parser.error("run.py requires the Mesh executable")
    receipt = verify_receipt(args.binary, json.loads(args.receipt.read_text()), profiled=bool(args.profile_dir),
                             commit=args.commit, go_version=args.go_version)
    if args.profile_dir:
        args.profile_dir.mkdir(parents=True, exist_ok=True)
    def interrupted(_signal, _frame):
        raise KeyboardInterrupt("benchmark interrupted; cleaning scratch processes")

    signal.signal(signal.SIGTERM, interrupted)
    result = {"schema_version": 2,
              "metadata": {"host": platform.node(), "arch": platform.machine(),
                           "kernel": platform.release(),
                           "production_source_revision": receipt["embedded_vcs_revision"] if receipt["production_equivalent_to_vcs_revision"] else None,
                           "embedded_vcs_revision": receipt["embedded_vcs_revision"],
                           "harness_revision": receipt["harness_revision"],
                           "go_version": receipt["binary"]["go_version"], "build_receipt": receipt, "unix_transport": True,
                           "idle_seconds_requested": args.idle_seconds,
                           "settle_seconds": args.settle_seconds, "list_fields": args.list_fields,
                           "repeats": args.repeats, "binary_bytes": args.binary.stat().st_size,
                           "cpu_ticks_per_second": os.sysconf("SC_CLK_TCK"),
                           "sqlite_counter": "committed WAL transactions and frames at 5 Hz",
                           "profiled": bool(args.profile_dir), "timestamp_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())},
              "cases": []}
    for count in args.sessions:
        with scratch_case(args.scratch_parent, args.binary) as temporary:
            result["cases"].append(run_case(args, count, temporary))
        print(f"measured and cleaned {count} scratch sessions", flush=True)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    args.output.with_suffix(".md").write_text(summary(result))


if __name__ == "__main__":
    main()
