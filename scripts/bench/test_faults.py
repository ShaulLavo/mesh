"""Real owned-worker regressions; invoke with the scratch Mesh executable."""
import argparse
import base64
from pathlib import Path
import os
import shutil
import socket
import subprocess
import tempfile
import signal
import threading
from types import SimpleNamespace
from unittest.mock import patch

import run as bench
from process_diagnostics import record_signal, signal_child, signal_self


def alive(pid, identity):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return fields[0] != "Z" and int(fields[19]) == identity
    except FileNotFoundError:
        return False


def fixture(binary, parent, mode):
    root = Path(tempfile.mkdtemp(prefix="mesh-m5-fault-", dir=parent))
    state = root / "state"
    state.mkdir()
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    env = {"PATH": os.environ["PATH"], "HOME": str(root), "SHELL": "/bin/sh", "TERM": "xterm-256color", "MESH_STATE_DIR": str(state), "MESH_CONFIG_DIR": str(root / "config")}
    with (root / "daemon.log").open("wb") as log:
        daemon = subprocess.Popen([str(binary), "daemon", "--tailnet-port", str(port), "--app-data-root", str(root / "apps")], env=env, stdout=log, stderr=log)
    worker = None
    try:
        bench.wait_until(lambda: bench.rpc(state, {"type": "host.info"})[0])
        response, _ = bench.rpc(state, {"type": "session.create", "cwd": str(root), "command": [os.sys.executable, str(bench.HERE / "workload.py")], "cols": 120, "rows": 40})
        sid = response["sessionId"]
        bench.wait_until(lambda: b"BENCH_READY" in base64.b64decode(bench.rpc(state, {"type": "session.logs", "sessionId": sid, "tail": 4096})[0].get("output", "")))
        worker = bench.own_worker(state, sid)
        identity = bench.proc_sample(worker)["start_ticks"]
        signal_child(daemon, signal.SIGKILL, "fault-fixture-daemon-death")
        daemon.wait()
        sessions = [sid]
        if mode == "partial":
            (state / "s" / sid / "meta.json").unlink()
            sessions = []
        if mode == "fallback":
            (state / "s" / sid / "sock").unlink()
        if mode == "cancel":
            previous = signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
            timer = threading.Timer(0.001, signal_self, args=(signal.SIGTERM, "fault-fixture-cancellation"))
            try:
                try:
                    timer.start()
                    raise KeyboardInterrupt("cancel after creating a session")
                finally:
                    bench.cleanup(state, sessions, daemon, binary)
            except KeyboardInterrupt:
                pass
            finally:
                timer.join()
                signal.signal(signal.SIGTERM, previous)
        else:
            bench.cleanup(state, sessions, daemon, binary)
        assert not alive(worker, identity), f"owned worker survives {mode}"
        print(f"PASS: cleanup {mode}", flush=True)
    finally:
        # Rescue the exact owned fixture even when testing the old broken code.
        if worker is not None:
            try:
                with socket.socket(socket.AF_UNIX) as conn:
                    conn.settimeout(1)
                    conn.connect(str(state / "s" / sid / "sock"))
                    bench.control(conn, {"type": "session.kill", "sessionId": sid})
            except OSError:
                pass
            try:
                bench.wait_until(lambda: not alive(worker, identity), timeout=5)
            except TimeoutError:
                if alive(worker, identity):
                    record_signal(worker, signal.SIGTERM, "fault-fixture-worker-rescue",
                                  {"kind": "verified_fixture_worker", "root": str(root)},
                                  {"status": "observed", "start_ticks": identity})
                    os.kill(worker, signal.SIGTERM)
                bench.wait_until(lambda: not alive(worker, identity), timeout=5)
        if daemon.poll() is None:
            signal_child(daemon, signal.SIGTERM, "fault-fixture-daemon-rescue")
            daemon.wait(timeout=5)
        shutil.rmtree(root)


def cancelled_spawn(binary, parent):
    started = []
    original = subprocess.Popen
    def spawn(*arguments, **keywords):
        child = original(*arguments, **keywords)
        started.append(child)
        signal_self(signal.SIGTERM, "fault-fixture-spawn-cancellation")
        return child
    previous = signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        with bench.scratch_case(parent.resolve(), binary) as root:
            args = SimpleNamespace(binary=binary, profile_dir=None, output=parent / "spawn-cancel.json")
            try:
                with patch.object(bench.subprocess, "Popen", spawn):
                    bench.run_case(args, 0, root)
            except KeyboardInterrupt:
                pass
        assert started[0].poll() is not None, "owned daemon survived cancellation during spawn"
        print("PASS: cancellation during spawn settles owned daemon", flush=True)
    finally:
        signal.signal(signal.SIGTERM, previous)
        for child in started:
            if child.poll() is None:
                signal_child(child, signal.SIGKILL, "fault-fixture-child-rescue")
                child.wait(timeout=5)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, required=True)
    args = parser.parse_args()
    cancelled_spawn(args.binary.resolve(), args.scratch_parent)
    # A live child forces retention even when the benchmark exits through an error.
    with bench.scratch_case(args.scratch_parent.resolve(), args.binary.resolve()) as root:
        child = subprocess.Popen(["sleep", "30"], cwd=root)
        try:
            with bench.scratch_case(root, args.binary.resolve()) as held:
                survivor = subprocess.Popen(["sleep", "30"], cwd=held)
                try:
                    raise RuntimeError("fixture failure")
                finally:
                    signal_child(child, signal.SIGTERM, "fault-fixture-child-cleanup")
                    child.wait()
        except RuntimeError as error:
            assert "retained" in str(error) and held.exists()
            signal_child(survivor, signal.SIGTERM, "fault-fixture-survivor-cleanup")
            survivor.wait()
            shutil.rmtree(held)
    print("PASS: failed case retains root until owned child ends")
    for mode in ("daemon-death", "partial", "cancel", "fallback"):
        fixture(args.binary.resolve(), args.scratch_parent, mode)
