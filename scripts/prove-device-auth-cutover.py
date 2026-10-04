#!/usr/bin/env python3
"""Disposable real CLI/daemon/worker cutover; external release and service fakes."""
import hashlib
import http.server
import importlib
import io
import json
import os
import plistlib
import platform
import select
import signal
import socket
import socketserver
import ssl
import struct
import subprocess
import sys
import tarfile
import threading
import time
from pathlib import Path

sys.dont_write_bytecode = True
fixture_tls = importlib.import_module("fixtures.tls")
published_releases = importlib.import_module("fixtures.published_releases")
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration" / "helpers"))
controls = importlib.import_module("mesh_control")
terminals = importlib.import_module("terminal_window")
round_trip = controls.round_trip
Terminal, PROMPT = terminals.Terminal, terminals.PROMPT
eventually, require = terminals.eventually, terminals.require

ROOT = Path(sys.argv[1]).resolve()
NEW, FLEET = [Path(x).resolve() for x in sys.argv[2:4]]
CONTROL_CLIENT = Path(sys.argv[4]).resolve()
HELPERS = Path(__file__).resolve().parents[1] / "integration" / "helpers"
ROOT.mkdir(parents=True, exist_ok=True)
RELEASES = {}
EVENTS = []
HOSTS = {}
LOCK = threading.RLock()


def event(kind, **fields):
    with LOCK:
        row = {"time": time.time(), "kind": kind, **fields}
        EVENTS.append(row)
        with (ROOT / "events.jsonl").open("a") as file:
            file.write(json.dumps(row) + "\n")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def version(binary):
    env = os.environ | {"HOME": str(ROOT), "MESH_STATE_DIR": str(ROOT / "unused-state"),
                        "MESH_CONFIG_DIR": str(ROOT / "unused-config")}
    return json.loads(subprocess.check_output([str(binary), "version", "--json"], env=env))


def publish(binary, previous):
    build = version(binary)
    platforms = [{"os": "linux", "arch": "amd64"}, {"os": "linux", "arch": "arm64"}, {"os": "darwin", "arch": "arm64"}]
    artifacts = []
    transitions = []
    for target in platforms:
        native = target == build["platform"]
        # Release manifests require three targets. Non-native provider placeholders
        # are unexecutable; downloading them proves no non-native execution.
        payload = binary.read_bytes() if native else b"unexecuted external release fixture: " + json.dumps(target).encode()
        content = io.BytesIO()
        with tarfile.open(fileobj=content, mode="w:gz") as archive:
            entry = tarfile.TarInfo("mesh")
            entry.mode = 0o755 if native else 0o644
            entry.size = len(payload)
            archive.addfile(entry, io.BytesIO(payload))
        archive = content.getvalue()
        name = "mesh_" + target["os"] + "_" + target["arch"] + ".tar.gz"
        artifacts.append({"platform": target, "archive": name, "sha256": digest(archive), "binarySha256": digest(payload)})
        transitions.append({"platform": target, "fromDigest": previous["digest"], "toDigest": digest(payload),
                            "proof": digest(b"disposable fixture transition")})
        RELEASES[f"/ShaulLavo/mesh/releases/download/{build['version']}/{name}"] = archive
    manifest = {"schema": 1, "version": build["version"], "commit": build["commit"], "artifacts": artifacts,
                "compatibility": {"stateReadMin": build["stateVersion"], "stateReadMax": build["stateVersion"],
                    "stateWrite": build["stateVersion"], "workerMin": 1, "workerMax": 1, "workerWrite": 1,
                    "journalVersion": 1, "transitions": transitions}}
    RELEASES[f"/ShaulLavo/mesh/releases/download/{build['version']}/mesh-release.json"] = json.dumps(manifest).encode()
    (ROOT / (build["version"] + "-manifest.json")).write_text(json.dumps(manifest, indent=2))
    return build


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class ReleaseHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        event("release", path=self.path)
        data = RELEASES.get(self.path)
        if data is None and self.path == "/repos/ShaulLavo/mesh/releases/latest":
            data = json.dumps({"tag_name": FLEET_BUILD["version"]}).encode()
        if data is None:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


