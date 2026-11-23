#!/usr/bin/env python3
"""Fleet benchmark comparing Mesh one-shot MCP execution against warm OpenSSH."""

import argparse
from contextlib import ExitStack, contextmanager
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import signal
import socket
import statistics
import struct
import subprocess
import sys
import tempfile
import threading
import time
from types import SimpleNamespace
import uuid

sys.dont_write_bytecode = True

REPO_ROOT = Path(__file__).resolve().parents[2]
BENCH_DIR = Path(__file__).resolve().parent
HELPERS_DIR = REPO_ROOT / "integration" / "helpers"

DEFAULT_HOSTS = ("local", "vps", "pi", "mac")
HOST_ALIASES = {"pc": "local"}
HOST_SPECS = {
    "local": {
        "kind": "local",
        "platform": "linux/amd64",
        "true_path": "/bin/true",
        "startup_timeout": 30,
    },
    "vps": {
        "kind": "remote",
        "platform": "linux/amd64",
        "mesh_host": "vps",
        "ssh_alias": "vps",
        "true_path": "/bin/true",
        "startup_timeout": 30,
        "use_mac_queue": False,
    },
    "pi": {
        "kind": "remote",
        "platform": "linux/arm64",
        "mesh_host": "pi",
        "ssh_alias": "raspberry-pi",
        "true_path": "/bin/true",
        "startup_timeout": 60,
        "use_mac_queue": False,
    },
    "mac": {
        "kind": "remote",
        "platform": "darwin/arm64",
        "mesh_host": "mac",
        "ssh_alias": "shaul-mac",
        "true_path": "/usr/bin/true",
        "startup_timeout": 45,
        "use_mac_queue": True,
    },
}

HANG_CAP = 120
DEFAULT_WARMUPS = 3
DEFAULT_PAIRS = 30
DEFAULT_SEED = 435
DEFAULT_WAIT_MS = 10000
MAC_QUEUE_WAIT_SECONDS = 600
MAC_QUEUE_TIMEOUT_SECONDS = 600
MAC_QUEUE_EXPECTED_SECONDS = 300

READY_PREFIX = "ONESHOT_READY "
SERVER_BENCH_PREFIX = "ONESHOT_SERVER_BENCH "
STOPPED_PREFIX = "ONESHOT_STOPPED "

PING_RTT_RE = re.compile(r"\bin\s+([0-9]+(?:\.[0-9]+)?)\s*(ns|µs|us|ms|s)\b")
REMOTE_CLEANUP_SNIPPET = r"""
import json, os, shutil, signal, subprocess, sys, time
from pathlib import Path

root = Path(sys.argv[1]).resolve()
if not root.name.startswith("mesh-oneshot-bench-"):
    raise SystemExit(f"refusing to clean unexpected path: {root}")

def ancestors():
    seen = set()
    pid = os.getpid()
    while pid > 1 and pid not in seen:
        seen.add(pid)
        if sys.platform == "linux":
            try:
                stat = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
                pid = int(stat[1])
            except OSError:
                break
        else:
            out = subprocess.run(["ps", "-o", "ppid=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
            pid = int(out) if out.isdigit() else 0
    return seen

def matching_pids(needle, exclude):
    found = []
    if sys.platform == "linux":
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit():
                continue
            pid = int(entry.name)
            if pid in exclude:
                continue
            try:
                stat = (entry / "stat").read_text().rsplit(")", 1)[1].split()
                if stat[0] == "Z":
                    continue
                cmd = (entry / "cmdline").read_bytes().replace(b"\x00", b" ").decode(errors="replace")
                exe = os.readlink(entry / "exe")
            except OSError:
                continue
            if needle in cmd or needle in exe:
                found.append(pid)
        return found
    out = subprocess.run(["ps", "-ax", "-o", "pid=,state=,args="], capture_output=True, text=True).stdout
    for line in out.splitlines():
        parts = line.strip().split(None, 2)
        if len(parts) < 3 or not parts[0].isdigit():
            continue
        pid = int(parts[0])
        if pid in exclude or parts[1].startswith("Z"):
            continue
        if needle in parts[2]:
            found.append(pid)
    return found

needle = str(root)
exclude = ancestors()
for sig, wait_s in ((signal.SIGTERM, 3.0), (signal.SIGKILL, 2.0)):
    pids = matching_pids(needle, exclude)
    if not pids:
        break
    for pid in pids:
        try:
            os.kill(pid, sig)
        except OSError:
            pass
    deadline = time.monotonic() + wait_s
    while time.monotonic() < deadline and matching_pids(needle, exclude):
        time.sleep(0.1)

remaining = matching_pids(needle, exclude)
removed = False
if not remaining:
    if root.exists():
        shutil.rmtree(root)
    removed = not root.exists()
print(json.dumps({"remaining_pids": remaining, "scratch_removed": removed}))
"""


# ---------------------------------------------------------------------------
# Pure helpers (unit-tested in test_oneshot.py)
# ---------------------------------------------------------------------------


def parse_hosts(text):
    if not text or not text.strip():
        raise ValueError("at least one host is required")
    selected = []
    for raw in text.split(","):
        name = raw.strip().lower()
        if not name:
            continue
        canonical = HOST_ALIASES.get(name, name)
        if canonical not in HOST_SPECS:
            valid = ", ".join(DEFAULT_HOSTS)
            raise ValueError(f"unknown host {raw!r}; expected one of: {valid}")
        if canonical not in selected:
            selected.append(canonical)
    if not selected:
        raise ValueError("at least one host is required")
    return selected


def required_platforms(hosts):
    needed = {"linux/amd64"}
    for host in hosts:
        needed.add(HOST_SPECS[host]["platform"])
    return sorted(needed)


def parse_prefixed_json(line, prefix):
    if not line.startswith(prefix):
        return None
    return json.loads(line[len(prefix):].strip())


def parse_tailscale_ping(stdout):
    samples_ms = []
    for line in stdout.splitlines():
        match = PING_RTT_RE.search(line)
        if not match:
            continue
        value = float(match.group(1))
        unit = match.group(2)
        samples_ms.append(duration_to_ms(value, unit))
    if not samples_ms:
        return {"available": False, "samples_ms": [], "reason": "no pong latency parsed"}
    return {
        "available": True,
        "samples_ms": samples_ms,
        "summary_ms": ms_stats(samples_ms),
    }


def duration_to_ms(value, unit):
    if unit == "ms":
        return value
    if unit in ("us", "µs"):
        return value / 1000.0
    if unit == "ns":
        return value / 1_000_000.0
    if unit == "s":
        return value * 1000.0
    raise ValueError(f"unknown duration unit {unit!r}")


def ms_stats(values):
    ordered = sorted(values)
    return {
        "min": ordered[0],
        "p50": statistics.median(ordered),
        "p90": ordered[math.ceil(len(ordered) * 0.9) - 1],
        "max": ordered[-1],
    }


def seconds_to_ms_stats(values):
    return ms_stats([value * 1000.0 for value in values])


def drop_warmups(samples, warmups):
    if warmups < 0:
        raise ValueError("warmups cannot be negative")
    return samples[:warmups], samples[warmups:]


