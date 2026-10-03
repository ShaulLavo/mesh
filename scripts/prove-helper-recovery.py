#!/usr/bin/env python3
"""Native historical executables; disposable external release/service providers."""
import hashlib
import base64
import http.server
import io
import importlib
import json
import os
import platform
import plistlib
import re
import signal
import shlex
import socket
import socketserver
import sqlite3
import ssl
import struct
import subprocess
import sys
import tarfile
import threading
import time
from pathlib import Path

from fixtures import helper_recovery_paths, published_releases, tls

sys.dont_write_bytecode = True
REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "integration" / "helpers"))
controls = importlib.import_module("mesh_control")
terminals = importlib.import_module("terminal_window")
round_trip = controls.round_trip
PROMPT, Terminal = terminals.PROMPT, terminals.Terminal
eventually, require = terminals.eventually, terminals.require

ROOT, FIXED = (Path(value).resolve() for value in sys.argv[1:3])
ROOT.mkdir(parents=True, exist_ok=True)
NATIVE = {"os": platform.system().lower(), "arch": {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())}
ASSETS, EVENTS = {}, []
HOSTS = {}
LOCK = threading.RLock()
HELPERS = REPO / "integration" / "helpers"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def event(kind, **fields):
    with LOCK:
        row = {"time": time.time(), "kind": kind, **fields}
        EVENTS.append(row)
        with (ROOT / "events.jsonl").open("a") as output:
            output.write(json.dumps(row) + "\n")


def build(binary):
    environment = os.environ | {"HOME": str(ROOT), "MESH_STATE_DIR": str(ROOT / "unused-state"), "MESH_CONFIG_DIR": str(ROOT / "unused-config")}
    return json.loads(subprocess.check_output([str(binary), "version", "--json"], env=environment, timeout=10))