class ProxyHandler(http.server.BaseHTTPRequestHandler):
    def do_CONNECT(self):
        require(self.path in ("github.com:443", "api.github.com:443"), "fixture refuses external proxy target " + self.path)
        self.send_response(200)
        self.end_headers()
        try:
            with TLS.wrap_socket(self.connection, server_side=True) as secure:
                ReleaseHandler(secure, self.client_address, self.server)
        except (ConnectionResetError, BrokenPipeError, ssl.SSLError):
            pass
        self.close_connection = True

    def log_message(self, *_):
        pass


class ServiceHandler(socketserver.StreamRequestHandler):
    def handle(self):
        request = json.loads(self.rfile.readline())
        host = HOSTS[request["host"]]
        args = request["args"]
        event("service", host=host.name, tool=request["tool"], action=args[0])
        status, output = 0, ""
        try:
            if request["tool"] == "launchctl":
                status, output = host.launchctl(args)
                self.wfile.write(json.dumps({"status": status, "output": output}).encode() + b"\n")
                return
            require(request["tool"] == "systemctl", "unexpected service tool")
            require(args[0] == "--user", "fixture accepts only user services")
            action = args[1]
            if action == "show":
                require(args[2:] == ["mesh.service", "--property=KillMode", "--value"], "unexpected service query")
                output = "process\n"
            elif action == "daemon-reload":
                require(len(args) == 2, "unexpected daemon reload")
            else:
                require(args[-1] in ("mesh.service", "mesh-update-helper.service"), "unexpected unit")
                unit = args[-1]
                if action == "stop":
                    host.stop(unit)
                elif action in ("start", "enable"):
                    host.start(unit)
                elif action == "restart" and "--no-block" in args:
                    host.queue_restart(unit)
                else:
                    raise RuntimeError("unsupported fixture service action")
        except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
            status, output = 1, str(error)
        self.wfile.write(json.dumps({"status": status, "output": output}).encode() + b"\n")


