"""Drive the real mesh mcp stdio server against an isolated second remote daemon."""
import base64
import hashlib
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
    def __init__(self, binary, root):
        super().__init__(binary, root)
        # MCP integration tests exercise real shell="login" contexts rather than
        # interactive PTY window_shell.sh tabs.
        self.environment["SHELL"] = "/bin/bash" if Path("/bin/bash").exists() else "/bin/sh"

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


def tool_data(reply):
    require("result" in reply, f"tool call failed at the protocol level: {reply}")
    return reply["result"]["structuredContent"]


def require_tool_error(reply, code, context):
    require("result" in reply and reply["result"]["isError"], f"{context}: expected a tool error, got {reply}")
    got = reply["result"]["structuredContent"]["errors"][0]["code"]
    require(got == code, f"{context}: expected {code}, got {got}: {reply}")


def exercise_file_tools(fixture):
    host_id = fixture.remote_id
    work = fixture.root / "filetools"
    work.mkdir()
    client = MCPClient(fixture.binary, fixture.environment)
    try:
        hosts = client.call_tool("mesh_hosts_list", {"timeout_ms": 5000})
        remote = next(h for h in tool_data(hosts)["data"]["hosts"] if h.get("hostId") == host_id)
        require(remote["capabilities"].get("files") is True, f"files capability missing: {remote}")

        # Create, then read back, with the exact target on every result.
        path = str(work / "a.txt")
        created = client.call_tool("mesh_file_write", {"host": host_id, "path": path, "content": "hello\n", "create_only": True})
        data = tool_data(created)["data"]
        require(not created["result"]["isError"] and data["created"] and data["size"] == "6", f"create failed: {created}")
        meta = created["result"]["_meta"]["com.github.shaullavo.mesh/target"]
        require(meta["hostId"] == host_id and meta["account"], f"write target is not exact: {meta}")
        digest = hashlib.sha256(b"hello\n").hexdigest()
        require(data["sha256"] == digest and (work / "a.txt").read_text() == "hello\n", f"write result: {data}")

        read = tool_data(client.call_tool("mesh_file_read", {"host": host_id, "path": path}))["data"]
        require(read["data"] == "hello\n" and read["encoding"] == "utf-8" and read["eof"] and read["sha256"] == digest and read["target"]["hostId"] == host_id, f"read: {read}")

        # A digest conflict changes nothing, and the right digest replaces atomically.
        conflict = client.call_tool("mesh_file_write", {"host": host_id, "path": path, "content": "other\n", "expected_sha256": "0" * 64})
        require_tool_error(conflict, "file.conflict", "wrong digest")
        require((work / "a.txt").read_text() == "hello\n", "a conflicting write changed the file")
        replaced = client.call_tool("mesh_file_write", {"host": host_id, "path": path, "content": "hello world\n", "expected_sha256": digest})
        require(not replaced["result"]["isError"], f"replace failed: {replaced}")
        again = client.call_tool("mesh_file_write", {"host": host_id, "path": path, "content": "x", "create_only": True})
        require_tool_error(again, "file.conflict", "create_only on an existing file")

        # Patch: exact edit lands, an ambiguous or absent old_text changes nothing.
        digest = hashlib.sha256(b"hello world\n").hexdigest()
        ambiguous = client.call_tool("mesh_file_patch", {"host": host_id, "path": path, "expected_sha256": digest, "edits": [{"old_text": "o", "new_text": "0"}]})
        require_tool_error(ambiguous, "file.patch_mismatch", "ambiguous patch")
        patched = client.call_tool("mesh_file_patch", {"host": host_id, "path": path, "expected_sha256": digest, "edits": [{"old_text": "world", "new_text": "mesh"}]})
        require(not patched["result"]["isError"] and (work / "a.txt").read_text() == "hello mesh\n", f"patch failed: {patched}")

        # Binary content travels as Base64 both ways.
        blob = bytes([0, 1, 2, 0xFF, 0xFE, 10])
        binary_path = str(work / "blob.bin")
        wrote = client.call_tool("mesh_file_write", {"host": host_id, "path": binary_path, "content_base64": base64.b64encode(blob).decode(), "create_only": True})
        require(not wrote["result"]["isError"], f"binary write failed: {wrote}")
        got = tool_data(client.call_tool("mesh_file_read", {"host": host_id, "path": binary_path}))["data"]
        require(got["encoding"] == "base64" and base64.b64decode(got["data"]) == blob, f"binary read: {got}")

        # Payload bounds: a write over 1 MiB is refused before anything is sent.
        too_big = client.call_tool("mesh_file_write", {"host": host_id, "path": str(work / "big.txt"), "content": "x" * (1 << 20) + "y", "create_only": True})
        require(too_big.get("error", {}).get("code") == -32602 and not (work / "big.txt").exists(), f"oversize write: {too_big}")

        # A read continues across pages, and a replaced file ends the continuation.
        large = work / "large.txt"
        large.write_bytes(b"0123456789abcdef\n" * 160000)
        first = tool_data(client.call_tool("mesh_file_read", {"host": host_id, "path": str(large), "max_bytes": 1 << 20}))["data"]
        require(first["nextCursor"] and not first["eof"] and first["end"] == str(1 << 20), f"first page: {first}")
        second = tool_data(client.call_tool("mesh_file_read", {"host": host_id, "path": str(large), "cursor": first["nextCursor"], "max_bytes": 1 << 20}))["data"]
        require(second["start"] == first["end"], f"continuation: {second}")
        replacement = work / "large.new"
        replacement.write_bytes(large.read_bytes())
        os.rename(replacement, large)
        changed = client.call_tool("mesh_file_read", {"host": host_id, "path": str(large), "cursor": second["nextCursor"]})
        require_tool_error(changed, "file.changed", "continuation after replacement")

        # Symlinks, pipes and sockets are refused, and listing and search work.
        os.mkfifo(work / "pipe")
        os.symlink(work / "a.txt", work / "link")
        for name in ("pipe", "link"):
            refused = client.call_tool("mesh_file_read", {"host": host_id, "path": str(work / name)})
            require_tool_error(refused, "file.not_regular", f"read of {name}")
        listing = tool_data(client.call_tool("mesh_file_list", {"host": host_id, "path": str(work)}))["data"]
        kinds = {e["name"]: e["kind"] for e in listing["entries"]}
        require(kinds.get("a.txt") == "file" and kinds.get("link") == "symlink" and kinds.get("pipe") == "other", f"listing: {kinds}")
        found = tool_data(client.call_tool("mesh_file_search", {"host": host_id, "path": str(work), "pattern": "hello m\\w+"}))["data"]
        require([m["path"] for m in found["matches"]] == [str(work / "a.txt")] and found["matches"][0]["line"] == 1, f"search: {found}")
        relative = client.call_tool("mesh_file_read", {"host": host_id, "path": "a.txt"})
        require(relative.get("error", {}).get("code") == -32602, f"relative path without a context: {relative}")

        # mesh_copy moves a directory both ways with no size limit.
        source = fixture.root / "copy-source"
        source.mkdir()
        (source / "one.txt").write_text("copied\n")
        remote_copy = work / "copied"
        up = client.call_tool("mesh_copy", {"host": host_id, "direction": "upload", "local_path": str(source), "remote_path": str(remote_copy), "recursive": True})
        require(not up["result"]["isError"] and (remote_copy / "one.txt").read_text() == "copied\n", f"upload failed: {up}")
        back = fixture.root / "copy-back"
        down = client.call_tool("mesh_copy", {"host": host_id, "direction": "download", "local_path": str(back), "remote_path": str(remote_copy), "recursive": True})
        require(not down["result"]["isError"] and (back / "one.txt").read_text() == "copied\n", f"download failed: {down}")

        # Profile and fleet tools act on this machine at an expected revision.
        profile = tool_data(client.call_tool("mesh_profile_get", {}))["data"]
        require(profile["state"] == "absent", f"profile before: {profile}")
        document = {"schemaVersion": 1, "description": "integration", "instructions": "be careful"}
        set_one = client.call_tool("mesh_profile_set", {"expected_revision": 0, "profile": document})
        require(not set_one["result"]["isError"] and tool_data(set_one)["data"]["revision"] == 1, f"profile set: {set_one}")
        stale = client.call_tool("mesh_profile_set", {"expected_revision": 0, "profile": dict(document, description="stale edit")})
        require_tool_error(stale, "profile.conflict", "stale profile revision")
        after = tool_data(client.call_tool("mesh_profile_get", {}))["data"]
        require(after["revision"] == 1 and after["profile"]["description"] == "integration", f"profile after: {after}")
        fleet = client.call_tool("mesh_fleet_status", {})
        require(not fleet["result"]["isError"] and "members" in tool_data(fleet)["data"], f"fleet status: {fleet}")
        rc, _ = client.close_eof()
        require(rc == 0, f"file tool client EOF exit = {rc}")
    finally:
        client.terminate()


