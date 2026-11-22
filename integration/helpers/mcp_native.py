"""Drive the real mesh mcp stdio server against an isolated second remote daemon."""
import base64
import json
import os
from pathlib import Path
import select
import socket
import subprocess
import sys
import tempfile
import time

from mesh_control import round_trip
from terminal_window import Fixture, eventually, require


class MCPFixture(Fixture):
    def request_worker_kill(self, connections, socket_path, deadline):
        res_path = socket_path.parent / ".reservation"
        if not res_path.exists():
            return super().request_worker_kill(connections, socket_path, deadline)
        ref = json.loads(res_path.read_text())
        if ref.get("purpose") != "pipe-execution":
            return super().request_worker_kill(connections, socket_path, deadline)
        conn = connections.enter_context(socket.socket(socket.AF_UNIX))
        conn.settimeout(deadline.remaining(1, "connect the pipe teardown"))
        try:
            conn.connect(str(socket_path))
        except (ConnectionRefusedError, FileNotFoundError):
            return None
        raw = json.dumps({
            "type": "execution.kill",
            "requestId": "teardown",
            "sessionId": ref["id"],
            "instance": ref["instance"],
        }).encode()
        conn.sendall(b"\x01" + len(raw).to_bytes(4, "big") + raw)
        return conn

PROTO = "2026-07-28"
META = {
    "io.modelcontextprotocol/protocolVersion": PROTO,
    "io.modelcontextprotocol/clientCapabilities": {},
}


