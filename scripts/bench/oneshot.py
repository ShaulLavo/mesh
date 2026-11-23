#!/usr/bin/env python3
"""Fleet benchmark comparing Mesh one-shot MCP execution against warm OpenSSH."""

import argparse
from contextlib import ExitStack, contextmanager
import ctypes
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import queue
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
        "tailscale_dns_prefix": "vps",
        "ssh_alias": "vps",
        "true_path": "/bin/true",
        "startup_timeout": 30,
        "use_mac_queue": False,
    },
    "pi": {
        "kind": "remote",
        "platform": "linux/arm64",
        "tailscale_dns_prefix": "pi",
        "ssh_alias": "raspberry-pi",
        "true_path": "/bin/true",
        "startup_timeout": 60,
        "use_mac_queue": False,
    },
    "mac": {
        "kind": "remote",
        "platform": "darwin/arm64",
        "tailscale_dns_prefix": "shauls-macbook-air",
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
SIGN_P_THRESHOLD = 0.01
MAC_QUEUE_WAIT_SECONDS = 600
MAC_QUEUE_TIMEOUT_SECONDS = 600
MAC_QUEUE_EXPECTED_SECONDS = 300
OWNER_TOKEN_ENV = "ONESHOT_BENCH_OWNER_TOKEN"
PR_SET_CHILD_SUBREAPER = 36

STAGE_READY_PREFIX = "ONESHOT_STAGE_READY "
READY_PREFIX = "ONESHOT_READY "
SERVER_BENCH_PREFIX = "ONESHOT_SERVER_BENCH "
STOPPED_PREFIX = "ONESHOT_STOPPED "

PING_RTT_RE = re.compile(r"\bin\s+([0-9]+(?:\.[0-9]+)?)\s*(ns|µs|us|ms|s)\b")
MASTER_PID_RE = re.compile(r"Master running \(pid=(\d+)\)")

REMOTE_STAGE_WRAPPER = r"""
set -eu
scratch=$1; py=$2; host=$3; ip=$4; cid=$5; st=$6; tp=$7; sw=$8; sc=$9; tok=${10}
mkdir -p "$scratch"
chmod 700 "$scratch"
cleanup_stage() {
  if [ -f "$scratch/oneshot.py" ]; then
    "$py" "$scratch/oneshot.py" --cleanup-scratch --scratch "$scratch" --owner-token "$tok" || true
  elif [ -d "$scratch" ]; then
    rm -rf -- "$scratch"
  fi
}
trap cleanup_stage EXIT INT TERM HUP
printf 'ONESHOT_STAGE_READY {"scratch":"%s"}\n' "$scratch"
while [ ! -f "$scratch/stage.done" ]; do
  [ -f "$scratch/stop" ] && exit 0
  sleep 0.05
done
trap - EXIT INT TERM HUP
exec "$py" "$scratch/oneshot.py" --remote-agent --self-clean \
  --scratch "$scratch" --binary "$scratch/mesh" --host-name "$host" \
  --bind-ip "$ip" --client-id "$cid" --startup-timeout "$st" \
  --true-path "$tp" --server-warmups "$sw" --server-count "$sc" \
  --owner-token "$tok"
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
        samples_ms.append(duration_to_ms(float(match.group(1)), match.group(2)))
    if not samples_ms:
        return {"available": False, "samples_ms": [], "reason": "no pong latency parsed"}
    return {
        "available": True,
        "samples_ms": samples_ms,
        "summary_ms": ms_stats(samples_ms),
    }


def parse_tailscale_peer_ipv4(status_obj, dns_prefix):
    peers = (status_obj or {}).get("Peer") or {}
    want = dns_prefix.lower().rstrip(".")
    for peer in peers.values():
        dns = (peer.get("DNSName") or "").lower().rstrip(".")
        host = (peer.get("HostName") or "").lower()
        short_dns = dns.split(".", 1)[0] if dns else ""
        if short_dns != want and host != want:
            continue
        ipv4 = first_ipv4(peer.get("TailscaleIPs") or [])
        if ipv4:
            return ipv4
    return None


def first_ipv4(addresses):
    for addr in addresses:
        if "." in addr and ":" not in addr:
            return addr
    return None


def parse_master_check_pid(text):
    match = MASTER_PID_RE.search(text or "")
    return int(match.group(1)) if match else None


def parse_ssh_connection(text):
    parts = (text or "").strip().split()
    if len(parts) != 4:
        return None
    return {
        "client_ip": parts[0],
        "client_port": parts[1],
        "server_ip": parts[2],
        "server_port": parts[3],
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


def is_path_within(candidate, parent):
    if not candidate or not parent:
        return False
    norm_parent = os.path.normpath(str(parent))
    norm_cand = os.path.normpath(str(candidate))
    return norm_cand == norm_parent or norm_cand.startswith(norm_parent + os.sep)


def is_owned_worker_row(row, exact_binary, state_sessions_dir):
    if not exact_binary or not state_sessions_dir:
        return False
    exe = row.get("exe") or ""
    argv = row.get("argv") or []
    binary_match = os.path.normpath(exe) == os.path.normpath(str(exact_binary))
    if not binary_match and argv:
        binary_match = os.path.normpath(argv[0]) == os.path.normpath(str(exact_binary))
    if not binary_match or "session-worker" not in argv:
        return False
    return any(is_path_within(arg, state_sessions_dir) for arg in argv)


def seed_owned_pids(rows, owned_roots, exact_binary, state_sessions_dir, meta_pids, owner_token, exclude_pids):
    owned = {}
    root_set = {(int(pid), str(start)) for pid, start in owned_roots}
    for row in rows:
        pid = row["pid"]
        start = str(row["start"])
        if pid in exclude_pids:
            continue
        if (pid, start) in root_set:
            owned[pid] = start
            continue
        if owner_token and row.get("owner_token") == owner_token:
            owned[pid] = start
            continue
        if is_owned_worker_row(row, exact_binary, state_sessions_dir):
            owned[pid] = start
    add_meta_children(rows, owned, meta_pids, exclude_pids)
    return owned


def add_meta_children(rows, owned, meta_pids, exclude_pids):
    if not meta_pids:
        return
    meta_set = {int(pid) for pid in meta_pids}
    for row in rows:
        pid = row["pid"]
        if pid in exclude_pids or pid in owned or pid not in meta_set:
            continue
        # Accept a meta.json child PID only when its parent is an owned worker
        # or it leads its own session/group (detached worker command).
        if row.get("ppid") in owned or row.get("pgid") == pid:
            owned[pid] = str(row["start"])


def discover_owned_processes(
    rows, owned_roots, exact_binary, state_sessions_dir, meta_pids, owner_token, exclude_pids
):
    owned = seed_owned_pids(
        rows, owned_roots, exact_binary, state_sessions_dir, meta_pids, owner_token, exclude_pids
    )
    changed = True
    while changed:
        changed = expand_owned_descendants(rows, owned, exclude_pids)
    return [(pid, owned[pid]) for pid in sorted(owned)]


def expand_owned_descendants(rows, owned, exclude_pids):
    added = False
    owned_pgids = {row.get("pgid") for row in rows if row["pid"] in owned and row.get("pgid")}
    for row in rows:
        pid = row["pid"]
        if pid in exclude_pids or pid in owned:
            continue
        if row.get("ppid") in owned or (row.get("pgid") in owned_pgids and row.get("pgid") == row.get("pid")):
            owned[pid] = str(row["start"])
            added = True
    return added


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


def build_ssh_exec_command(cwd, true_path):
    return f"cd {shlex.quote(str(cwd))} && exec {shlex.quote(str(true_path))}"


def classify_promise_verdict(pair_count, clear_slowdown):
    if pair_count <= 0:
        return "n/a"
    if clear_slowdown:
        return "slowdown"
    min_possible_p = 1.0 / (2 ** pair_count)
    if min_possible_p >= SIGN_P_THRESHOLD:
        return "inconclusive"
    return "no clear slowdown"


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
    summary["promise_verdict"] = classify_promise_verdict(len(measured_pairs), policy["clear_slowdown"])
    summary["at_or_under_ssh_p50"] = mesh_p50 <= ssh_p50
    return summary


def evaluate_cleanup_status(cleanup_map, required_keys):
    failed = []
    for key in required_keys:
        entry = cleanup_map.get(key)
        if not isinstance(entry, dict) or entry.get("clean") is not True:
            failed.append(key)
    for key, entry in cleanup_map.items():
        if key not in failed and (not isinstance(entry, dict) or entry.get("clean") is not True):
            failed.append(key)
    return not failed, failed


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
# Thread-backed marker reader (never misses coalesced buffered lines)
# ---------------------------------------------------------------------------


class MarkerStream:
    """Reads lines from a pipe on a background thread so coalesced lines are never missed."""

    def __init__(self, stream):
        self._stream = stream
        self._queue = queue.Queue()
        self._thread = threading.Thread(target=self._pump, daemon=True)
        self._thread.start()

    def _pump(self):
        try:
            for line in self._stream:
                self._queue.put(line)
        finally:
            self._queue.put(None)

    def read_marker(self, prefix, timeout, on_eof_detail=None):
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                detail = on_eof_detail() if on_eof_detail else ""
                raise TimeoutError(f"timed out waiting for {prefix.strip()} after {timeout}s: {detail}")
            try:
                line = self._queue.get(timeout=min(0.5, remaining))
            except queue.Empty:
                continue
            if line is None:
                detail = on_eof_detail() if on_eof_detail else ""
                raise RuntimeError(f"stream closed before {prefix.strip()}: {detail}")
            parsed = parse_prefixed_json(line, prefix)
            if parsed is not None:
                return parsed


# ---------------------------------------------------------------------------
# Stdlib-only process identity, verified signalling, and daemon RPC
# ---------------------------------------------------------------------------


def enable_linux_subreaper():
    if sys.platform != "linux":
        return False
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        return libc.prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) == 0
    except OSError:
        return False


def reap_children():
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            return
        if pid <= 0:
            return


def process_start_id(pid):
    if sys.platform == "linux":
        return linux_start_id(pid)
    return posix_start_id(pid)


def linux_start_id(pid):
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
    except OSError:
        return None
    if fields[0] == "Z":
        return None
    return fields[19]


def posix_start_id(pid):
    result = subprocess.run(
        ["ps", "-o", "state=,lstart=", "-p", str(pid)],
        capture_output=True,
        text=True,
    )
    line = result.stdout.strip()
    if not line:
        return None
    parts = line.split(None, 1)
    if len(parts) < 2 or parts[0].startswith("Z"):
        return None
    return parts[1].strip()


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


def list_process_rows():
    if sys.platform == "linux":
        return list_linux_process_rows()
    return list_posix_process_rows()


def list_linux_process_rows():
    rows = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        row = read_linux_process_row(entry)
        if row is not None:
            rows.append(row)
    return rows


def read_linux_process_row(entry):
    try:
        fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
        if fields[0] == "Z":
            return None
        raw_cmd = (entry / "cmdline").read_bytes()
        argv = [part.decode(errors="replace") for part in raw_cmd.split(b"\x00") if part]
        exe = os.readlink(entry / "exe")
    except OSError:
        return None
    token = read_linux_owner_token(entry)
    return {
        "pid": int(entry.name),
        "ppid": int(fields[1]),
        "pgid": int(fields[2]),
        "start": fields[19],
        "exe": exe,
        "argv": argv,
        "owner_token": token,
    }


def read_linux_owner_token(entry):
    prefix = f"{OWNER_TOKEN_ENV}=".encode()
    try:
        data = (entry / "environ").read_bytes()
    except OSError:
        return None
    for item in data.split(b"\x00"):
        if item.startswith(prefix):
            return item[len(prefix):].decode(errors="replace")
    return None


def list_posix_process_rows():
    result = subprocess.run(
        ["ps", "-ax", "-o", "pid=,ppid=,pgid=,state=,lstart=,command="],
        capture_output=True,
        text=True,
    )
    rows = []
    for line in result.stdout.splitlines():
        row = parse_posix_ps_row(line)
        if row is not None:
            rows.append(row)
    return rows


def parse_posix_ps_row(line):
    # Format: PID PPID PGID STATE Wday Mon Day HH:MM:SS YYYY COMMAND...
    parts = line.strip().split(None, 9)
    if len(parts) < 10 or not parts[0].isdigit():
        return None
    if parts[3].startswith("Z"):
        return None
    start = " ".join(parts[4:9])
    command = parts[9]
    argv = split_command_safe(command)
    return {
        "pid": int(parts[0]),
        "ppid": int(parts[1]),
        "pgid": int(parts[2]),
        "start": start,
        "exe": argv[0] if argv else "",
        "argv": argv,
        "owner_token": None,
    }


def split_command_safe(command):
    try:
        return shlex.split(command)
    except ValueError:
        return command.split()


def read_session_meta_pids(state_dir):
    if not state_dir:
        return []
    sessions_dir = Path(state_dir) / "s"
    if not sessions_dir.exists():
        return []
    pids = []
    for meta in sessions_dir.glob("*/meta.json"):
        pid = read_meta_pid(meta)
        if pid is not None:
            pids.append(pid)
    return pids


def read_meta_pid(meta_path):
    try:
        data = json.loads(meta_path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    pid = data.get("pid")
    return int(pid) if isinstance(pid, int) and pid > 1 else None


class OwnedRegistry:
    """Tracks exact (pid, start_id) identities persisted inside the scratch directory."""

    def __init__(self, path):
        self.path = Path(path)
        self._records = []

    def record(self, pid):
        start = process_start_id(pid)
        if start is None:
            return None
        entry = (int(pid), str(start))
        if entry not in self._records:
            self._records.append(entry)
            self.path.parent.mkdir(parents=True, exist_ok=True)
            with self.path.open("a", encoding="utf-8") as stream:
                stream.write(json.dumps({"pid": entry[0], "start": entry[1]}) + "\n")
        return entry

    def load_all(self):
        seen = set(self._records)
        if not self.path.exists():
            return sorted(seen)
        for line in self.path.read_text(errors="replace").splitlines():
            item = parse_registry_line(line)
            if item is not None:
                seen.add(item)
        return sorted(seen)


def parse_registry_line(line):
    try:
        data = json.loads(line)
        return int(data["pid"]), str(data["start"])
    except (ValueError, KeyError, TypeError):
        return None


def signal_verified_process(pid, expected_start, sig):
    if sys.platform == "linux" and hasattr(os, "pidfd_open") and hasattr(signal, "pidfd_send_signal"):
        return signal_linux_pidfd(pid, expected_start, sig)
    current = process_start_id(pid)
    if current != str(expected_start):
        return False
    try:
        os.kill(pid, sig)
        return True
    except OSError:
        return False


def signal_linux_pidfd(pid, expected_start, sig):
    try:
        fd = os.pidfd_open(pid)
    except OSError:
        return False
    try:
        if linux_start_id(pid) != str(expected_start):
            return False
        signal.pidfd_send_signal(fd, sig)
        return True
    except OSError:
        return False
    finally:
        os.close(fd)


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


def shutdown_workers_gracefully(state_dir, wait_seconds=3.0):
    if not state_dir:
        return
    sessions_dir = Path(state_dir) / "s"
    if not sessions_dir.exists():
        return
    sockets = list(sessions_dir.glob("*/sock"))
    for sock in sockets:
        kill_worker_socket(sock)
    deadline = time.monotonic() + wait_seconds
    while time.monotonic() < deadline and any(sock.exists() for sock in sockets):
        time.sleep(0.05)


def cleanup_owned_tree(
    scratch_path,
    *,
    registry=None,
    exact_binary=None,
    state_dir=None,
    owner_token=None,
    daemon_proc=None,
):
    scratch = Path(scratch_path).resolve()
    if registry is None:
        registry = OwnedRegistry(scratch / "owned-pids.jsonl")
    if state_dir is None:
        state_dir = scratch / "state"
    if exact_binary is None and (scratch / "mesh").exists():
        exact_binary = scratch / "mesh"
    shutdown_workers_gracefully(state_dir)
    stop_popen(daemon_proc, grace=5.0)
    reap_children()
    exclude = ancestor_pids()
    sessions_dir = Path(state_dir) / "s"
    for sig, wait_s in ((signal.SIGTERM, 3.0), (signal.SIGKILL, 2.0)):
        targets = find_live_owned(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude)
        if not targets:
            return []
        for pid, start in targets:
            signal_verified_process(pid, start, sig)
        wait_owned_gone(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude, wait_s)
    return [pid for pid, _ in find_live_owned(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude)]


def find_live_owned(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude):
    reap_children()
    rows = list_process_rows()
    roots = registry.load_all()
    meta_pids = read_session_meta_pids(state_dir)
    return discover_owned_processes(rows, roots, exact_binary, sessions_dir, meta_pids, owner_token, exclude)


def wait_owned_gone(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not find_live_owned(registry, exact_binary, sessions_dir, state_dir, owner_token, exclude):
            return
        time.sleep(0.05)


def stop_popen(proc, grace=5.0):
    if proc is None or proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5.0)


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


def wait_daemon_ready(proc, state_dir, bind_ip, port, log_path, timeout, stop_event=None):
    sock_path = state_dir / "daemon.sock"
    deadline = time.monotonic() + timeout
    host_info = None
    while time.monotonic() < deadline:
        if stop_event is not None and stop_event.is_set():
            raise RuntimeError("cancelled during daemon startup")
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


def run_server_bench(sock_path, cwd, true_path, warmups, count, stop_event=None, stop_file=None):
    all_samples = []
    total = warmups + count
    for index in range(total):
        if is_cancelled(stop_event, stop_file):
            raise RuntimeError("server benchmark cancelled")
        key = f"srv-{uuid.uuid4().hex[:12]}-{index}"
        all_samples.append(run_one_server_sample(sock_path, cwd, true_path, key))
    warm, measured = drop_warmups(all_samples, warmups)
    return {"warmups": warm, "samples": measured}


def is_cancelled(stop_event, stop_file):
    if stop_event is not None and stop_event.is_set():
        return True
    return stop_file is not None and Path(stop_file).exists()


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

    def __init__(self, scratch, binary, host_name, bind_ip, client_id, startup_timeout, true_path, owner_token):
        self.scratch = Path(scratch).resolve()
        self.binary = str(Path(binary).resolve())
        self.host_name = host_name
        self.bind_ip = bind_ip
        self.client_id = client_id
        self.startup_timeout = startup_timeout
        self.true_path = true_path
        self.owner_token = owner_token
        self.work_dir = self.scratch / "work"
        self.log_path = self.scratch / "daemon.log"
        self.registry = OwnedRegistry(self.scratch / "owned-pids.jsonl")
        self.proc = None
        self.log_stream = None
        self.state_dir = self.scratch / "state"
        self.env = None
        self.port = None
        self.host_info = None

    def start(self, stop_event=None):
        self.scratch.mkdir(parents=True, exist_ok=True)
        self.scratch.chmod(0o700)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        bindir, status_path = write_fake_tailscale(self.scratch, self.host_name, self.bind_ip)
        self.port = pick_free_port(self.bind_ip)
        extra = {
            "MESH_FAKE_TAILSCALE_STATUS": str(status_path),
            "PATH": str(bindir) + os.pathsep + os.environ.get("PATH", os.defpath),
            OWNER_TOKEN_ENV: self.owner_token,
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
        self.registry.record(self.proc.pid)
        self.host_info = wait_daemon_ready(
            self.proc, self.state_dir, self.bind_ip, self.port, self.log_path, self.startup_timeout, stop_event
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

    def server_bench(self, warmups, count, stop_event=None, stop_file=None):
        return run_server_bench(
            self.state_dir / "daemon.sock",
            str(self.work_dir),
            self.true_path,
            warmups,
            count,
            stop_event=stop_event,
            stop_file=stop_file,
        )

    def stop(self):
        remaining = cleanup_owned_tree(
            self.scratch,
            registry=self.registry,
            exact_binary=self.binary,
            state_dir=self.state_dir,
            owner_token=self.owner_token,
            daemon_proc=self.proc,
        )
        self.proc = None
        if self.log_stream is not None:
            self.log_stream.close()
            self.log_stream = None
        return remaining


# ---------------------------------------------------------------------------
# Remote agent and standalone scratch-cleanup submodes
# ---------------------------------------------------------------------------


def remove_scratch_if_clean(scratch, remaining):
    if remaining:
        return False
    if scratch.exists():
        shutil.rmtree(scratch)
    return not scratch.exists()


def run_cleanup_scratch(args):
    scratch = Path(args.scratch).resolve()
    if not scratch.name.startswith("mesh-oneshot-bench-"):
        raise SystemExit(f"refusing to clean unexpected scratch path: {scratch}")
    remaining = cleanup_owned_tree(scratch, owner_token=args.owner_token or None)
    removed = remove_scratch_if_clean(scratch, remaining)
    result = {
        "clean": not remaining and removed,
        "remaining_pids": remaining,
        "scratch_removed": removed,
        "remote_scratch": str(scratch),
    }
    print(json.dumps(result, separators=(",", ":")))
    return 0 if result["clean"] else 1


def run_remote_agent(args):
    enable_linux_subreaper()
    stop_requested = threading.Event()

    def handle_signal(_signum, _frame):
        stop_requested.set()

    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, handle_signal)

    scratch = Path(args.scratch).resolve()
    daemon = IsolatedDaemon(
        scratch=scratch,
        binary=args.binary,
        host_name=args.host_name,
        bind_ip=args.bind_ip,
        client_id=args.client_id,
        startup_timeout=args.startup_timeout,
        true_path=args.true_path,
        owner_token=args.owner_token,
    )
    trigger_bench = scratch / "run-server-bench"
    trigger_stop = scratch / "stop"
    exit_code = 0
    try:
        ready = daemon.start(stop_event=stop_requested)
        emit_line(READY_PREFIX, ready)
        wait_remote_triggers(daemon, trigger_bench, trigger_stop, stop_requested, args)
    except Exception:
        exit_code = 1
        raise
    finally:
        cleanup_result = finish_remote_agent(daemon, scratch, args.self_clean)
        emit_line(STOPPED_PREFIX, cleanup_result)
        if not cleanup_result["clean"]:
            exit_code = 1
    return exit_code


def finish_remote_agent(daemon, scratch, self_clean):
    # Ignore termination signals while tearing down owned daemon and workers.
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, signal.SIG_IGN)
    remaining = daemon.stop()
    removed = remove_scratch_if_clean(scratch, remaining) if self_clean else (not scratch.exists())
    clean = (not remaining) and (removed if self_clean else True)
    return {
        "clean": clean,
        "remaining_pids": remaining,
        "scratch_removed": removed,
        "remote_scratch": str(scratch),
    }


def wait_remote_triggers(daemon, trigger_bench, trigger_stop, stop_requested, args):
    while not stop_requested.is_set():
        if daemon.proc.poll() is not None:
            raise RuntimeError(f"isolated daemon exited early ({daemon.proc.returncode})")
        if trigger_stop.exists():
            return
        if trigger_bench.exists():
            result = daemon.server_bench(
                args.server_warmups, args.server_count, stop_event=stop_requested, stop_file=trigger_stop
            )
            emit_line(SERVER_BENCH_PREFIX, result)
            return
        time.sleep(0.05)


def emit_line(prefix, payload):
    sys.stdout.write(prefix + json.dumps(payload, separators=(",", ":")) + "\n")
    sys.stdout.flush()


# ---------------------------------------------------------------------------
# Owned OpenSSH ControlMaster (used for both orchestration and timed SSH arm)
# ---------------------------------------------------------------------------


def linux_socket_inodes(pid):
    if sys.platform != "linux":
        return []
    fd_dir = Path(f"/proc/{pid}/fd")
    if not fd_dir.exists():
        return []
    inodes = []
    for item in fd_dir.iterdir():
        inode = read_socket_inode(item)
        if inode is not None:
            inodes.append(inode)
    return sorted(inodes)


def read_socket_inode(fd_path):
    try:
        target = os.readlink(fd_path)
    except OSError:
        return None
    if target.startswith("socket:[") and target.endswith("]"):
        return target[8:-1]
    return None


class SSHMaster:
    """Explicitly owned OpenSSH ControlMaster (-N -M) with ProxyCommand=false fallback guard."""

    def __init__(self, base_argv, target, control_path, env, registry=None):
        self.base_argv = list(base_argv)
        self.target = target
        self.control_path = Path(control_path)
        self.env = env
        self.registry = registry
        self.proc = None
        self.master_pid = None
        self.stderr_lines = []

    def start(self, timeout=25.0):
        self.control_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.control_path.parent.chmod(0o700)
        argv = self.base_argv + [
            "-N", "-M",
            "-o", "ControlMaster=yes",
            "-o", f"ControlPath={self.control_path}",
            "-o", "ControlPersist=no",
            "-o", "ExitOnForwardFailure=yes",
            self.target,
        ]
        self.proc = subprocess.Popen(
            argv,
            env=self.env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        if self.registry is not None:
            self.registry.record(self.proc.pid)
        self.stderr_lines, _ = start_stderr_drain(self.proc.stderr)
        self.master_pid = self._wait_ready(timeout)
        return self.master_pid

    def _wait_ready(self, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                detail = "".join(self.stderr_lines)[-2000:]
                raise RuntimeError(f"SSH master exited ({self.proc.returncode}): {detail}")
            pid = self.check()
            if pid is None:
                time.sleep(0.05)
                continue
            if pid != self.proc.pid:
                raise RuntimeError(f"SSH master PID mismatch: expected {self.proc.pid}, got {pid}")
            return pid
        detail = "".join(self.stderr_lines)[-2000:]
        raise TimeoutError(f"SSH master did not become ready within {timeout}s: {detail}")

    def check(self):
        result = subprocess.run(
            self.base_argv + ["-o", f"ControlPath={self.control_path}", "-O", "check", self.target],
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=10,
        )
        if result.returncode != 0:
            return None
        return parse_master_check_pid(result.stderr + "\n" + result.stdout)

    def client_argv(self, extra_options=()):
        return self.base_argv + [
            "-o", "ControlMaster=no",
            "-o", f"ControlPath={self.control_path}",
            "-o", "ControlPersist=no",
            "-o", "ProxyCommand=false",
            *extra_options,
            "-T",
            self.target,
        ]

    def run(self, remote_command, timeout=60):
        return subprocess.run(
            self.client_argv() + [remote_command],
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def popen(self, remote_command):
        return subprocess.Popen(
            self.client_argv() + [remote_command],
            env=self.env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def scp_upload(self, local_path, remote_path, host_ip=None, timeout=120):
        opts = [
            "-q",
            "-o", "BatchMode=yes",
            "-o", "ControlMaster=no",
            "-o", f"ControlPath={self.control_path}",
            "-o", "ControlPersist=no",
            "-o", "ProxyCommand=false",
        ]
        if host_ip:
            opts.extend(["-o", f"HostName={host_ip}"])
        return subprocess.run(
            [shutil.which("scp"), *opts, str(local_path), f"{self.target}:{remote_path}"],
            env=self.env,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def close(self):
        if self.proc is None:
            return
        try:
            subprocess.run(
                self.base_argv + ["-o", f"ControlPath={self.control_path}", "-O", "exit", self.target],
                env=self.env,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=10,
            )
        except OSError:
            pass
        stop_popen(self.proc, grace=5.0)
        self.proc = None


# ---------------------------------------------------------------------------
# Coordinator mode (runs on PC)
# ---------------------------------------------------------------------------


def load_coordinator_helpers():
    for path in (str(BENCH_DIR), str(HELPERS_DIR)):
        if path not in sys.path:
            sys.path.insert(0, path)
    import isolation
    import mcp_native
    import ssh_command
    return SimpleNamespace(
        isolation=isolation,
        mcp_native=mcp_native,
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


def resolve_tailscale_ip_local(dns_prefix):
    if not shutil.which("tailscale"):
        raise RuntimeError("tailscale CLI is required to resolve target Tailscale IPv4 addresses")
    res = subprocess.run(["tailscale", "status", "--json"], capture_output=True, text=True, timeout=15)
    if res.returncode != 0:
        raise RuntimeError(f"tailscale status --json failed: {res.stderr}")
    ip = parse_tailscale_peer_ipv4(json.loads(res.stdout), dns_prefix)
    if not ip:
        raise RuntimeError(f"could not find Tailscale IPv4 for peer {dns_prefix!r}")
    return ip


def measure_tailscale_rtt(ip):
    if not shutil.which("tailscale"):
        return {"available": False, "samples_ms": [], "reason": "tailscale CLI not found"}
    result = subprocess.run(
        ["tailscale", "ping", "-c", "5", "--until-direct=false", ip],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=25,
    )
    parsed = parse_tailscale_ping(result.stdout + "\n" + result.stderr)
    if not parsed["available"] and result.returncode != 0:
        parsed["reason"] = (result.stderr or result.stdout).strip()[:300]
    return parsed


def remote_ssh_base_argv(target_ip):
    return [
        shutil.which("ssh"),
        "-o", "BatchMode=yes",
        "-o", "ConnectTimeout=15",
        "-o", f"HostName={target_ip}",
    ]


def verify_ssh_effective_hostname(ssh_alias, target_ip):
    res = subprocess.run(
        [shutil.which("ssh"), "-G", "-o", f"HostName={target_ip}", ssh_alias],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=10,
    )
    if res.returncode != 0:
        raise RuntimeError(f"ssh -G failed for {ssh_alias}: {res.stderr}")
    effective = next((line.split(None, 1)[1].strip() for line in res.stdout.splitlines() if line.startswith("hostname ")), None)
    if effective is None:
        raise RuntimeError(f"ssh -G did not report hostname for {ssh_alias}")
    if effective != target_ip:
        raise RuntimeError(f"ssh effective hostname {effective!r} != {target_ip!r}")
    return effective


def probe_remote_host(orch_master, spec, expected_ip):
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
        "    'ssh_connection': os.environ.get('SSH_CONNECTION', ''),\n"
        "    'platform': platform.platform(),\n"
        "    'load_average': list(os.getloadavg()),\n"
        "    'availability': avail,\n"
        "}))\n"
    )
    shell = "zsh" if spec.get("use_mac_queue") else "sh"
    remote_cmd = f"{shell} -lc {shlex.quote('python3 -c ' + shlex.quote(probe_script))}"
    result = orch_master.run(remote_cmd, timeout=30)
    if result.returncode != 0:
        raise RuntimeError(f"SSH probe failed on {spec['ssh_alias']}: {result.stderr or result.stdout}")
    info = json.loads(result.stdout.strip().splitlines()[-1])
    if info.get("tailscale_ip") != expected_ip:
        raise RuntimeError(f"remote tailscale ip {info.get('tailscale_ip')!r} != {expected_ip!r}")
    conn = parse_ssh_connection(info.get("ssh_connection"))
    if not conn or conn["server_ip"] != expected_ip:
        raise RuntimeError(f"orchestration SSH did not land on {expected_ip!r}: {info.get('ssh_connection')!r}")
    return info


def prepare_pc_client(pc_binary, client_root, owner_token):
    env, _, config_dir = isolated_env(client_root, {OWNER_TOKEN_ENV: owner_token})
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


def time_warm_ssh(ssh_master, remote_cmd, expected_inodes):
    if ssh_master.proc.poll() is not None:
        raise RuntimeError(f"SSH master exited before sample ({ssh_master.proc.returncode})")
    started = time.perf_counter_ns()
    result = subprocess.run(
        ssh_master.client_argv() + [remote_cmd],
        env=ssh_master.env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=HANG_CAP,
    )
    elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    if result.returncode != 0:
        raise RuntimeError(f"warm ssh command failed ({result.returncode}): {result.stderr.decode(errors='replace')}")
    if ssh_master.proc.poll() is not None:
        raise RuntimeError("SSH master exited during sample")
    current_inodes = linux_socket_inodes(ssh_master.master_pid)
    if expected_inodes and not set(expected_inodes).issubset(set(current_inodes)):
        raise RuntimeError(f"SSH master socket inodes changed: before={expected_inodes}, after={current_inodes}")
    return {"seconds": elapsed, "exit": 0, "master_pid": ssh_master.master_pid}


def verify_ssh_master_route(ssh_master, expected_server_ip):
    res = ssh_master.run('printf "%s\\n" "$SSH_CONNECTION"', timeout=15)
    if res.returncode != 0:
        raise RuntimeError(f"SSH master route check failed: {res.stderr}")
    conn = parse_ssh_connection(res.stdout)
    if not conn or conn["server_ip"] != expected_server_ip:
        raise RuntimeError(f"SSH master routed to {conn}, want server_ip={expected_server_ip}")
    return conn


def start_local_openssh(ssh_root, helpers, stack, pc_registry):
    sshd = shutil.which("sshd")
    if not sshd:
        return None, "sshd executable not found"
    if not shutil.which("ssh") or not shutil.which("ssh-keygen"):
        return None, "ssh or ssh-keygen not found"
    ssh_root.mkdir(parents=True, exist_ok=True)
    ssh_root.chmod(0o700)
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
        return None, f"ssh-keygen failed: {keygen.stderr.strip()}"
    env = {"PATH": os.environ.get("PATH", os.defpath), "HOME": str(ssh_root), "LC_ALL": "C"}
    fixture = SimpleNamespace(root=ssh_root, local=state_dir, environment=env)
    try:
        base_ssh, port = helpers.ssh_command.start_openssh(fixture, sshd, stack)
        record_sshd_pid(ssh_root / "sshd.pid", pc_registry)
        cleaned_base = strip_control_options(base_ssh[:-1])
        target = base_ssh[-1]
        master = SSHMaster(cleaned_base, target, ssh_root / "cm" / "c", env, registry=pc_registry)
        stack.callback(master.close)
        master.start()
        return master, f"owned sshd on 127.0.0.2:{port}"
    except Exception as error:
        return None, str(error)


def record_sshd_pid(pid_file, registry):
    try:
        pid = int(pid_file.read_text().strip())
    except (OSError, ValueError):
        return
    registry.record(pid)


def strip_control_options(argv):
    cleaned = []
    idx = 0
    while idx < len(argv):
        if argv[idx] == "-o" and idx + 1 < len(argv) and argv[idx + 1].startswith("Control"):
            idx += 2
            continue
        if argv[idx] == "-T":
            idx += 1
            continue
        cleaned.append(argv[idx])
        idx += 1
    return cleaned


def run_paired_samples(
    mcp_client, ctx_handle, true_path, cwd, ssh_master, expected_inodes, args, helpers, run_id, host_name
):
    ssh_cmd = build_ssh_exec_command(cwd, true_path)
    warmups = collect_phase(
        mcp_client, ctx_handle, true_path, ssh_master, ssh_cmd, expected_inodes,
        args.warmups, args.seed, helpers, f"{run_id}-{host_name}-w",
    )
    measured = collect_phase(
        mcp_client, ctx_handle, true_path, ssh_master, ssh_cmd, expected_inodes,
        args.pairs, args.seed + 1, helpers, f"{run_id}-{host_name}-m",
    )
    return warmups, measured


def collect_one_pair(order, mcp_client, ctx_handle, true_path, ssh_master, ssh_cmd, expected_inodes, key):
    pair = {"order": order}
    for mode in order:
        if mode == "mesh":
            pair["mesh"] = time_mcp_exec(mcp_client, ctx_handle, true_path, key)
        else:
            pair["ssh"] = time_warm_ssh(ssh_master, ssh_cmd, expected_inodes)
    return pair


def collect_phase(
    mcp_client, ctx_handle, true_path, ssh_master, ssh_cmd, expected_inodes, count, seed, helpers, prefix
):
    if ssh_master is None:
        return [
            {"order": ["mesh"], "mesh": time_mcp_exec(mcp_client, ctx_handle, true_path, f"{prefix}-{index}")}
            for index in range(count)
        ]
    return [
        collect_one_pair(order, mcp_client, ctx_handle, true_path, ssh_master, ssh_cmd, expected_inodes, f"{prefix}-{index}")
        for index, order in enumerate(helpers.ssh_command.orders(count, seed))
    ]


def collect_mux_proof(ssh_master, expected_ip):
    check_before = ssh_master.check()
    if check_before != ssh_master.master_pid:
        raise RuntimeError(f"SSH master -O check before failed: got {check_before}, want {ssh_master.master_pid}")
    conn = verify_ssh_master_route(ssh_master, expected_ip)
    inodes_before = linux_socket_inodes(ssh_master.master_pid)
    return {
        "master_pid": ssh_master.master_pid,
        "control_path": str(ssh_master.control_path),
        "proxy_command_guard": "false",
        "ssh_connection": conn,
        "check_before": True,
        "tcp_socket_inodes_before": inodes_before,
    }, inodes_before


def finalize_mux_proof(ssh_master, mux_proof):
    check_after = ssh_master.check()
    if check_after != ssh_master.master_pid:
        raise RuntimeError(f"SSH master -O check after failed: got {check_after}, want {ssh_master.master_pid}")
    mux_proof["check_after"] = True
    mux_proof["tcp_socket_inodes_after"] = linux_socket_inodes(ssh_master.master_pid)


def benchmark_local(pc_binary, client_env, client_config, client_id, pc_scratch, pc_registry, owner_token, args, helpers, run_id):
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
        owner_token=owner_token,
    )
    with ExitStack() as stack:
        stack.callback(daemon.stop)
        ready = daemon.start()
        write_hosts_json(client_config, ready, "local")
        ssh_master, ssh_note = start_local_openssh(pc_scratch / "local-ssh", helpers, stack, pc_registry)
        mux_proof, expected_inodes = (collect_mux_proof(ssh_master, "127.0.0.2") if ssh_master else (None, []))
        mcp_client = helpers.mcp_native.MCPClient(str(pc_binary), client_env, args=("--identity", "bench"))
        pc_registry.record(mcp_client.proc.pid)
        stack.callback(close_mcp_client, mcp_client)
        ctx_handle = open_mcp_context(mcp_client, ready["host_id"], ready["cwd"], run_id, "local")
        warmups, measured = run_paired_samples(
            mcp_client, ctx_handle, ready["true_path"], ready["cwd"],
            ssh_master, expected_inodes, args, helpers, run_id, "local",
        )
        if ssh_master is not None:
            finalize_mux_proof(ssh_master, mux_proof)
        close_mcp_client(mcp_client)
        server_result = daemon.server_bench(args.warmups, args.pairs)
        ssh_available = ssh_master is not None
        summary = summarize_host(measured, server_result["samples"], ssh_available, helpers.ssh_command.summarize)
        return {
            "status": "ok",
            "host": "local",
            "platform": "linux/amd64",
            "target_ip": "127.0.0.1",
            "ssh_target_ip": "127.0.0.2" if ssh_available else None,
            "network_matched": True,
            "daemon_port": ready["port"],
            "cwd": ready["cwd"],
            "true_path": ready["true_path"],
            "load_average": list(os.getloadavg()),
            "rtt": {"available": False, "samples_ms": [], "reason": "local loopback"},
            "ssh": {
                "available": ssh_available,
                "alias": "owned-sshd",
                "detail": ssh_note,
                "command": build_ssh_exec_command(ready["cwd"], ready["true_path"]),
                "mux_proof": mux_proof,
                **({} if ssh_available else {"reason": ssh_note}),
            },
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


def build_remote_launcher_command(host_name, spec, probe, remote_scratch, client_id, owner_token, args):
    wrapper_argv = [
        "/bin/sh", "-c", REMOTE_STAGE_WRAPPER, "oneshot-stage",
        remote_scratch,
        probe["python3"],
        host_name,
        probe["tailscale_ip"],
        client_id,
        str(spec["startup_timeout"]),
        spec["true_path"],
        str(args.warmups),
        str(args.pairs),
        owner_token,
    ]
    if spec.get("use_mac_queue"):
        queue_cmd = build_mac_queue_command(
            wrapper_argv,
            lane=args.mac_lane,
            thread=args.mac_thread,
            expected_seconds=MAC_QUEUE_EXPECTED_SECONDS,
            wait_seconds=MAC_QUEUE_WAIT_SECONDS,
            timeout_seconds=MAC_QUEUE_TIMEOUT_SECONDS,
        )
        return f"zsh -lc {shlex.quote(queue_cmd)}"
    return " ".join(shlex.quote(part) for part in wrapper_argv)


def stage_files_into_remote(orch_master, target_ip, binary_path, remote_scratch):
    for src, name in ((binary_path, "mesh"), (Path(__file__).resolve(), "oneshot.py")):
        cp = orch_master.scp_upload(src, f"{remote_scratch}/{name}", host_ip=target_ip, timeout=120)
        if cp.returncode != 0:
            raise RuntimeError(f"scp {name} to {orch_master.target} failed: {cp.stderr or cp.stdout}")
    finish_stage = orch_master.run(
        f"chmod +x {shlex.quote(remote_scratch + '/mesh')} {shlex.quote(remote_scratch + '/oneshot.py')} "
        f"&& touch {shlex.quote(remote_scratch + '/stage.done')}",
        timeout=30,
    )
    if finish_stage.returncode != 0:
        raise RuntimeError(f"failed to finalize staging on {orch_master.target}: {finish_stage.stderr}")


def remote_recovery_cleanup(orch_master, spec, remote_python, remote_scratch, owner_token, args):
    script = (
        f"if [ -f {shlex.quote(remote_scratch + '/oneshot.py')} ]; then "
        f"{shlex.quote(remote_python)} {shlex.quote(remote_scratch + '/oneshot.py')} "
        f"--cleanup-scratch --scratch {shlex.quote(remote_scratch)} --owner-token {shlex.quote(owner_token)}; "
        f"elif [ -d {shlex.quote(remote_scratch)} ]; then "
        f"rm -rf -- {shlex.quote(remote_scratch)} && "
        f"printf '%s\\n' '{{\"clean\":true,\"remaining_pids\":[],\"scratch_removed\":true,\"remote_scratch\":\"'\"$1\"'\"}}'; "
        f"else "
        f"printf '%s\\n' '{{\"clean\":true,\"remaining_pids\":[],\"scratch_removed\":true,\"remote_scratch\":\"'\"$1\"'\"}}'; "
        f"fi"
    )
    shell_argv = ["/bin/sh", "-c", script, "oneshot-recover", remote_scratch]
    if spec.get("use_mac_queue"):
        queue_cmd = build_mac_queue_command(
            shell_argv,
            lane=args.mac_lane,
            thread=args.mac_thread,
            expected_seconds=60,
            wait_seconds=MAC_QUEUE_WAIT_SECONDS,
            timeout_seconds=120,
        )
        cmd = f"zsh -lc {shlex.quote(queue_cmd)}"
        timeout = MAC_QUEUE_WAIT_SECONDS + 150
    else:
        cmd = " ".join(shlex.quote(part) for part in shell_argv)
        timeout = 60
    result = orch_master.run(cmd, timeout=timeout)
    if result.returncode != 0:
        return {
            "clean": False,
            "error": (result.stderr or result.stdout).strip()[:1000],
            "remote_scratch": remote_scratch,
        }
    return json.loads(result.stdout.strip().splitlines()[-1])


def benchmark_remote(
    host_name, binaries, client_env, client_config, client_id, pc_scratch, pc_registry, owner_token, args, helpers, run_id, cleanup_report
):
    spec = HOST_SPECS[host_name]
    target_ip = resolve_tailscale_ip_local(spec["tailscale_dns_prefix"])
    verify_ssh_effective_hostname(spec["ssh_alias"], target_ip)
    rtt = measure_tailscale_rtt(target_ip)
    cleanup_report[host_name] = {"clean": False, "reason": "not yet verified"}
    with ExitStack() as stack:
        orch_master = SSHMaster(
            remote_ssh_base_argv(target_ip),
            spec["ssh_alias"],
            pc_scratch / f"orch-{host_name}" / "c",
            os.environ,
            registry=pc_registry,
        )
        stack.callback(orch_master.close)
        orch_master.start()
        probe = probe_remote_host(orch_master, spec, target_ip)
        remote_scratch = f"{probe['home']}/mesh-oneshot-bench-{run_id}-{host_name}"
        return run_remote_session(
            host_name, spec, target_ip, orch_master, probe, rtt, remote_scratch,
            binaries, client_env, client_config, client_id, pc_scratch, pc_registry, owner_token,
            args, helpers, run_id, cleanup_report,
        )


def run_remote_session(
    host_name, spec, target_ip, orch_master, probe, rtt, remote_scratch,
    binaries, client_env, client_config, client_id, pc_scratch, pc_registry, owner_token,
    args, helpers, run_id, cleanup_report,
):
    remote_proc = None
    marker_stream = None
    stderr_lines = []
    stopped_payload = None
    try:
        launcher_cmd = build_remote_launcher_command(host_name, spec, probe, remote_scratch, client_id, owner_token, args)
        remote_proc = orch_master.popen(launcher_cmd)
        pc_registry.record(remote_proc.pid)
        stderr_lines, _ = start_stderr_drain(remote_proc.stderr)
        marker_stream = MarkerStream(remote_proc.stdout)

        def detail_fn():
            return f"rc={remote_proc.poll()} stderr={''.join(stderr_lines)[-2000:]}"

        stage_timeout = (MAC_QUEUE_WAIT_SECONDS + 30) if spec.get("use_mac_queue") else 30
        marker_stream.read_marker(STAGE_READY_PREFIX, stage_timeout, detail_fn)
        stage_files_into_remote(orch_master, target_ip, binaries[spec["platform"]], remote_scratch)
        ready = marker_stream.read_marker(READY_PREFIX, spec["startup_timeout"] + 30, detail_fn)
        result, stopped_payload = execute_remote_measurements(
            host_name, spec, target_ip, orch_master, probe, rtt, remote_scratch,
            remote_proc, marker_stream, detail_fn, ready, binaries["linux/amd64"],
            client_env, client_config, pc_scratch, pc_registry, args, helpers, run_id,
        )
        return result
    finally:
        with helpers.isolation.protected_cleanup():
            teardown_remote(
                host_name, spec, orch_master, probe["python3"], remote_scratch,
                remote_proc, marker_stream, stopped_payload, owner_token, args, cleanup_report,
            )


def execute_remote_measurements(
    host_name, spec, target_ip, orch_master, probe, rtt, remote_scratch,
    remote_proc, marker_stream, detail_fn, ready, pc_binary,
    client_env, client_config, pc_scratch, pc_registry, args, helpers, run_id,
):
    with ExitStack() as stack:
        write_hosts_json(client_config, ready, host_name)
        ssh_master = SSHMaster(
            remote_ssh_base_argv(target_ip),
            spec["ssh_alias"],
            pc_scratch / f"ssh-{host_name}" / "c",
            os.environ,
            registry=pc_registry,
        )
        stack.callback(ssh_master.close)
        ssh_master.start()
        mux_proof, expected_inodes = collect_mux_proof(ssh_master, target_ip)
        mcp_client = helpers.mcp_native.MCPClient(str(pc_binary), client_env, args=("--identity", "bench"))
        pc_registry.record(mcp_client.proc.pid)
        stack.callback(close_mcp_client, mcp_client)
        ctx_handle = open_mcp_context(mcp_client, ready["host_id"], ready["cwd"], run_id, host_name)
        warmups, measured = run_paired_samples(
            mcp_client, ctx_handle, ready["true_path"], ready["cwd"],
            ssh_master, expected_inodes, args, helpers, run_id, host_name,
        )
        finalize_mux_proof(ssh_master, mux_proof)
        close_mcp_client(mcp_client)
    trigger = orch_master.run(f"touch {shlex.quote(remote_scratch + '/run-server-bench')}", timeout=20)
    if trigger.returncode != 0:
        raise RuntimeError(f"failed to trigger server benchmark on {host_name}: {trigger.stderr}")
    server_result = marker_stream.read_marker(SERVER_BENCH_PREFIX, 180, detail_fn)
    stopped_payload = marker_stream.read_marker(STOPPED_PREFIX, 45, detail_fn)
    remote_proc.wait(timeout=30)
    summary = summarize_host(measured, server_result["samples"], True, helpers.ssh_command.summarize)
    result = {
        "status": "ok",
        "host": host_name,
        "platform": spec["platform"],
        "target_ip": ready["ip"],
        "ssh_target_ip": target_ip,
        "network_matched": ready["ip"] == target_ip,
        "daemon_port": ready["port"],
        "cwd": ready["cwd"],
        "true_path": ready["true_path"],
        "remote_platform": probe["platform"],
        "load_average": probe["load_average"],
        "availability": probe.get("availability"),
        "rtt": rtt,
        "ssh": {
            "available": True,
            "alias": spec["ssh_alias"],
            "effective_hostname": target_ip,
            "command": build_ssh_exec_command(ready["cwd"], ready["true_path"]),
            "mux_proof": mux_proof,
        },
        "warmups": warmups,
        "pairs": measured,
        "server_warmups": server_result["warmups"],
        "server_samples": server_result["samples"],
        "summary": summary,
    }
    return result, stopped_payload


def teardown_remote(
    host_name, spec, orch_master, remote_python, remote_scratch,
    remote_proc, marker_stream, stopped_payload, owner_token, args, cleanup_report,
):
    try:
        if remote_proc is not None and remote_proc.poll() is None:
            stopped_payload = request_remote_stop(orch_master, remote_scratch, remote_proc, marker_stream)
        if isinstance(stopped_payload, dict) and stopped_payload.get("clean") is True:
            cleanup_report[host_name] = stopped_payload
            return
        cleanup_report[host_name] = remote_recovery_cleanup(
            orch_master, spec, remote_python, remote_scratch, owner_token, args
        )
    except Exception as error:
        cleanup_report[host_name] = {"clean": False, "error": str(error), "remote_scratch": remote_scratch}


def request_remote_stop(orch_master, remote_scratch, remote_proc, marker_stream):
    try:
        orch_master.run(f"touch {shlex.quote(remote_scratch + '/stop')}", timeout=10)
    except Exception:
        pass
    try:
        payload = marker_stream.read_marker(STOPPED_PREFIX, 20) if marker_stream else None
        remote_proc.wait(timeout=15)
        return payload
    except Exception:
        stop_popen(remote_proc, grace=5.0)
        return None


@contextmanager
def signal_guard(helpers):
    with helpers.isolation.termination_cleanup():
        yield


def run_coordinator(args):
    helpers = load_coordinator_helpers()
    enable_linux_subreaper()
    run_id = uuid.uuid4().hex[:8]
    owner_token = f"oneshot-{run_id}-{uuid.uuid4().hex[:12]}"
    pc_scratch = None
    pc_registry = None
    report = {
        "schema": 2,
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
            "orchestration_transport": "OpenSSH ControlMaster + scp (no canonical Mesh state or daemon)",
            "server_phase_method": "two fresh Unix-socket RPCs (execution.create then execution.wait, even if create returns terminal)",
            "mcp_phase_method": "black-box mesh_exec call including initial output retrieval",
        },
        "binaries": {},
        "hosts": {},
        "cleanup": {},
    }
    try:
        with signal_guard(helpers):
            pc_scratch = Path(tempfile.mkdtemp(prefix="mesh-oneshot-bench-", dir=str(args.scratch_parent))).resolve()
            pc_scratch.chmod(0o700)
            pc_registry = OwnedRegistry(pc_scratch / "owned-pids.jsonl")
            report["scratch_root"] = str(pc_scratch)
            execute_fleet_run(args, helpers, run_id, owner_token, pc_scratch, pc_registry, report)
    except (Exception, SystemExit) as error:
        report["status"] = "error"
        report["error"] = str(error)
    finally:
        with helpers.isolation.protected_cleanup():
            cleanup_pc(pc_scratch, pc_registry, owner_token, report)
            apply_cleanup_gate(args, report)
    report["environment"]["load_average_end"] = list(os.getloadavg())
    write_outputs(args, report)
    return 0 if report["status"] == "ok" else 1


def apply_cleanup_gate(args, report):
    required = ["pc"] + [h for h in args.hosts if h != "local" and h in report["cleanup"]]
    all_clean, failed_keys = evaluate_cleanup_status(report["cleanup"], required)
    if all_clean:
        return
    report["status"] = "error"
    msg = f"cleanup failed or unverified for: {', '.join(failed_keys)}"
    report["error"] = f"{report['error']}; {msg}" if report.get("error") else msg


def run_one_host(
    host_name, binaries, client_env, client_config, client_id, pc_scratch, pc_registry, owner_token, args, helpers, run_id, cleanup_report
):
    if host_name == "local":
        return benchmark_local(
            binaries["linux/amd64"], client_env, client_config, client_id,
            pc_scratch, pc_registry, owner_token, args, helpers, run_id,
        )
    return benchmark_remote(
        host_name, binaries, client_env, client_config, client_id,
        pc_scratch, pc_registry, owner_token, args, helpers, run_id, cleanup_report,
    )


def execute_fleet_run(args, helpers, run_id, owner_token, pc_scratch, pc_registry, report):
    binaries = resolve_binaries(args, pc_scratch)
    for plat, path in binaries.items():
        report["binaries"][plat] = {"path": str(path), "sha256": sha256_file(path)}
    pc_binary = binaries["linux/amd64"]
    ver = subprocess.run([str(pc_binary), "version"], capture_output=True, text=True, timeout=15)
    report["environment"]["binary_version"] = ver.stdout.strip()
    client_env, client_config, client_id = prepare_pc_client(pc_binary, pc_scratch / "pc-client", owner_token)
    failures = 0
    for host_name in args.hosts:
        try:
            report["hosts"][host_name] = run_one_host(
                host_name, binaries, client_env, client_config, client_id,
                pc_scratch, pc_registry, owner_token, args, helpers, run_id, report["cleanup"],
            )
        except Exception as error:
            failures += 1
            report["hosts"][host_name] = {"status": "error", "host": host_name, "error": str(error)}
    report["status"] = "ok" if failures == 0 else "error"
    if failures:
        report["error"] = f"{failures} host(s) failed"


def cleanup_pc(pc_scratch, pc_registry, owner_token, report):
    if pc_scratch is None:
        report["cleanup"]["pc"] = {"clean": True, "remaining_pids": [], "scratch_removed": True}
        return
    try:
        remaining = cleanup_owned_tree(
            pc_scratch,
            registry=pc_registry,
            state_dir=pc_scratch / "local-daemon" / "state",
            owner_token=owner_token,
        )
        removed = remove_scratch_if_clean(pc_scratch, remaining)
        report["cleanup"]["pc"] = {
            "clean": not remaining and removed,
            "remaining_pids": remaining,
            "scratch_removed": removed,
            "scratch_root": str(pc_scratch),
        }
    except Exception as error:
        report["cleanup"]["pc"] = {
            "clean": False,
            "error": str(error),
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

    # Internal flags used on remote hosts (agent and cleanup).
    parser.add_argument("--remote-agent", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--cleanup-scratch", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--self-clean", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--scratch", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--binary", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--host-name", help=argparse.SUPPRESS)
    parser.add_argument("--bind-ip", help=argparse.SUPPRESS)
    parser.add_argument("--client-id", help=argparse.SUPPRESS)
    parser.add_argument("--owner-token", default="", help=argparse.SUPPRESS)
    parser.add_argument("--startup-timeout", type=int, default=30, help=argparse.SUPPRESS)
    parser.add_argument("--true-path", default="/bin/true", help=argparse.SUPPRESS)
    parser.add_argument("--server-warmups", type=int, default=DEFAULT_WARMUPS, help=argparse.SUPPRESS)
    parser.add_argument("--server-count", type=int, default=DEFAULT_PAIRS, help=argparse.SUPPRESS)

    args = parser.parse_args(argv)
    if args.remote_agent or args.cleanup_scratch:
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
    if args.cleanup_scratch:
        return run_cleanup_scratch(args)
    if args.remote_agent:
        return run_remote_agent(args)
    return run_coordinator(args)


if __name__ == "__main__":
    raise SystemExit(main())