def validate_mcp_exec_reply(reply):
    if "error" in reply:
        raise RuntimeError(f"MCP mesh_exec protocol error: {reply}")
    result = reply.get("result")
    if not isinstance(result, dict) or result.get("isError"):
        raise RuntimeError(f"MCP mesh_exec returned tool error: {reply}")
    data = result.get("structuredContent", {}).get("data", {})
    if data.get("state") != "exited" or data.get("wait") != "completed":
        raise RuntimeError(f"MCP mesh_exec did not complete cleanly: {data}")
    termination = (data.get("result") or {}).get("termination") or {}
    if termination.get("kind") != "exit" or termination.get("code") != 0:
        raise RuntimeError(f"MCP mesh_exec exited non-zero: {data}")
    return data


def validate_server_wait_reply(reply):
    if reply.get("type") != "execution.receipt":
        raise RuntimeError(f"execution.wait unexpected reply type: {reply}")
    if reply.get("executionWait") != "completed":
        raise RuntimeError(f"execution.wait did not complete: {reply}")
    receipt = reply.get("executionReceipt") or {}
    termination = (receipt.get("result") or {}).get("termination") or {}
    if termination.get("kind") != "exit" or termination.get("code") != 0:
        raise RuntimeError(f"execution.wait non-zero termination: {reply}")
    return receipt


def match_scratch_pids(rows, scratch_path, exclude_pids):
    needle = str(scratch_path)
    matched = []
    for pid, command in rows:
        if pid in exclude_pids:
            continue
        if needle in command:
            matched.append(pid)
    return matched


def build_mac_queue_command(remote_argv, lane, thread, expected_seconds, wait_seconds, timeout_seconds):
    queue_argv = [
        "$HOME/.bun/bin/bun",
        "$HOME/.agents/skills/machine-fleet/scripts/mac-queue.ts",
        "run",
        "--lane", shlex.quote(lane),
        "--thread", shlex.quote(thread),
        "--task", shlex.quote("Mesh oneshot benchmark"),
        "--expected-seconds", str(expected_seconds),
        "--wait-seconds", str(wait_seconds),
        "--timeout-seconds", str(timeout_seconds),
        "--",
    ]
    quoted_child = [shlex.quote(arg) for arg in remote_argv]
    return " ".join(queue_argv + quoted_child)


def summarize_server_samples(samples):
    if not samples:
        return None
    return {
        "count": len(samples),
        "create_ms": seconds_to_ms_stats([row["create_seconds"] for row in samples]),
        "wait_ms": seconds_to_ms_stats([row["wait_seconds"] for row in samples]),
        "total_ms": seconds_to_ms_stats([row["total_seconds"] for row in samples]),
    }


def summarize_host(measured_pairs, server_samples, ssh_available, summarize_fn):
    mesh_values = [pair["mesh"]["seconds"] for pair in measured_pairs]
    summary = {
        "count": len(measured_pairs),
        "mesh_mcp_ms": seconds_to_ms_stats(mesh_values) if mesh_values else None,
        "warm_ssh_ms": None,
        "server_ms": summarize_server_samples(server_samples),
        "comparison": None,
        "promise_verdict": "n/a",
        "at_or_under_ssh_p50": None,
    }
    if not ssh_available or not measured_pairs:
        return summary
    ssh_values = [pair["ssh"]["seconds"] for pair in measured_pairs]
    policy = summarize_fn(measured_pairs)
    mesh_p50 = policy["mesh_seconds"]["median"]
    ssh_p50 = policy["ssh_seconds"]["median"]
    summary["warm_ssh_ms"] = seconds_to_ms_stats(ssh_values)
    summary["comparison"] = {
        "p50_ratio": mesh_p50 / ssh_p50,
        "p50_delta_ms": (mesh_p50 - ssh_p50) * 1000.0,
        "paired_ratio": policy["paired_ratio"],
        "paired_delta_ms": {k: v * 1000.0 for k, v in policy["paired_delta_seconds"].items()},
        "pairs_exceeding_both_margins": policy["pairs_exceeding_both_margins"],
        "one_sided_sign_p": policy["one_sided_sign_p"],
        "clear_slowdown": policy["clear_slowdown"],
    }
    summary["promise_verdict"] = "slowdown" if policy["clear_slowdown"] else "pass"
    summary["at_or_under_ssh_p50"] = mesh_p50 <= ssh_p50
    return summary


def fmt_ms(value):
    if value is None:
        return "n/a"
    return f"{value:.1f}"


def fmt_dist_cell(dist):
    if not dist:
        return "n/a"
    return f"{dist['p50']:.1f} / {dist['p90']:.1f} ({dist['min']:.1f}–{dist['max']:.1f})"


def format_markdown_table(report):
    lines = [
        "| Host | RTT p50 (ms) | Warm SSH p50/p90 (min–max) ms | Mesh MCP p50/p90 (min–max) ms | Ratio (p50) | Delta (p50) | Server create/wait/total p50 (ms) | Promise | <= SSH p50 |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |",
    ]
    for host_name, entry in report.get("hosts", {}).items():
        lines.append(format_host_row(host_name, entry))
    return "\n".join(lines) + "\n"


def format_host_row(host_name, entry):
    if entry.get("status") != "ok":
        raw = str(entry.get("error", "error")).splitlines()[0][:120]
        reason = raw.replace("|", "/")
        return f"| {host_name} | error | error | error | n/a | n/a | n/a | error ({reason}) | n/a |"
    rtt = entry.get("rtt") or {}
    rtt_text = fmt_ms(rtt["summary_ms"]["p50"]) if rtt.get("available") else "local" if host_name == "local" else "n/a"
    summary = entry["summary"]
    ssh_cell = fmt_dist_cell(summary.get("warm_ssh_ms"))
    if not entry.get("ssh", {}).get("available", True):
        ssh_cell = f"n/a ({entry['ssh'].get('reason', 'unavailable')})"
    mcp_cell = fmt_dist_cell(summary.get("mesh_mcp_ms"))
    comp = summary.get("comparison")
    ratio_text = f"{comp['p50_ratio']:.2f}x" if comp else "n/a"
    delta_text = f"{comp['p50_delta_ms']:+.1f} ms" if comp else "n/a"
    srv = summary.get("server_ms")
    srv_text = (
        f"{srv['create_ms']['p50']:.1f} / {srv['wait_ms']['p50']:.1f} / {srv['total_ms']['p50']:.1f}"
        if srv else "n/a"
    )
    verdict = summary.get("promise_verdict", "n/a")
    under = summary.get("at_or_under_ssh_p50")
    under_text = "n/a" if under is None else ("yes" if under else "no")
    return f"| {host_name} | {rtt_text} | {ssh_cell} | {mcp_cell} | {ratio_text} | {delta_text} | {srv_text} | {verdict} | {under_text} |"


# ---------------------------------------------------------------------------
# Stdlib-only daemon control framing and process cleanup (shared local+remote)
# ---------------------------------------------------------------------------


def receive_exact(conn, size):
    buf = bytearray()
    while len(buf) < size:
        chunk = conn.recv(size - len(buf))
        if not chunk:
            raise EOFError("daemon closed before completing control frame")
        buf.extend(chunk)
    return bytes(buf)