def candidate_descriptor(binary):
    metadata = build(binary)
    artifacts = []
    for target in ({"os": "linux", "arch": "amd64"}, {"os": "linux", "arch": "arm64"}, {"os": "darwin", "arch": "arm64"}):
        payload = binary.read_bytes() if target == NATIVE else b"unexecutable non-native descriptor placeholder"
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
            entry = tarfile.TarInfo("mesh")
            entry.size, entry.mode = len(payload), 0o755
            archive.addfile(entry, io.BytesIO(payload))
        name = "mesh_" + target["os"] + "_" + target["arch"] + ".tar.gz"
        artifacts.append({"platform": target, "archive": name, "sha256": digest(buffer.getvalue()), "binarySha256": digest(payload)})
        ASSETS[f"/ShaulLavo/mesh/releases/download/{metadata['version']}/{name}"] = buffer.getvalue()
    descriptor = {"schema": 1, "version": metadata["version"], "commit": metadata["commit"], "artifacts": artifacts,
        "compatibility": {"stateReadMin": 1, "stateReadMax": metadata["stateVersion"], "stateWrite": metadata["stateVersion"],
            "workerMin": 1, "workerMax": metadata["workerProtocol"], "workerWrite": metadata["workerProtocol"], "journalVersion": 1, "transitions": []}}
    raw = json.dumps(descriptor).encode()
    ASSETS[f"/ShaulLavo/mesh/releases/download/{metadata['version']}/mesh-release.json"] = raw
    (ROOT / "fixed-helper-descriptor.json").write_bytes(raw)
    event("fixed-helper-input", provider="PR-source executable and external fixture descriptor; unpublished", build=metadata)
    return metadata


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class Release(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        payload = ASSETS.get(self.path)
        if payload is None:
            self.send_error(404)
            return
        event("release-input", path=self.path, digest=digest(payload))
        self.send_response(200)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass


class Proxy(http.server.BaseHTTPRequestHandler):
    def do_CONNECT(self):
        require(self.path in ("github.com:443", "api.github.com:443"), "release proxy escaped allowed origins")
        self.send_response(200)
        self.end_headers()
        try:
            with TLS.wrap_socket(self.connection, server_side=True) as connection:
                Release(connection, self.client_address, self.server)
        except (ssl.SSLError, BrokenPipeError, ConnectionResetError):
            pass
        self.close_connection = True

    def log_message(self, *_):
        pass


class Services(socketserver.StreamRequestHandler):
    def handle(self):
        request = json.loads(self.rfile.readline())
        try:
            status, text = HOSTS[request["host"]].service(request["args"])
        except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
            status, text = 1, str(error)
        self.wfile.write(json.dumps({"status": status, "text": text}).encode() + b"\n")


def mapped_image(pid, executable, expected):
    installed = executable.stat()
    image = Path("/proc") / str(pid) / "exe"
    if NATIVE["os"] == "darwin":
        output = subprocess.check_output(["lsof", "-a", "-p", str(pid), "-d", "txt", "-F", "finD"], text=True, timeout=5)
        records, row = [], {}
        for line in output.splitlines():
            if line.startswith("f"):
                records.append(row)
                row = {}
            if line[:1] in ("i", "D", "n"):
                row[line[0]] = line[1:]
        records.append(row)
        matches = [row for row in records if row.get("i") == str(installed.st_ino) and row.get("D")
            and int(row["D"], 0) == installed.st_dev and row.get("n")]
        require(len(matches) == 1, "native mapped image inode was not uniquely identified")
        reported = Path(matches[0]["n"])
        image = executable
        if reported.exists():
            stat = reported.stat()
            if (stat.st_dev, stat.st_ino) == (installed.st_dev, installed.st_ino):
                image = reported
    loaded = image.stat()
    require((loaded.st_dev, loaded.st_ino) == (installed.st_dev, installed.st_ino), "mapped executable inode differs")
    require(digest(image.read_bytes()) == expected, "mapped executable digest differs")
    os.kill(pid, 0)
    event("mapped-image", pid=pid, digest=expected, inode=loaded.st_ino, device=loaded.st_dev, provider="native lsof" if NATIVE["os"] == "darwin" else "native procfs")


class Installation:
    def __init__(self, name, original, proxy, service):
        self.name, self.root = name, ROOT / name
        self.root.mkdir()
        self.home, self.state, self.config = (self.root / item for item in ("home", "state", "config"))
        self.home.mkdir()
        self.config.mkdir()
        self.binary = self.home / ".local" / "bin" / "mesh"
        self.binary.parent.mkdir(parents=True)
        self.binary.write_bytes(original["executable"].read_bytes())
        self.binary.chmod(0o755)
        self.tools = self.home / "tools"
        self.tools.mkdir()
        (self.tools / "tailscale").symlink_to(HELPERS / "fake_tailscale")
        tool = self.tools / ("launchctl" if NATIVE["os"] == "darwin" else "systemctl")
        tool.write_text("#!" + sys.executable + "\n" + '''import json, os, socket, sys
with socket.create_connection(("127.0.0.1", int(os.environ["RECOVERY_SERVICE_PORT"])), timeout=20) as connection:
    connection.sendall(json.dumps({"host": os.environ["RECOVERY_HOST"], "args": sys.argv[1:]}).encode() + b"\\n")
    result = json.loads(connection.makefile("rb").readline())
print(result["text"], end="")
sys.exit(result["status"])
''')
        tool.chmod(0o755)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        tailscale = self.root / "tailscale.json"
        tailscale.write_text(json.dumps({"BackendState": "Running", "Self": {"DNSName": name + ".fixture.test.", "HostName": name, "TailscaleIPs": ["127.0.0.1"], "Online": True}, "Peer": {}}))
        self.environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
        self.environment.update(HOME=str(self.home), MESH_STATE_DIR=str(self.state), MESH_CONFIG_DIR=str(self.config),
            PATH=str(self.tools) + os.pathsep + self.environment.get("PATH", os.defpath), SHELL=str(HELPERS / "window_shell.sh"),
            TERM="xterm-256color", NO_COLOR="1", MESH_FAKE_TAILSCALE_STATUS=str(tailscale), RECOVERY_HOST=name,
            RECOVERY_SERVICE_PORT=str(service), HTTPS_PROXY=f"http://127.0.0.1:{proxy}", HTTP_PROXY=f"http://127.0.0.1:{proxy}",
            NO_PROXY="127.0.0.1,localhost", PYTHONDONTWRITEBYTECODE="1", **tls.certificate_environment(ROOT))
        for variable, directory in (("XDG_CACHE_HOME", "cache"), ("XDG_CONFIG_HOME", "xdg-config"), ("XDG_STATE_HOME", "xdg-state"), ("XDG_RUNTIME_DIR", "runtime")):
            self.environment[variable] = str(self.root / directory)
            Path(self.environment[variable]).mkdir(mode=0o700)
        self.plists, self.processes, self.logs = {}, {}, []
        self.closed = False
        self.departing = {}
        self.delay, self.fail_candidate, self.fail_original, self.helper_failure = 0, False, False, ""
        self.workers = []
        self.terminals = []
        if NATIVE["os"] == "darwin":
            agents = self.home / "Library" / "LaunchAgents"
            agents.mkdir(parents=True)
            path = agents / "dev.shaulavo.mesh.plist"
            template = (REPO / "scripts" / "install" / "assets" / path.name).read_text()
            for token, value in {"@MESH_BINARY@": "${HOME}/.local/bin/mesh", "@MESH_PORT@": str(self.port), "@MESH_SSH_PORT@": "0", "@MESH_WEBSOCKET_PATH@": "/mesh", "@MESH_STDOUT@": str(self.root / "daemon.log"), "@MESH_STDERR@": str(self.root / "daemon.err.log")}.items():
                template = template.replace(token, value)
            require("@MESH_" not in template, "unresolved daemon plist")
            path.write_text(template)
            self.plists["daemon"] = path
        HOSTS[name] = self

    def service(self, args):
        event("service", host=self.name, action=args[1] if args[0] == "--user" else args[0])
        if NATIVE["os"] == "darwin":
            action = args[0]
            if action == "bootstrap":
                require(args[1] == "gui/" + str(os.getuid()), "wrong launchd fixture domain")
                path = Path(args[2])
                require(path.parent == self.home / "Library" / "LaunchAgents", "plist escaped isolated home")
                data = plistlib.loads(path.read_bytes())
                unit = "helper" if data["Label"] == "dev.shaulavo.mesh-update-helper" else "daemon"
                self.plists[unit] = path
                return self.start(unit)
            unit = "helper" if args[-1].endswith("mesh-update-helper") else "daemon"
            if action == "print":
                return self.query(unit)
            if action == "bootout":
                self.stop(unit)
                self.departing[unit] = time.monotonic() + self.delay
                return 0, ""
            require(action == "kickstart" and args[1] == "-k", "unknown launchctl action")
        else:
            require(args[0] == "--user", "systemd fixture requires user scope")
            action = args[1]
            unit = "helper" if "mesh-update-helper.service" in args else "daemon"
            if action == "show":
                if "--property=KillMode" in args:
                    return 0, "process\n"
                require("--property=MainPID" in args and unit == "helper", "unexpected systemd query")
                process = self.processes.get(unit)
                return 0, str(process.pid if process and process.poll() is None else 0) + "\n"
            if action == "daemon-reload":
                return 0, ""
            if action == "stop":
                self.stop(unit)
                return 0, ""
            if action in ("start", "enable"):
                return self.start(unit)
            require(action == "restart", "unknown systemctl action")
        if self.helper_failure == "restart" and unit == "helper":
            self.helper_failure = ""
            return 1, "injected helper restart failure\n"
        self.restart(unit)
        return 0, ""

    def query(self, unit):
        if time.monotonic() < self.departing.get(unit, 0):
            event("delayed-removal", host=self.name, unit=unit)
            return 0, "state = exiting\n"
        process = self.processes.get(unit)
        if process and process.poll() is None:
            return 0, f"state = running\npid = {process.pid}\n"
        return 1, "not loaded\n"

    def start(self, unit):
        with LOCK:
            if self.closed:
                return 1, "fixture closed\n"
            current = self.processes.get(unit)
            if current and current.poll() is None:
                return 0, ""
            if unit == "daemon":
                original = digest(self.binary.read_bytes()) == HISTORICAL[0]["build"]["digest"]
                if (original and self.fail_original) or (not original and self.fail_candidate):
                    return 1, "injected daemon start failure\n"
            if NATIVE["os"] == "darwin":
                data = plistlib.loads(self.plists[unit].read_bytes())
                if unit == "daemon":
                    require(data["AbandonProcessGroup"] is True, "daemon plist can kill original workers")
                command = data["ProgramArguments"]
            elif unit == "daemon":
                command = [str(self.binary), "daemon", "--tailnet-port", str(self.port), "--ssh-port", "0"]
            else:
                receipt = self.receipt()
                lines = Path(receipt["servicePath"]).read_text().splitlines()
                commands = [line.removeprefix("ExecStart=") for line in lines if line.startswith("ExecStart=")]
                require(len(commands) == 1, "helper service requires one production command")
                command = shlex.split(commands[0])
            if unit == "helper":
                receipt = self.receipt()
                launcher = self.state / "update" / "helper" / "current"
                require(launcher.resolve() == Path(receipt["executable"]).resolve(), "helper launcher differs from authoritative receipt")
                require(command == [str(launcher), "update-helper", "--state-dir", str(self.state)], "helper production arguments differ")
            if unit == "helper" and self.helper_failure == "readiness":
                command = [self.prior_receipt["executable"], "update-helper", "--state-dir", str(self.state)]
            log = (self.root / (unit + ".log")).open("ab")
            self.logs.append(log)
            process = subprocess.Popen(command, env=self.environment, cwd=self.root, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
            self.processes[unit] = process
            event("start", host=self.name, unit=unit, pid=process.pid)
            return 0, ""

    def stop(self, unit):
        with LOCK:
            process = self.processes.get(unit)
            if process and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=3)
                event("stop", host=self.name, unit=unit, pid=process.pid)

    def restart(self, unit):
        self.stop(unit)
        self.start(unit)

    def control(self, kind):
        return round_trip(str(self.state / "daemon.sock"), {"type": kind, "requestId": "helper-recovery-proof"})

    def ready(self):
        try:
            return self.control("host.info").get("type") == "host.info.result"
        except (OSError, RuntimeError):
            return False

    def command(self, binary, *args, expected=0):
        result = subprocess.run([str(binary), *args], env=self.environment, cwd=self.root, capture_output=True, timeout=100, check=False)
        event("command", host=self.name, operation=args[0], status=result.returncode)
        if result.returncode:
            (self.root / "command-error.log").write_bytes(result.stderr)
        if result.returncode != expected:
            raise RuntimeError(f"fixture {args[0]} exit {result.returncode}, expected {expected}; see command-error.log")
        return result

    def seed(self):
        self.id = self.control("host.info")["host"]["id"]
        terminal = Terminal([str(self.binary), "local", "--", str(HELPERS / "window_shell.sh")], self.environment, self.root, timeout=8)
        self.terminals.append(terminal)
        terminal.expect(PROMPT)
        self.session = self.control("session.list")["sessions"][0]["id"]
        start = len(terminal.drain())
        terminal.send("printf '__SHELL__%s__END__\\n' \"$$\"\n")
        match = eventually(lambda: re.search(rb"__SHELL__([0-9]+)__END__", terminal.drain()[start:]), "original shell PID missing", timeout=8)
        self.shell = int(match[1])
        self.worker = self.socket_pid(self.state / "s" / self.session / "sock")
        self.worker_inode = self.binary.stat().st_ino
        self.workers.extend((self.worker, self.shell))
        terminal.close()
        self.terminals.remove(terminal)
        key_blob = struct.pack(">I", 11) + b"ssh-ed25519" + struct.pack(">I", 32) + os.urandom(32)
        authorized = self.state / "authorized_keys"
        authorized.touch(mode=0o600, exist_ok=True)
        authorized.write_bytes(b"ssh-ed25519 " + base64.b64encode(key_blob) + b" disposable-recovery-fixture\n")
        self.identity = digest((self.state / "identity.key").read_bytes())
        self.keys = digest((self.state / "authorized_keys").read_bytes())
        self.database = next(self.root.rglob("mesh.db"))
        self.database_inode = self.database.stat().st_ino
        self.check_io()

    def socket_pid(self, path):
        with socket.socket(socket.AF_UNIX) as connection:
            connection.connect(str(path))
            if NATIVE["os"] == "darwin":
                return connection.getsockopt(0, 2)
            return struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]

    def check_retained(self):
        require(self.control("host.info")["host"]["id"] == self.id, "daemon identity changed")
        require(digest((self.state / "identity.key").read_bytes()) == self.identity, "identity key changed")
        require(digest((self.state / "authorized_keys").read_bytes()) == self.keys, "authorized keys changed")
        require(self.database.stat().st_ino == self.database_inode, "database was replaced")
        with sqlite3.connect(self.database) as database:
            require(database.execute("PRAGMA integrity_check").fetchone() == ("ok",), "database integrity changed")
            require(database.execute("SELECT id FROM sessions WHERE id = ?", (self.session,)).fetchone() == (self.session,), "original database session row disappeared")
        require(self.socket_pid(self.state / "s" / self.session / "sock") == self.worker, "worker PID changed")
        meta = json.loads((self.state / "s" / self.session / "meta.json").read_text())
        require(meta["pid"] == self.shell, "shell PID changed")
        for pid in self.workers:
            os.kill(pid, 0)
        candidates = [self.binary, *self.binary.parent.glob(".mesh-update-*.previous")]
        original_image = next((path for path in candidates if path.stat().st_ino == self.worker_inode), None)
        require(original_image is not None, "original worker image inode was not retained")
        mapped_image(self.worker, original_image, HISTORICAL[0]["build"]["digest"])
        require(any(row["id"] == self.session for row in self.control("session.list")["sessions"]), "session disappeared")
        event("preserved", host=self.name, worker=self.worker, shell=self.shell, databaseIntegrity="ok", databaseInode=self.database_inode)

    def check_io(self):
        terminal = Terminal([str(self.binary), "local", "-r"], self.environment, self.root, timeout=8)
        self.terminals.append(terminal)
        try:
            terminal.expect(PROMPT)
            start = len(terminal.drain())
            terminal.send("printf '__IO__%s__END__\\n' \"$$\"\n")
            terminal.expect("__IO__" + str(self.shell) + "__END__", since=start)
        finally:
            terminal.close()
            self.terminals.remove(terminal)
        event("retained-io", host=self.name, worker=self.worker, shell=self.shell)

    def journal(self):
        return json.loads((self.state / "update" / "installation.json").read_text())

    def receipt(self):
        return json.loads((self.state / "update" / "helper" / "installed.json").read_text())

    def prove_daemon(self, expected):
        pid = self.socket_pid(self.state / "daemon.sock")
        require(pid == self.processes["daemon"].pid, "daemon socket owner differs from service process")
        mapped_image(pid, self.binary, expected["digest"])
        observed = self.control("host.info")["host"]["build"]
        require(observed["digest"] == expected["digest"] and observed["version"] == expected["version"], "daemon executing metadata differs")

    def prove_helper(self, expected):
        receipt = self.receipt()
        require(receipt["digest"] == expected["digest"], "helper receipt digest differs")
        process = self.processes["helper"]
        require(process.poll() is None, "helper is stopped")
        mapped_image(process.pid, Path(receipt["executable"]), expected["digest"])

    def recover(self, expected=0):
        journal = self.journal()
        return self.command(FIXED, "update-helper", "recover", "--state-dir", str(self.state), "--replacement", str(FIXED),
            "--version", FIXED_BUILD["version"], "--sha256", FIXED_BUILD["digest"], "--operation", journal["request"]["id"],
            "--generation", str(journal["request"]["generation"]), "--phase", journal["phase"], "--original-sha256", journal["request"]["current"]["digest"], "--json", expected=expected)

    def close(self):
        with LOCK:
            self.closed = True
        for terminal in self.terminals:
            terminal.close()
        self.stop("helper")
        self.stop("daemon")
        for pid in self.workers:
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        for log in self.logs:
            log.close()


