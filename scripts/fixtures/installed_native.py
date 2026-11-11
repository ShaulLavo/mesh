"""Actual legacy daemons, helpers, attached PTYs and service-manager fixtures."""
import http.client
import importlib
import json
import os
import plistlib
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import sys
import time
import uuid
from xml.sax.saxutils import escape
from pathlib import Path

from fixtures import native_services

REPO = Path(__file__).resolve().parents[2]
HELPERS = REPO / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))
terminal_module = importlib.import_module("terminal_window")
controls = importlib.import_module("mesh_control")


def cleanup_all(actions):
    failures = []
    for action in actions:
        try:
            action()
        except BaseException as error:
            failures.append(str(error))
    if failures:
        raise RuntimeError("; ".join(failures))


def peer_pid(path):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(2)
        connection.connect(str(path))
        if sys.platform == "darwin":
            return connection.getsockopt(0, 2)
        return struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]


class Units:
    def __init__(self, root, environment):
        self.root, self.environment = root, environment
        self.managers = {}
        self.links, self.dropins = [], []
        self.registered = []
        for role, label in (("daemon", "mesh"), ("helper", "mesh-update-helper")):
            manager = native_services.Manager(root)
            manager.label = "dev.shaulavo." + label if sys.platform == "darwin" else label
            if manager.exists():
                raise RuntimeError(f"refusing to replace existing {manager.label} in the native manager")
            self.managers[role] = manager

    def register(self, role, path):
        manager = self.managers[role]
        manager.plist = path
        if manager.system == "Darwin":
            data = plistlib.loads(path.read_bytes())
            data["EnvironmentVariables"] = self.environment
            path.write_bytes(plistlib.dumps(data))
            self.registered.append(role)
            return
        runtime = Path(manager.environment["XDG_RUNTIME_DIR"]) / "systemd" / "user"
        link = runtime / (manager.label + ".service")
        dropin = runtime / (manager.label + ".service.d")
        if link.exists() or link.is_symlink() or dropin.exists():
            raise RuntimeError("refusing to overwrite existing native service configuration")
        # Track each registration before invoking the manager or writing files.
        self.registered.append(role)
        self.links.append((link, path))
        self.dropins.append(dropin)
        dropin.mkdir(parents=True)
        lines = ["[Service]"] + ["Environment=" + json.dumps(key + "=" + value) for key, value in self.environment.items()]
        (dropin / "50-installed-fixture.conf").write_text("\n".join(lines) + "\n")
        manager.systemctl("link", "--runtime", str(path))
        manager.systemctl("daemon-reload")

    def start(self, role):
        manager = self.managers[role]
        if manager.system == "Linux":
            manager.systemctl("start", manager.label + ".service")
        else:
            manager.launchctl("bootstrap", manager.domain, str(manager.plist))
        manager.started = True
        manager.pid = native_services.wait_for(manager.manager_pid, "native service did not report a PID")
        return manager.pid

    def stop(self, role):
        manager = self.managers[role]
        if manager.system == "Linux":
            manager.systemctl("stop", manager.label + ".service", check=False)
            return
        manager.launchctl("bootout", manager.domain + "/" + manager.label, check=False)
        native_services.wait_for(lambda: not manager.exists(), "launchd fixture service did not leave its domain")

    def close(self):
        actions = [lambda role=role: self.stop(role) for role in reversed(self.registered)]
        actions.extend(lambda link=link, target=target: self.unlink(link, target) for link, target in self.links)
        actions.extend(lambda directory=directory: self.remove_dropin(directory) for directory in self.dropins)
        actions.extend(lambda manager=manager: self.verify_removed(manager) for manager in self.managers.values())
        cleanup_all(actions)

    def unlink(self, link, target):
        if not link.is_symlink():
            return
        if link.resolve() != target.resolve():
            raise RuntimeError("native unit link changed owner during fixture execution")
        link.unlink()

    def remove_dropin(self, directory):
        (directory / "50-installed-fixture.conf").unlink(missing_ok=True)
        if directory.exists():
            directory.rmdir()

    def verify_removed(self, manager):
        if manager.system == "Linux":
            manager.systemctl("daemon-reload")
            manager.systemctl("reset-failed", manager.label + ".service", check=False)
        native_services.wait_for(lambda: not manager.exists(), "owned native service remains registered")
        if manager.pid:
            native_services.wait_for(lambda: not native_services.alive(manager.pid), "owned service process remains alive")