def daemon_rpc(socket_path, request, timeout=30.0):
    payload = json.dumps(request, separators=(",", ":")).encode()
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(timeout)
        conn.connect(str(socket_path))
        conn.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
        header = receive_exact(conn, 5)
        if header[0] != 1:
            raise RuntimeError(f"unexpected control frame kind {header[0]}")
        length = struct.unpack(">I", header[1:])[0]
        if length > 4 << 20:
            raise RuntimeError(f"oversized control response: {length}")
        return json.loads(receive_exact(conn, length))


def ancestor_pids():
    seen = set()
    pid = os.getpid()
    while pid > 1 and pid not in seen:
        seen.add(pid)
        pid = parent_pid(pid)
    return seen


def parent_pid(pid):
    if sys.platform == "linux":
        try:
            fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
            return int(fields[1])
        except OSError:
            return 0
    result = subprocess.run(["ps", "-o", "ppid=", "-p", str(pid)], capture_output=True, text=True)
    text = result.stdout.strip()
    return int(text) if text.isdigit() else 0


def list_host_processes():
    if sys.platform == "linux":
        return list_linux_processes()
    return list_posix_processes()


def list_linux_processes():
    rows = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        row = read_linux_proc(entry)
        if row is not None:
            rows.append(row)
    return rows


def read_linux_proc(entry):
    try:
        fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return None
        cmdline = (entry / "cmdline").read_bytes().replace(b"\x00", b" ").decode(errors="replace")
        exe = os.readlink(entry / "exe")
    except OSError:
        return None
    return int(entry.name), f"{exe} {cmdline}"


def list_posix_processes():
    result = subprocess.run(["ps", "-ax", "-o", "pid=,state=,args="], capture_output=True, text=True)
    rows = []
    for line in result.stdout.splitlines():
        parts = line.strip().split(None, 2)
        if len(parts) < 3 or not parts[0].isdigit():
            continue
        if parts[1].startswith("Z"):
            continue
        rows.append((int(parts[0]), parts[2]))
    return rows


def find_scratch_pids(scratch_path):
    return match_scratch_pids(list_host_processes(), Path(scratch_path).resolve(), ancestor_pids())


def worker_kill_request(socket_path):
    res_path = socket_path.parent / ".reservation"
    default = {"type": "session.kill", "sessionId": socket_path.parent.name}
    if not res_path.exists():
        return default
    try:
        ref = json.loads(res_path.read_text())
    except (OSError, json.JSONDecodeError):
        return default
    if ref.get("purpose") not in ("pipe-execution", "line-execution"):
        return default
    return {
        "type": "execution.kill",
        "requestId": "bench-teardown",
        "sessionId": ref["id"],
        "instance": ref["instance"],
    }


def kill_worker_socket(socket_path):
    try:
        daemon_rpc(socket_path, worker_kill_request(socket_path), timeout=2.0)
    except OSError:
        pass


def stop_scratch_processes(scratch_path, daemon_proc=None, state_dir=None):
    if state_dir and Path(state_dir).exists():
        for sock in Path(state_dir).glob("s/*/sock"):
            kill_worker_socket(sock)
    stop_popen(daemon_proc, grace=5.0)
    for sig, wait_s in ((signal.SIGTERM, 3.0), (signal.SIGKILL, 2.0)):
        pids = find_scratch_pids(scratch_path)
        if not pids:
            return []
        signal_pids(pids, sig)
        wait_pids_gone(scratch_path, wait_s)
    return find_scratch_pids(scratch_path)


def stop_popen(proc, grace=5.0):
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5.0)


def signal_pids(pids, sig):
    for pid in pids:
        try:
            os.kill(pid, sig)
        except OSError:
            pass


def wait_pids_gone(scratch_path, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline and find_scratch_pids(scratch_path):
        time.sleep(0.1)


def pick_free_port(bind_ip):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind((bind_ip, 0))
        return listener.getsockname()[1]


def write_fake_tailscale(root, host_name, bind_ip):
    bindir = root / "bin"
    bindir.mkdir(parents=True, exist_ok=True)
    status_path = root / "tailscale.json"
    status_path.write_text(json.dumps({
        "BackendState": "Running",
        "Self": {
            "DNSName": f"{host_name}.fixture.test.",
            "HostName": host_name,
            "TailscaleIPs": [bind_ip],
            "Online": True,
        },
        "Peer": {},
    }))
    shim = bindir / "tailscale"
    shim.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        "if [ \"$#\" -ne 2 ] || [ \"$1\" != status ] || [ \"$2\" != --json ]; then\n"
        "  echo 'fake tailscale only supports: tailscale status --json' >&2\n"
        "  exit 2\n"
        "fi\n"
        "exec /bin/cat \"$MESH_FAKE_TAILSCALE_STATUS\"\n"
    )
    shim.chmod(0o755)
    return bindir, status_path


def isolated_env(root, extra=None):
    env = os.environ.copy()
    for key in ("MESH_SESSION_ID", "MESH_HOST_ID", "MESH_DEPTH", "BASH_ENV", "ENV",
                "MESH_STATE_DIR", "MESH_CONFIG_DIR"):
        env.pop(key, None)
    home = root / "home"
    state = root / "state"
    config = root / "config"
    for directory in (home, state, config):
        directory.mkdir(parents=True, exist_ok=True)
        directory.chmod(0o700)
    env.update({
        "HOME": str(home),
        "XDG_STATE_HOME": str(home / ".local" / "state"),
        "XDG_CONFIG_HOME": str(home / ".config"),
        "MESH_STATE_DIR": str(state),
        "MESH_CONFIG_DIR": str(config),
        "NO_COLOR": "1",
    })
    if extra:
        env.update(extra)
    return env, state, config


def wait_daemon_ready(proc, state_dir, bind_ip, port, log_path, timeout):
    sock_path = state_dir / "daemon.sock"
    deadline = time.monotonic() + timeout
    host_info = None
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            detail = log_path.read_text(errors="replace")[-4000:] if log_path.exists() else ""
            raise RuntimeError(f"isolated daemon exited ({proc.returncode}): {detail}")
        if host_info is None and sock_path.exists():
            host_info = probe_host_info(sock_path)
        if host_info is not None and tcp_reachable(bind_ip, port):
            return host_info
        time.sleep(0.05)
    detail = log_path.read_text(errors="replace")[-4000:] if log_path.exists() else ""
    raise TimeoutError(f"isolated daemon did not become ready on {bind_ip}:{port} within {timeout}s: {detail}")


def probe_host_info(sock_path):
    try:
        reply = daemon_rpc(sock_path, {"type": "host.info", "requestId": "oneshot-ready"}, timeout=5.0)
    except OSError:
        return None
    if reply.get("type") != "host.info.result":
        return None
    return reply["host"]


def tcp_reachable(host, port):
    try:
        with socket.create_connection((host, port), timeout=0.2):
            return True
    except OSError:
        return False


def run_server_bench(sock_path, cwd, true_path, warmups, count):
    all_samples = []
    total = warmups + count
    for index in range(total):
        key = f"srv-{uuid.uuid4().hex[:12]}-{index}"
        all_samples.append(run_one_server_sample(sock_path, cwd, true_path, key))
    warm, measured = drop_warmups(all_samples, warmups)
    return {"warmups": warm, "samples": measured}


