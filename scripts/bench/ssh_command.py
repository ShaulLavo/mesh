#!/usr/bin/env python3
"""Compare fresh ordinary Mesh SSH commands with fresh OpenSSH commands."""

import argparse
from contextlib import contextmanager
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import pwd
import random
import shlex
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
HELPERS = Path(__file__).resolve().parents[2] / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))

from isolation import TERMINATION_SIGNALS, protected_cleanup, termination_cleanup  # noqa: E402 - shared integration helpers
from mesh_control import round_trip  # noqa: E402
from process_tree import ECHILD, Kernel, Reaper, Tree  # noqa: E402
from ssh_sessions import SSHFixture  # noqa: E402

PAYLOAD = bytes(range(256)) * 256
STDERR = b"mesh-ssh-benchmark-stderr\n"
COMMAND = ["/bin/sh", "-c", "/bin/cat; printf 'mesh-ssh-benchmark-stderr\\n' >&2"]
RATIO = 1.5
DELTA_SECONDS = 0.050
MIN_FRACTION = 0.8
SIGN_P = 0.01
TIMEOUT = 120  # Hang cap, never a latency target.


def distribution(values):
    ordered = sorted(values)
    return {"min": ordered[0], "median": statistics.median(ordered),
            "p90": ordered[math.ceil(len(ordered) * 0.9) - 1], "max": ordered[-1]}


def summarize(pairs):
    ratios = [pair["mesh"]["seconds"] / pair["ssh"]["seconds"] for pair in pairs]
    deltas = [pair["mesh"]["seconds"] - pair["ssh"]["seconds"] for pair in pairs]
    exceeding = sum(ratio > RATIO and delta > DELTA_SECONDS for ratio, delta in zip(ratios, deltas))
    count = len(pairs)
    tail = sum(math.comb(count, k) for k in range(exceeding, count + 1)) / 2 ** count
    return {"mesh_seconds": distribution([pair["mesh"]["seconds"] for pair in pairs]),
            "ssh_seconds": distribution([pair["ssh"]["seconds"] for pair in pairs]),
            "paired_ratio": distribution(ratios), "paired_delta_seconds": distribution(deltas),
            "pairs_exceeding_both_margins": exceeding, "one_sided_sign_p": tail,
            "clear_slowdown": exceeding >= math.ceil(count * MIN_FRACTION) and tail < SIGN_P}


def orders(count, seed):
    result = [index % 2 == 0 for index in range(count)]
    random.Random(seed).shuffle(result)
    return [["mesh", "ssh"] if mesh_first else ["ssh", "mesh"] for mesh_first in result]


def checked_run(argv, environment, *, payload=b"", timeout=TIMEOUT):
    process = subprocess.Popen(argv, env=environment, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        stdout, stderr = process.communicate(payload, timeout=timeout)
    except BaseException:
        process.kill()
        process.communicate(timeout=5)
        raise
    return subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)


def sample(argv, environment):
    started = time.perf_counter_ns()
    result = checked_run(argv, environment, payload=PAYLOAD)
    elapsed = (time.perf_counter_ns() - started) / 1_000_000_000
    if (result.returncode, result.stdout, result.stderr) != (0, PAYLOAD, STDERR):
        raise RuntimeError(f"command output/status mismatch: exit={result.returncode}, "
                           f"stdout bytes={len(result.stdout)}, stderr={result.stderr[:1000]!r}")
    return {"seconds": elapsed, "exit": result.returncode, "stdout_bytes": len(result.stdout),
            "stdout_sha256": hashlib.sha256(result.stdout).hexdigest(),
            "stderr_sha256": hashlib.sha256(result.stderr).hexdigest()}


def collect(commands, environment, count, seed, pairs):
    for order in orders(count, seed):
        pair = {"order": order}
        pairs.append(pair)
        for name in order:
            pair[name] = sample(commands[name], environment)


def unused_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.2", 0))
        return listener.getsockname()[1]


def wait_listener(process, port, log):
    deadline = time.monotonic() + TIMEOUT
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"OpenSSH fixture exited: {log.read_text()}")
        if listening(port):
            return
        time.sleep(0.05)
    raise TimeoutError(f"OpenSSH fixture did not listen: {log.read_text()}")


def listening(port):
    try:
        with socket.create_connection(("127.0.0.2", port), timeout=0.2):
            return True
    except OSError:
        return False


def openssh_options(port, key, known_hosts):
    options = {"BatchMode": "yes", "IdentitiesOnly": "yes", "IdentityAgent": "none",
               "ControlMaster": "no", "ControlPath": "none", "ControlPersist": "no",
               "StrictHostKeyChecking": "yes", "UserKnownHostsFile": str(known_hosts),
               "GlobalKnownHostsFile": "/dev/null", "UpdateHostKeys": "no",
               "PreferredAuthentications": "publickey", "ConnectTimeout": str(TIMEOUT),
               "LogLevel": "ERROR"}
    argv = [shutil.which("ssh"), "-F", "/dev/null", "-T", "-p", str(port), "-i", str(key)]
    for name, value in options.items():
        argv.extend(["-o", f"{name}={value}"])
    return argv + [f"{pwd.getpwuid(os.getuid()).pw_name}@127.0.0.2"]