def exercise_file_contexts(fixture):
    host_id = fixture.remote_id
    work = fixture.root / "ctxfiles"
    work.mkdir()
    for name in ("filer", "stranger"):
        res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        require(res.returncode == 0 and not res.stderr, f"identity create {name} failed: {res.stdout!r} {res.stderr!r}")

    filer = MCPClient(fixture.binary, fixture.environment, args=("--identity", "filer"))
    try:
        opened = filer.call_tool("mesh_context_open", {"host": host_id, "request_key": "file-ctx-1", "cwd": str(work), "shell": "sh"})
        require(not opened["result"]["isError"], f"context open failed: {opened}")
        handle = tool_data(opened)["data"]["handle"]

        # A relative path resolves inside the context, and the handle alone names the host.
        wrote = filer.call_tool("mesh_file_write", {"context": handle, "path": "rel.txt", "content": "relative\n", "create_only": True})
        require(not wrote["result"]["isError"], f"relative write failed: {wrote}")
        target = wrote["result"]["_meta"]["com.github.shaullavo.mesh/target"]
        require(target["hostId"] == host_id and target["contextId"] == handle and target["cwd"] == str(work), f"workspace missing from the target: {target}")
        require((work / "rel.txt").read_text() == "relative\n", "the write did not land in the context directory")
        digest = tool_data(wrote)["data"]["sha256"]

        read = tool_data(filer.call_tool("mesh_file_read", {"context": handle, "path": "rel.txt"}))["data"]
        require(read["data"] == "relative\n" and read["path"] == str(work / "rel.txt") and read["target"]["cwd"] == str(work), f"relative read: {read}")
        listing = tool_data(filer.call_tool("mesh_file_list", {"context": handle, "path": "."}))["data"]
        require([e["name"] for e in listing["entries"]] == ["rel.txt"], f"relative list: {listing}")
        found = tool_data(filer.call_tool("mesh_file_search", {"context": handle, "path": ".", "pattern": "relative"}))["data"]
        require(len(found["matches"]) == 1 and found["matches"][0]["path"] == str(work / "rel.txt"), f"relative search: {found}")
        patched = filer.call_tool("mesh_file_patch", {"context": handle, "path": "rel.txt", "expected_sha256": digest, "edits": [{"old_text": "relative", "new_text": "patched"}]})
        require(not patched["result"]["isError"] and (work / "rel.txt").read_text() == "patched\n", f"relative patch: {patched}")

        # Another identity on the same account and host cannot use the context.
        stranger = MCPClient(fixture.binary, fixture.environment, args=("--identity", "stranger"))
        try:
            denied = stranger.call_tool("mesh_file_read", {"context": handle, "path": "rel.txt"})
            require_tool_error(denied, "context.not_found", "another identity's context")
            absolute = stranger.call_tool("mesh_file_read", {"host": host_id, "path": str(work / "rel.txt")})
            require(not absolute["result"]["isError"], f"an absolute read needs no context: {absolute}")
        finally:
            stranger.terminate()

        # No identity, no context.
        anonymous = MCPClient(fixture.binary, fixture.environment)
        try:
            refused = anonymous.call_tool("mesh_file_read", {"context": handle, "path": "rel.txt"})
            require_tool_error(refused, "usage.invalid", "a context handle without an identity")
        finally:
            anonymous.terminate()

        # Closing the context ends the workspace.
        closed = filer.call_tool("mesh_context_close", {"context": handle})
        require(not closed["result"]["isError"], f"context close failed: {closed}")
        after = filer.call_tool("mesh_file_read", {"context": handle, "path": "rel.txt"})
        require_tool_error(after, "context.closed", "read in a closed context")
        rc, _ = filer.close_eof()
        require(rc == 0, f"file context client EOF exit = {rc}")
    finally:
        filer.terminate()