def run_one_server_sample(sock_path, cwd, true_path, key):
    create_req = {
        "type": "execution.create",
        "requestId": key,
        "requestKey": key,
        "kind": "pipe",
        "cwd": cwd,
        "command": [true_path],
        "stdin": "closed",
    }
    t0 = time.perf_counter_ns()
    create_reply = daemon_rpc(sock_path, create_req, timeout=30.0)
    t1 = time.perf_counter_ns()
    if create_reply.get("type") != "execution.receipt":
        raise RuntimeError(f"execution.create failed: {create_reply}")
    rec = create_reply["executionReceipt"]
    wait_req = {
        "type": "execution.wait",
        "requestId": f"wait-{key}",
        "sessionId": rec["sessionId"],
        "instance": rec["instance"],
        "timeoutMillis": 30000,
    }
    wait_reply = daemon_rpc(sock_path, wait_req, timeout=40.0)
    t2 = time.perf_counter_ns()
    validate_server_wait_reply(wait_reply)
    return {
        "create_seconds": (t1 - t0) / 1_000_000_000,
        "wait_seconds": (t2 - t1) / 1_000_000_000,
        "total_seconds": (t2 - t0) / 1_000_000_000,
        "create_phase": rec.get("phase"),
        "exit": 0,
    }


class IsolatedDaemon:
    """Runs one isolated mesh daemon inside an owned scratch directory."""

    def __init__(self, scratch, binary, host_name, bind_ip, client_id, startup_timeout, true_path):
        self.scratch = Path(scratch).resolve()
        self.binary = str(Path(binary).resolve())
        self.host_name = host_name
        self.bind_ip = bind_ip
        self.client_id = client_id
        self.startup_timeout = startup_timeout
        self.true_path = true_path
        self.work_dir = self.scratch / "work"
        self.log_path = self.scratch / "daemon.log"
        self.proc = None
        self.log_stream = None
        self.state_dir = None
        self.env = None
        self.port = None
        self.host_info = None

    def start(self):
        self.scratch.mkdir(parents=True, exist_ok=True)
        self.scratch.chmod(0o700)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        bindir, status_path = write_fake_tailscale(self.scratch, self.host_name, self.bind_ip)
        self.port = pick_free_port(self.bind_ip)
        extra = {
            "MESH_FAKE_TAILSCALE_STATUS": str(status_path),
            "PATH": str(bindir) + os.pathsep + os.environ.get("PATH", os.defpath),
        }
        self.env, self.state_dir, _ = isolated_env(self.scratch, extra)
        self.log_stream = self.log_path.open("wb")
        self.proc = subprocess.Popen(
            [self.binary, "daemon", "--tailnet-port", str(self.port), "--ssh-port", "0"],
            env=self.env,
            stdin=subprocess.DEVNULL,
            stdout=self.log_stream,
            stderr=subprocess.STDOUT,
        )
        self.host_info = wait_daemon_ready(
            self.proc, self.state_dir, self.bind_ip, self.port, self.log_path, self.startup_timeout
        )
        approve = subprocess.run(
            [self.binary, "device", "approve", "--allow-root", "--", self.client_id],
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=30,
        )
        if approve.returncode != 0:
            raise RuntimeError(f"device approve failed ({approve.returncode}): {approve.stdout} {approve.stderr}")
        return {
            "host_id": self.host_info["id"],
            "mesh_identity": self.host_info["meshIdentity"],
            "machine_name": self.host_info.get("machineName", self.host_name),
            "ip": self.bind_ip,
            "port": self.port,
            "daemon_pid": self.proc.pid,
            "cwd": str(self.work_dir),
            "true_path": self.true_path,
        }

    def server_bench(self, warmups, count):
        return run_server_bench(self.state_dir / "daemon.sock", str(self.work_dir), self.true_path, warmups, count)

    def stop(self):
        remaining = stop_scratch_processes(self.scratch, daemon_proc=self.proc, state_dir=self.state_dir)
        self.proc = None
        if self.log_stream is not None:
            self.log_stream.close()
            self.log_stream = None
        return remaining


# ---------------------------------------------------------------------------
# Remote agent mode (runs on vps, pi, or mac inside its scratch directory)
# ---------------------------------------------------------------------------


def run_remote_agent(args):
    stop_requested = threading.Event()

    def handle_signal(_signum, _frame):
        stop_requested.set()

    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, handle_signal)

    daemon = IsolatedDaemon(
        scratch=args.scratch,
        binary=args.binary,
        host_name=args.host_name,
        bind_ip=args.bind_ip,
        client_id=args.client_id,
        startup_timeout=args.startup_timeout,
        true_path=args.true_path,
    )
    trigger_bench = Path(args.scratch) / "run-server-bench"
    trigger_stop = Path(args.scratch) / "stop"
    try:
        ready = daemon.start()
        emit_line(READY_PREFIX, ready)
        wait_remote_triggers(daemon, trigger_bench, trigger_stop, stop_requested, args)
    finally:
        remaining = daemon.stop()
        emit_line(STOPPED_PREFIX, {"remaining_pids": remaining})
    return 0


def wait_remote_triggers(daemon, trigger_bench, trigger_stop, stop_requested, args):
    while not stop_requested.is_set():
        if daemon.proc.poll() is not None:
            raise RuntimeError(f"isolated daemon exited early ({daemon.proc.returncode})")
        if trigger_bench.exists():
            result = daemon.server_bench(args.server_warmups, args.server_count)
            emit_line(SERVER_BENCH_PREFIX, result)
            return
        if trigger_stop.exists():
            return
        time.sleep(0.05)


def emit_line(prefix, payload):
    sys.stdout.write(prefix + json.dumps(payload, separators=(",", ":")) + "\n")
    sys.stdout.flush()


# ---------------------------------------------------------------------------
# Coordinator mode (runs on PC)
# ---------------------------------------------------------------------------


def load_coordinator_helpers():
    for path in (str(BENCH_DIR), str(HELPERS_DIR)):
        if path not in sys.path:
            sys.path.insert(0, path)
    import isolation
    import mcp_native
    import ssh_agents
    import ssh_command
    return SimpleNamespace(
        isolation=isolation,
        mcp_native=mcp_native,
        ssh_agents=ssh_agents,
        ssh_command=ssh_command,
    )