class Host:
    def __init__(self, name, original, proxy, service):
        self.name = name
        self.root = ROOT / name
        self.root.mkdir()
        self.state = self.root / "state"
        self.config = self.root / "config"
        self.config.mkdir()
        self.home = self.root / "home"
        self.home.mkdir()
        self.tools = self.home / "bin"
        self.tools.mkdir()
        (self.tools / "tailscale").symlink_to(HELPERS / "fake_tailscale")
        service_cli = self.tools / ("launchctl" if OLD_BUILD["platform"]["os"] == "darwin" else "systemctl")
        service_cli.write_text("#!" + sys.executable + "\n" + '''import json, os, socket, sys
with socket.create_connection(("127.0.0.1", int(os.environ["FAKE_SERVICE_PORT"])), timeout=30) as sock:
    sock.sendall(json.dumps({"host": os.environ["FAKE_HOST"], "tool": os.path.basename(sys.argv[0]), "args": sys.argv[1:]}).encode() + b"\\n")
    data = json.loads(sock.makefile("rb").readline())
print(data["output"], end="")
sys.exit(data["status"])
''')
        service_cli.chmod(0o755)
        self.binary = self.home / ".local" / "bin" / "mesh"
        self.binary.parent.mkdir(parents=True)
        self.binary.write_bytes(original["executable"].read_bytes())
        self.original_build = original["build"]
        self.binary.chmod(0o755)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            self.port = listener.getsockname()[1]
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({"BackendState": "Running", "Self": {"DNSName": name + ".fixture.test.",
            "HostName": name, "TailscaleIPs": ["127.0.0.1"], "Online": True}, "Peer": {}}))
        self.environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
        self.environment.update({"HOME": str(self.home), "MESH_STATE_DIR": str(self.state), "MESH_CONFIG_DIR": str(self.config),
            "PATH": str(self.tools) + os.pathsep + self.environment.get("PATH", os.defpath),
            "XDG_CACHE_HOME": str(self.root / "cache"), "XDG_CONFIG_HOME": str(self.root / "xdg-config"),
            "XDG_STATE_HOME": str(self.root / "xdg-state"), "XDG_RUNTIME_DIR": str(self.root / "runtime"),
            "SHELL": str(HELPERS / "window_shell.sh"), "TERM": "xterm-256color", "NO_COLOR": "1",
            "MESH_FAKE_TAILSCALE_STATUS": str(status), "FAKE_SERVICE_PORT": str(service), "FAKE_HOST": name,
            "HTTPS_PROXY": "http://127.0.0.1:" + str(proxy), "HTTP_PROXY": "http://127.0.0.1:" + str(proxy),
            "NO_PROXY": "127.0.0.1,localhost", "SSL_CERT_FILE": str(ROOT / "cert.pem"), "SSL_CERT_DIR": str(ROOT / "empty-certs"),
            "PYTHONDONTWRITEBYTECODE": "1"})
        Path(self.environment["XDG_RUNTIME_DIR"]).mkdir(mode=0o700)
        if OLD_BUILD["platform"]["os"] == "darwin":
            agents = self.home / "Library" / "LaunchAgents"
            agents.mkdir(parents=True)
            self.plist = agents / "dev.shaulavo.mesh.plist"
            (self.home / ".local" / "state" / "mesh").mkdir(parents=True)
            template = (HELPERS.parents[1] / "scripts" / "install" / "assets" / self.plist.name).read_text()
            substitutions = {"@MESH_BINARY@": "${HOME}/.local/bin/mesh", "@MESH_PORT@": str(self.port),
                "@MESH_SSH_PORT@": "0", "@MESH_WEBSOCKET_PATH@": "/mesh",
                "@MESH_STDOUT@": "${HOME}/.local/state/mesh/daemon.log",
                "@MESH_STDERR@": "${HOME}/.local/state/mesh/daemon.err.log"}
            for token, value in substitutions.items():
                template = template.replace(token, value)
            require("@MESH_" not in template, "unresolved production daemon plist token")
            self.plist.write_text(template)
            self.service_plists = {"mesh.service": self.plist}
        self.processes = {}
        self.restarts = []
        self.logs = []
        self.terminals = []
        self.workers = []
        self.id = None
        HOSTS[name] = self

    def launchctl(self, args):
        domain = "gui/" + str(os.getuid())
        labels = {"dev.shaulavo.mesh": "mesh.service", "dev.shaulavo.mesh-update-helper": "mesh-update-helper.service"}
        action = args[0]
        if action == "bootstrap":
            require(len(args) == 3 and args[1] == domain, "unexpected launchd bootstrap domain")
            path = Path(args[2]).resolve()
            require(path.parent == self.plist.parent.resolve(), "launchd plist escaped fixture")
            data = plistlib.loads(path.read_bytes())
            label = data["Label"]
            require(label in labels, "unexpected launchd label")
            if label == "dev.shaulavo.mesh":
                require(data["AbandonProcessGroup"] is True, "daemon plist can kill workers")
                require(path == self.plist.resolve(), "wrong daemon plist")
            else:
                expected = json.loads((self.state / "update" / "helper" / "installed.json").read_text())
                launcher = self.state / "update" / "helper" / "current"
                require(launcher.resolve() == Path(expected["executable"]).resolve(), "helper launcher missed the installed executable")
                require(data["ProgramArguments"] == [str(launcher), "update-helper", "--state-dir", str(self.state)],
                        "helper plist arguments differ from the real installation")
            self.service_plists[labels[label]] = path
            self.start(labels[label])
            return 0, ""
        require(action in ("print", "bootout", "kickstart"), "unexpected launchd action")
        if action == "kickstart":
            require(len(args) == 3 and args[1] == "-k", "unexpected launchd kickstart")
        else:
            require(len(args) == 2, "unexpected launchd query/stop")
        scope, separator, label = args[-1].rpartition("/")
        require(separator and scope == domain and label in labels, "unexpected launchd service target")
        unit = labels[label]
        process = self.processes.get(unit)
        if action == "print":
            if process and process.poll() is None:
                return 0, "fixture service running\n"
            return 113, f'Bad request.\nCould not find service "{label}" in domain for user gui: {os.getuid()}\n'
        if action == "bootout":
            self.stop(unit)
        else:
            self.queue_restart(unit)
        return 0, ""

    def start(self, unit):
        with LOCK:
            current = self.processes.get(unit)
            if current and current.poll() is None:
                return
            if OLD_BUILD["platform"]["os"] == "darwin":
                data = plistlib.loads(self.service_plists[unit].read_bytes())
                if unit == "mesh.service":
                    require(data["AbandonProcessGroup"] is True, "daemon plist can kill retained workers")
                command = data["ProgramArguments"]
                event("production-plist", host=self.name, unit=unit, asset=self.service_plists[unit].name)
            elif unit == "mesh.service":
                command = [str(self.binary), "daemon", "--tailnet-port", str(self.port), "--ssh-port", "0"]
            else:
                record = json.loads((self.state / "update" / "helper" / "installed.json").read_text())
                command = [record["executable"], "update-helper", "--state-dir", str(self.state)]
            log = (self.root / (unit + ".log")).open("ab")
            self.logs.append(log)
            process = subprocess.Popen(command, env=self.environment, cwd=self.root, stdin=subprocess.DEVNULL,
                                       stdout=log, stderr=subprocess.STDOUT)
            self.processes[unit] = process
            event("start", host=self.name, unit=unit, pid=process.pid)

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

    def queue_restart(self, unit):
        restart = threading.Timer(0.2, self.restart, args=[unit])
        with LOCK:
            self.restarts.append(restart)
            restart.start()

    def control(self, kind):
        return round_trip(str(self.state / "daemon.sock"), {"type": kind, "requestId": "cutover-proof"})

    def ready(self):
        try:
            result = self.control("host.info")
            if result.get("type") == "host.info.result":
                self.id = result["host"]["id"]
                self.mesh_identity = result["host"]["meshIdentity"]
                return result
        except (OSError, RuntimeError):
            pass
        return False

    def command(self, *args, timeout=90):
        result = subprocess.run([str(self.binary), *args], env=self.environment, cwd=self.root, capture_output=True, timeout=timeout, check=False)
        event("cli", host=self.name, operation=args[0], status=result.returncode,
              stdoutBytes=len(result.stdout), stderrBytes=len(result.stderr))
        require(result.returncode == 0, f"{self.name} fixture {args[0]} failed with status {result.returncode}")
        return result

    def seed(self):
        terminal = Terminal([str(self.binary), "local", "--", str(HELPERS / "window_shell.sh")], self.environment, self.root, timeout=8)
        self.terminals.append(terminal)
        terminal.expect(PROMPT)
        session = self.control("session.list")["sessions"][0]["id"]
        meta = json.loads((self.state / "s" / session / "meta.json").read_text())
        self.workers.append(meta["pid"])
        start = len(terminal.drain())
        terminal.send("printf '__RETAINED_PID__%s__END__\\n' \"$$\"\n")
        import re
        match = eventually(lambda: re.search(rb"__RETAINED_PID__([0-9]+)__END__", terminal.drain()[start:]), "shell pid not printed", timeout=8)
        self.session, self.shell_pid = session, int(match[1])
        self.worker_pid = self.worker_identity(session)
        self.workers.append(self.worker_pid)
        terminal.close()
        self.terminals.remove(terminal)
        event("seed", host=self.name, session=session, worker=self.worker_pid, shell=self.shell_pid)

    def worker_identity(self, session):
        with socket.socket(socket.AF_UNIX) as sock:
            sock.connect(str(self.state / "s" / session / "sock"))
            if OLD_BUILD["platform"]["os"] == "darwin":
                # Darwin's SOL_LOCAL/LOCAL_PEERPID prove the actual socket owner.
                return sock.getsockopt(0, 2)
            return struct.unpack("3i", sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]

    def check_retained(self):
        meta = json.loads((self.state / "s" / self.session / "meta.json").read_text())
        require(meta["pid"] == self.shell_pid, "shell metadata PID changed")
        require(self.worker_identity(self.session) == self.worker_pid, "retained worker process changed")
        for pid in (self.worker_pid, self.shell_pid):
            os.kill(pid, 0)
        require(any(row["id"] == self.session for row in self.control("session.list")["sessions"]), "daemon lost retained session")

    def prove_loaded_image(self, expected):
        observed = self.control("host.info")["host"]["build"]
        require(observed["digest"] == expected["digest"] and observed["version"] == expected["version"],
                "daemon reports a different executing build")
        with socket.socket(socket.AF_UNIX) as sock:
            sock.connect(str(self.state / "daemon.sock"))
            pid = sock.getsockopt(0, 2) if native_platform["os"] == "darwin" else struct.unpack(
                "3i", sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]
        require(pid == self.processes["mesh.service"].pid, "daemon socket belongs to another process")
        installed = self.binary.stat()
        image = Path("/proc") / str(pid) / "exe"
        if native_platform["os"] == "darwin":
            output = subprocess.check_output(["lsof", "-a", "-p", str(pid), "-d", "txt", "-F", "finD"],
                                             text=True, timeout=5)
            records, record = [], {}
            for line in output.splitlines():
                if line.startswith("f"):
                    records.append(record)
                    record = {}
                if line[:1] in ("i", "D", "n"):
                    record[line[0]] = line[1:]
            records.append(record)
            matched = [row for row in records if row.get("i") == str(installed.st_ino)
                and row.get("D") and int(row["D"], 0) == installed.st_dev and row.get("n")]
            require(len(matched) == 1, "native lsof did not identify the exact daemon image inode")
            image = Path(matched[0]["n"])
        mapped = image.stat()
        require((mapped.st_dev, mapped.st_ino) == (installed.st_dev, installed.st_ino),
                "loaded daemon image differs from the installed inode")
        require(digest(image.read_bytes()) == expected["digest"], "loaded daemon image bytes differ")
        event("loaded-image", host=self.name, pid=pid, version=expected["version"], digest=expected["digest"],
              device=mapped.st_dev, inode=mapped.st_ino, provider="native lsof" if native_platform["os"] == "darwin" else "native procfs")

    def check_io(self):
        terminal = Terminal([str(self.binary), "local", "-r"], self.environment, self.root, timeout=8)
        self.terminals.append(terminal)
        try:
            terminal.expect(PROMPT)
            start = len(terminal.drain())
            terminal.send("printf '__CUTOVER_IO__%s__END__\\n' \"$$\"\n")
            terminal.expect("__CUTOVER_IO__" + str(self.shell_pid) + "__END__", since=start)
        finally:
            terminal.close()
            self.terminals.remove(terminal)
        event("retained-io", host=self.name, worker=self.worker_pid, shell=self.shell_pid)

    def close(self):
        for terminal in self.terminals:
            terminal.close()
        self.stop("mesh-update-helper.service")
        self.stop("mesh.service")
        for pid in self.workers:
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        for log in self.logs:
            log.close()