def historical_failure_phase(host, expected):
    try:
        return host.journal()["phase"] == expected
    except FileNotFoundError:
        return False


def observe_failed_rollback(host):
    command = [str(host.binary), "update", "--local", "--version", "v0.1.151", "--yes", "--json"]
    process = subprocess.Popen(command, env=host.environment, cwd=host.root, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        eventually(lambda: historical_failure_phase(host, "rollback_failed"), "historical failed rollback receipt missing", timeout=60)
        event("historical-rollback-failed-observed", host=host.name, provider="unchanged published v149 Engine")
        # The historical client observes through the daemon; restore connectivity after its real failure receipt.
        host.fail_original = False
        require(host.start("daemon")[0] == 0, "original fixture daemon could not restart")
        _, stderr = process.communicate(timeout=15)
        require(process.returncode == 1, "historical failed rollback client did not report failure")
        (host.root / "historical-rollback-error.log").write_bytes(stderr)
        event("command", host=host.name, operation="update", status=process.returncode)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)


def failed_historical_update(host, rollback_failed=False):
    host.fail_candidate, host.fail_original = True, rollback_failed
    if rollback_failed:
        observe_failed_rollback(host)
    else:
        host.command(host.binary, "update", "--local", "--version", "v0.1.151", "--yes", "--json", expected=1)
    expected = "rollback_failed" if rollback_failed else "rolled_back"
    require(host.journal()["phase"] == expected, "historical Engine did not write expected failure receipt")
    event("historical-failure-receipt", host=host.name, phase=expected, operation=host.journal()["request"]["id"])
    host.fail_original = False
    host.start("daemon")
    eventually(host.ready, "original daemon did not return", timeout=10)
    host.prove_daemon(HISTORICAL[0]["build"])
    host.check_retained()
    host.prior_receipt = host.receipt()
    host.prove_helper(HISTORICAL[0]["build"])


