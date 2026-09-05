#!/usr/bin/env python3
"""Crash real workers and exercise one recovery transaction over both transports."""

import base64
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
from urllib.parse import urlparse

sys.dont_write_bytecode = True
from mesh_control import round_trip
from public_http_fixture import receive_frame, receive_headers
from terminal_window import Fixture, Terminal, PROMPT, eventually, require, run_outside_containing_session
from ssh_sessions import SSHFixture


def websocket_request(endpoint, request):
    address = urlparse(endpoint)
    with socket.create_connection((address.hostname, address.port), timeout=10) as connection:
        key = base64.b64encode(os.urandom(16)).decode()
        connection.sendall((f"GET {address.path} HTTP/1.1\r\nHost: {address.netloc}\r\n"
                            "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
                            f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
        response = receive_headers(connection)
        require(response.startswith(b"HTTP/1.1 101 "), f"WebSocket upgrade failed: {response!r}")
        data = json.dumps(request).encode()
        frame = b"\x01" + struct.pack(">I", len(data)) + data
        mask = os.urandom(4)
        masked = bytes(value ^ mask[index % 4] for index, value in enumerate(frame))
        connection.sendall(b"\x82\xfe" + struct.pack(">H", len(frame)) + mask + masked)
        opcode, response = receive_frame(connection)
        require(opcode == 2 and response[0] == 1, f"unexpected recovery frame: {opcode} {response!r}")
        require(struct.unpack(">I", response[1:5])[0] == len(response) - 5, "incomplete Mesh response")
        return json.loads(response[5:])


def kill_worker(state, session_id):
    metadata = json.loads((state / "s" / session_id / "meta.json").read_text())
    parent = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(metadata["pid"])]))
    arguments = subprocess.check_output(["ps", "-o", "args=", "-p", str(parent)]).decode()
    require("session-worker" in arguments and str(state / "s" / session_id) in arguments,
            f"refusing to kill a process outside the fixture: {arguments}")
    os.kill(parent, signal.SIGKILL)
    return metadata["pid"]


def races(fixture):
    fixture.start_remote()
    daemon_socket = str(fixture.remote / "daemon.sock")
    response = round_trip(daemon_socket, {"type": "session.create", "requestId": "race-source",
                          "command": [str(fixture.helpers / "window_shell.sh")], "cwd": str(fixture.root)})
    require(response.get("type") == "session.created", f"create failed: {response}")
    source = response["sessionId"]
    checkpoint = fixture.remote / "s" / source / "recovery.json"
    eventually(checkpoint.exists, "worker never acknowledged a checkpoint")
    kill_worker(fixture.remote, source)
    endpoint = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]["endpoint"]
    barrier = threading.Barrier(12)
    clients = []
    local_environment = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
    for _ in range(4):
        client = Terminal([fixture.binary, "recover", source], local_environment, fixture.root)
        fixture.terminals.append(client)
        clients.append(client)

    def recover(index):
        request = {"type": "session.recover", "requestId": f"race-{index}", "sessionId": source}
        barrier.wait(timeout=3)
        if index % 2:
            return websocket_request(endpoint, request)
        return round_trip(daemon_socket, request)

    try:
        with ThreadPoolExecutor(max_workers=12) as pool:
            responses = list(pool.map(recover, range(12)))
        for response in responses:
            require(response.get("type") == "session.recovered", f"recovery failed: {response}")
        replacements = {response["recoveryResult"]["sessionId"] for response in responses}
        require(len(replacements) == 1 and source not in replacements, f"duplicate replacements: {replacements}")
        replacement = replacements.pop()
        directories = list((fixture.remote / "s").glob("*/meta.json"))
        require(len(directories) == 2, f"racing callers published {len(directories) - 1} replacements")
        metadata = json.loads((fixture.remote / "s" / replacement / "meta.json").read_text())
        require(metadata.get("recoveredFrom") == source, f"replacement lost its source: {metadata}")
        require(checkpoint.exists(), "recovery removed the previous checkpoint")
        repeated = round_trip(daemon_socket, {"type": "session.recover", "requestId": "lost-reply-retry", "sessionId": source})
        require(repeated["sessionId"] == replacement, f"retry created another attempt: {repeated}")
        attached = [assert_recovery_client(fixture, client, replacement) for client in clients]
        require(sum(attached) == 1, f"detached-only recovery attached {sum(attached)} direct clients")
    finally:
        for client in clients:
            client.crash()
    print("PASS: concurrent direct clients, Unix requests and WebSocket requests published one retained replacement")


