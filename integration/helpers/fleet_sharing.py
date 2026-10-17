#!/usr/bin/env python3
"""Prove controller connection sharing with isolated real daemons and CLI clients."""

from contextlib import ExitStack
import base64
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile

from isolation import check_environment, refuse_live_paths, termination_cleanup
from mesh_control import request_id, round_trip
from terminal_window import Fixture, Terminal, eventually, require, run_outside_containing_session


class Device(Fixture):
    def __init__(self, binary, root, name):
        self.local, self.remote = Path(root) / "local", Path(root) / "remote"
        self.terminals, self.daemon, self.daemon_log = [], None, None
        super().__init__(binary, root)
        self.name = name
        self.port = unused_port()
        tools = self.root / "bin"
        tools.mkdir()
        (tools / "tailscale").symlink_to(self.helpers / "fake_tailscale")
        status = self.root / "tailscale.json"
        status.write_text(json.dumps({
            "BackendState": "Running",
            "Self": {"DNSName": name + ".fixture.test.", "HostName": name,
                     "TailscaleIPs": ["127.0.0.1"], "Online": True},
            "Peer": {},
        }))
        self.environment.update({"MESH_FAKE_TAILSCALE_STATUS": str(status),
                                 "PATH": str(tools) + os.pathsep + self.environment["PATH"]})
        self.id = json.loads(self.command("device", "identity", "--json").stdout)["id"]

    def command(self, *arguments, success=True):
        self.validate_destinations()
        result = subprocess.run([self.binary, *arguments], env=self.environment,
                                stdin=subprocess.DEVNULL, capture_output=True, timeout=8)
        if success:
            require(result.returncode == 0,
                    f"{self.name} {arguments!r} failed: {result.stdout!r} {result.stderr!r}")
        return result

    def start(self):
        require(self.daemon is None, f"{self.name} daemon already running")
        self.daemon_log = open(self.root / "daemon.log", "ab")
        self.daemon = subprocess.Popen(
            [self.binary, "daemon", "--tailnet-port", str(self.port), "--ssh-port", "0"],
            env=self.environment, stdin=subprocess.DEVNULL,
            stdout=self.daemon_log, stderr=subprocess.STDOUT,
        )
        eventually(self.ready, f"{self.name} daemon did not start")
        response = round_trip(str(self.local / "daemon.sock"),
                              {"type": "host.info", "requestId": request_id("fleet-host-info")})
        require(response.get("type") == "host.info.result" and "host" in response,
                f"{self.name} daemon rejected host.info: {response}")
        info = response["host"]
        require(info["id"] == self.id and info["machineName"] == self.name,
                f"{self.name} daemon declared the wrong identity or name: {info}")

    def ready(self):
        if self.daemon.poll() is not None:
            raise RuntimeError(f"{self.name} daemon exited: {(self.root / 'daemon.log').read_text()}")
        if not (self.local / "daemon.sock").exists():
            return False
        try:
            with socket.create_connection(("127.0.0.1", self.port), timeout=0.1):
                return True
        except OSError:
            return False

    def record(self):
        return {"id": self.id, "meshIdentity": self.id, "tailscaleName": self.name + ".fixture.test",
                "addresses": ["127.0.0.1"], "endpoint": f"ws://127.0.0.1:{self.port}/mesh"}

    def save_connections(self, *devices):
        path = self.config / "hosts.json"
        refuse_live_paths({"connections": str(path)})
        path.write_text(json.dumps({"version": 1, "hosts": [device.record() for device in devices]}))
        path.chmod(0o600)

    def approve(self, *devices):
        for device in devices:
            self.command("device", "approve", "--allow-root", "--", device.id)

    def book(self):
        path = self.config / "connection-sharing.json"
        return json.loads(path.read_text()) if path.exists() else {"peers": []}

    def shared_ids(self):
        return {host["id"] for peer in self.book()["peers"] for host in peer.get("hosts", [])}

    def controller_ids(self):
        return {peer["identity"] for peer in self.book()["peers"]}

    def close(self):
        process = self.daemon
        super().close()
        require(process is None or process.poll() is not None, "fixture daemon survived cleanup")
        require(not (self.local / "daemon.sock").exists(), "fixture daemon socket survived cleanup")


def unused_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def execute_on(source, destination, marker, devices):
    command = ["/bin/sh", "-c", "printf '%s\\n%s\\n' " + marker + ' "$MESH_HOST_ID"']
    terminal = Terminal([source.binary, destination.name, "--", *command],
                        source.environment, source.root, timeout=8)
    source.terminals.append(terminal)
    terminal.expect(marker.encode() + b"\r\n")
    terminal.expect(destination.id.encode() + b"\r\n")
    terminal.expect_exit()
    require(terminal.status == 0, f"{source.name} command failed: {terminal.drain()!r}")
    records = matching_sessions(destination, command)
    require(len(records) == 1, f"command did not create exactly one session on {destination.name}: {records}")
    for other in devices:
        if other is destination:
            continue
        require(not matching_sessions(other, command), f"command was relayed or executed on {other.name}")


def matching_sessions(device, command):
    records = [json.loads(path.read_text()) for path in (device.local / "s").glob("*/meta.json")]
    return [record for record in records if record["command"] == command]