def start_openssh(fixture, sshd, stack):
    port = unused_port()
    key = fixture.local / "identity.key"
    exported = checked_run([shutil.which("ssh-keygen"), "-y", "-f", str(key)], fixture.environment)
    exported.check_returncode()
    authorized = fixture.root / "openssh-authorized-keys"
    authorized.write_bytes(exported.stdout)
    authorized.chmod(0o600)
    known = fixture.root / "known_hosts"
    known.write_bytes(f"[127.0.0.2]:{port} ".encode() + exported.stdout)
    config = fixture.root / "sshd_config"
    config.write_text(f"""ListenAddress 127.0.0.2
Port {port}
HostKey "{key}"
PidFile "{fixture.root / 'sshd.pid'}"
AuthorizedKeysFile "{authorized}"
StrictModes yes
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
UseDNS no
PermitRootLogin prohibit-password
PrintMotd no
LogLevel INFO
""")
    preflight = checked_run([sshd, "-t", "-f", str(config)], fixture.environment)
    if preflight.returncode:
        raise RuntimeError(f"owned OpenSSH prerequisite failed: {preflight.stderr.decode(errors='replace')}")
    log = fixture.root / "sshd.log"
    stream = stack.enter_context(log.open("wb"))
    process = subprocess.Popen([sshd, "-D", "-e", "-f", str(config)], env=fixture.environment,
                               stdin=subprocess.DEVNULL, stdout=stream, stderr=stream)
    stack.callback(stop_process, process)
    wait_listener(process, port, log)
    return openssh_options(port, key, known), port


def stop_process(process):
    if process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=3)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


class Descendants:
    """Reuse the integration owner's handle-based discovery and descendant join."""

    def __init__(self):
        self.kernel = Kernel()
        self.kernel.require()
        if self.kernel.children(os.getpid()):
            raise RuntimeError("benchmark needs its own process with no pre-existing children")
        self.kernel.enable_subreaper()
        self.kernel.default_sigchld()
        self.tree = Tree(self.kernel, os.getpid())
        self.reaper = Reaper(self.kernel)

    def close(self):
        deadline = time.monotonic() + 15
        gentle_until = time.monotonic() + 3
        while time.monotonic() < deadline:
            tick = min(deadline, time.monotonic() + 0.05)
            self.tree.discover(tick, time.monotonic)
            sig = signal.SIGTERM if time.monotonic() < gentle_until else signal.SIGKILL
            self.tree.deliver(sig, "ssh-command-benchmark-cleanup", lambda _pid, _start: True,
                              tick + 0.05, time.monotonic)
            if self.reaper.reap(tick + 0.1) == ECHILD:
                self.tree.close()
                return
            time.sleep(0.02)
        self.tree.close()
        raise RuntimeError("benchmark descendants did not join; scratch retained")


def prepare_mesh(fixture):
    fixture.start_ssh()
    host = round_trip(str(fixture.local / "daemon.sock"), {"type": "host.info", "requestId": "benchmark"})["host"]
    approved = checked_run([fixture.binary, "device", "approve", "--allow-root", "--", host["id"]],
                           fixture.environment)
    approved.check_returncode()
    (fixture.config / "hosts.json").write_text(json.dumps({"version": 1, "hosts": [{
        "id": host["id"], "meshIdentity": host["meshIdentity"], "addresses": ["127.0.0.2"],
        "endpoint": "ws://127.0.0.2:7777/mesh"}]}))
    return [fixture.binary, "ssh", "--ssh-port", str(fixture.port), host["id"], "--"] + COMMAND


def fingerprint(path):
    with Path(path).open("rb") as stream:
        return {"path": str(path), "sha256": hashlib.file_digest(stream, "sha256").hexdigest()}


def environment_record(args):
    ssh_version = subprocess.run([shutil.which("ssh"), "-V"], capture_output=True, timeout=TIMEOUT)
    sshd_version = subprocess.run([args.sshd, "-V"], capture_output=True, timeout=TIMEOUT)
    return {"utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "platform": platform.platform(), "python": sys.version, "cpu_count": os.cpu_count(),
            "load_average": os.getloadavg(), "uid": os.getuid(), "target": "127.0.0.2, same host and uid",
            "mesh_binary": fingerprint(args.binary), "runner": fingerprint(Path(__file__).resolve()),
            "ssh": ssh_version.stderr.decode().strip(),
            "sshd": (sshd_version.stdout + sshd_version.stderr).decode().strip(),
            "scratch_filesystem": subprocess.check_output(["df", "-T", str(args.scratch_parent)], text=True),
            "transport": "ordinary mesh ssh command mode vs OpenSSH ssh to owned OpenSSH sshd",
            "connection_policy": "fresh process, TCP connection and public-key authentication per sample; no PTY or multiplexing",
            "host_verification": "Mesh pinned identity; OpenSSH pinned known_hosts; same Ed25519 key",
            "command": COMMAND, "stdin_bytes": len(PAYLOAD), "seed": args.seed,
            "threshold": {"ratio_strictly_over": RATIO, "delta_seconds_strictly_over": DELTA_SECONDS,
                          "minimum_exceeding_fraction": MIN_FRACTION, "sign_p_strictly_below": SIGN_P}}