def sha256_file(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def git_info():
    commit = subprocess.run(["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"], capture_output=True, text=True)
    status = subprocess.run(["git", "-C", str(REPO_ROOT), "status", "--porcelain"], capture_output=True, text=True)
    return {
        "commit": commit.stdout.strip() if commit.returncode == 0 else None,
        "dirty": bool(status.stdout.strip()) if status.returncode == 0 else None,
    }


def resolve_binaries(args, scratch_root):
    explicit = {
        "linux/amd64": args.binary_linux_amd64,
        "linux/arm64": args.binary_linux_arm64,
        "darwin/arm64": args.binary_darwin_arm64,
    }
    needed = required_platforms(args.hosts)
    resolved = {}
    for target in needed:
        path = explicit[target]
        if path is not None:
            verify_executable(path, target)
            resolved[target] = path.resolve()
            continue
        if not args.build:
            flag = "--binary-" + target.replace("/", "-")
            raise SystemExit(f"missing {flag} for selected hosts (or pass --build)")
        resolved[target] = build_binary(target, scratch_root / "builds")
    return resolved


def verify_executable(path, target):
    if not path.is_file():
        raise SystemExit(f"binary for {target} does not exist: {path}")
    if target == "linux/amd64" and not os.access(path, os.X_OK):
        raise SystemExit(f"linux/amd64 binary is not executable: {path}")


def build_binary(target, out_dir):
    out_dir.mkdir(parents=True, exist_ok=True)
    goos, goarch = target.split("/", 1)
    dest = out_dir / f"mesh-{goos}-{goarch}"
    env = os.environ.copy()
    env.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
    result = subprocess.run(
        ["go", "build", "-trimpath", "-o", str(dest), "./cmd/mesh"],
        cwd=str(REPO_ROOT),
        env=env,
        capture_output=True,
        text=True,
        timeout=300,
    )
    if result.returncode != 0:
        raise RuntimeError(f"go build failed for {target}: {result.stderr}")
    return dest


def run_fleet_cmd(argv, timeout=60):
    return subprocess.run(
        argv,
        env=os.environ,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def measure_tailscale_rtt(ip):
    if not shutil.which("tailscale"):
        return {"available": False, "samples_ms": [], "reason": "tailscale CLI not found"}
    result = run_fleet_cmd(["tailscale", "ping", "-c", "5", "--until-direct=false", ip], timeout=25)
    parsed = parse_tailscale_ping(result.stdout + "\n" + result.stderr)
    if not parsed["available"] and result.returncode != 0:
        parsed["reason"] = (result.stderr or result.stdout).strip()[:300]
    return parsed


def probe_remote_host(spec):
    mesh_host = spec["mesh_host"]
    probe_script = (
        "import json, os, platform, shutil, subprocess\n"
        "def ts_ip():\n"
        "    for cmd in ('tailscale', '/Applications/Tailscale.app/Contents/MacOS/Tailscale'):\n"
        "        exe = shutil.which(cmd) or (cmd if os.path.exists(cmd) else None)\n"
        "        if not exe: continue\n"
        "        r = subprocess.run([exe, 'ip', '-4'], capture_output=True, text=True)\n"
        "        if r.returncode == 0 and r.stdout.strip():\n"
        "            return r.stdout.strip().splitlines()[0]\n"
        "    return ''\n"
        "avail = None\n"
        "avail_script = os.path.expanduser('~/.agents/skills/machine-fleet/scripts/availability.py')\n"
        "if os.path.exists(avail_script):\n"
        "    r = subprocess.run([os.sys.executable, avail_script, 'status'], capture_output=True, text=True)\n"
        "    if r.returncode == 0:\n"
        "        try: avail = json.loads(r.stdout)\n"
        "        except Exception: avail = None\n"
        "print(json.dumps({\n"
        "    'home': os.path.expanduser('~'),\n"
        "    'python3': os.sys.executable,\n"
        "    'tailscale_ip': ts_ip(),\n"
        "    'platform': platform.platform(),\n"
        "    'load_average': list(os.getloadavg()),\n"
        "    'availability': avail,\n"
        "}))\n"
    )
    shell_cmd = f"python3 -c {shlex.quote(probe_script)}"
    if spec.get("use_mac_queue"):
        argv = ["mesh", "ssh", mesh_host, "--", "zsh", "-lc", shell_cmd]
    else:
        argv = ["mesh", "ssh", mesh_host, "--", "sh", "-lc", shell_cmd]
    result = run_fleet_cmd(argv, timeout=30)
    if result.returncode != 0:
        raise RuntimeError(f"probe failed on {mesh_host}: {result.stderr or result.stdout}")
    info = json.loads(result.stdout.strip().splitlines()[-1])
    if not info.get("tailscale_ip"):
        raise RuntimeError(f"could not determine tailscale IPv4 on {mesh_host}: {info}")
    return info


def prepare_pc_client(pc_binary, client_root):
    env, _, config_dir = isolated_env(client_root)
    ident = subprocess.run(
        [str(pc_binary), "device", "identity", "--json"],
        env=env,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=30,
    )
    if ident.returncode != 0:
        raise RuntimeError(f"pc device identity failed: {ident.stderr}")
    device_id = json.loads(ident.stdout)["id"]
    mcp_id = subprocess.run(
        [str(pc_binary), "mcp", "identity", "create", "bench", "--json"],
        env=env,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=30,
    )
    if mcp_id.returncode != 0:
        raise RuntimeError(f"pc mcp identity create failed: {mcp_id.stderr}")
    return env, config_dir, device_id


def write_hosts_json(config_dir, ready, host_name):
    hosts_path = config_dir / "hosts.json"
    hosts_path.write_text(json.dumps({
        "version": 1,
        "hosts": [{
            "id": ready["host_id"],
            "meshIdentity": ready["mesh_identity"],
            "tailscaleName": f"{host_name}.fixture.test",
            "addresses": [ready["ip"]],
            "endpoint": f"ws://{ready['ip']}:{ready['port']}/mesh",
        }],
    }))
    hosts_path.chmod(0o600)


def open_mcp_context(mcp_client, host_id, cwd, run_id, host_name):
    reply = mcp_client.call_tool(
        "mesh_context_open",
        {
            "host": host_id,
            "request_key": f"ctx-{run_id}-{host_name}",
            "cwd": cwd,
            "shell": "none",
            "timeout_ms": 30000,
        },
        timeout=45.0,
    )
    if "error" in reply or reply.get("result", {}).get("isError"):
        raise RuntimeError(f"mesh_context_open failed on {host_name}: {reply}")
    return reply["result"]["structuredContent"]["data"]["handle"]


def time_mcp_exec(mcp_client, ctx_handle, true_path, request_key):
    args = {
        "context": ctx_handle,
        "request_key": request_key,
        "argv": [true_path],
        "wait_ms": DEFAULT_WAIT_MS,
    }
    started = time.perf_counter_ns()
    reply = mcp_client.call_tool("mesh_exec", args, timeout=HANG_CAP)
    elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    data = validate_mcp_exec_reply(reply)
    return {"seconds": elapsed, "exit": 0, "state": data["state"], "wait": data["wait"]}


def time_warm_ssh(ssh_argv, env):
    started = time.perf_counter_ns()
    result = subprocess.run(
        ssh_argv,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=HANG_CAP,
    )
    elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    if result.returncode != 0:
        raise RuntimeError(f"warm ssh command failed ({result.returncode}): {result.stderr.decode(errors='replace')}")
    return {"seconds": elapsed, "exit": 0}


def build_remote_ssh_argv(ssh_alias, control_path):
    return [
        shutil.which("ssh"),
        "-o", "BatchMode=yes",
        "-o", "ConnectTimeout=15",
        "-o", "ControlMaster=auto",
        "-o", f"ControlPath={control_path}",
        "-o", "ControlPersist=60",
        "-T",
        ssh_alias,
    ]


def warm_ssh_master(prefix_argv, env):
    result = subprocess.run(
        prefix_argv + ["true"],
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=30,
    )
    if result.returncode != 0:
        raise RuntimeError(f"SSH master warmup failed ({result.returncode}): {result.stderr.decode(errors='replace')}")


def close_ssh_master(prefix_argv, env):
    try:
        subprocess.run(
            prefix_argv[:-1] + ["-O", "exit", prefix_argv[-1]],
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=10,
        )
    except OSError:
        pass


def start_local_openssh(ssh_root, helpers, stack):
    sshd = shutil.which("sshd")
    if not sshd:
        return None, None, "sshd executable not found"
    if not shutil.which("ssh") or not shutil.which("ssh-keygen"):
        return None, None, "ssh or ssh-keygen not found"
    ssh_root.mkdir(parents=True, exist_ok=True)
    state_dir = ssh_root / "state"
    state_dir.mkdir(parents=True, exist_ok=True)
    key_path = state_dir / "identity.key"
    keygen = subprocess.run(
        ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key_path)],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=15,
    )
    if keygen.returncode != 0:
        return None, None, f"ssh-keygen failed: {keygen.stderr.strip()}"
    env = {"PATH": os.environ.get("PATH", os.defpath), "HOME": str(ssh_root), "LC_ALL": "C"}
    fixture = SimpleNamespace(root=ssh_root, local=state_dir, environment=env)
    try:
        base_ssh, port = helpers.ssh_command.start_openssh(fixture, sshd, stack)
        mux_dir = ssh_root / "cm"
        mux_dir.mkdir(mode=0o700, exist_ok=True)
        mux_argv = helpers.ssh_agents.multiplexed(base_ssh, mux_dir / "c")
        stack.callback(close_ssh_master, mux_argv, env)
        warm_ssh_master(mux_argv, env)
        return mux_argv, env, f"owned sshd on 127.0.0.2:{port}"
    except Exception as error:
        return None, None, str(error)


