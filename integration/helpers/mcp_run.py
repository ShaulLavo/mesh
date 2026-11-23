"""Drive mesh_run through the real mesh mcp server against a local daemon
(Unix socket) and a remote daemon (authenticated WebSocket)."""
import base64
import os
from pathlib import Path
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


def read_reply(client, req_id, timeout=HANG):
    deadline = time.monotonic() + timeout
    while True:
        msg = client.read_message(timeout=max(0.1, deadline - time.monotonic()))
        if msg.get("id") == req_id:
            return msg


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


def exercise_short_controls_beside_a_run(client, handle, label):
    run_id = client.request("tools/call", {"name": "mesh_run", "arguments": {"context": handle, "command": "sleep 1; echo slow"}})
    get_id = client.request("tools/call", {"name": "mesh_context_get", "arguments": {"context": handle}})
    first = client.read_message(timeout=HANG)
    require(first.get("id") == get_id and not first["result"]["isError"], f"{label}: a short control waited behind a run: {first}")
    slow = run_data(read_reply(client, run_id), label)
    require(slow["stdout"]["data"] == "slow\n", f"{label}: slow run {slow}")


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


def main():
    binary = sys.argv[1]
    with tempfile.TemporaryDirectory(prefix="mesh-mcp-run-") as directory:
        root = Path(directory)
        fixture = MCPFixture(binary, root)
        local = None
        try:
            fixture.start_remote()
            local = start_local_daemon(fixture)
            for name in ("runner", "run-stranger"):
                res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
                require(res.returncode == 0, f"identity create {name} failed: {res.stdout!r} {res.stderr!r}")
            client = MCPClient(fixture.binary, fixture.environment, args=("--identity", "runner"))
            try:
                for label, host_id in (("websocket", fixture.remote_id), ("unix", local[2])):
                    work = root / f"work-{label}"
                    work.mkdir()
                    handle = open_context(client, host_id, work, f"run-{label}")
                    exercise_results(client, handle, label)
                    exercise_cancel(client, handle, work, label)
                    exercise_short_controls_beside_a_run(client, handle, label)
                    exercise_refusals(fixture, client, handle, host_id, label)
            finally:
                client.terminate()
        finally:
            if local is not None:
                stop_local_daemon(local[0], local[1])
            fixture.close()
    print("PASS: mesh_run over Unix and WebSocket: results, binary and control output, deadline, cancel kills the command, short controls beside a run, refusals")


if __name__ == "__main__":
    main()