def exercise(args, report, root):
    from contextlib import ExitStack

    fixture = SSHFixture(args.binary, root)
    # The parent owns descendant cleanup before this function starts either daemon.
    with ExitStack() as stack:
        stack.callback(fixture.stop_daemon)
        mesh = prepare_mesh(fixture)
        ssh, port = start_openssh(fixture, args.sshd, stack)
        commands = {"mesh": mesh, "ssh": ssh + [shlex.join(COMMAND)]}
        report["endpoints"] = {"mesh_port": fixture.port, "openssh_port": port}
        report["argv"] = commands
        before = fixture.daemon_listing()
        report["warmups"], report["pairs"] = [], []
        collect(commands, fixture.environment, 4, args.seed, report["warmups"])
        collect(commands, fixture.environment, args.pairs, args.seed + 1, report["pairs"])
        if fixture.daemon_listing() != before or list((fixture.local / "s").glob("*")):
            raise RuntimeError("ordinary command benchmark created persistent session state")
        report["summary"] = summarize(report["pairs"])
        report["status"] = "slowdown" if report["summary"]["clear_slowdown"] else "pass"


@contextmanager
def allocation_signals():
    # Deliver pending termination after the caller has recorded the new root.
    previous = signal.pthread_sigmask(signal.SIG_BLOCK, TERMINATION_SIGNALS)
    try:
        yield
    finally:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)


def fixture_logs(root):
    return {path.name: path.read_text(errors="replace")[-8000:]
            for path in root.glob("*.log")}


def finish(owner, root, report):
    owner.close()
    report["cleanup"] = {"descendants_joined": True, "scratch_removed": root is None}
    if root is None:
        return
    report["logs"] = fixture_logs(root)
    shutil.rmtree(root)
    report["cleanup"]["scratch_removed"] = not root.exists()


def execute(args, report):
    owner = Descendants()
    root = None
    try:
        with allocation_signals():
            root = Path(tempfile.mkdtemp(prefix="ms-ssh-bench-", dir=args.scratch_parent))
            report["scratch_root"] = str(root)
            report["cleanup"] = {"descendants_joined": False, "scratch_removed": False}
            report["resources"] = "one scratch root, two loopback listeners, owned daemons and bounded command children; no installs or persistent sessions"
        exercise(args, report, root)
    finally:
        with protected_cleanup():
            finish(owner, root, report)


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path, help="existing mesh_integration binary; never built or installed here")
    parser.add_argument("--output", required=True, type=Path, help="JSON result outside the disposable root")
    parser.add_argument("--scratch-parent", required=True, type=Path, help="existing short SSD-backed directory")
    parser.add_argument("--sshd", default=shutil.which("sshd"), help="existing OpenSSH server executable")
    parser.add_argument("--pairs", type=int, default=40)
    parser.add_argument("--seed", type=int, default=435)
    args = parser.parse_args(argv)
    if args.pairs < 20:
        parser.error("--pairs must be at least 20")
    args.binary = args.binary.resolve()
    args.scratch_parent = args.scratch_parent.resolve()
    if len(str(args.scratch_parent)) > 45 or not args.scratch_parent.is_dir():
        parser.error("--scratch-parent needs an existing path of at most 45 characters for Unix sockets")
    if not args.binary.is_file() or not os.access(args.binary, os.X_OK):
        parser.error("--binary needs an existing executable artifact")
    if not args.sshd or not all(shutil.which(tool) for tool in ("ssh", "ssh-keygen", "df", "cat")):
        parser.error("existing sshd, ssh, ssh-keygen, df and cat are required; the runner installs nothing")
    return args


def main(argv=None):
    args = arguments(argv)
    report = {"schema": 1, "benchmark": "mesh-ssh-command-vs-openssh", "status": "error"}
    try:
        with termination_cleanup():
            report["environment"] = environment_record(args)
            execute(args, report)
    except (Exception, SystemExit) as error:
        report["status"] = "error"
        report["error"] = str(error)
    try:
        args.output.write_text(json.dumps(report, indent=2) + "\n")
    except OSError as error:
        print(f"benchmark report could not be written: {error}", file=sys.stderr)
        return 2
    print(json.dumps({"status": report["status"], "output": str(args.output),
                      "summary": report.get("summary"), "error": report.get("error")}))
    return {"pass": 0, "slowdown": 1, "error": 2}[report["status"]]


if __name__ == "__main__":
    raise SystemExit(main())
