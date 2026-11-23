"""Drive mesh_run through the real mesh mcp server against a local daemon
(Unix socket) and a remote daemon (authenticated WebSocket)."""
import base64
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import time

from mcp_native import MCPClient, MCPFixture, require_tool_error, run_cli, tool_data
from mesh_control import round_trip
from terminal_window import eventually, require

HANG = 30.0
MAX_OUTPUT = 262144


def call(client, name, arguments, timeout=HANG):
    """call_tool that skips replies to earlier, cancelled requests."""
    req_id = client.request("tools/call", {"name": name, "arguments": arguments})
    return read_reply(client, req_id, timeout)


class Replies:
    """Unbuffered line reader for the server's stdout. MCPClient.read_message
    selects on the descriptor but reads through a buffered file, so replies
    that arrive together wait in that buffer while select sees nothing."""

    def __init__(self, client):
        self.fd = client.proc.stdout.fileno()
        self.buffer = b""
        # Replies read while waiting for another ID, kept for their caller.
        self.kept = {}

    def next(self, deadline):
        while b"\n" not in self.buffer:
            remaining = deadline - time.monotonic()
            require(remaining > 0, "timed out waiting for an MCP message")
            ready, _, _ = select.select([self.fd], [], [], remaining)
            if ready:
                chunk = os.read(self.fd, 1 << 16)
                require(bool(chunk), "MCP server closed stdout unexpectedly")
                self.buffer += chunk
        line, self.buffer = self.buffer.split(b"\n", 1)
        return json.loads(line)


def replies(client):
    if not hasattr(client, "replies"):
        client.replies = Replies(client)
    return client.replies


def read_message(client, timeout=HANG):
    return replies(client).next(time.monotonic() + timeout)


def read_reply(client, req_id, timeout=HANG):
    deadline = time.monotonic() + timeout
    reader = replies(client)
    while req_id not in reader.kept:
        msg = reader.next(deadline)
        if "id" in msg:
            reader.kept[msg["id"]] = msg
    return reader.kept.pop(req_id)


def run_data(reply, context):
    require("result" in reply and not reply["result"]["isError"], f"{context}: mesh_run failed: {reply}")
    return tool_data(reply)["data"]