def run_paired_samples(mcp_client, ctx_handle, true_path, ssh_argv, ssh_env, args, helpers, run_id, host_name):
    warmups = collect_phase(
        mcp_client, ctx_handle, true_path, ssh_argv, ssh_env,
        args.warmups, args.seed, helpers, f"{run_id}-{host_name}-w",
    )
    measured = collect_phase(
        mcp_client, ctx_handle, true_path, ssh_argv, ssh_env,
        args.pairs, args.seed + 1, helpers, f"{run_id}-{host_name}-m",
    )
    return warmups, measured


def collect_one_pair(order, mcp_client, ctx_handle, true_path, ssh_argv, ssh_env, key):
    pair = {"order": order}
    for mode in order:
        if mode == "mesh":
            pair["mesh"] = time_mcp_exec(mcp_client, ctx_handle, true_path, key)
        else:
            pair["ssh"] = time_warm_ssh(ssh_argv + ["true"], ssh_env)
    return pair


def collect_phase(mcp_client, ctx_handle, true_path, ssh_argv, ssh_env, count, seed, helpers, prefix):
    if ssh_argv is None:
        return [
            {"order": ["mesh"], "mesh": time_mcp_exec(mcp_client, ctx_handle, true_path, f"{prefix}-{index}")}
            for index in range(count)
        ]
    return [
        collect_one_pair(order, mcp_client, ctx_handle, true_path, ssh_argv, ssh_env, f"{prefix}-{index}")
        for index, order in enumerate(helpers.ssh_command.orders(count, seed))
    ]


def benchmark_local(pc_binary, client_env, client_config, client_id, pc_scratch, args, helpers, run_id):
    daemon_root = pc_scratch / "local-daemon"
    daemon_root.mkdir(parents=True, exist_ok=True)
    daemon = IsolatedDaemon(
        scratch=daemon_root,
        binary=pc_binary,
        host_name="local",
        bind_ip="127.0.0.1",
        client_id=client_id,
        startup_timeout=HOST_SPECS["local"]["startup_timeout"],
        true_path=HOST_SPECS["local"]["true_path"],
    )
    with ExitStack() as stack:
        ready = daemon.start()
        stack.callback(daemon.stop)
        write_hosts_json(client_config, ready, "local")
        ssh_argv, ssh_env, ssh_note = start_local_openssh(pc_scratch / "local-ssh", helpers, stack)
        mcp_client = helpers.mcp_native.MCPClient(str(pc_binary), client_env, args=("--identity", "bench"))
        stack.callback(close_mcp_client, mcp_client)
        ctx_handle = open_mcp_context(mcp_client, ready["host_id"], ready["cwd"], run_id, "local")
        warmups, measured = run_paired_samples(
            mcp_client, ctx_handle, ready["true_path"], ssh_argv, ssh_env, args, helpers, run_id, "local"
        )
        close_mcp_client(mcp_client)
        server_result = daemon.server_bench(args.warmups, args.pairs)
        ssh_available = ssh_argv is not None
        summary = summarize_host(measured, server_result["samples"], ssh_available, helpers.ssh_command.summarize)
        return {
            "status": "ok",
            "host": "local",
            "platform": "linux/amd64",
            "target_ip": "127.0.0.1",
            "daemon_port": ready["port"],
            "load_average": list(os.getloadavg()),
            "rtt": {"available": False, "samples_ms": [], "reason": "local loopback"},
            "ssh": {"available": ssh_available, "alias": "owned-sshd", "detail": ssh_note,
                    **({} if ssh_available else {"reason": ssh_note})},
            "warmups": warmups,
            "pairs": measured,
            "server_warmups": server_result["warmups"],
            "server_samples": server_result["samples"],
            "summary": summary,
        }


def close_mcp_client(client):
    if client.proc.poll() is not None:
        return
    try:
        client.close_eof(timeout=5.0)
    except Exception:
        client.terminate()


def check_agent_alive(proc, prefix, stderr_lines):
    if proc.poll() is None:
        return
    err = "".join(stderr_lines)[-3000:]
    raise RuntimeError(f"remote agent exited ({proc.returncode}) before {prefix.strip()}: {err}")


def read_marker_line(proc, prefix, timeout, stderr_lines):
    import select
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        remaining = max(0.05, min(0.5, deadline - time.monotonic()))
        ready, _, _ = select.select([proc.stdout], [], [], remaining)
        if not ready:
            check_agent_alive(proc, prefix, stderr_lines)
            continue
        line = proc.stdout.readline()
        if not line:
            err = "".join(stderr_lines)[-3000:]
            raise RuntimeError(f"remote agent closed stdout before {prefix.strip()} (rc={proc.poll()}): {err}")
        parsed = parse_prefixed_json(line, prefix)
        if parsed is not None:
            return parsed
    err = "".join(stderr_lines)[-3000:]
    raise TimeoutError(f"timed out waiting for {prefix.strip()} after {timeout}s: {err}")


def start_stderr_drain(stream):
    lines = []

    def pump():
        for line in stream:
            lines.append(line)
            if len(lines) > 400:
                del lines[:200]

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    return lines, thread