def read_exec_with_progress(client, req_id, expected_token, timeout=15.0):
    progress_values = []
    while True:
        msg = client.read_message(timeout=timeout)
        if msg.get("method") == "notifications/progress":
            require(msg["params"]["progressToken"] == expected_token, f"wrong progress token: {msg}")
            progress_values.append(msg["params"]["progress"])
            continue
        require(msg.get("id") == req_id, f"unexpected reply while waiting on mesh_exec: {msg}")
        return msg, progress_values


def read_identity_secret(config_dir, name):
    path = Path(config_dir) / "mcp" / "identities" / f"{name}.json"
    data = json.loads(path.read_text())
    cred = data.get("credential", "")
    require(isinstance(cred, str) and len(cred) >= 16, f"missing credential in {path}: {data}")
    return cred


def exercise_contexts_exec_and_isolation(fixture):
    host_id = fixture.remote_id
    ctx_dir_1 = str(fixture.root)
    ctx_dir_2 = str(fixture.root / "ctx-sub")
    ctx_dir_3 = str(fixture.root / "ctx-sub-3")
    Path(ctx_dir_2).mkdir(exist_ok=True)
    Path(ctx_dir_3).mkdir(exist_ok=True)
    launch_marker = fixture.root / "replay-launch-count.txt"

    # Create two distinct application identities under the same account.
    secrets = []
    for name in ("alpha", "beta"):
        res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        require(res.returncode == 0 and not res.stderr, f"identity create {name} failed: rc={res.returncode} out={res.stdout!r} err={res.stderr!r}")
        secret = read_identity_secret(fixture.config, name)
        require(secret not in res.stdout and secret not in res.stderr, f"credential leaked by identity create {name}")
        secrets.append(secret)

    def assert_no_secret(payload, label):
        text = payload if isinstance(payload, str) else json.dumps(payload)
        for sec in secrets:
            require(sec not in text, f"credential leaked in {label}")

    alpha = MCPClient(fixture.binary, fixture.environment, args=("--identity", "alpha"))
    ctx_handle = ""
    short_job = ""
    long_job = ""
    try:
        # Capabilities report executionContext=true.
        hosts_resp = alpha.call_tool("mesh_hosts_list", {"timeout_ms": 5000})
        hosts_list = hosts_resp["result"]["structuredContent"]["data"]["hosts"]
        remote_host = next((h for h in hosts_list if h.get("hostId") == host_id), None)
        require(remote_host is not None and remote_host.get("capabilities", {}).get("executionContext") is True, f"executionContext capability missing: {remote_host}")

        # Open context with secret env overlay; secret must never appear in response.
        opened = alpha.call_tool("mesh_context_open", {
            "host": host_id,
            "request_key": "ctx-open-1",
            "cwd": ctx_dir_1,
            "shell": "sh",
            "env": {"APP_TEST_SECRET": "top-secret-42"},
            "label": "alpha-work",
        })
        assert_no_secret(opened, "context open")
        require(not opened["result"]["isError"], f"context open failed: {opened}")
        require("top-secret-42" not in json.dumps(opened), f"secret leaked in open reply: {opened}")
        open_data = opened["result"]["structuredContent"]["data"]
        ctx_handle = open_data["handle"]
        require(ctx_handle.startswith(f"mc1.{host_id}."), f"unexpected context handle: {open_data}")
        require(open_data["revision"] == "1" and open_data["envNames"] == ["APP_TEST_SECRET"] and open_data["uncertainJobs"] == 0, f"unexpected open data: {open_data}")

        # Replay with same key & intent returns original revision-1 snapshot; changed intent conflicts.
        replayed = alpha.call_tool("mesh_context_open", {
            "host": host_id,
            "request_key": "ctx-open-1",
            "cwd": ctx_dir_1,
            "shell": "sh",
            "env": {"APP_TEST_SECRET": "top-secret-42"},
            "label": "alpha-work",
        })
        require(not replayed["result"]["isError"] and replayed["result"]["structuredContent"]["data"]["handle"] == ctx_handle, f"context open replay failed: {replayed}")
        conflict = alpha.call_tool("mesh_context_open", {
            "host": host_id,
            "request_key": "ctx-open-1",
            "cwd": ctx_dir_2,
            "shell": "sh",
        })
        require(conflict["result"]["isError"] and conflict["result"]["structuredContent"]["errors"][0]["code"] == "request.key_conflict", f"expected request.key_conflict: {conflict}")

        # Update context to revision 2; stale expected_revision returns context.conflict.
        updated = alpha.call_tool("mesh_context_update", {
            "context": ctx_handle,
            "expected_revision": 1,
            "cwd": ctx_dir_2,
            "set_env": {"EXTRA_FLAG": "yes"},
            "label": "alpha-v2",
        })
        require(not updated["result"]["isError"] and updated["result"]["structuredContent"]["data"]["revision"] == "2", f"context update failed: {updated}")
        stale = alpha.call_tool("mesh_context_update", {
            "context": ctx_handle,
            "expected_revision": 1,
            "label": "stale",
        })
        require(stale["result"]["isError"] and stale["result"]["structuredContent"]["errors"][0]["code"] == "context.conflict", f"expected context.conflict: {stale}")

        # Retried open and context_receipt return original revision-1 snapshot; context_get returns current rev 2.
        rec_open = alpha.call_tool("mesh_context_receipt", {"host": host_id, "request_key": "ctx-open-1"})
        require(not rec_open["result"]["isError"] and rec_open["result"]["structuredContent"]["data"]["revision"] == "1", f"expected rev 1 from context_receipt: {rec_open}")
        got_cur = alpha.call_tool("mesh_context_get", {"context": ctx_handle})
        require(not got_cur["result"]["isError"] and got_cur["result"]["structuredContent"]["data"]["revision"] == "2" and got_cur["result"]["structuredContent"]["data"]["cwd"] == ctx_dir_2, f"expected rev 2 from context_get: {got_cur}")

        listed_ctx = alpha.call_tool("mesh_context_list", {"host": host_id})
        require(not listed_ctx["result"]["isError"] and any(c["handle"] == ctx_handle for c in listed_ctx["result"]["structuredContent"]["data"]["contexts"]), f"context missing from list: {listed_ctx}")

        # Strict command/argv exclusivity: command + empty argv must fail with -32602.
        bad_both = alpha.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-bad-both",
            "command": "echo hi",
            "argv": [],
        })
        require(bad_both.get("error", {}).get("code") == -32602, f"expected -32602 for command + empty argv: {bad_both}")

        # Short mesh_exec job: verifies durable cwd, target _meta, inherited env, and single launch on replay.
        short_cmd = (
            f"echo x >> '{launch_marker}'; "
            "printf 'cwd=%s;sec=%s;extra=%s;job=%s' \"$PWD\" \"$APP_TEST_SECRET\" \"$EXTRA_FLAG\" \"$JOB_VAR\"; "
            "printf 'err-line' >&2; cd /; export APP_TEST_SECRET=mutated"
        )
        short_exec = alpha.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-short-1",
            "command": short_cmd,
            "env": {"JOB_VAR": "per-job"},
            "wait_ms": 10000,
        })
        assert_no_secret(short_exec, "short mesh_exec")
        require(not short_exec["result"]["isError"], f"short mesh_exec failed: {short_exec}")
        short_meta = short_exec["result"].get("_meta", {}).get("com.github.shaullavo.mesh/target", {})
        short_data = short_exec["result"]["structuredContent"]["data"]
        short_job = short_data["job"]
        require(short_meta.get("hostId") == host_id and short_meta.get("contextId") == ctx_handle and short_meta.get("cwd") == ctx_dir_2, f"unexpected _meta target: {short_meta}")
        require(short_data["target"]["cwd"] == ctx_dir_2 and short_data["receipt"]["cwd"] == ctx_dir_2, f"durable cwd missing on short exec: {short_data}")
        require(short_data["state"] == "exited" and short_data["wait"] == "completed", f"short exec state: {short_data}")
        require(short_data["result"]["termination"]["code"] == 0, f"short exec exit: {short_data}")
        require(short_data["stdout"]["data"] == f"cwd={ctx_dir_2};sec=top-secret-42;extra=yes;job=per-job", f"short exec stdout: {short_data}")
        require(short_data["stderr"]["data"] == "err-line", f"short exec stderr: {short_data}")
        require(len(launch_marker.read_text().splitlines()) == 1, "expected 1 launch after initial short mesh_exec")

        # Child cd/export never mutates context.
        after_exec_ctx = alpha.call_tool("mesh_context_get", {"context": ctx_handle})
        require(after_exec_ctx["result"]["structuredContent"]["data"]["revision"] == "2" and after_exec_ctx["result"]["structuredContent"]["data"]["cwd"] == ctx_dir_2, f"exec mutated context: {after_exec_ctx}")

        # Edit context to ctx_dir_3, then replay exec-short-1 with changed wait_ms & output_bytes:
        # must still report original ctx_dir_2 on BOTH target objects and launch only once!
        updated3 = alpha.call_tool("mesh_context_update", {
            "context": ctx_handle,
            "expected_revision": 2,
            "cwd": ctx_dir_3,
        })
        require(not updated3["result"]["isError"] and updated3["result"]["structuredContent"]["data"]["revision"] == "3", f"context update to rev 3 failed: {updated3}")
        replayed_exec = alpha.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-short-1",
            "command": short_cmd,
            "env": {"JOB_VAR": "per-job"},
            "wait_ms": 5000,
            "output_bytes": 16,
        })
        require(not replayed_exec["result"]["isError"], f"replayed mesh_exec failed: {replayed_exec}")
        rep_meta = replayed_exec["result"].get("_meta", {}).get("com.github.shaullavo.mesh/target", {})
        rep_data = replayed_exec["result"]["structuredContent"]["data"]
        require(rep_data["job"] == short_job, f"replayed job handle changed: {rep_data}")
        require(rep_meta.get("cwd") == ctx_dir_2 and rep_data["target"]["cwd"] == ctx_dir_2, f"replay cwd drifted after context edit: meta={rep_meta} data={rep_data['target']}")
        require(len(rep_data["stdout"]["data"]) == 16, f"replay output_bytes ignored: {rep_data['stdout']}")
        require(len(launch_marker.read_text().splitlines()) == 1, "replay launched command a second time")

        # Changed command intent under same request_key returns request.key_conflict.
        exec_conflict = alpha.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-short-1",
            "command": "echo different",
            "wait_ms": 5000,
        })
        require(exec_conflict["result"]["isError"] and exec_conflict["result"]["structuredContent"]["errors"][0]["code"] == "request.key_conflict", f"expected request.key_conflict on changed command: {exec_conflict}")

        # Text (utf-8) and binary (base64) cursor continuation on mesh_job_output.
        bin_exec = alpha.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-bin-1",
            "argv": ["/bin/sh", "-c", "printf 'ABCDEFGH'; printf '\\000\\377\\001\\376\\002\\375' >&2"],
            "wait_ms": 10000,
            "output_bytes": 4,
        })
        require(not bin_exec["result"]["isError"], f"binary exec failed: {bin_exec}")
        bin_data = bin_exec["result"]["structuredContent"]["data"]
        bin_job = bin_data["job"]
        require(bin_data["stdout"]["encoding"] == "utf-8" and bin_data["stdout"]["data"] == "ABCD", f"text page 1: {bin_data['stdout']}")
        text_p2 = alpha.call_tool("mesh_job_output", {"job": bin_job, "context": ctx_handle, "stream": "stdout", "cursor": bin_data["stdout"]["nextCursor"], "max_bytes": 64})
        require(text_p2["result"]["structuredContent"]["data"]["data"] == "EFGH", f"text page 2: {text_p2}")
        require(bin_data["stderr"]["encoding"] == "base64" and base64.b64decode(bin_data["stderr"]["data"]) == b"\x00\xff\x01\xfe", f"binary page 1: {bin_data['stderr']}")
        bin_p2 = alpha.call_tool("mesh_job_output", {"job": bin_job, "context": ctx_handle, "stream": "stderr", "cursor": bin_data["stderr"]["nextCursor"], "max_bytes": 64})
        bin_p2_data = bin_p2["result"]["structuredContent"]["data"]
        require(bin_p2_data["encoding"] == "base64" and base64.b64decode(bin_p2_data["data"]) == b"\x02\xfd", f"binary page 2: {bin_p2}")

        # Shell modes: login (command + argv) and none (argv ok, command refused).
        login_ctx = alpha.call_tool("mesh_context_open", {"host": host_id, "request_key": "ctx-login", "cwd": ctx_dir_1, "shell": "login"})
        require(not login_ctx["result"]["isError"], f"login context open failed: {login_ctx}")
        login_handle = login_ctx["result"]["structuredContent"]["data"]["handle"]
        login_cmd = alpha.call_tool("mesh_exec", {"context": login_handle, "request_key": "login-cmd", "command": "printf 'login-ok'", "wait_ms": 10000})
        require(not login_cmd["result"]["isError"] and login_cmd["result"]["structuredContent"]["data"]["stdout"]["data"] == "login-ok", f"login command failed: {login_cmd}")
        login_argv = alpha.call_tool("mesh_exec", {"context": login_handle, "request_key": "login-argv", "argv": ["/bin/echo", "-n", "argv-ok"], "wait_ms": 10000})
        require(not login_argv["result"]["isError"] and login_argv["result"]["structuredContent"]["data"]["stdout"]["data"] == "argv-ok", f"login argv failed: {login_argv}")

        none_ctx = alpha.call_tool("mesh_context_open", {"host": host_id, "request_key": "ctx-none", "cwd": ctx_dir_1, "shell": "none"})
        require(not none_ctx["result"]["isError"], f"none context open failed: {none_ctx}")
        none_handle = none_ctx["result"]["structuredContent"]["data"]["handle"]
        none_cmd = alpha.call_tool("mesh_exec", {"context": none_handle, "request_key": "none-cmd", "command": "echo nope", "wait_ms": 5000})
        require(none_cmd["result"]["isError"] and none_cmd["result"]["structuredContent"]["errors"][0]["code"] == "usage.invalid", f"shell=none accepted command string: {none_cmd}")
        none_argv = alpha.call_tool("mesh_exec", {"context": none_handle, "request_key": "none-argv", "argv": ["/bin/echo", "-n", "none-ok"], "wait_ms": 10000})
        require(not none_argv["result"]["isError"] and none_argv["result"]["structuredContent"]["data"]["stdout"]["data"] == "none-ok", f"shell=none argv failed: {none_argv}")

        # CLI parity: create context job via CLI and verify MCP sees matching status/output.
        cli_create = run_cli(
            fixture.binary,
            fixture.environment,
            "session",
            "create",
            "--host",
            host_id,
            "--kind",
            "pipe",
            "--identity",
            "alpha",
            "--context",
            ctx_handle,
            "--request-key",
            "cli-ctx-short-1",
            "--json",
            "--",
            "/bin/sh",
            "-c",
            "printf 'cli-ctx-ok'",
        )
        require(cli_create.returncode == 0, f"CLI context create failed: {cli_create.stdout!r} {cli_create.stderr!r}")
        cli_rec = json.loads(cli_create.stdout)["data"]
        cli_job_handle = f"mj1.{host_id}.{cli_rec['sessionId']}.{cli_rec['instance']}"
        cli_wait = alpha.call_tool("mesh_job_wait", {"job": cli_job_handle, "context": ctx_handle, "wait_ms": 10000})
        require(not cli_wait["result"]["isError"] and cli_wait["result"]["structuredContent"]["data"]["state"] == "exited", f"MCP wait on CLI context job failed: {cli_wait}")
        cli_out = alpha.call_tool("mesh_job_output", {"job": cli_job_handle, "context": ctx_handle, "stream": "stdout"})
        require(cli_out["result"]["structuredContent"]["data"]["data"] == "cli-ctx-ok", f"CLI/MCP context output mismatch: {cli_out}")

        # Quiet long mesh_exec job with progressToken: progress MUST be strictly increasing (never 0, 0, 0).
        long_req_id = alpha.request("tools/call", {
            "name": "mesh_exec",
            "arguments": {
                "context": ctx_handle,
                "request_key": "exec-long-1",
                "argv": ["/bin/sleep", "60"],
                "wait_ms": 2200,
            },
        }, progress_token="exec-long-prog")
        long_exec, prog_vals = read_exec_with_progress(alpha, long_req_id, "exec-long-prog")
        require(len(prog_vals) == 1 and prog_vals[0] == 0, f"quiet mesh_exec emitted non-monotonic progress: {prog_vals}")
        require(not long_exec["result"]["isError"], f"long mesh_exec failed: {long_exec}")
        long_data = long_exec["result"]["structuredContent"]["data"]
        long_job = long_data["job"]
        require(long_data["state"] == "running" and long_data["wait"] == "timed_out", f"long exec state: {long_data}")

        # Cancel a live mesh_exec request via notifications/cancelled: accepted job MUST keep running!
        cancel_req_id = alpha.request("tools/call", {
            "name": "mesh_exec",
            "arguments": {
                "context": ctx_handle,
                "request_key": "exec-cancel-call-1",
                "argv": ["/bin/sleep", "60"],
                "wait_ms": 30000,
            },
        }, progress_token="exec-cancel-tok")
        cancel_prog = alpha.read_message(timeout=10.0)
        require(cancel_prog.get("method") == "notifications/progress" and cancel_prog["params"]["progressToken"] == "exec-cancel-tok", f"missing cancel-call progress: {cancel_prog}")
        alpha.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": cancel_req_id, "reason": "stop waiting"}})
        cancelled_rec = alpha.call_tool("mesh_job_receipt", {"context": ctx_handle, "request_key": "exec-cancel-call-1"})
        require(not cancelled_rec["result"]["isError"] and cancelled_rec["result"]["structuredContent"]["data"]["state"] == "running", f"cancelling mesh_exec stopped its job: {cancelled_rec}")
        cancel_call_job = cancelled_rec["result"]["structuredContent"]["data"]["job"]
        alpha.call_tool("mesh_job_cancel", {"job": cancel_call_job, "context": ctx_handle})
        alpha.call_tool("mesh_job_wait", {"job": cancel_call_job, "context": ctx_handle, "wait_ms": 10000})

        # mesh_jobs_list scoped to context lists alpha's context jobs.
        ctx_jobs = alpha.call_tool("mesh_jobs_list", {"context": ctx_handle, "all": True})
        ctx_job_ids = [j["job"] for j in ctx_jobs["result"]["structuredContent"]["data"]["jobs"]]
        require(short_job in ctx_job_ids and long_job in ctx_job_ids, f"missing jobs in context list: {ctx_job_ids}")

        rc, alpha_stderr = alpha.close_eof()
        assert_no_secret(alpha_stderr, "alpha stderr")
        require(rc == 0, f"alpha EOF exit = {rc}")
    finally:
        alpha.terminate()

    # App isolation: second identity beta on the same account/host sees zero alpha contexts/jobs
    # and is denied access to alpha's context and job handles.
    beta = MCPClient(fixture.binary, fixture.environment, args=("--identity", "beta"))
    try:
        beta_ctxs = beta.call_tool("mesh_context_list", {"host": host_id})
        assert_no_secret(beta_ctxs, "beta context list")
        require(not beta_ctxs["result"]["isError"] and beta_ctxs["result"]["structuredContent"]["data"]["contexts"] == [], f"beta saw alpha contexts: {beta_ctxs}")
        beta_jobs = beta.call_tool("mesh_jobs_list", {"host": host_id, "all": True})
        require(not beta_jobs["result"]["isError"] and beta_jobs["result"]["structuredContent"]["data"]["jobs"] == [], f"beta saw alpha jobs: {beta_jobs}")
        beta_get = beta.call_tool("mesh_context_get", {"context": ctx_handle})
        require(beta_get["result"]["isError"] and beta_get["result"]["structuredContent"]["errors"][0]["code"] == "context.not_found", f"beta read alpha context: {beta_get}")
        beta_exec = beta.call_tool("mesh_exec", {"context": ctx_handle, "request_key": "beta-intrude", "argv": ["/bin/true"]})
        require(beta_exec["result"]["isError"] and beta_exec["result"]["structuredContent"]["errors"][0]["code"] == "context.not_found", f"beta ran in alpha context: {beta_exec}")
        beta_job = beta.call_tool("mesh_job_status", {"job": long_job})
        require(beta_job["result"]["isError"] and beta_job["result"]["structuredContent"]["errors"][0]["code"] == "access.denied", f"beta inspected alpha job: {beta_job}")
        rc, beta_stderr = beta.close_eof()
        assert_no_secret(beta_stderr, "beta stderr")
        require(rc == 0, f"beta EOF exit = {rc}")
    finally:
        beta.terminate()

    # Fresh alpha frontend recovers context and running job by request_key with NO discovery,
    # cancels the running job, closes the context, and confirms replay + closed-context rules.
    alpha2 = MCPClient(fixture.binary, fixture.environment, args=("--identity", "alpha"))
    try:
        fresh_ctx_rec = alpha2.call_tool("mesh_context_receipt", {"host": host_id, "request_key": "ctx-open-1"})
        require(not fresh_ctx_rec["result"]["isError"] and fresh_ctx_rec["result"]["structuredContent"]["data"]["handle"] == ctx_handle, f"fresh context receipt failed: {fresh_ctx_rec}")

        fresh_job_rec = alpha2.call_tool("mesh_job_receipt", {"context": ctx_handle, "request_key": "exec-long-1"})
        require(not fresh_job_rec["result"]["isError"] and fresh_job_rec["result"]["structuredContent"]["data"]["job"] == long_job, f"fresh job receipt failed: {fresh_job_rec}")

        cancel_long = alpha2.call_tool("mesh_job_cancel", {"job": long_job, "context": ctx_handle})
        require(not cancel_long["result"]["isError"], f"cancel long context job failed: {cancel_long}")
        waited_long = alpha2.call_tool("mesh_job_wait", {"job": long_job, "context": ctx_handle, "wait_ms": 10000})
        require(waited_long["result"]["structuredContent"]["data"]["state"] == "killed", f"long context job not killed: {waited_long}")

        closed = alpha2.call_tool("mesh_context_close", {"context": ctx_handle, "expected_revision": 3})
        require(not closed["result"]["isError"] and closed["result"]["structuredContent"]["data"]["state"] == "closed", f"context close failed: {closed}")

        # Replaying exec-short-1 AFTER context closure still succeeds with original durable cwd & single launch!
        replay_after_close = alpha2.call_tool("mesh_exec", {
            "context": ctx_handle,
            "request_key": "exec-short-1",
            "command": short_cmd,
            "env": {"JOB_VAR": "per-job"},
            "wait_ms": 1000,
            "output_bytes": 32,
        })
        assert_no_secret(replay_after_close, "replay after close")
        require(not replay_after_close["result"]["isError"], f"replay after close failed: {replay_after_close}")
        require(replay_after_close["result"]["structuredContent"]["data"]["job"] == short_job, f"replay after close job mismatch: {replay_after_close}")
        require(replay_after_close["result"]["structuredContent"]["data"]["target"]["cwd"] == ctx_dir_2, f"replay after close cwd mismatch: {replay_after_close}")
        require(len(launch_marker.read_text().splitlines()) == 1, "replay after close launched command again")

        after_close_exec = alpha2.call_tool("mesh_exec", {"context": ctx_handle, "request_key": "after-close", "argv": ["/bin/true"]})
        require(after_close_exec["result"]["isError"] and after_close_exec["result"]["structuredContent"]["errors"][0]["code"] == "context.closed", f"expected context.closed: {after_close_exec}")

        # Existing job handles and receipts remain readable after context close.
        still_readable = alpha2.call_tool("mesh_job_status", {"job": short_job, "context": ctx_handle})
        require(not still_readable["result"]["isError"] and still_readable["result"]["structuredContent"]["data"]["state"] == "exited", f"job unreadable after context close: {still_readable}")
        rc, alpha2_stderr = alpha2.close_eof()
        assert_no_secret(alpha2_stderr, "alpha2 stderr")
        require(rc == 0, f"alpha2 EOF exit = {rc}")
    finally:
        alpha2.terminate()


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
            exercise_contexts_exec_and_isolation(fixture)
            exercise_long_job_input_cancel_and_eof(fixture)
            exercise_file_tools(fixture)
            exercise_file_contexts(fixture)
            exercise_host_mismatch_and_revocation(fixture)
        finally:
            fixture.close()
    print("PASS: native MCP stdio server verified across fresh connect, contexts, mesh_exec, app isolation, file tools, file contexts, copy, profile tools, short/long jobs, CLI parity, wait vs job cancel, EOF, identity mismatch, and live revocation")


if __name__ == "__main__":
    main()