def pid_gone(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return True
    return False


def read_pid(path):
    eventually(lambda: path.exists() and path.read_text().strip().isdigit(), f"no pid written to {path}")
    return int(path.read_text().strip())


def open_context(client, host_id, work, key):
    opened = call(client, "mesh_context_open", {"host": host_id, "request_key": key, "cwd": str(work), "shell": "sh"})
    require(not opened["result"]["isError"], f"context open on {host_id} failed: {opened}")
    return tool_data(opened)["data"]["handle"]


def exercise_results(client, handle, label):
    done = run_data(call(client, "mesh_run", {"context": handle, "command": "printf out; printf err >&2; exit 3"}), label)
    require(done["state"] == "exited" and done["termination"] == {"kind": "exit", "code": 3, "signal": None}, f"{label}: exit result {done}")
    require(done["stdout"]["data"] == "out" and done["stderr"]["data"] == "err", f"{label}: streams {done}")
    require("job" not in done and "requestKey" not in done, f"{label}: a run must carry no handle: {done}")

    # Maximum-size binary output survives base64 on both hops, with the rest counted.
    big = run_data(call(client, "mesh_run", {"context": handle, "command": "head -c 300000 /dev/urandom", "output_bytes": MAX_OUTPUT}), label)
    stdout = big["stdout"]
    require(stdout["encoding"] == "base64" and len(base64.b64decode(stdout["data"])) == MAX_OUTPUT, f"{label}: binary page {stdout['encoding']} {len(stdout['data'])}")
    require(stdout["totalBytes"] == 300000 and stdout["truncated"], f"{label}: binary totals {stdout['totalBytes']} {stdout['truncated']}")

    # Terminal control characters stay escaped in the rendered text.
    reply = call(client, "mesh_run", {"context": handle, "command": "printf 'a\\033[2Jb\\007'"})
    text = reply["result"]["content"][0]["text"]
    require("\x1b" not in text and "\x07" not in text and "\\u001b" in text, f"{label}: control characters reached the text: {text!r}")

    timed = run_data(call(client, "mesh_run", {"context": handle, "command": "exec sleep 30", "timeout_ms": 300}), label)
    require(timed["state"] == "timed_out" and timed["termination"]["kind"] == "signal" and timed["termination"]["signal"] == signal.SIGTERM, f"{label}: deadline {timed}")


def exercise_cancel(client, handle, work, label):
    pid_file = work / f"cancel-{label}.pid"
    req_id = client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": f"echo $$ > {pid_file}; exec sleep 30"}})
    pid = read_pid(pid_file)
    client.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": req_id, "reason": "stop"}})
    eventually(lambda: pid_gone(pid), f"{label}: a cancelled run kept its command {pid}")
    # The frontend keeps working on a fresh pooled connection.
    again = run_data(call(client, "mesh_run", {"context": handle, "argv": ["/bin/echo", "-n", "after"]}), label)
    require(again["stdout"]["data"] == "after", f"{label}: run after cancel {again}")


def gated_command(ready, release):
    """A command that reports it started, then waits until released."""
    return f"touch {ready}; while [ ! -e {release} ]; do sleep 0.05; done; echo slow"


def exercise_short_controls_beside_a_run(client, handle, work, label):
    ready, release = work / f"beside-{label}.ready", work / f"beside-{label}.release"
    run_id = client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": gated_command(ready, release)}})
    eventually(ready.exists, f"{label}: the gated run never started")
    get_id = client.request("tools/call", {"name": "mesh_context_get", "arguments": {"context": handle}})
    first = read_message(client)
    require(first.get("id") == get_id and not first["result"]["isError"], f"{label}: a short control waited behind a run: {first}")
    release.touch()
    slow = run_data(read_reply(client, run_id), label)
    require(slow["stdout"]["data"] == "slow\n", f"{label}: slow run {slow}")


def exercise_revocation(fixture, client, handle, work):
    """Revoking the device mid-run kills the command's group and returns its
    slot: once re-approved, a full set of concurrent runs is admitted."""
    pid_file = work / "revoked.pid"
    req_id = client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": f"echo $$ > {pid_file}; exec sleep 30"}})
    pid = read_pid(pid_file)
    device = json_field(run_cli(fixture.binary, fixture.environment, "device", "identity", "--json"), "id")
    remote_env = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
    revoked = run_cli(fixture.binary, remote_env, "device", "revoke", "--", device)
    require(revoked.returncode == 0, f"revoke failed: {revoked.stderr!r}")
    lost = read_reply(client, req_id)
    require("result" in lost and lost["result"]["isError"], f"a revoked run reported success: {lost}")
    eventually(lambda: pid_gone(pid), f"a revoked run kept its command {pid}")
    approved = run_cli(fixture.binary, remote_env, "device", "approve", "--allow-root", "--", device)
    require(approved.returncode == 0, f"re-approve failed: {approved.stderr!r}")
    run_full_slot_set(client, handle, work)


def run_full_slot_set(client, handle, work, count=8):
    """Holds count runs at once; one charge still held would refuse the last."""
    release = work / "slots.release"
    gates = [work / f"slot-{index}.ready" for index in range(count)]
    ids = [client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": gated_command(gate, release)}}) for gate in gates]
    eventually(lambda: all(gate.exists() for gate in gates), "not every concurrent run started")
    release.touch()
    for req_id in ids:
        run_data(read_reply(client, req_id), "concurrent run")


def exercise_daemon_stop(client, handle, work, local):
    """Stopping the daemon mid-run cleans up the command's group."""
    pid_file = work / "stopped.pid"
    req_id = client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": f"echo $$ > {pid_file}; exec sleep 30"}})
    pid = read_pid(pid_file)
    stop_local_daemon(local[0], local[1])
    lost = read_reply(client, req_id)
    require("result" in lost and lost["result"]["isError"], f"a run cut off by a daemon stop reported success: {lost}")
    eventually(lambda: pid_gone(pid), f"a daemon stop left the command {pid}")


def json_field(result, name):
    require(result.returncode == 0, f"command failed: {result.stderr!r}")
    return json.loads(result.stdout)[name]


def exercise_host(fixture, client, host_id, label):
    work = fixture.root / f"work-{label}"
    work.mkdir()
    handle = open_context(client, host_id, work, f"run-{label}")
    exercise_results(client, handle, label)
    exercise_cancel(client, handle, work, label)
    exercise_short_controls_beside_a_run(client, handle, work, label)
    exercise_refusals(fixture, client, handle, host_id, label)
    return handle, work


def exercise_refusals(fixture, client, handle, host_id, label):
    stranger = MCPClient(fixture.binary, fixture.environment, args=("--identity", "run-stranger"))
    try:
        require_tool_error(call(stranger, "mesh_run", {"context": handle, "command": "true"}), "context.not_found", f"{label}: another identity")
    finally:
        stranger.terminate()
    closing = open_context(client, host_id, fixture.root, f"run-close-{label}")
    closed = call(client, "mesh_context_close", {"context": closing})
    require(not closed["result"]["isError"], f"{label}: close failed: {closed}")
    require_tool_error(call(client, "mesh_run", {"context": closing, "command": "true"}), "context.closed", f"{label}: closed context")


def start_local_daemon(fixture):
    log = open(fixture.root / "local-daemon.log", "ab")
    daemon = subprocess.Popen([fixture.binary, "daemon", "--tailnet-port", "0", "--ssh-port", "0"], env=fixture.environment,
                              stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
    eventually(lambda: (fixture.local / "daemon.sock").exists(), "local daemon did not create its socket")
    info = round_trip(str(fixture.local / "daemon.sock"), {"type": "host.info", "requestId": "run-fixture"})
    return daemon, log, info["host"]["id"]


def stop_local_daemon(daemon, log):
    daemon.terminate()
    try:
        daemon.wait(timeout=10)
    except subprocess.TimeoutExpired:
        daemon.kill()
        daemon.wait(timeout=HANG)
    log.close()


def create_identities(fixture):
    for name in ("runner", "run-stranger"):
        res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        require(res.returncode == 0, f"identity create {name} failed: {res.stdout!r} {res.stderr!r}")


def exercise_all(fixture, local):
    client = MCPClient(fixture.binary, fixture.environment, args=("--identity", "runner"))
    try:
        remote_handle, remote_work = exercise_host(fixture, client, fixture.remote_id, "websocket")
        local_handle, local_work = exercise_host(fixture, client, local[2], "unix")
        exercise_revocation(fixture, client, remote_handle, remote_work)
        exercise_daemon_stop(client, local_handle, local_work, local)
    finally:
        client.terminate()


def run_fixture(binary, root):
    fixture = MCPFixture(binary, root)
    local = None
    try:
        fixture.start_remote()
        local = start_local_daemon(fixture)
        create_identities(fixture)
        exercise_all(fixture, local)
    finally:
        # Stopping an already stopped daemon is harmless.
        if local is not None:
            stop_local_daemon(local[0], local[1])
        fixture.close()


def main():
    with tempfile.TemporaryDirectory(prefix="mesh-mcp-run-") as directory:
        run_fixture(sys.argv[1], Path(directory))
    print("PASS: mesh_run over Unix and WebSocket: results, binary and control output, deadline, cancel, short controls beside a run, refusals, revocation and daemon stop clean up the group")


if __name__ == "__main__":
    main()