class Installation:
    def __init__(self, root, host, pair, preparer):
        self.root, self.host, self.pair = root / host["name"], host, pair
        self.root.mkdir()
        self.home, self.state = self.root / "home", self.root / "state"
        self.binary = self.home / ".local" / "bin" / "mesh"
        self.binary.parent.mkdir(parents=True)
        shutil.copy2(pair["daemon"]["executable"], self.binary)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        (self.tools / "tailscale").symlink_to(HELPERS / "fake_tailscale")
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({"BackendState": "Running", "Self": {"DNSName": "installed.fixture.test.",
            "HostName": "installed", "TailscaleIPs": ["127.0.0.1"], "Online": True}, "Peer": {}}))
        self.environment = {"HOME": str(self.home), "PATH": str(self.tools) + os.pathsep + os.environ.get("PATH", os.defpath),
            "MESH_STATE_DIR": str(self.state), "MESH_CONFIG_DIR": str(self.root / "config"),
            "MESH_FAKE_TAILSCALE_STATUS": str(status), "TERM": "xterm-256color", "NO_COLOR": "1",
            "SHELL": str(HELPERS / "window_shell.sh"), "PYTHONDONTWRITEBYTECODE": "1"}
        self.units = Units(self.root, self.environment)
        self.preparer = preparer
        self.terminal, self.stream = None, None
        self.response = None
        self.worker, self.shell, self.listener = None, None, None
        self.cleaned = False

    def prepare(self):
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        directory = self.home / ("Library/LaunchAgents" if sys.platform == "darwin" else ".config/systemd/user")
        directory.mkdir(parents=True)
        manager = self.units.managers["daemon"]
        daemon_path = directory / (manager.label + (".plist" if sys.platform == "darwin" else ".service"))
        self.render_daemon(daemon_path)
        kind, domain = ("launchd", manager.domain) if sys.platform == "darwin" else ("systemd", "")
        result = subprocess.run([str(self.preparer), str(self.state), str(self.pair["helper"]["executable"]),
            str(directory), kind, domain], capture_output=True, text=True, check=True, timeout=20)
        helper = json.loads(result.stdout)
        self.helper = helper
        self.units.register("daemon", daemon_path)
        self.units.register("helper", Path(helper["servicePath"]))
        self.units.start("daemon")
        native_services.wait_for(self.ready, "actual baseline daemon did not become ready")
        initial_helper = {"stopped": True, "digest": helper["digest"]}
        if self.host["helperRunning"]:
            self.units.start("helper")
            initial_helper = self.check_helper()
        elif self.units.managers["helper"].manager_pid() != 0:
            raise RuntimeError("PC fixture helper must begin stopped")
        daemon_image = native_services.mapped_image(manager.pid, self.binary)
        if daemon_image["sha256"] != self.pair["daemon"]["artifact"]["binarySha256"]:
            raise RuntimeError("running baseline daemon differs from its published image")
        return {"daemon": daemon_image, "helper": initial_helper}

    def render_daemon(self, path):
        template = (REPO / "scripts" / "install" / "assets" / path.name).read_text()
        replacements = {"@MESH_BINARY@": str(self.binary), "@MESH_BINARY_DIR@": str(self.binary.parent),
            "@MESH_PORT@": str(self.port), "@MESH_SSH_PORT@": "0", "@MESH_WEBSOCKET_PATH@": "/mesh",
            "@MESH_STDOUT@": str(self.root / "daemon.log"), "@MESH_STDERR@": str(self.root / "daemon.err")}
        for token, value in replacements.items():
            template = template.replace(token, escape(value) if sys.platform == "darwin" else value)
        if "@MESH_" in template:
            raise RuntimeError("unresolved production daemon service template")
        if sys.platform != "darwin":
            template += "\n[Service]\nStandardOutput=append:" + str(self.root / "daemon.log")
            template += "\nStandardError=append:" + str(self.root / "daemon.err") + "\n"
        path.write_text(template)

    def control(self, operation, **fields):
        return controls.round_trip(str(self.state / "daemon.sock"), {"type": operation, "requestId": "installed-native", **fields})

    def ready(self):
        try:
            return self.control("host.info").get("type") == "host.info.result"
        except (OSError, RuntimeError):
            return False

    def check_helper(self):
        manager = self.units.managers["helper"]
        manager.pid = manager.manager_pid()
        image = native_services.mapped_image(manager.pid, self.helper["executable"])
        if image["sha256"] != self.pair["helper"]["artifact"]["binarySha256"]:
            raise RuntimeError("running baseline helper differs from its published image")
        return image

    def seed(self):
        client_environment = self.prepare_client()
        self.terminal = terminal_module.Terminal([str(self.binary), self.remote_id, "--", str(HELPERS / "window_shell.sh")],
            client_environment, self.root, timeout=10)
        self.terminal.expect(terminal_module.PROMPT)
        sessions = self.control("session.list")["sessions"]
        if len(sessions) != 1:
            raise RuntimeError("native fixture must own exactly its one original session")
        self.session = sessions[0]["id"]
        self.worker = peer_pid(self.state / "s" / self.session / "sock")
        self.shell = json.loads((self.state / "s" / self.session / "meta.json").read_text())["pid"]
        self.io()
        marker = self.root / "listener.json"
        self.challenge = marker.with_suffix(".challenge")
        command = shlex.join([sys.executable, str(Path(__file__).with_name("installed_listener.py")), str(marker)])
        self.terminal.send(command + " &\n")
        native_services.wait_for(marker.exists, "session listener did not start")
        self.listener = json.loads(marker.read_text())
        response = self.control("service.upsert", service={"name": "catchup", "kind": "proxy",
            "target": str(self.listener["port"])})
        if response.get("type") != "service.upserted":
            raise RuntimeError(f"native service registration failed: {response!r}")
        self.open_stream()

    def prepare_client(self):
        config = self.root / "cc"
        config.mkdir()
        environment = self.environment | {"MESH_STATE_DIR": str(self.root / "c"), "MESH_CONFIG_DIR": str(config)}
        result = subprocess.run([str(self.binary), "device", "identity", "--json"], env=environment,
            capture_output=True, text=True, check=True, timeout=10)
        client_id = json.loads(result.stdout)["id"]
        subprocess.run([str(self.binary), "device", "approve", "--allow-root", "--", client_id],
            env=self.environment, capture_output=True, text=True, check=True, timeout=10)
        host = self.control("host.info")["host"]
        self.remote_id = host["id"]
        path = config / "hosts.json"
        path.write_text(json.dumps({"version": 1, "hosts": [{"id": host["id"], "meshIdentity": host["meshIdentity"],
            "tailscaleName": "installed.fixture.test", "addresses": ["127.0.0.1"],
            "endpoint": f"ws://127.0.0.1:{self.port}/mesh"}]}))
        path.chmod(0o600)
        return environment

    def io(self):
        start = len(self.terminal.drain())
        self.terminal.send("printf '__ATTACHED__%s__END__\\n' \"$$\"\n")
        self.terminal.expect("__ATTACHED__" + str(self.shell) + "__END__", since=start)

    def open_stream(self):
        if self.stream:
            if self.response:
                self.response.close()
            self.stream.close()
        self.stream = http.client.HTTPConnection("127.0.0.1", self.port, timeout=2)
        self.stream.request("GET", "/catchup/stream")
        response = self.stream.getresponse()
        if response.status != 200 or response.readline() != (str(self.listener["pid"]) + "\n").encode():
            raise RuntimeError("active service stream does not reach the original session listener")
        self.response = response

    def restart_control(self):
        daemon = self.units.managers["daemon"]
        original = daemon.pid
        start = time.monotonic()
        self.units.stop("daemon")
        self.require_stream_closed()
        self.units.start("daemon")
        native_services.wait_for(self.ready, "baseline did not reconnect after native restart")
        self.open_stream()
        self.wait_attachment_resume()
        gap_bound = time.monotonic() - start
        if gap_bound > 10 or daemon.pid == original:
            raise RuntimeError("native restart did not replace daemon within the route-gap bound")
        if peer_pid(self.state / "s" / self.session / "sock") != self.worker:
            raise RuntimeError("native service restart replaced the original worker")
        meta = json.loads((self.state / "s" / self.session / "meta.json").read_text())
        if meta["pid"] != self.shell:
            raise RuntimeError("native service restart replaced the original shell")
        self.io()
        daemon_image = native_services.mapped_image(daemon.pid, self.binary)
        worker_image = native_services.mapped_image(self.worker, self.binary)
        helper = self.check_helper() if self.host["helperRunning"] else {"stopped": True}
        return {"control": "actual published pair native restart; no update acceptance claimed", "host": self.host["name"],
            "worker": self.worker, "shell": self.shell, "listener": self.listener, "attachedClient": self.terminal.pid,
            "originalDaemon": original, "daemon": daemon.pid, "daemonImage": daemon_image, "workerImage": worker_image,
            "routeGapUpperBoundSeconds": gap_bound, "helper": helper}

    def wait_attachment_resume(self):
        token = uuid.uuid4().hex
        temporary = self.challenge.with_suffix(".tmp")
        temporary.write_text(token)
        start = len(self.terminal.drain())
        temporary.replace(self.challenge)
        self.terminal.expect("__RESUMED__" + token + "__", since=start)

    def require_stream_closed(self):
        for _ in range(256):
            try:
                line = self.response.readline()
            except ConnectionResetError:
                return
            if not line:
                return
        raise RuntimeError("old daemon's service stream remained open after native stop")

    def close(self):
        try:
            self.close_session()
        finally:
            self.units.close()
        self.cleaned = True

    def close_session(self):
        cleanup_all((self.close_stream, self.stop_listener, self.stop_worker, self.close_terminal, self.verify_session_stopped))

    def close_stream(self):
        if self.response:
            self.response.close()
        if self.stream:
            self.stream.close()

    def stop_listener(self):
        if self.listener and native_services.alive(self.listener["pid"]):
            os.kill(self.listener["pid"], signal.SIGTERM)
            if self.terminal.poll() is not None:
                return
            start = len(self.terminal.drain())
            self.terminal.send("wait\n")
            self.terminal.expect(terminal_module.PROMPT, since=start)

    def stop_worker(self):
        if not self.worker:
            return
        if self.ready():
            self.control("session.kill", sessionId=self.session)
            return
        path = self.state / "s" / self.session / "sock"
        if path.exists() and peer_pid(path) == self.worker:
            os.kill(self.worker, signal.SIGTERM)

    def close_terminal(self):
        if self.terminal:
            self.terminal.close()

    def verify_session_stopped(self):
        for pid in (self.listener and self.listener["pid"], self.worker, self.shell):
            if pid:
                native_services.wait_for(lambda: not native_services.alive(pid), "owned session process remains after teardown")


def prove(root, output, host, pair, preparer):
    installation = Installation(root, host, pair, preparer)
    record = {"host": host["name"], "passed": False}
    try:
        record["initial"] = installation.prepare()
        installation.seed()
        record.update(installation.restart_control())
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        if isinstance(error, subprocess.CalledProcessError):
            record["commandError"] = error.stderr[-65536:]
        raise
    finally:
        try:
            installation.close()
            record["cleanup"] = {"servicesAbsent": True, "processesAbsent": True}
        except BaseException as error:
            record["passed"] = False
            record["cleanupError"] = str(error)
            raise
        finally:
            retain_logs(installation.root, output, host["name"])
            (output / (host["name"] + "-native-pair.json")).write_text(json.dumps(record, indent=2) + "\n")
    return record


def retain_logs(root, output, name):
    for filename in ("daemon.log", "daemon.err"):
        path = root / filename
        if path.is_file():
            copy_log_tail(path, output / (name + "-" + filename))


def copy_log_tail(source, destination):
    with source.open("rb") as stream:
        stream.seek(max(0, source.stat().st_size - 65536))
        destination.write_bytes(stream.read(65536))