def remote_cleanup_and_verify(spec, remote_python, remote_scratch):
    mesh_host = spec["mesh_host"]
    argv = ["mesh", "ssh", mesh_host, "--", remote_python, "-c", REMOTE_CLEANUP_SNIPPET, remote_scratch]
    result = run_fleet_cmd(argv, timeout=45)
    if result.returncode != 0:
        return {
            "clean": False,
            "error": (result.stderr or result.stdout).strip()[:1000],
            "remote_scratch": remote_scratch,
        }
    data = json.loads(result.stdout.strip().splitlines()[-1])
    return {
        "clean": not data["remaining_pids"] and bool(data["scratch_removed"]),
        "remaining_pids": data["remaining_pids"],
        "scratch_removed": data["scratch_removed"],
        "remote_scratch": remote_scratch,
    }


def stage_remote_files(mesh_host, binary_path, remote_scratch):
    mk = run_fleet_cmd(["mesh", "ssh", mesh_host, "--", "mkdir", "-p", remote_scratch], timeout=30)
    if mk.returncode != 0:
        raise RuntimeError(f"mkdir {remote_scratch} on {mesh_host} failed: {mk.stderr}")
    for src, name in ((binary_path, "mesh"), (Path(__file__).resolve(), "oneshot.py")):
        cp = run_fleet_cmd(["mesh", "cp", str(src), f"{mesh_host}:{remote_scratch}/{name}"], timeout=120)
        if cp.returncode != 0:
            raise RuntimeError(f"mesh cp {name} to {mesh_host} failed: {cp.stderr or cp.stdout}")
    chmod = run_fleet_cmd(
        ["mesh", "ssh", mesh_host, "--", "chmod", "+x", f"{remote_scratch}/mesh", f"{remote_scratch}/oneshot.py"],
        timeout=30,
    )
    if chmod.returncode != 0:
        raise RuntimeError(f"chmod on {mesh_host} failed: {chmod.stderr}")


def launch_remote_agent(host_name, spec, probe, remote_scratch, client_id, args):
    agent_argv = [
        probe["python3"],
        f"{remote_scratch}/oneshot.py",
        "--remote-agent",
        "--scratch", remote_scratch,
        "--binary", f"{remote_scratch}/mesh",
        "--host-name", host_name,
        "--bind-ip", probe["tailscale_ip"],
        "--client-id", client_id,
        "--startup-timeout", str(spec["startup_timeout"]),
        "--true-path", spec["true_path"],
        "--server-warmups", str(args.warmups),
        "--server-count", str(args.pairs),
    ]
    if spec.get("use_mac_queue"):
        shell_cmd = build_mac_queue_command(
            agent_argv,
            lane=args.mac_lane,
            thread=args.mac_thread,
            expected_seconds=MAC_QUEUE_EXPECTED_SECONDS,
            wait_seconds=MAC_QUEUE_WAIT_SECONDS,
            timeout_seconds=MAC_QUEUE_TIMEOUT_SECONDS,
        )
        launch_argv = ["mesh", "ssh", spec["mesh_host"], "--", "zsh", "-lc", shell_cmd]
    else:
        launch_argv = ["mesh", "ssh", spec["mesh_host"], "--", *agent_argv]
    return subprocess.Popen(
        launch_argv,
        env=os.environ,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )


def benchmark_remote(host_name, binaries, client_env, client_config, client_id, pc_scratch, args, helpers, run_id, cleanup_report):
    spec = HOST_SPECS[host_name]
    probe = probe_remote_host(spec)
    rtt = measure_tailscale_rtt(probe["tailscale_ip"])
    remote_scratch = f"{probe['home']}/mesh-oneshot-bench-{run_id}-{host_name}"
    remote_proc = None
    stderr_lines = []
    try:
        stage_remote_files(spec["mesh_host"], binaries[spec["platform"]], remote_scratch)
        remote_proc = launch_remote_agent(host_name, spec, probe, remote_scratch, client_id, args)
        stderr_lines, _ = start_stderr_drain(remote_proc.stderr)
        ready_timeout = spec["startup_timeout"] + (MAC_QUEUE_WAIT_SECONDS if spec.get("use_mac_queue") else 30)
        ready = read_marker_line(remote_proc, READY_PREFIX, ready_timeout, stderr_lines)
        return execute_remote_measurements(
            host_name, spec, probe, rtt, remote_scratch, remote_proc, stderr_lines,
            ready, binaries["linux/amd64"], client_env, client_config, pc_scratch, args, helpers, run_id,
        )
    finally:
        teardown_remote(host_name, spec, probe["python3"], remote_scratch, remote_proc, cleanup_report)


def execute_remote_measurements(
    host_name, spec, probe, rtt, remote_scratch, remote_proc, stderr_lines,
    ready, pc_binary, client_env, client_config, pc_scratch, args, helpers, run_id,
):
    ssh_dir = pc_scratch / f"ssh-{host_name}"
    ssh_dir.mkdir(mode=0o700, exist_ok=True)
    ssh_argv = build_remote_ssh_argv(spec["ssh_alias"], ssh_dir / "c")
    with ExitStack() as stack:
        write_hosts_json(client_config, ready, host_name)
        stack.callback(close_ssh_master, ssh_argv, os.environ)
        warm_ssh_master(ssh_argv, os.environ)
        mcp_client = helpers.mcp_native.MCPClient(str(pc_binary), client_env, args=("--identity", "bench"))
        stack.callback(close_mcp_client, mcp_client)
        ctx_handle = open_mcp_context(mcp_client, ready["host_id"], ready["cwd"], run_id, host_name)
        warmups, measured = run_paired_samples(
            mcp_client, ctx_handle, ready["true_path"], ssh_argv, os.environ, args, helpers, run_id, host_name
        )
        close_mcp_client(mcp_client)
        close_ssh_master(ssh_argv, os.environ)
    trigger = run_fleet_cmd(["mesh", "ssh", spec["mesh_host"], "--", "touch", f"{remote_scratch}/run-server-bench"], timeout=20)
    if trigger.returncode != 0:
        raise RuntimeError(f"failed to trigger server benchmark on {host_name}: {trigger.stderr}")
    server_result = read_marker_line(remote_proc, SERVER_BENCH_PREFIX, 180, stderr_lines)
    read_marker_line(remote_proc, STOPPED_PREFIX, 30, stderr_lines)
    remote_proc.wait(timeout=30)
    summary = summarize_host(measured, server_result["samples"], True, helpers.ssh_command.summarize)
    return {
        "status": "ok",
        "host": host_name,
        "platform": spec["platform"],
        "target_ip": ready["ip"],
        "daemon_port": ready["port"],
        "remote_platform": probe["platform"],
        "load_average": probe["load_average"],
        "availability": probe.get("availability"),
        "rtt": rtt,
        "ssh": {"available": True, "alias": spec["ssh_alias"]},
        "warmups": warmups,
        "pairs": measured,
        "server_warmups": server_result["warmups"],
        "server_samples": server_result["samples"],
        "summary": summary,
    }


def teardown_remote(host_name, spec, remote_python, remote_scratch, remote_proc, cleanup_report):
    if remote_proc is not None and remote_proc.poll() is None:
        try:
            run_fleet_cmd(["mesh", "ssh", spec["mesh_host"], "--", "touch", f"{remote_scratch}/stop"], timeout=10)
            remote_proc.wait(timeout=15)
        except Exception:
            stop_popen(remote_proc, grace=5.0)
    cleanup_report[host_name] = remote_cleanup_and_verify(spec, remote_python, remote_scratch)