for binary in (NEW, FLEET, CONTROL_CLIENT):
    require(binary.is_file() and os.access(binary, os.X_OK), "proof requires executable inputs")
native_platform = {"os": platform.system().lower(), "arch": {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())}
require(version(NEW)["platform"] == version(FLEET)["platform"] == native_platform,
        "inputs must execute on the native runner platform")
require(not os.environ.get("AUTH_CUTOVER_PLATFORM") or os.environ["AUTH_CUTOVER_PLATFORM"] == native_platform["os"] + "/" + native_platform["arch"],
        "runner differs from the required CI platform")
if native_platform["os"] == "darwin":
    require(os.environ.get("GITHUB_ACTIONS") == "true" and os.environ.get("RUNNER_OS") == "macOS",
            "Darwin executable fixture requires a disposable GitHub macOS runner")
historical = published_releases.acquire(ROOT / "published", native_platform)
for bundle in historical:
    build = version(bundle["executable"])
    manifest = bundle["manifest"]
    artifact = next(row for row in manifest["artifacts"] if row["platform"] == native_platform)
    require(build["platform"] == native_platform and build["version"] == manifest["version"]
        and build["commit"] == manifest["commit"] and build["digest"] == artifact["binarySha256"],
        "executed published artifact differs from its descriptor")
    bundle["build"] = build
    for name, data in bundle["files"].items():
        RELEASES[f"/ShaulLavo/mesh/releases/download/{manifest['version']}/{name}"] = data
    event("published-input", version=build["version"], platform=native_platform, digest=build["digest"],
        source="verified public GitHub artifact", manifestDigest=digest(bundle["files"]["mesh-release.json"]))