def assert_recovery_client(fixture, client, expected_id):
    def answered():
        output = client.drain()
        return PROMPT in output or client.poll() is not None
    eventually(answered, "recovery client neither attached nor returned an error")
    if client.poll() is None:
        actual_id, _ = fixture.shell_identity(client)
        require(actual_id == expected_id, f"recovery client attached {actual_id}, expected {expected_id}")
        return True
    output = client.drain()
    require(client.poll() == 1 and f"session {expected_id}: session is already attached".encode() in output,
            f"recovery client did not resolve the expected replacement: status={client.poll()}, output={output!r}")
    return False


def ssh_recovery(fixture):
    fixture.start_ssh()
    daemon_socket = str(fixture.local / "daemon.sock")
    created = round_trip(daemon_socket, {"type": "session.create", "requestId": "ssh-recovery-source",
                         "command": [str(fixture.helpers / "window_shell.sh")], "cwd": str(fixture.root)})
    require(created.get("type") == "session.created", f"SSH fixture create failed: {created}")
    source = created["sessionId"]
    marker = fixture.root / "explicit-recipe"
    recipe = {"argv": ["/bin/sh", "-c", 'printf "%s" "$1" > "$2"; exec "$3"', "mesh-recipe",
                       "literal argument with spaces", str(marker), str(fixture.helpers / "window_shell.sh")],
              "cwd": str(fixture.root)}
    configured = round_trip(daemon_socket, {"type": "session.recovery-command", "requestId": "ssh-save-recipe",
                            "sessionId": source, "recoveryCommand": recipe})
    require(configured.get("type") == "ok", f"recipe acknowledgement failed: {configured}")
    kill_worker(fixture.local, source)
    recovered = fixture.ssh_terminal(["recover", source])
    with ThreadPoolExecutor(max_workers=4) as pool:
        responses = list(pool.map(lambda index: round_trip(daemon_socket, {
            "type": "session.recover", "requestId": f"ssh-race-{index}", "sessionId": source,
        }), range(4)))
    recovered.expect(PROMPT)
    replacement, _ = fixture.shell_identity(recovered)
    require(replacement != source, "SSH recovery reused the interrupted source")
    for response in responses:
        require(response.get("type") == "session.recovered" and response["sessionId"] == replacement,
                f"SSH and daemon recovery chose different replacements: {response}")
    require(not marker.exists(), "default SSH recovery executed the explicit recipe")
    conflict = fixture.ssh_terminal(["recover", source])
    assert_recovery_client(fixture, conflict, replacement)
    require(conflict.poll() == 1, "SSH recovery took over an attached replacement")
    kill_worker(fixture.local, replacement)
    recovered.crash()
    explicit = fixture.ssh_terminal(["recover", source, "--command"])
    explicit.expect(PROMPT)
    current, _ = fixture.shell_identity(explicit)
    require(current != replacement, "SSH command recovery reused the previous attempt")
    require(marker.read_text() == "literal argument with spaces", "SSH command recovery changed recipe argv")
    kill_worker(fixture.local, current)
    explicit.crash()
    marker.unlink()
    shell = fixture.ssh_terminal(["recover", current, "--shell"])
    shell.expect(PROMPT)
    shell_id, _ = fixture.shell_identity(shell)
    require(shell_id != current and not marker.exists(), "SSH --shell executed the saved recipe")
    require((fixture.local / "s" / source / "recovery.json").exists(), "SSH recovery removed previous output")
    invalid = fixture.exec_ssh("recover", source)
    require(invalid.returncode != 0 and b"require a terminal" in invalid.stderr, "SSH recovery accepted a missing PTY")
    print("PASS: real OpenSSH shares daemon recovery, preserves ownership and argv, and requires explicit command execution")


def saved_target(path):
    try:
        return json.loads(path.read_text()).get("remote")
    except (FileNotFoundError, json.JSONDecodeError):
        return None