def recovery_and_chain(host):
    failed_historical_update(host)
    before_journal = (host.state / "update" / "installation.json").read_bytes()
    before_daemon = host.processes["daemon"].pid
    before_database = digest(host.database.read_bytes())
    for failure in ("restart", "readiness"):
        host.helper_failure = failure
        failed = host.recover(expected=1)
        expected_cause = b"injected helper restart failure" if failure == "restart" else b"running helper image did not become ready"
        require(expected_cause in failed.stderr, "native failure did not exercise its requested service boundary")
        host.helper_failure = ""
        eventually(lambda: host.processes["helper"].poll() is None, "prior helper not restored", timeout=5)
        host.prove_helper(HISTORICAL[0]["build"])
        require(host.receipt() == host.prior_receipt, "failed recovery changed prior receipt")
        host.check_retained()
        event("prior-helper-restored", host=host.name, cause=failure)
    result = json.loads(host.recover().stdout)
    require(result["pid"] == host.processes["helper"].pid, "recovery PID is not actual service PID")
    require(host.processes["daemon"].pid == before_daemon, "helper-only repair restarted daemon")
    require((host.state / "update" / "installation.json").read_bytes() == before_journal, "helper-only repair edited journal")
    require(digest(host.database.read_bytes()) == before_database, "helper-only repair changed database bytes")
    host.prove_helper(FIXED_BUILD)
    host.prove_daemon(HISTORICAL[0]["build"])
    host.check_retained()
    host.check_io()
    prior_path = Path(host.prior_receipt["executable"]).parent / "installed.json"
    require(json.loads(prior_path.read_text()) == host.prior_receipt, "prior exact receipt was not retained")
    host.fail_candidate, host.delay = False, 0.4
    for release in HISTORICAL[1:]:
        host.command(host.binary, "update", "--local", "--version", release["build"]["version"], "--yes", "--json")
        require(host.journal()["phase"] == "committed", "ordinary historical update did not commit")
        eventually(host.ready, "historical daemon did not become ready", timeout=10)
        host.prove_daemon(release["build"])
        host.prove_helper(FIXED_BUILD)
        host.check_retained()
        host.check_io()
        event("historical-hop", host=host.name, version=release["build"]["version"], phase="committed", helperDigest=host.receipt()["digest"])
    if NATIVE["os"] == "darwin":
        require(any(row["kind"] == "delayed-removal" and row.get("host") == host.name and row.get("unit") == "daemon" for row in EVENTS), "delayed outgoing registration was not exercised")