OLD_BUILD = historical[0]["build"]
NEW_BUILD = publish(NEW, historical[-1]["build"])
FLEET_BUILD = publish(FLEET, NEW_BUILD)
inputs = [NEW, FLEET, CONTROL_CLIENT, *(bundle["executable"] for bundle in historical)]
original_digests = {binary: digest(binary.read_bytes()) for binary in inputs}
fixture_tls.fixture_certificates(ROOT)
(ROOT / "empty-certs").mkdir(exist_ok=True)
TLS = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
TLS.load_cert_chain(ROOT / "server.pem", ROOT / "server.key")
proxy = Server(("127.0.0.1", 0), ProxyHandler)
services = Server(("127.0.0.1", 0), ServiceHandler)
for server in (proxy, services):
    threading.Thread(target=server.serve_forever, daemon=True).start()
hosts = []


def wait_for_restarts():
    with LOCK:
        pending = [restart for host in hosts for restart in host.restarts]
    for restart in pending:
        restart.join(timeout=10)
        require(not restart.is_alive(), "fixture service restart did not finish")


def local_cutover(host, target):
    prior_id = host.id
    before = len(EVENTS)
    result = host.command("update", "--local", "--version", target["version"], "--yes", "--json")
    run = json.loads(result.stdout)
    require(len(run["fleet"]["members"]) == 1 and run["fleet"]["members"][0]["endpoint"] == "unix://" + str(host.state / "daemon.sock"), "local plan left own Unix endpoint")
    require(run["coordinator"] == host.id and run["targets"][0]["state"] == "updated", "local daemon did not settle its own run")
    retained = run["targets"][0]["workers"][0]
    require(retained["pid"] == host.worker_pid and retained["shellPid"] == host.shell_pid, "installer receipt did not preserve actual worker and shell")
    require(digest(host.binary.read_bytes()) == target["digest"], "local update did not install requested binary")
    eventually(host.ready, "updated daemon unhealthy", timeout=10)
    require(host.id == prior_id, "local update changed identity")
    host.check_retained()
    host.prove_loaded_image(target)
    host.check_io()
    wait_for_restarts()
    touched = {row["host"] for row in EVENTS[before:] if row["kind"] in ("start", "stop")}
    require(touched == {host.name}, "local update changed another installation: " + repr(touched))
    print("PASS real local cutover", host.name, target["version"], "worker", host.worker_pid, "shell", host.shell_pid, flush=True)