def remote_survival(fixture):
    original, outer_id, outer_pid, inner_id, inner_pid = fixture.nested()
    checkpoint = fixture.local / "s" / outer_id / "recovery.json"
    expected = {"hostId": fixture.remote_id, "sessionId": inner_id}
    eventually(lambda: saved_target(checkpoint) == expected, "outer checkpoint lost the exact remote identity")
    fixture.crash_worker(original, outer_id, outer_pid)
    eventually(lambda: not fixture.attached(fixture.remote, inner_id), "crashed local chain remained attached remotely")
    restored = Terminal([fixture.binary, "recover", outer_id], fixture.environment, fixture.root)
    fixture.terminals.append(restored)
    restored.expect(PROMPT)
    require(fixture.shell_identity(restored) == (inner_id, inner_pid), "recovery selected a different remote process")
    require(len(list((fixture.local / "s").glob("*/meta.json"))) == 1, "remote recovery recreated the local shell chain")
    conflict = subprocess.run([fixture.binary, "recover", outer_id], env=fixture.environment,
                              stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
    require(conflict.returncode != 0 and b"attached" in conflict.stderr,
            f"remote recovery stole an attached target: {conflict.stderr!r}")
    kill_worker(fixture.remote, inner_id)
    restored.crash()
    restarted = Terminal([fixture.binary, "recover", outer_id], fixture.environment, fixture.root)
    fixture.terminals.append(restarted)
    restarted.expect(PROMPT)
    replacement_id, _ = fixture.shell_identity(restarted)
    require(replacement_id != inner_id, "interrupted remote target reused its old session ID")
    replacement = json.loads((fixture.remote / "s" / replacement_id / "meta.json").read_text())
    require(replacement.get("recoveredFrom") == inner_id, "remote target recovery lost attempt ancestry")
    kill_worker(fixture.remote, replacement_id)
    restarted.crash()
    continued = Terminal([fixture.binary, "recover", outer_id], fixture.environment, fixture.root)
    fixture.terminals.append(continued)
    continued.expect(PROMPT)
    current_id, _ = fixture.shell_identity(continued)
    current = json.loads((fixture.remote / "s" / current_id / "meta.json").read_text())
    require(current.get("recoveredFrom") == replacement_id,
            "old remote hint did not follow an interrupted replacement to the current attempt")
    require(saved_target(checkpoint) == expected, "remote recovery rewrote the local hint")
    require(len(list((fixture.local / "s").glob("*/meta.json"))) == 1, "interrupted remote recovery created a local shell")
    missing = round_trip(str(fixture.remote / "daemon.sock"), {
        "type": "session.remove", "requestId": "forget-exact-target", "sessionId": inner_id,
    })
    require(missing.get("type") == "ok", f"could not forget old remote target: {missing}")
    failure = subprocess.run([fixture.binary, "recover", outer_id], env=fixture.environment,
                             stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=8)
    require(failure.returncode != 0 and b"missing" in failure.stderr,
            f"missing exact target selected another remote session: {failure.stderr!r}")
    fixture.stop_daemon()
    failure = subprocess.run([fixture.binary, "recover", outer_id], env=fixture.environment,
                             stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=8)
    require(failure.returncode != 0 and b"saved target unavailable" in failure.stderr,
            f"offline target lost its retry context: {failure.stderr!r}")
    require(saved_target(checkpoint) == expected, "missing or offline target erased the saved hint")
    fallback = Terminal([fixture.binary, "recover", outer_id, "--shell"], fixture.environment, fixture.root)
    fixture.terminals.append(fallback)
    fallback.expect(PROMPT)
    local_id, _ = fixture.shell_identity(fallback)
    require((fixture.local / "s" / local_id / "meta.json").exists(), "explicit shell did not recover locally")
    print("PASS: exact remote survival, attachment ownership, repeated remote restart, missing/offline targets and local shell")


def main():
    mode, binary = sys.argv[1:]
    with tempfile.TemporaryDirectory(prefix="mesh-recovery-") as root:
        fixture_type = SSHFixture if mode == "ssh" else Fixture
        fixture = fixture_type(binary, root)
        try:
            if mode == "races":
                races(fixture)
            elif mode == "ssh":
                ssh_recovery(fixture)
            else:
                remote_survival(fixture)
        except Exception:
            for terminal in fixture.terminals:
                print(terminal.drain().decode(errors="replace"), file=sys.stderr)
            if fixture.daemon_log is not None:
                fixture.daemon_log.flush()
                print((fixture.root / "daemon.log").read_text(), file=sys.stderr)
            raise
        finally:
            fixture.close()


def run():
    try:
        main()
        return 0
    except Exception as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(run))
