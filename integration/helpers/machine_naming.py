#!/usr/bin/env python3
"""Exercise destination naming with real disposable daemons and retained terminals."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import re
import socket
import struct
import subprocess
import tempfile

from mesh_control import receive, round_trip
from recovery_transactions import build_control_client
from terminal_window import Fixture, Terminal, PROMPT, eventually, require


def command(fixture, environment, *arguments):
    return subprocess.run([fixture.binary, *arguments], env=environment, capture_output=True, timeout=8)


def remote_request(fixture, host, request, environment=None, pin=None):
    result = subprocess.run([str(fixture.root / "control-client"), host["endpoint"],
                             pin or fixture.remote_id, json.dumps(request)],
                            env=environment or fixture.environment, capture_output=True, timeout=12)
    require(result.returncode == 0, "authenticated naming control did not receive a response")
    return json.loads(result.stdout)


def same_claim(first, second):
    return all(first.get(field) == second.get(field) for field in ("id", "meshIdentity", "machineName", "nameRevision"))


def read_watch(connection):
    header = receive(connection, 5)
    require(header[0] == 1, "name watch returned a non-control frame")
    size = struct.unpack(">I", header[1:])[0]
    require(size <= 4 << 20, "name watch response exceeded frame limit")
    return json.loads(receive(connection, size))


def watch(fixture):
    connection = socket.socket(socket.AF_UNIX)
    connection.settimeout(5)
    connection.connect(str(fixture.remote / "daemon.sock"))
    payload = json.dumps({"type": "state.watch", "requestId": "name-watch",
                          "watch": {"topics": ["host"]}}).encode()
    connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
    return connection, read_watch(connection)


def destination_environment(fixture):
    return fixture.environment | {"MESH_STATE_DIR": str(fixture.remote),
                                  "MESH_CONFIG_DIR": str(fixture.root / "remote-config")}


def start_replacement(fixture, binary, port):
    environment = destination_environment(fixture) | {
        "MESH_FAKE_TAILSCALE_STATUS": str(fixture.root / "tailscale.json"),
        "PATH": str(fixture.root / "bin") + os.pathsep + fixture.environment["PATH"],
    }
    fixture.daemon_log = open(fixture.root / "daemon.log", "ab")
    fixture.daemon = subprocess.Popen([str(binary), "daemon", "--tailnet-port", str(port), "--ssh-port", "0"],
                                     env=environment, stdin=subprocess.DEVNULL,
                                     stdout=fixture.daemon_log, stderr=subprocess.STDOUT)
    def ready():
        try:
            response = round_trip(str(fixture.remote / "daemon.sock"),
                                  {"type": "host.info", "requestId": "restart-name"})
            return response.get("type") == "host.info.result"
        except (OSError, RuntimeError):
            return False
    try:
        eventually(ready, "replacement destination did not start")
    except RuntimeError as error:
        raise RuntimeError(f"{error}; daemon: {(fixture.root / 'daemon.log').read_text()[-2000:]}") from error


def printed_marker(output, marker):
    return re.search(rb"(?:^|[\r\n])" + re.escape(marker.encode()) + rb"\r?\n", output) is not None


def terminal_marker(fixture, terminal, marker, session_identity):
    start = len(terminal.drain())
    terminal.send("printf '" + marker + "\\n'\n")
    terminal.expect(marker.encode() + b"\r\n", since=start)
    require(printed_marker(terminal.drain()[start:], marker), "terminal did not print a complete naming I/O marker")
    require(fixture.shell_identity(terminal) == session_identity, "rename replaced the retained shell or session")


def prove(fixture, args):
    fixture.start_remote()
    host = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]
    port = int(host["endpoint"].split(":")[-1].split("/")[0])
    info_request = {"type": "host.info", "requestId": "naming-baseline"}
    local_socket = str(fixture.remote / "daemon.sock")
    initial = round_trip(local_socket, info_request)["host"]
    require(initial.get("machineName") == "pc" and initial.get("nameRevision") == 1,
            "destination host.info lacks a persisted machine-owned name and revision")
    if args.control_client:
        (fixture.root / "control-client").symlink_to(Path(args.control_client).resolve())
    else:
        build_control_client(fixture)
    require(remote_request(fixture, host, info_request)["host"] == initial,
            "authenticated device and destination disagree on initial claim")
    terminal = Terminal([fixture.binary, "pc"], fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect(PROMPT)
    shell_identity = fixture.shell_identity(terminal)
    session_id, shell_pid = shell_identity
    worker_pid = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(shell_pid)]))
    metadata = (fixture.remote / "s" / session_id / "meta.json").read_bytes()
    terminal_marker(fixture, terminal, "BEFORE_NAME_IO", shell_identity)
    watch_connection, snapshot = watch(fixture)
    try:
        require(snapshot["type"] == "state.snapshot" and snapshot["stateSnapshot"]["host"]["machineName"] == "pc",
                "initial watch snapshot lacks the destination name")
        unauthorized = fixture.environment | {"MESH_STATE_DIR": str(fixture.root / "unauthorized")}
        actor = command(fixture, unauthorized, "device", "identity", "--json")
        require(actor.returncode == 0, "disposable unauthorized identity failed")
        actor_id = json.loads(actor.stdout)["id"]
        rename = {"type": "host.rename", "requestId": "rename-one",
                  "rename": {"targetId": fixture.remote_id, "machineName": " Fixture-Destination ", "expectedRevision": 1}}
        denied = subprocess.run([str(fixture.root / "control-client"), host["endpoint"], fixture.remote_id,
                                 json.dumps(rename)], env=unauthorized, capture_output=True, timeout=12)
        require(denied.returncode != 0, "unapproved device renamed the destination")
        trusted = command(fixture, destination_environment(fixture), "update", "trust", "--", actor_id)
        require(trusted.returncode == 0, "disposable updater trust failed")
        updater = subprocess.run([str(fixture.root / "control-client"), host["endpoint"], fixture.remote_id,
                                  json.dumps(rename)], env=unauthorized, capture_output=True, timeout=12)
        require(updater.returncode != 0, "updater-only authority renamed the destination")
        wrong_pin = subprocess.run([str(fixture.root / "control-client"), host["endpoint"], actor_id,
                                    json.dumps(rename)], env=fixture.environment, capture_output=True, timeout=12)
        require(wrong_pin.returncode != 0, "forged destination pin admitted a rename")
        forged = remote_request(fixture, host, {"type": "host.info.result", "requestId": "forged-name", "host": {
            "id": fixture.remote_id, "meshIdentity": fixture.remote_id, "machineName": "forged-pc", "nameRevision": 999}})
        require(forged["type"] == "error" and forged.get("errorCode") == "control.unknown",
                "peer-supplied host declaration mutated destination state")
        require(round_trip(local_socket, info_request)["host"] == initial, "denied requests changed name or identity")
        invalid = rename | {"rename": rename["rename"] | {"machineName": "ls"}}
        require(remote_request(fixture, host, invalid)["type"] == "error", "reserved destination name accepted")
        wrong_target = rename | {"rename": rename["rename"] | {"targetId": actor_id}}
        refused = remote_request(fixture, host, wrong_target)
        require(refused["type"] == "error" and refused.get("errorCode") == "host.name_target", "wrong exact target accepted")
        receipt = remote_request(fixture, host, rename)
        require(receipt["type"] == "host.renamed" and receipt["host"]["nameRevision"] == 2
                and receipt["host"]["machineName"] == "fixture-destination", "authorized rename did not commit once")
        require("reconnect" in receipt["message"] and "Unreachable" in receipt["message"], "rename promised global uniqueness")
        while True:
            event = read_watch(watch_connection)
            if event["type"] == "state.event" and event["stateEvent"]["kind"] == "host.changed":
                break
        require(same_claim(event["stateEvent"]["payload"]["host"], receipt["host"]), "name publication differs from committed receipt")
        first_claim = receipt["host"]
        for environment in (fixture.environment, destination_environment(fixture)):
            observed = (remote_request(fixture, host, info_request) if environment == fixture.environment
                        else round_trip(local_socket, info_request))
            require(observed["host"] == first_claim, "local and remote name readers disagree after commit")
        retried = remote_request(fixture, host, rename)
        require(retried["host"] == first_claim, "retry advanced destination name revision")
        stale = rename | {"rename": rename["rename"] | {"machineName": "stale-pc"}}
        require(remote_request(fixture, host, stale).get("errorCode") == "host.name_revision", "stale rename replay accepted")
        terminal_marker(fixture, terminal, "AFTER_NAME_IO", shell_identity)
        os.kill(worker_pid, 0)
        require((fixture.remote / "s" / session_id / "meta.json").read_bytes() == metadata, "rename changed retained worker metadata")
        terminal.send(b"\x1d")
        terminal.expect_exit()
        terminal.close()
        fixture.terminals.remove(terminal)
        fixture.stop_daemon()
        name_path = fixture.remote / "machine-name.json"
        saved_name = name_path.read_bytes()
        if args.baseline:
            start_replacement(fixture, args.baseline, port)
            legacy = round_trip(local_socket, info_request)["host"]
            require(legacy["id"] == first_claim["id"], "source baseline changed host identity")
            require(name_path.read_bytes() == saved_name, "source baseline rewrote new name state")
            os.kill(shell_pid, 0)
            os.kill(worker_pid, 0)
            fixture.stop_daemon()
        start_replacement(fixture, fixture.binary, port)
        require(round_trip(local_socket, info_request)["host"] == first_claim, "restart or source transition lost committed name")
        require(remote_request(fixture, host, rename)["host"] == first_claim, "restart lost the idempotent receipt")
        reconnect, current = watch(fixture)
        reconnect.close()
        require(same_claim(current["stateSnapshot"]["host"], first_claim), "reconnect watch did not converge")
        race_names = ["first-race-pc", "second-race-pc"]
        def race(name):
            return remote_request(fixture, host, {"type": "host.rename", "requestId": name,
                                  "rename": {"targetId": fixture.remote_id, "machineName": name, "expectedRevision": 2}})
        with ThreadPoolExecutor(max_workers=2) as pool:
            raced = list(pool.map(race, race_names))
        require(sorted(result["type"] for result in raced) == ["error", "host.renamed"], "simultaneous distinct renames both committed")
        final = remote_request(fixture, host, info_request)["host"]
        require(final["nameRevision"] == 3 and final["id"] == initial["id"], "rename race changed identity or incremented twice")
        require(remote_request(fixture, host, rename).get("errorCode") == "host.name_revision", "old receipt replayed after another rename")
        resumed = Terminal([fixture.binary, session_id], fixture.environment, fixture.root)
        fixture.terminals.append(resumed)
        resumed.expect(PROMPT)
        terminal_marker(fixture, resumed, "RECONNECTED_NAME_IO", shell_identity)
        os.kill(worker_pid, 0)
        if args.evidence:
            output = Path(args.evidence)
            output.mkdir(parents=True, exist_ok=True)
            (output / "retained-terminal.ansi").write_bytes(resumed.drain())
            result = {"os": os.uname().sysname, "initialName": "pc", "renamedName": first_claim["machineName"],
                      "initialRevision": 1, "renamedRevision": 2, "raceRevision": 3,
                      "stableHostID": final["id"] == initial["id"], "retainedSession": session_id,
                      "retainedShellPID": shell_pid, "retainedWorkerPID": worker_pid,
                      "inputOutputBeforeAfterReconnect": True, "unauthorizedAndUpdaterDenied": True,
                      "wrongPinAndTargetDenied": True, "destinationWatchConverged": True,
                      "sourceBaselineTransition": bool(args.baseline), "publishedReleaseTransition": False,
                      "readerCutoverComplete": False}
            (output / "checks.txt").write_text(json.dumps(result, indent=2) + "\n")
        print("PASS: destination name/revision, full-device authorization, forgery/target rejection, durable retry/restart, concurrent rename, watch reconnect, retained worker/shell/session I/O")
    finally:
        watch_connection.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("binary")
    parser.add_argument("--control-client")
    parser.add_argument("--baseline")
    parser.add_argument("--evidence")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="mesh-name-") as directory:
        fixture = Fixture(args.binary, Path(directory))
        try:
            prove(fixture, args)
        finally:
            fixture.close()


if __name__ == "__main__":
    main()