try:
    fixture_tls.prepare_fixture_tls(ROOT, event)
    for index, original in enumerate((historical[0], historical[1], historical[2], historical[0])):
        host = Host("h" + str(index), original, proxy.server_address[1], services.server_address[1])
        hosts.append(host)
        host.start("mesh.service")
        eventually(host.ready, "published daemon failed to start", timeout=10)
        host.prove_loaded_image(original["build"])
        host.seed()
    old_digests = [digest(host.binary.read_bytes()) for host in hosts]
    for index, host in enumerate(hosts):
        start = next(position for position, bundle in enumerate(historical)
            if bundle["build"]["digest"] == host.original_build["digest"])
        for bundle in historical[start + 1:]:
            local_cutover(host, bundle["build"])
        local_cutover(host, NEW_BUILD)
        for later_index in range(index + 1, len(hosts)):
            require(digest(hosts[later_index].binary.read_bytes()) == old_digests[later_index],
                    "local update changed not-yet-updated host")
    for host in hosts:
        with socket.create_connection(("127.0.0.1", host.port), timeout=2) as raw:
            raw.sendall(b"GET /mesh HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\n"
                        b"Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
                        b"Sec-WebSocket-Key: Zml4dHVyZS1hdXRoLXByb29m\r\n\r\n")
            require(raw.recv(4096).startswith(b"HTTP/1.1 426"), "new daemon admitted a raw legacy control")
    print("PASS all four new daemons deny unauthenticated legacy controls", flush=True)
    for host in hosts:
        peers = []
        for peer in hosts:
            if peer is host:
                continue
            host.command("device", "approve", "--allow-root", "--", peer.id)
            host.command("update", "trust", "--", peer.id)
            peers.append({"id": peer.id, "meshIdentity": peer.mesh_identity, "addresses": ["127.0.0.1"], "alias": peer.name, "endpoint": f"ws://127.0.0.1:{peer.port}/mesh"})
        (host.config / "hosts.json").write_text(json.dumps({"version": 1, "hosts": peers}))
    for host in hosts:
        listing = host.command("ls").stdout
        for peer in hosts:
            if peer is not host:
                require(peer.session.encode() in listing, "mutual authenticated listing lost session " + host.name + "->" + peer.name)
    print("PASS 12 directed authenticated reconnects after explicit grants and pins", flush=True)
    for index, host in enumerate(hosts):
        peer = hosts[(index + 1) % len(hosts)]
        terminal = Terminal([str(host.binary), peer.name, "-r"], host.environment, host.root, timeout=8)
        host.terminals.append(terminal)
        terminal.expect(PROMPT)
        start = len(terminal.drain())
        terminal.send("printf '__AFTER_PID__%s__END__\\n' \"$$\"\n")
        terminal.expect("__AFTER_PID__" + str(peer.shell_pid) + "__END__", since=start)
        terminal.close()
        host.terminals.remove(terminal)
    print("PASS all four original sessions reattach remotely with unchanged shell PIDs", flush=True)
    controller, destination = hosts[:2]
    retained = Terminal([str(controller.binary), destination.name, "-r"], controller.environment, controller.root, timeout=8)
    controller.terminals.append(retained)
    retained.expect(PROMPT)
    # A reconnecting CLI may use the new grant. DialOnce probes keep the old socket
    # identity observable across immediate reapproval without reconnecting it.
    probe = subprocess.Popen([str(CONTROL_CLIENT), f"ws://127.0.0.1:{destination.port}/mesh", destination.id,
                              "grant-lifetime"], env=controller.environment, cwd=controller.root,
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        require(select.select([probe.stdout], [], [], 5)[0], "original-grant probe did not connect")
        require(probe.stdout.readline() == b"READY\n", "original-grant probe did not report readiness")
        destination.command("device", "revoke", "--", controller.id)
        destination.command("device", "approve", "--allow-root", "--", controller.id)
        stdout, _ = probe.communicate(b"\n", timeout=8)
        require(probe.returncode == 0 and b"old active/passive sockets retired; fresh grant connected" in stdout,
                "immediate reapproval healed an original socket or refused the fresh grant")
    finally:
        if probe.poll() is None:
            probe.kill()
            probe.wait()
    destination.check_retained()
    destination.command("device", "revoke", "--", controller.id)
    retained.expect_exit()
    retained.close()
    controller.terminals.remove(retained)
    destination.command("device", "approve", "--allow-root", "--", controller.id)
    fresh = Terminal([str(controller.binary), destination.name, "-r"], controller.environment, controller.root, timeout=8)
    controller.terminals.append(fresh)
    fresh.expect(PROMPT)
    fresh.close()
    controller.terminals.remove(fresh)
    destination.check_retained()
    print("PASS original dual-authority sockets retire after immediate same-key reapproval; permanent revocation retires the CLI; fresh grant reattaches retained worker", flush=True)
    coordinator = hosts[0]
    fleet_file = coordinator.root / "fleet.json"
    fleet_file.write_text(json.dumps({"version": 1, "name": "four-disposable-hosts", "revision": 1,
        "members": [{"id": host.id, "alias": host.name, "endpoint":
           "unix:" + str(host.state / "daemon.sock") if host is coordinator else f"ws://127.0.0.1:{host.port}/mesh",
            "platform": OLD_BUILD["platform"]} for host in hosts]}))
    coordinator.command("update", "--fleet", str(fleet_file), "--version", FLEET_BUILD["version"], "--yes", "--json", timeout=120)
    for host in hosts:
        require(digest(host.binary.read_bytes()) == FLEET_BUILD["digest"], "fleet update missed host " + host.name)
        eventually(host.ready, "fleet daemon unhealthy", timeout=10)
        host.check_retained()
        host.prove_loaded_image(FLEET_BUILD)
        host.check_io()
    print("PASS real daemon-owned authenticated fleet update installed second patch on all four hosts; original workers preserved", flush=True)
    (ROOT / "result.json").write_text(json.dumps({"historical": [bundle["build"] for bundle in historical], "new": NEW_BUILD, "fleet": FLEET_BUILD,
        "candidateReleaseProvider": "external fixture descriptors for two PR-source candidate patch builds",
        "historicalReleaseProvider": "unchanged public GitHub artifacts, descriptors and joined receipts",
        "nativePlatform": native_platform,
        "peerPIDProvider": "native LOCAL_PEERPID" if native_platform["os"] == "darwin" else "native SO_PEERCRED",
        "serviceProvider": "production plist argv with fake launchctl registration" if native_platform["os"] == "darwin" else "systemd-command fixture",
        "serviceRegistrationProven": False,
        "hosts": [{"name": host.name, "originalBuild": host.original_build, "identityDigest": digest(host.id.encode()), "worker": host.worker_pid, "shell": host.shell_pid,
                   "session": host.session} for host in hosts]}, indent=2))
finally:
    try:
        for host in reversed(hosts):
            host.close()
        for server in (services, proxy):
            server.shutdown()
            server.server_close()
    finally:
        fixture_tls.verify_artifacts(original_digests, sys.exc_info()[1], event)