@contextmanager
def signal_guard(helpers):
    with helpers.isolation.termination_cleanup():
        yield


def run_coordinator(args):
    helpers = load_coordinator_helpers()
    run_id = uuid.uuid4().hex[:8]
    pc_scratch = None
    report = {
        "schema": 1,
        "benchmark": "mesh-oneshot-vs-warm-ssh",
        "status": "error",
        "run_id": run_id,
        "environment": {
            "utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "platform": platform.platform(),
            "python": sys.version,
            "cpu_count": os.cpu_count(),
            "load_average_start": list(os.getloadavg()),
            "git": git_info(),
            "warmups": args.warmups,
            "pairs": args.pairs,
            "seed": args.seed,
            "wait_ms": DEFAULT_WAIT_MS,
            "hosts_requested": args.hosts,
        },
        "binaries": {},
        "hosts": {},
        "cleanup": {},
    }
    try:
        with signal_guard(helpers):
            pc_scratch = Path(tempfile.mkdtemp(prefix="mesh-oneshot-bench-", dir=str(args.scratch_parent))).resolve()
            report["scratch_root"] = str(pc_scratch)
            execute_fleet_run(args, helpers, run_id, pc_scratch, report)
    except (Exception, SystemExit) as error:
        report["status"] = "error"
        report["error"] = str(error)
    finally:
        with helpers.isolation.protected_cleanup():
            cleanup_pc(pc_scratch, report)
    report["environment"]["load_average_end"] = list(os.getloadavg())
    write_outputs(args, report)
    return 0 if report["status"] == "ok" else 1


def run_one_host(host_name, binaries, client_env, client_config, client_id, pc_scratch, args, helpers, run_id, cleanup_report):
    if host_name == "local":
        return benchmark_local(
            binaries["linux/amd64"], client_env, client_config, client_id, pc_scratch, args, helpers, run_id
        )
    return benchmark_remote(
        host_name, binaries, client_env, client_config, client_id,
        pc_scratch, args, helpers, run_id, cleanup_report,
    )


def execute_fleet_run(args, helpers, run_id, pc_scratch, report):
    binaries = resolve_binaries(args, pc_scratch)
    for plat, path in binaries.items():
        report["binaries"][plat] = {"path": str(path), "sha256": sha256_file(path)}
    pc_binary = binaries["linux/amd64"]
    ver = subprocess.run([str(pc_binary), "version"], capture_output=True, text=True, timeout=15)
    report["environment"]["binary_version"] = ver.stdout.strip()
    client_env, client_config, client_id = prepare_pc_client(pc_binary, pc_scratch / "pc-client")
    failures = 0
    for host_name in args.hosts:
        try:
            report["hosts"][host_name] = run_one_host(
                host_name, binaries, client_env, client_config, client_id,
                pc_scratch, args, helpers, run_id, report["cleanup"],
            )
        except Exception as error:
            failures += 1
            report["hosts"][host_name] = {"status": "error", "host": host_name, "error": str(error)}
    report["status"] = "ok" if failures == 0 else "error"
    if failures:
        report["error"] = f"{failures} host(s) failed"


def cleanup_pc(pc_scratch, report):
    if pc_scratch is None:
        report["cleanup"]["pc"] = {"clean": True, "remaining_pids": [], "scratch_removed": True}
        return
    remaining = stop_scratch_processes(pc_scratch)
    removed = False
    if not remaining:
        if pc_scratch.exists():
            shutil.rmtree(pc_scratch)
        removed = not pc_scratch.exists()
    report["cleanup"]["pc"] = {
        "clean": not remaining and removed,
        "remaining_pids": remaining,
        "scratch_removed": removed,
        "scratch_root": str(pc_scratch),
    }


def write_outputs(args, report):
    table = format_markdown_table(report)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")
    if args.markdown:
        args.markdown.parent.mkdir(parents=True, exist_ok=True)
        args.markdown.write_text(table)
    sys.stdout.write(table)
    if report.get("error"):
        sys.stderr.write(f"oneshot benchmark error: {report['error']}\n")


# ---------------------------------------------------------------------------
# Argument parsing and entry point
# ---------------------------------------------------------------------------


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary-linux-amd64", type=Path, help="prebuilt linux/amd64 mesh binary")
    parser.add_argument("--binary-linux-arm64", type=Path, help="prebuilt linux/arm64 mesh binary")
    parser.add_argument("--binary-darwin-arm64", type=Path, help="prebuilt darwin/arm64 mesh binary")
    parser.add_argument("--build", action="store_true", help="build missing target binaries from the checkout")
    parser.add_argument("--hosts", default=",".join(DEFAULT_HOSTS), help="comma-separated hosts (local,vps,pi,mac)")
    parser.add_argument("--pairs", type=int, default=DEFAULT_PAIRS, help="measured paired samples per host (default 30)")
    parser.add_argument("--warmups", type=int, default=DEFAULT_WARMUPS, help="warmup samples dropped before timing (default 3)")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED, help="RNG seed for paired ordering")
    parser.add_argument("--output", type=Path, help="JSON output path")
    parser.add_argument("--markdown", type=Path, help="optional Markdown table output path")
    parser.add_argument("--scratch-parent", type=Path, default=Path("/work/tmp"), help="local scratch parent directory")
    parser.add_argument("--mac-lane", default="mesh-oneshot-bench", help="lane name for mac-queue.ts")
    parser.add_argument("--mac-thread", default=os.environ.get("T3_THREAD_ID", "oneshot-bench"), help="thread ID for mac-queue.ts")

    # Internal flags used when running as the remote host daemon agent.
    parser.add_argument("--remote-agent", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--scratch", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--binary", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--host-name", help=argparse.SUPPRESS)
    parser.add_argument("--bind-ip", help=argparse.SUPPRESS)
    parser.add_argument("--client-id", help=argparse.SUPPRESS)
    parser.add_argument("--startup-timeout", type=int, default=30, help=argparse.SUPPRESS)
    parser.add_argument("--true-path", default="/bin/true", help=argparse.SUPPRESS)
    parser.add_argument("--server-warmups", type=int, default=DEFAULT_WARMUPS, help=argparse.SUPPRESS)
    parser.add_argument("--server-count", type=int, default=DEFAULT_PAIRS, help=argparse.SUPPRESS)

    args = parser.parse_args(argv)
    if args.remote_agent:
        return args
    try:
        args.hosts = parse_hosts(args.hosts)
    except ValueError as error:
        parser.error(str(error))
    if args.pairs < 1:
        parser.error("--pairs must be at least 1")
    if args.warmups < 0:
        parser.error("--warmups must be non-negative")
    if not args.scratch_parent.is_dir():
        parser.error(f"--scratch-parent does not exist: {args.scratch_parent}")
    return args


def main(argv=None):
    args = parse_args(argv)
    if args.remote_agent:
        return run_remote_agent(args)
    return run_coordinator(args)


if __name__ == "__main__":
    raise SystemExit(main())