SOCKET_ROOT = helper_recovery_paths.validate_socket_root(ROOT)
event("fixture-socket-paths", **SOCKET_ROOT)
require(NATIVE == build(FIXED)["platform"], "helper must be a native executable")
require(not build(FIXED)["modified"], "fixed helper requires a clean committed source checkout")
require(not os.environ.get("HELPER_RECOVERY_PLATFORM") or os.environ["HELPER_RECOVERY_PLATFORM"] == NATIVE["os"] + "/" + NATIVE["arch"], "runner platform mismatch")
if NATIVE["os"] == "darwin":
    require(os.environ.get("GITHUB_ACTIONS") == "true" and os.environ.get("RUNNER_OS") == "macOS", "Darwin fixture requires disposable GitHub runner")
HISTORICAL = published_releases.acquire(ROOT / "published", NATIVE)
for bundle in HISTORICAL:
    bundle["build"] = build(bundle["executable"])
    manifest = bundle["manifest"]
    artifact = next(row for row in manifest["artifacts"] if row["platform"] == NATIVE)
    require(bundle["build"]["platform"] == NATIVE and bundle["build"]["version"] == manifest["version"]
        and bundle["build"]["commit"] == manifest["commit"] and bundle["build"]["digest"] == artifact["binarySha256"], "executed historical artifact differs from published descriptor")
    for name, payload in bundle["files"].items():
        ASSETS[f"/ShaulLavo/mesh/releases/download/{manifest['version']}/{name}"] = payload
    event("published-input", build=bundle["build"], manifestDigest=digest(bundle["files"]["mesh-release.json"]), provider="unchanged public GitHub artifacts and joined receipts")