def assert_new_server_grants(server, *controllers):
    contents = (server.local / "authorized_keys").read_text()
    for controller in controllers:
        key = base64.urlsafe_b64decode(controller.id + "=")
        wire = b"\0\0\0\x0bssh-ed25519\0\0\0\x20" + key
        require(base64.b64encode(wire).decode() in contents,
                f"new server {server.name} did not approve controller {controller.name}")


def pair_controllers(pc, mac):
    pc.save_connections(mac)
    mac.save_connections(pc)
    pc.approve(mac)
    mac.approve(pc)
    pc.command("fleet", "trust", mac.id, "--yes")
    mac.command("fleet", "trust", pc.id, "--yes")


def offline_addition(pc, mac, first, devices):
    mac.stop_daemon()
    original_connections = (mac.config / "hosts.json").read_bytes()
    first.approve(pc, mac)
    assert_new_server_grants(first, pc, mac)
    pc.save_connections(mac, first)
    require(first.id not in mac.shared_ids(), "offline Mac imported the new server")
    require(not (mac.config / "machine-names" / (first.id + ".json")).exists(),
            "Mac already knew the new server's name")
    mac.start()
    execute_on(mac, first, "MAC_DISCOVERED_PC_ADDITION", devices)
    require(first.id in mac.shared_ids(), "Mac did not retain the PC connection snapshot")
    require((mac.config / "hosts.json").read_bytes() == original_connections,
            "propagation copied the PC address book into Mac configuration")
    pc.stop_daemon()
    mac.stop_daemon()
    mac.start()
    execute_on(mac, first, "MAC_USED_CACHE_WITH_PC_OFFLINE", devices)
    require(first.id in mac.shared_ids(), "offline controller lost the retained connection")
    pc.start()


def reverse_unknown_name(pc, mac, second, devices):
    pc.stop_daemon()
    second.approve(pc, mac)
    assert_new_server_grants(second, pc, mac)
    mac.save_connections(pc, second)
    require(second.id not in pc.shared_ids(), "PC already imported the reverse connection")
    original_connections = (pc.config / "hosts.json").read_bytes()
    execute_on(pc, second, "PC_DISCOVERED_MAC_ADDITION", devices)
    require(second.id in pc.shared_ids(), "unknown name did not pull Mac connections")
    require((pc.config / "hosts.json").read_bytes() == original_connections,
            "reverse propagation rewrote locally adopted connections")
    pc.start()


def target_is_not_controller(pc, mac, target):
    pc.approve(target)
    target.save_connections(pc)
    target.command("fleet", "trust", pc.id, "--yes")
    result = target.command("fleet", "sync", success=False)
    require(result.returncode != 0, "ordinary approved target fetched the controller registry")
    require(not target.shared_ids(), "refused target retained shared controller connections")
    require(pc.controller_ids() == {mac.id}, "adopting or approving a server made it a controller")
    require(mac.controller_ids() == {pc.id}, "imported server became a controller on Mac")


def revoked_controller_cannot_fetch(pc, mac):
    previous = mac.shared_ids()
    pc.command("device", "revoke", mac.id)
    result = mac.command("fleet", "sync", success=False)
    require(result.returncode != 0, "trusted controller fetched connections after device approval was revoked")
    require(mac.shared_ids() == previous, "failed controller authentication discarded its retained snapshot")
    pc.approve(mac)
    mac.command("fleet", "sync")


def untrust_removes_imports(pc, mac, first, second, devices):
    mac.command("fleet", "untrust", pc.id, "--yes")
    require(not mac.controller_ids() and not mac.shared_ids(), "untrust retained controller imports")
    mac.command("fleet", "sync")
    result = mac.command(first.name, "--", "/bin/true", success=False)
    require(result.returncode != 0 and b"neither a declared machine name" in result.stderr,
            f"untrusted controller still supplied a target: {result.stdout!r} {result.stderr!r}")
    require(not mac.shared_ids(), "unknown-name discovery reimported an untrusted controller")
    execute_on(mac, second, "MAC_KEPT_LOCAL_CONNECTION_AFTER_UNTRUST", devices)


def prove(binary, root):
    devices = []
    with ExitStack() as cleanup:
        for name in ("pc", "mac", "upcloud-4", "upcloud-2"):
            directory = root / name
            directory.mkdir(mode=0o700)
            device = Device.__new__(Device)
            cleanup.callback(device.close)
            device.__init__(binary, directory, name)
            devices.append(device)
        pc, mac, first, second = devices
        for device in devices:
            device.start()
        pair_controllers(pc, mac)
        offline_addition(pc, mac, first, devices)
        reverse_unknown_name(pc, mac, second, devices)
        revoked_controller_cannot_fetch(pc, mac)
        target_is_not_controller(pc, mac, first)
        untrust_removes_imports(pc, mac, first, second, devices)


def run():
    check_environment()
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("fleet sharing exceeded 25 seconds")))
    signal.alarm(25)
    try:
        with termination_cleanup(), tempfile.TemporaryDirectory(prefix="mesh-fleet-sharing-") as directory:
            prove_with_logs(sys.argv[1], Path(directory))
    finally:
        signal.alarm(0)
    print("PASS: real daemon offline discovery, reverse unknown-name sync, cached direct execution, controller admission and untrust")


def prove_with_logs(binary, root):
    try:
        prove(binary, root)
    except BaseException:
        for path in sorted(root.glob("*/daemon.log")):
            print(f"{path.parent.name}: {path.read_text()[-4000:]}", file=sys.stderr)
        raise


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(run))