class MCPClient:
    def __init__(self, binary, env, args=()):
        self.proc = subprocess.Popen(
            [binary, "mcp", *args],
            env=env,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.next_id = 1

    def send_raw(self, obj):
        line = (json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8")
        self.proc.stdin.write(line)
        self.proc.stdin.flush()

    def request(self, method, params=None, req_id=None, progress_token=None):
        if req_id is None:
            req_id = self.next_id
            self.next_id += 1
        meta = dict(META)
        if progress_token is not None:
            meta["progressToken"] = progress_token
        body = {"_meta": meta}
        if params:
            body.update(params)
        self.send_raw({"jsonrpc": "2.0", "id": req_id, "method": method, "params": body})
        return req_id

    def read_message(self, timeout=15.0):
        ready, _, _ = select.select([self.proc.stdout], [], [], timeout)
        require(bool(ready), f"timed out waiting for MCP stdout message after {timeout}s")
        line = self.proc.stdout.readline()
        require(bool(line), "MCP server closed stdout unexpectedly")
        return json.loads(line.decode("utf-8"))

    def call_tool(self, name, arguments=None, timeout=15.0):
        req_id = self.request("tools/call", {"name": name, "arguments": arguments or {}})
        while True:
            msg = self.read_message(timeout=timeout)
            if "method" in msg:
                continue
            require(msg.get("id") == req_id, f"unexpected reply ID: {msg}")
            return msg

    def close_eof(self, timeout=3.0):
        if self.proc.stdin and not self.proc.stdin.closed:
            self.proc.stdin.close()
        rc = self.proc.wait(timeout=timeout)
        stderr = self.proc.stderr.read().decode("utf-8", errors="replace")
        return rc, stderr

    def terminate(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(timeout=5)


def run_cli(binary, env, *args):
    res = subprocess.run(
        [binary, *args],
        env=env,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=15,
        check=False,
    )
    return res


def create_pipe_job_cli(fixture, host_id, key, command, stdin_mode="closed"):
    res = run_cli(
        fixture.binary,
        fixture.environment,
        "session",
        "create",
        "--host",
        host_id,
        "--kind",
        "pipe",
        "--stdin",
        stdin_mode,
        "--cwd",
        str(fixture.root),
        "--request-key",
        key,
        "--json",
        "--",
        *command,
    )
    require(res.returncode == 0 and not res.stderr, f"CLI session create failed: rc={res.returncode} out={res.stdout!r} err={res.stderr!r}")
    return json.loads(res.stdout)["data"]


def check_usage_and_offline_stdout(binary, root):
    env = dict(os.environ, MESH_CONFIG_DIR=str(root / "offline-cfg"), MESH_STATE_DIR=str(root / "offline-state"))
    for bad_args in (("--bogus",), ("extra-arg",), ("--privacy",)):
        res = run_cli(binary, env, "mcp", *bad_args)
        require(res.returncode == 2 and res.stdout == "" and res.stderr != "", f"bad usage stdout/exit for {bad_args}: rc={res.returncode} out={res.stdout!r} err={res.stderr!r}")

    # Offline MCP invocation returns clean JSON-RPC only on stdout.
    client = MCPClient(binary, env)
    try:
        client.send_raw({"jsonrpc": "2.0", "id": 99, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}}})
        legacy = client.read_message()
        require(legacy.get("error", {}).get("code") == -32601 and PROTO in legacy["error"]["message"], f"legacy initialize not refused: {legacy}")

        skill = client.call_tool("mesh_skill")
        require(not skill["result"]["isError"] and "Mesh" in skill["result"]["content"][0]["text"], f"offline mesh_skill failed: {skill}")

        hosts = client.call_tool("mesh_hosts_list", {"cached": True})
        require(hosts["result"]["structuredContent"]["operation"] == "hosts.list", f"offline hosts failed: {hosts}")
        rc, _ = client.close_eof()
        require(rc == 0, f"offline MCP EOF exit = {rc}")
    finally:
        client.terminate()


def exercise_short_and_long_jobs(fixture):
    host_id = fixture.remote_id

    # 1. Start a short pipe job via CLI on the second authorized daemon, then
    # read it from a FRESH mesh mcp frontend as its VERY FIRST request (no discover/list).
    short_receipt = create_pipe_job_cli(
        fixture,
        host_id,
        "mcp-short-1",
        ["/bin/sh", "-c", "printf 'hello-stdout'; printf 'hello-stderr' >&2"],
    )
    short_handle = f"mj1.{host_id}.{short_receipt['sessionId']}.{short_receipt['instance']}"

    fresh = MCPClient(fixture.binary, fixture.environment)
    try:
        # First request with NO prior server/discover or tools/list!
        wait_resp = fresh.call_tool("mesh_job_wait", {"job": short_handle, "wait_ms": 10000})
        wait_env = wait_resp["result"]["structuredContent"]
        require(not wait_resp["result"]["isError"] and wait_env["status"] == "ok", f"short wait failed: {wait_resp}")
        require(wait_env["data"]["state"] == "exited" and wait_env["data"]["wait"] == "completed", f"short wait state: {wait_env}")
        require(wait_env["data"]["result"]["termination"]["code"] == 0, f"short exit code: {wait_env}")

        # Compare CLI vs MCP receipt & output.
        cli_inspect = run_cli(
            fixture.binary,
            fixture.environment,
            "session",
            "inspect",
            "--host",
            host_id,
            short_receipt["sessionId"],
            "--instance",
            short_receipt["instance"],
            "--json",
        )
        require(cli_inspect.returncode == 0, f"CLI inspect failed: {cli_inspect.stderr}")
        cli_rec = json.loads(cli_inspect.stdout)["data"]

        mcp_status = fresh.call_tool("mesh_job_status", {"job": short_handle})
        mcp_rec = mcp_status["result"]["structuredContent"]["data"]["receipt"]
        for field in ("hostId", "sessionId", "instance", "requestKey", "kind", "phase"):
            require(cli_rec[field] == mcp_rec[field], f"CLI/MCP mismatch on {field}: cli={cli_rec[field]} mcp={mcp_rec[field]}")

        # Output parity on stdout & stderr.
        out_stdout = fresh.call_tool("mesh_job_output", {"job": short_handle, "stream": "stdout"})
        out_stderr = fresh.call_tool("mesh_job_output", {"job": short_handle, "stream": "stderr"})
        require(out_stdout["result"]["structuredContent"]["data"]["data"] == "hello-stdout", f"stdout mismatch: {out_stdout}")
        require(out_stdout["result"]["structuredContent"]["data"]["encoding"] == "utf-8", f"stdout encoding: {out_stdout}")
        require(out_stderr["result"]["structuredContent"]["data"]["data"] == "hello-stderr", f"stderr mismatch: {out_stderr}")

        cli_logs = run_cli(
            fixture.binary,
            fixture.environment,
            "session",
            "logs",
            "--host",
            host_id,
            short_receipt["sessionId"],
            "--instance",
            short_receipt["instance"],
            "--stream",
            "stdout",
            "--json",
        )
        require(base64.b64decode(json.loads(cli_logs.stdout)["data"]["bytesBase64"]) == b"hello-stdout", "CLI logs mismatch")

        # Receipt lookup by request_key.
        rec_lookup = fresh.call_tool("mesh_job_receipt", {"host": host_id, "request_key": "mcp-short-1"})
        require(rec_lookup["result"]["structuredContent"]["data"]["job"] == short_handle, f"receipt lookup mismatch: {rec_lookup}")

        # Discovery, hosts list capabilities, host inspect, jobs list, sessions list, services list.
        disc_id = fresh.request("server/discover")
        disc = fresh.read_message()
        require(disc["id"] == disc_id and "mesh_skill" in disc["result"]["instructions"], f"discover reply: {disc}")

        hosts_resp = fresh.call_tool("mesh_hosts_list", {"timeout_ms": 5000})
        hosts_list = hosts_resp["result"]["structuredContent"]["data"]["hosts"]
        remote_host = next((h for h in hosts_list if h.get("hostId") == host_id), None)
        require(remote_host is not None and remote_host["reachability"] == "reachable", f"remote host missing: {hosts_list}")
        require(remote_host.get("capabilities", {}).get("executionPipe") is True, f"executionPipe capability missing: {remote_host}")

        inspect_resp = fresh.call_tool("mesh_host_inspect", {"host": host_id})
        require(inspect_resp["result"]["structuredContent"]["data"]["host"]["hostId"] == host_id, f"host inspect: {inspect_resp}")

        jobs_resp = fresh.call_tool("mesh_jobs_list", {"host": host_id, "all": True})
        listed_items = jobs_resp["result"]["structuredContent"]["data"]["jobs"]
        listed_jobs = [j["job"] for j in listed_items]
        require(short_handle in listed_jobs, f"short job missing from mesh_jobs_list: {listed_jobs}")
        short_item = next(j for j in listed_items if j["job"] == short_handle)
        require(bool(short_item.get("createdAt")) and not str(short_item["createdAt"]).startswith("0001-"), f"invalid createdAt: {short_item}")

        sessions_resp = fresh.call_tool("mesh_sessions_list", {"host": host_id, "all": True})
        require(not sessions_resp["result"]["isError"], f"sessions list error: {sessions_resp}")

        svc_resp = fresh.call_tool("mesh_services_list", {"host": host_id})
        require(not svc_resp["result"]["isError"], f"services list error: {svc_resp}")

        missing_svc = fresh.call_tool("mesh_service_inspect", {"host": host_id, "name": "does-not-exist"})
        require(missing_svc["result"]["isError"] and missing_svc["result"]["structuredContent"]["errors"][0]["code"] == "service.not_found", f"missing service error: {missing_svc}")

        # Remove finished job output while keeping receipt.
        rm_resp = fresh.call_tool("mesh_job_remove", {"job": short_handle})
        require(not rm_resp["result"]["isError"] and rm_resp["result"]["structuredContent"]["data"]["receipt"]["removal"] == "removed", f"job remove failed: {rm_resp}")

        rc, _ = fresh.close_eof()
        require(rc == 0, f"fresh MCP EOF exit = {rc}")
    finally:
        fresh.terminate()


def exercise_long_job_input_cancel_and_eof(fixture):
    host_id = fixture.remote_id
    # Long interactive pipe job: echoes stdin line, then waits for EOF or signal.
    long_receipt = create_pipe_job_cli(
        fixture,
        host_id,
        "mcp-long-1",
        ["/bin/sh", "-c", "trap '' USR1; read line; printf 'got:%s\\n' \"$line\"; exec /bin/sleep 60"],
        stdin_mode="open",
    )
    long_handle = f"mj1.{host_id}.{long_receipt['sessionId']}.{long_receipt['instance']}"

    client = MCPClient(fixture.binary, fixture.environment)
    try:
        # Write stdin via mesh_job_input, verify output.
        inp = client.call_tool("mesh_job_input", {"job": long_handle, "write_key": "w1", "offset": "0", "data": "ping\n"})
        require(not inp["result"]["isError"] and inp["result"]["structuredContent"]["data"]["length"] == 5, f"job input failed: {inp}")

        def saw_echo():
            r = client.call_tool("mesh_job_output", {"job": long_handle, "stream": "stdout"})
            return r["result"]["structuredContent"]["data"]["data"] == "got:ping\n"

        eventually(saw_echo, "long job did not echo stdin")

        # Start a wait with progressToken, then cancel the WAIT via notifications/cancelled.
        wait_id = client.request("tools/call", {"name": "mesh_job_wait", "arguments": {"job": long_handle, "wait_ms": 30000}}, progress_token="tok-long")
        prog = client.read_message(timeout=5.0)
        require(prog.get("method") == "notifications/progress" and prog["params"]["progressToken"] == "tok-long", f"missing wait progress: {prog}")

        client.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": wait_id, "reason": "stop waiting"}})

        # Job MUST still be running after cancelling the wait!
        status = client.call_tool("mesh_job_status", {"job": long_handle})
        require(status["result"]["structuredContent"]["data"]["state"] == "running", f"wait cancel stopped job: {status}")

        # Now start a second wait and close stdin (EOF) -> frontend exits <= 2s and job STILL keeps running!
        client.request("tools/call", {"name": "mesh_job_wait", "arguments": {"job": long_handle, "wait_ms": 30000}})
        time.sleep(0.2)
        t0 = time.monotonic()
        rc, _ = client.close_eof(timeout=3.0)
        elapsed = time.monotonic() - t0
        require(rc == 0 and elapsed < 2.5, f"stdin EOF took {elapsed:.2f}s rc={rc}")
    finally:
        client.terminate()

    # Fresh frontend confirms the job survived EOF, sends a signal, then cancels it via mesh_job_cancel.
    client2 = MCPClient(fixture.binary, fixture.environment)
    try:
        still = client2.call_tool("mesh_job_status", {"job": long_handle})
        require(still["result"]["structuredContent"]["data"]["state"] == "running", f"EOF killed job: {still}")

        sig = client2.call_tool("mesh_job_signal", {"job": long_handle, "signal": "usr1"})
        require("result" in sig and not sig["result"]["isError"] and sig["result"]["structuredContent"]["data"]["delivery"] == "delivered", f"job signal failed: {sig}")

        cancelled = client2.call_tool("mesh_job_cancel", {"job": long_handle})
        require(not cancelled["result"]["isError"] and cancelled["result"]["structuredContent"]["data"]["delivery"] == "delivered", f"job cancel failed: {cancelled}")

        ended = client2.call_tool("mesh_job_wait", {"job": long_handle, "wait_ms": 10000})
        require(ended["result"]["structuredContent"]["data"]["state"] == "killed", f"job not killed: {ended}")
        rc, _ = client2.close_eof()
        require(rc == 0, f"client2 EOF exit = {rc}")
    finally:
        client2.terminate()


def exercise_host_mismatch_and_revocation(fixture):
    host_id = fixture.remote_id
    hosts_path = fixture.config / "hosts.json"
    original_hosts = hosts_path.read_text()

    # 1. Host identity mismatch: pin a different valid Ed25519 identity for the remote endpoint.
    other_state = fixture.root / "other-identity"
    other_id = json.loads(run_cli(fixture.binary, fixture.environment | {"MESH_STATE_DIR": str(other_state)}, "device", "identity", "--json").stdout)["id"]
    mismatch_cfg = json.loads(original_hosts)
    mismatch_cfg["hosts"][0]["meshIdentity"] = other_id
    mismatch_dir = fixture.root / "mismatch-config"
    mismatch_dir.mkdir()
    (mismatch_dir / "hosts.json").write_text(json.dumps(mismatch_cfg))

    mismatch_env = fixture.environment | {"MESH_CONFIG_DIR": str(mismatch_dir)}
    mm_client = MCPClient(fixture.binary, mismatch_env)
    try:
        bad = mm_client.call_tool("mesh_job_receipt", {"host": host_id, "request_key": "mcp-short-1"})
        code = bad["result"]["structuredContent"]["errors"][0]["code"]
        require(bad["result"]["isError"] and code == "host.identity_mismatch", f"expected host.identity_mismatch, got: {bad}")

        # Keyed input write with mismatched TLS pin must report host.identity_mismatch, not input.outcome_unknown.
        dummy_handle = f"mj1.{host_id}.7K3D.0123456789abcdef0123456789abcdef"
        bad_input = mm_client.call_tool("mesh_job_input", {"job": dummy_handle, "write_key": "pin-test", "offset": "0", "data": "x"})
        input_code = bad_input["result"]["structuredContent"]["errors"][0]["code"]
        require(bad_input["result"]["isError"] and input_code == "host.identity_mismatch", f"expected host.identity_mismatch on job_input, got: {bad_input}")
        mm_client.close_eof()
    finally:
        mm_client.terminate()

    # 2. Revocation during an active wait while the accepted job continues!
    rev_receipt = create_pipe_job_cli(fixture, host_id, "mcp-rev-1", ["/bin/sleep", "60"])
    rev_handle = f"mj1.{host_id}.{rev_receipt['sessionId']}.{rev_receipt['instance']}"
    client_identity = json.loads(run_cli(fixture.binary, fixture.environment, "device", "identity", "--json").stdout)["id"]
    remote_env = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}

    rev_client = MCPClient(fixture.binary, fixture.environment)
    try:
        wait_id = rev_client.request("tools/call", {"name": "mesh_job_wait", "arguments": {"job": rev_handle, "wait_ms": 20000}}, progress_token="rev-wait")
        prog = rev_client.read_message(timeout=5.0)
        require(prog.get("method") == "notifications/progress", f"revocation wait did not start: {prog}")

        # Revoke client device on the remote daemon while wait is live.
        revoked = run_cli(fixture.binary, remote_env, "device", "revoke", "--", client_identity)
        require(revoked.returncode == 0, f"revoke failed: {revoked.stderr}")

        # Active wait fails cleanly with isError:true.
        while True:
            msg = rev_client.read_message(timeout=15.0)
            if "method" in msg:
                continue
            require(msg["id"] == wait_id and msg["result"]["isError"], f"revoked wait did not fail: {msg}")
            break

        # Subsequent MCP call from the revoked client is also denied.
        after = rev_client.call_tool("mesh_job_status", {"job": rev_handle})
        require(after["result"]["isError"], f"revoked client was not denied: {after}")

        # Accepted job on the remote daemon MUST STILL BE LIVE!
        direct = round_trip(
            str(fixture.remote / "daemon.sock"),
            {"type": "execution.inspect", "requestId": "check-live", "sessionId": rev_receipt["sessionId"], "instance": rev_receipt["instance"]},
        )
        require(direct.get("executionReceipt", {}).get("phase") == "live", f"revocation killed accepted job: {direct}")

        # Clean up the remote sleep job directly over the remote local socket and wait for exit.
        round_trip(
            str(fixture.remote / "daemon.sock"),
            {"type": "execution.kill", "requestId": "cleanup-kill", "sessionId": rev_receipt["sessionId"], "instance": rev_receipt["instance"]},
        )
        round_trip(
            str(fixture.remote / "daemon.sock"),
            {"type": "execution.wait", "requestId": "cleanup-wait", "sessionId": rev_receipt["sessionId"], "instance": rev_receipt["instance"], "timeoutMillis": 10000},
        )
        rev_client.close_eof()
    finally:
        rev_client.terminate()


def main():
    binary = sys.argv[1]
    with tempfile.TemporaryDirectory(prefix="mesh-mcp-native-") as directory:
        root = Path(directory)
        check_usage_and_offline_stdout(binary, root)
        (root / "fleet").mkdir()
        fixture = MCPFixture(binary, root / "fleet")
        try:
            fixture.start_remote()
            exercise_short_and_long_jobs(fixture)
            exercise_long_job_input_cancel_and_eof(fixture)
            exercise_host_mismatch_and_revocation(fixture)
        finally:
            fixture.close()
    print("PASS: native MCP stdio server verified across fresh connect, short/long jobs, CLI parity, wait vs job cancel, EOF, identity mismatch, and live revocation")


if __name__ == "__main__":
    main()