FIXED_BUILD = candidate_descriptor(FIXED)
original_digests = {binary: digest(binary.read_bytes()) for binary in (FIXED, *(bundle["executable"] for bundle in HISTORICAL))}
tls.fixture_certificates(ROOT)
(ROOT / "empty-certs").mkdir()
TLS = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
TLS.load_cert_chain(ROOT / "server.pem", ROOT / "server.key")
proxy, services = Server(("127.0.0.1", 0), Proxy), Server(("127.0.0.1", 0), Services)
hosts = []
try:
    for server in (proxy, services):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    tls.prepare_fixture_tls(ROOT, event)
    for name in ("repair", "rollback"):
        host = Installation(name, HISTORICAL[0], proxy.server_address[1], services.server_address[1])
        hosts.append(host)
        host.start("daemon")
        eventually(host.ready, "published v149 daemon did not start", timeout=10)
        host.prove_daemon(HISTORICAL[0]["build"])
        event("published-v149-started", host=name, stateRoot=str(host.state), build=HISTORICAL[0]["build"])
        host.seed()
        if name == "repair":
            recovery_and_chain(host)
            continue
        failed_historical_update(host, rollback_failed=True)
        host.fail_candidate = False
        result = json.loads(host.recover().stdout)
        require(result["phase"] == "rolled_back" and host.journal()["phase"] == "rolled_back" and host.journal().get("verified"), "genuine existing Engine restoration did not settle rollback_failed")
        host.prove_daemon(HISTORICAL[0]["build"])
        host.prove_helper(FIXED_BUILD)
        host.check_retained()
        host.check_io()
        event("genuine-rollback-settlement", phase=host.journal()["phase"], host=name)
    (ROOT / "result.json").write_text(json.dumps({"nativePlatform": NATIVE, "historical": [bundle["build"] for bundle in HISTORICAL], "fixedHelper": FIXED_BUILD,
        "historicalProvider": "actual public archives/descriptors/joined receipts", "fixedHelperProvider": "unpublished PR-source fixture descriptor",
        "serviceProvider": "external command provider using fixture-owned native processes", "serviceRegistrationProven": False,
        "liveOwnerHostVerified": False, "socketRoot": SOCKET_ROOT,
        "chain": ["v0.1.149", "v0.1.151", "v0.1.159"]}, indent=2) + "\n")
    print("PASS native helper recovery, failure restoration, genuine rollback settlement and published v149 -> v151 -> v159 retained-session chain", flush=True)
finally:
    for host in reversed(hosts):
        host.close()
    for server in (services, proxy):
        server.shutdown()
        server.server_close()
    tls.verify_artifacts(original_digests, sys.exc_info()[1], event)
