"""Drive the real mesh mcp --http loopback server against an isolated second remote daemon."""
import http.client
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time

from mcp_native import MCPFixture, run_cli
from terminal_window import eventually, require

PROTO = "2026-07-28"
META = {
    "io.modelcontextprotocol/protocolVersion": PROTO,
    "io.modelcontextprotocol/clientCapabilities": {},
}
ALLOWED_ORIGIN = "http://127.0.0.1:3456"


def redact_record(rec):
    if not isinstance(rec, dict):
        return "<redacted>"
    out = dict(rec)
    if "token" in out:
        out["token"] = "[REDACTED]"
    return out


def redact_text(text, *secrets):
    out = text
    for secret in secrets:
        if secret:
            out = out.replace(secret, "[REDACTED]")
    return out


def create_app_job_cli(fixture, host_id, identity, ctx_key, job_key, command, stdin_mode="closed"):
    ctx_res = run_cli(
        fixture.binary,
        fixture.environment,
        "context",
        "open",
        "--host",
        host_id,
        "--identity",
        identity,
        "--request-key",
        ctx_key,
        "--cwd",
        str(fixture.root),
        "--json",
    )
    require(ctx_res.returncode == 0 and not ctx_res.stderr, f"CLI context open failed: rc={ctx_res.returncode} out={ctx_res.stdout!r} err={ctx_res.stderr!r}")
    ctx_handle = json.loads(ctx_res.stdout)["data"]["handle"]
    job_res = run_cli(
        fixture.binary,
        fixture.environment,
        "session",
        "create",
        "--host",
        host_id,
        "--identity",
        identity,
        "--context",
        ctx_handle,
        "--kind",
        "pipe",
        "--stdin",
        stdin_mode,
        "--cwd",
        str(fixture.root),
        "--request-key",
        job_key,
        "--json",
        "--",
        *command,
    )
    require(job_res.returncode == 0 and not job_res.stderr, f"CLI app session create failed: rc={job_res.returncode} out={job_res.stdout!r} err={job_res.stderr!r}")
    return json.loads(job_res.stdout)["data"]


def make_body(req_id, method, params=None, progress_token=None, version=PROTO, caps=None):
    meta = {
        "io.modelcontextprotocol/protocolVersion": version,
        "io.modelcontextprotocol/clientCapabilities": {} if caps is None else caps,
    }
    if progress_token is not None:
        meta["progressToken"] = progress_token
    body = {"_meta": meta}
    if params:
        body.update(params)
    return json.dumps({"jsonrpc": "2.0", "id": req_id, "method": method, "params": body}, separators=(",", ":")).encode("utf-8")


def send_http(authority, method, path, body=b"", headers=None, timeout=15.0):
    host, port_s = authority.rsplit(":", 1)
    conn = http.client.HTTPConnection(host, int(port_s), timeout=timeout)
    try:
        conn.putrequest(method, path, skip_host=True, skip_accept_encoding=True)
        hdrs = dict(headers or {})
        if not any(k.lower() == "host" for k in hdrs):
            hdrs["Host"] = authority
        if body and not any(k.lower() == "content-length" for k in hdrs):
            hdrs["Content-Length"] = str(len(body))
        for k, v in hdrs.items():
            conn.putheader(k, v)
        conn.endheaders(body)
        resp = conn.getresponse()
        raw = resp.read()
        return resp.status, dict(resp.getheaders()), raw
    finally:
        conn.close()


def open_sse(authority, path, body, headers, timeout=15.0):
    host, port_s = authority.rsplit(":", 1)
    conn = http.client.HTTPConnection(host, int(port_s), timeout=timeout)
    conn.putrequest("POST", path, skip_host=True, skip_accept_encoding=True)
    hdrs = dict(headers)
    if not any(k.lower() == "host" for k in hdrs):
        hdrs["Host"] = authority
    hdrs["Content-Length"] = str(len(body))
    for k, v in hdrs.items():
        conn.putheader(k, v)
    conn.endheaders(body)
    resp = conn.getresponse()
    return conn, resp


def read_sse_event(resp):
    """Read one SSE event payload from an open HTTPResponse, or return None on EOF."""
    data_lines = []
    while True:
        line = resp.readline()
        if not line:
            return None
        text = line.decode("utf-8").rstrip("\r\n")
        if text == "":
            if data_lines:
                return json.loads("\n".join(data_lines))
            continue
        if text.startswith("data:"):
            val = text[5:]
            if val.startswith(" "):
                val = val[1:]
            data_lines.append(val)


def std_headers(token, method, name=None, accept="application/json, text/event-stream", extra=None):
    hdrs = {
        "Content-Type": "application/json",
        "Accept": accept,
        "Mcp-Protocol-Version": PROTO,
        "Mcp-Method": method,
    }
    if token:
        hdrs["Authorization"] = f"Bearer {token}"
    if name:
        hdrs["Mcp-Name"] = name
    if extra:
        hdrs.update(extra)
    return hdrs


def check_startup_usage_errors(binary, root):
    env = dict(os.environ, MESH_CONFIG_DIR=str(root / "bad-cfg"), MESH_STATE_DIR=str(root / "bad-state"))
    for bad_args in (
        ("mcp", "--http", "127.0.0.1:0"),
        ("mcp", "--http", "0.0.0.0:8765", "--identity", "agent"),
        ("mcp", "--allow-origin", ALLOWED_ORIGIN),
        ("mcp", "--token-file", str(root / "tok.json")),
        ("mcp", "--http", "127.0.0.1:0", "--identity", "missing"),
        ("mcp", "--privacy", "--http", "127.0.0.1:0", "--identity", "missing"),
    ):
        res = run_cli(binary, env, *bad_args)
        require(
            res.returncode == 2 and res.stdout == "" and res.stderr != "",
            f"bad HTTP startup {bad_args}: rc={res.returncode} out={res.stdout!r} err={res.stderr!r}",
        )


def wait_token_file(path, proc, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            stderr = proc.stderr.read().decode("utf-8", errors="replace")
            raise AssertionError(f"mesh mcp --http exited early (rc={proc.returncode}): {stderr!r}")
        if path.exists():
            try:
                data = json.loads(path.read_text(encoding="utf-8"))
                if data.get("token") and data.get("listener"):
                    return data
            except (OSError, json.JSONDecodeError):
                pass
        time.sleep(0.02)
    raise AssertionError(f"token file {path} never appeared")


def check_endpoint_and_protocol_matrix(authority, token):
    discover = make_body(1, "server/discover")

    # 1. Auth refusal and inert legacy headers without auth.
    status, hdrs, _ = send_http(authority, "POST", "/mcp", discover, std_headers(None, "server/discover"))
    require(status == 401 and hdrs.get("Www-Authenticate", hdrs.get("WWW-Authenticate")) == "Bearer", f"unauth: {status} {hdrs}")

    status, _, _ = send_http(authority, "POST", "/mcp", discover, std_headers("mht1_wrong", "server/discover"))
    require(status == 401, f"wrong token: {status}")

    status, _, _ = send_http(
        authority,
        "POST",
        "/mcp",
        discover,
        std_headers(None, "server/discover", extra={"Mcp-Session-Id": "old", "Last-Event-ID": "9"}),
    )
    require(status == 401, f"legacy headers without auth: {status}")

    # 2. Exact Host, Origin, path, and method rules.
    port = authority.rsplit(":", 1)[1]
    status, _, _ = send_http(authority, "POST", "/mcp", discover, std_headers(token, "server/discover", extra={"Host": f"localhost:{port}"}))
    require(status == 403, f"wrong Host: {status}")

    status, _, _ = send_http(authority, "POST", "/mcp", discover, std_headers(token, "server/discover", extra={"Origin": "http://127.0.0.1:9999"}))
    require(status == 403, f"unconfigured origin: {status}")

    status, _, _ = send_http(authority, "POST", "/mcp", discover, std_headers(token, "server/discover", extra={"Origin": "https://evil.example"}))
    require(status == 403, f"public origin: {status}")

    status, _, _ = send_http(authority, "POST", "/mcp", discover, std_headers(token, "server/discover", extra={"Origin": ALLOWED_ORIGIN}))
    require(status == 200, f"allowed origin: {status}")

    status, _, _ = send_http(authority, "POST", "/other", discover, std_headers(token, "server/discover"))
    require(status == 404, f"wrong path: {status}")

    status, _, _ = send_http(authority, "POST", "/mcp?x=1", discover, std_headers(token, "server/discover"))
    require(status == 404, f"query string: {status}")

    for m in ("GET", "DELETE"):
        status, hdrs, _ = send_http(authority, m, "/mcp", b"", {"Authorization": f"Bearer {token}"})
        require(status == 405 and hdrs.get("Allow") == "POST", f"{m} status={status} hdrs={hdrs}")

    # 3. Section 2.2 status & error table.
    status, _, raw = send_http(authority, "POST", "/mcp", discover, {"Authorization": f"Bearer {token}", "Content-Type": "text/plain", "Accept": "application/json, text/event-stream"})
    require(status == 415, f"bad content-type: {status}")

    status, _, raw = send_http(authority, "POST", "/mcp", discover, std_headers(token, "server/discover", accept="application/json"))
    require(status == 406, f"missing sse accept: {status}")

    bad_hdrs = std_headers(token, "server/discover")
    del bad_hdrs["Mcp-Protocol-Version"]
    status, _, raw = send_http(authority, "POST", "/mcp", discover, bad_hdrs)
    require(status == 400 and json.loads(raw)["error"]["code"] == -32020, f"missing version header: {status} {raw}")

    old_body = make_body(2, "server/discover", version="2025-11-25")
    old_hdrs = std_headers(token, "server/discover")
    old_hdrs["Mcp-Protocol-Version"] = "2025-11-25"
    status, _, raw = send_http(authority, "POST", "/mcp", old_body, old_hdrs)
    err = json.loads(raw)["error"]
    require(status == 400 and err["code"] == -32022 and err["data"]["requested"] == "2025-11-25", f"unsupported version: {status} {raw}")

    legacy_init = json.dumps({"jsonrpc": "2.0", "id": 3, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}}}).encode()
    status, _, raw = send_http(authority, "POST", "/mcp", legacy_init, {"Authorization": f"Bearer {token}", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
    require(status == 400 and json.loads(raw)["error"]["code"] == -32020, f"legacy init without modern headers: {status} {raw}")

    modern_init = make_body(4, "initialize")
    status, _, raw = send_http(authority, "POST", "/mcp", modern_init, std_headers(token, "initialize"))
    require(status == 404 and json.loads(raw)["error"]["code"] == -32601, f"legacy init with modern headers: {status} {raw}")

    missing_meta = json.dumps({"jsonrpc": "2.0", "id": 5, "method": "server/discover", "params": {"_meta": {"io.modelcontextprotocol/protocolVersion": PROTO}}}).encode()
    status, _, raw = send_http(authority, "POST", "/mcp", missing_meta, std_headers(token, "server/discover"))
    require(status == 400 and json.loads(raw)["error"]["code"] == -32602, f"missing clientCaps: {status} {raw}")

    # 4. Both representations (JSON and SSE) + inert legacy headers on fresh connections.
    list_body = make_body(6, "tools/list")
    status, hdrs, json_raw = send_http(authority, "POST", "/mcp", list_body, std_headers(token, "tools/list"))
    require(status == 200 and hdrs.get("Content-Type", "").startswith("application/json"), f"JSON tools/list: {status} {hdrs}")
    require("Mcp-Session-Id" not in hdrs and "Last-Event-ID" not in hdrs, f"echoed legacy header: {hdrs}")

    status, hdrs2, legacy_raw = send_http(
        authority,
        "POST",
        "/mcp",
        list_body,
        std_headers(token, "tools/list", extra={"Mcp-Session-Id": "sess-1", "Last-Event-ID": "evt-7"}),
    )
    require(status == 200 and legacy_raw == json_raw and "Mcp-Session-Id" not in hdrs2 and "Last-Event-ID" not in hdrs2, f"legacy headers changed response: {status} {hdrs2}")

    conn, resp = open_sse(authority, "/mcp", list_body, std_headers(token, "tools/list", accept="text/event-stream, application/json;q=0.5"))
    try:
        require(resp.status == 200 and resp.getheader("Content-Type", "").startswith("text/event-stream"), f"SSE tools/list: {resp.status} {resp.getheaders()}")
        ev = read_sse_event(resp)
        require(ev is not None and ev["id"] == 6 and len(ev["result"]["tools"]) > 0, f"SSE tools/list event: {ev}")
        require(read_sse_event(resp) is None, "unexpected second SSE event on tools/list")
    finally:
        conn.close()


def call_tool_json(authority, token, req_id, name, arguments):
    body = make_body(req_id, "tools/call", {"name": name, "arguments": arguments})
    status, _, raw = send_http(authority, "POST", "/mcp", body, std_headers(token, "tools/call", name=name))
    require(status == 200, f"tools/call {name}: status={status} body={raw}")
    return json.loads(raw)


def exercise_remote_jobs_cancel_and_rotation(fixture, authority, token_path, token1):
    host_id = fixture.remote_id
    long_receipt = create_app_job_cli(
        fixture,
        host_id,
        "http-agent",
        "ctx-http-long-1",
        "mcp-http-long-1",
        ["/bin/sh", "-c", "read line; printf 'http-got:%s\\n' \"$line\"; exec /bin/sleep 60"],
        stdin_mode="open",
    )
    job_handle = f"mj1.{host_id}.{long_receipt['sessionId']}.{long_receipt['instance']}"

    # Stdin write & output read over HTTP.
    inp = call_tool_json(authority, token1, 10, "mesh_job_input", {"job": job_handle, "write_key": "hw1", "offset": "0", "data": "wave\n"})
    require(not inp["result"]["isError"] and inp["result"]["structuredContent"]["data"]["length"] == 5, f"HTTP job_input: {inp}")

    def saw_echo():
        out = call_tool_json(authority, token1, 11, "mesh_job_output", {"job": job_handle, "stream": "stdout"})
        return out["result"]["structuredContent"]["data"]["data"] == "http-got:wave\n"

    eventually(saw_echo, "remote job did not echo HTTP stdin")

    # 1. Stream cancellation: open SSE mesh_job_wait, receive progress, close stream -> job keeps running!
    wait_body = make_body(12, "tools/call", {"name": "mesh_job_wait", "arguments": {"job": job_handle, "wait_ms": 30000}}, progress_token="http-cancel-1")
    conn, resp = open_sse(authority, "/mcp", wait_body, std_headers(token1, "tools/call", name="mesh_job_wait"))
    require(resp.status == 200 and resp.getheader("Content-Type", "").startswith("text/event-stream"), f"wait SSE open: {resp.status}")
    prog = read_sse_event(resp)
    require(prog is not None and prog.get("method") == "notifications/progress" and prog["params"]["progressToken"] == "http-cancel-1", f"missing wait progress: {prog}")
    conn.close()

    status_after_cancel = call_tool_json(authority, token1, 13, "mesh_job_status", {"job": job_handle})
    require(status_after_cancel["result"]["structuredContent"]["data"]["state"] == "running", f"stream close killed job: {status_after_cancel}")

    # 2. Token rotation: open SSE wait under token1, rotate via CLI -> old SSE stream retires,
    # old token gets 401, new token works, token never logged, and remote job STILL runs!
    wait_body2 = make_body(14, "tools/call", {"name": "mesh_job_wait", "arguments": {"job": job_handle, "wait_ms": 30000}}, progress_token="http-rot-1")
    # SSE is close-delimited, so http.client hands the socket to the response
    # and conn2.sock is None; give the stream its 5 s read limit up front.
    conn2, resp2 = open_sse(authority, "/mcp", wait_body2, std_headers(token1, "tools/call", name="mesh_job_wait"), timeout=5.0)
    try:
        require(resp2.status == 200 and resp2.getheader("Content-Type", "").startswith("text/event-stream"), f"rot SSE open: {resp2.status}")
        prog2 = read_sse_event(resp2)
        require(prog2 is not None and prog2.get("method") == "notifications/progress", f"missing rot progress: {prog2}")

        rot = run_cli(
            fixture.binary,
            fixture.environment,
            "mcp",
            "token",
            "rotate",
            "--http",
            authority,
            "--identity",
            "http-agent",
            "--json",
        )
        require(rot.returncode == 0 and rot.stderr == "", f"token rotate failed: rc={rot.returncode} out={rot.stdout!r} err={rot.stderr!r}")
        rot_env = json.loads(rot.stdout)
        require(
            rot_env["status"] == "ok" and rot_env["data"]["generation"] == 2 and rot_env["data"]["retiredStreams"] >= 1,
            f"rotate result: {rot_env}",
        )

        # Retired SSE stream must end promptly with EOF (no terminal JSON-RPC result).
        tail = read_sse_event(resp2)
        require(tail is None, f"retired SSE stream delivered unexpected event: {tail}")
    finally:
        conn2.close()

    rec2 = json.loads(token_path.read_text(encoding="utf-8"))
    token2 = rec2["token"]
    require(rec2["generation"] == 2 and token2 and token2 != token1, f"rotated token file: {redact_record(rec2)}")
    require(stat.S_IMODE(token_path.stat().st_mode) == 0o600, f"rotated token file mode: {oct(token_path.stat().st_mode)}")
    require(token1 not in rot.stdout and token2 not in rot.stdout and token1 not in rot.stderr and token2 not in rot.stderr, "token leaked in rotate CLI output")

    # Old token now refused 401.
    status_old, _, _ = send_http(authority, "POST", "/mcp", make_body(15, "server/discover"), std_headers(token1, "server/discover"))
    require(status_old == 401, f"old token still accepted: {status_old}")

    # New token succeeds and confirms remote job survived rotation!
    status_after_rot = call_tool_json(authority, token2, 16, "mesh_job_status", {"job": job_handle})
    require(status_after_rot["result"]["structuredContent"]["data"]["state"] == "running", f"rotation killed remote job: {status_after_rot}")

    # Explicitly cancel the remote job via mesh_job_cancel and wait for killed.
    cancel_res = call_tool_json(authority, token2, 17, "mesh_job_cancel", {"job": job_handle})
    require(not cancel_res["result"]["isError"] and cancel_res["result"]["structuredContent"]["data"]["delivery"] == "delivered", f"HTTP job_cancel: {cancel_res}")
    ended = call_tool_json(authority, token2, 18, "mesh_job_wait", {"job": job_handle, "wait_ms": 10000})
    require(ended["result"]["structuredContent"]["data"]["state"] == "killed", f"HTTP job not killed: {ended}")
    return token2


def exercise_http_context_and_exec(fixture, authority, token):
    """Open a durable context and run a short job over loopback HTTP via shared dispatch."""
    host_id = fixture.remote_id
    opened = call_tool_json(
        authority,
        token,
        50,
        "mesh_context_open",
        {"host": host_id, "request_key": "http-exec-ctx-1", "cwd": str(fixture.root), "shell": "sh"},
    )
    require(not opened["result"]["isError"], f"HTTP mesh_context_open failed: {opened}")
    ctx_handle = opened["result"]["structuredContent"]["data"]["handle"]
    require(ctx_handle.startswith(f"mc1.{host_id}."), f"unexpected HTTP context handle: {ctx_handle}")

    ran = call_tool_json(
        authority,
        token,
        51,
        "mesh_exec",
        {
            "context": ctx_handle,
            "request_key": "http-exec-job-1",
            "command": "printf 'hello-http-exec\\n'",
            "wait_ms": 10000,
        },
    )
    require(not ran["result"]["isError"], f"HTTP mesh_exec failed: {ran}")
    data = ran["result"]["structuredContent"]["data"]
    require(data["state"] == "exited" and data["result"]["termination"]["code"] == 0, f"HTTP mesh_exec result: {data}")
    require(data["stdout"]["data"] == "hello-http-exec\n", f"HTTP mesh_exec stdout: {data}")

    closed = call_tool_json(authority, token, 52, "mesh_context_close", {"context": ctx_handle})
    require(not closed["result"]["isError"] and closed["result"]["structuredContent"]["data"]["state"] == "closed", f"HTTP mesh_context_close failed: {closed}")


def exercise_application_identity_isolation(fixture, root):
    host_id = fixture.remote_id
    for name in ("review-a", "review-b"):
        created = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        require(created.returncode == 0, f"identity create {name} failed: {created.stderr}")

    app_receipt = create_app_job_cli(
        fixture,
        host_id,
        "review-a",
        "ctx-http-a",
        "job-http-a",
        ["/bin/sh", "-c", "printf 'owned-by-a\\n'; exec /bin/sleep 60"],
        stdin_mode="open",
    )
    job_handle = f"mj1.{host_id}.{app_receipt['sessionId']}.{app_receipt['instance']}"

    tok_a = root / "review-a-token.json"
    tok_b = root / "review-b-token.json"
    proc_a = subprocess.Popen(
        [fixture.binary, "mcp", "--http", "127.0.0.1:0", "--identity", "review-a", "--token-file", str(tok_a)],
        env=fixture.environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    proc_b = subprocess.Popen(
        [fixture.binary, "mcp", "--http", "127.0.0.1:0", "--identity", "review-b", "--token-file", str(tok_b)],
        env=fixture.environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        rec_a = wait_token_file(tok_a, proc_a)
        rec_b = wait_token_file(tok_b, proc_b)
        auth_a, token_a = rec_a["listener"], rec_a["token"]
        auth_b, token_b = rec_b["listener"], rec_b["token"]

        # Owning listener review-a can read status, output, list, and cancel its own job.
        st_a = call_tool_json(auth_a, token_a, 101, "mesh_job_status", {"job": job_handle})
        require(not st_a["result"]["isError"] and st_a["result"]["structuredContent"]["data"]["state"] == "running", f"review-a status failed: {st_a}")

        def saw_a_output():
            out_a = call_tool_json(auth_a, token_a, 102, "mesh_job_output", {"job": job_handle, "stream": "stdout"})
            return not out_a["result"]["isError"] and out_a["result"]["structuredContent"]["data"]["data"] == "owned-by-a\n"

        eventually(saw_a_output, "review-a did not read its own job output over HTTP")

        list_a = call_tool_json(auth_a, token_a, 103, "mesh_jobs_list", {"host": host_id, "all": True})
        jobs_a = [item["job"] for item in list_a["result"]["structuredContent"]["data"]["jobs"]]
        require(job_handle in jobs_a, f"review-a job missing from review-a list: {jobs_a}")

        # Other listener review-b MUST be denied on review-a's job and MUST NOT list it.
        st_b = call_tool_json(auth_b, token_b, 201, "mesh_job_status", {"job": job_handle})
        require(
            st_b["result"]["isError"] and st_b["result"]["structuredContent"]["errors"][0]["code"] == "access.denied",
            f"review-b status was not access.denied: {st_b}",
        )
        out_b = call_tool_json(auth_b, token_b, 202, "mesh_job_output", {"job": job_handle, "stream": "stdout"})
        require(
            out_b["result"]["isError"] and out_b["result"]["structuredContent"]["errors"][0]["code"] == "access.denied",
            f"review-b output was not access.denied: {out_b}",
        )
        cancel_b = call_tool_json(auth_b, token_b, 203, "mesh_job_cancel", {"job": job_handle})
        require(
            cancel_b["result"]["isError"] and cancel_b["result"]["structuredContent"]["errors"][0]["code"] == "access.denied",
            f"review-b cancel was not access.denied: {cancel_b}",
        )
        list_b = call_tool_json(auth_b, token_b, 204, "mesh_jobs_list", {"host": host_id, "all": True})
        jobs_b = [item["job"] for item in list_b["result"]["structuredContent"]["data"]["jobs"]]
        require(job_handle not in jobs_b, f"review-b listed review-a job: {jobs_b}")

        # Clean up review-a's job through review-a's HTTP listener.
        cancel_a = call_tool_json(auth_a, token_a, 104, "mesh_job_cancel", {"job": job_handle})
        require(not cancel_a["result"]["isError"] and cancel_a["result"]["structuredContent"]["data"]["delivery"] == "delivered", f"review-a cancel failed: {cancel_a}")
        wait_a = call_tool_json(auth_a, token_a, 105, "mesh_job_wait", {"job": job_handle, "wait_ms": 10000})
        require(wait_a["result"]["structuredContent"]["data"]["state"] == "killed", f"review-a job not killed: {wait_a}")
    finally:
        for p in (proc_a, proc_b):
            if p.poll() is None:
                p.send_signal(signal.SIGTERM)
                try:
                    p.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait(timeout=5)


def main():
    binary = sys.argv[1]
    with tempfile.TemporaryDirectory(prefix="mesh-mcp-http-") as directory:
        root = Path(directory).resolve()
        check_startup_usage_errors(binary, root)

        (root / "fleet").mkdir()
        fixture = MCPFixture(binary, root / "fleet")
        proc = None
        try:
            fixture.start_remote()
            created = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", "http-agent", "--json")
            require(created.returncode == 0, f"identity create failed: {created.stderr}")

            token_path = root / "http-token.json"
            proc = subprocess.Popen(
                [
                    fixture.binary,
                    "mcp",
                    "--http",
                    "127.0.0.1:0",
                    "--identity",
                    "http-agent",
                    "--allow-origin",
                    ALLOWED_ORIGIN,
                    "--token-file",
                    str(token_path),
                ],
                env=fixture.environment,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            rec = wait_token_file(token_path, proc)
            require(stat.S_IMODE(token_path.stat().st_mode) == 0o600, f"token file mode: {oct(token_path.stat().st_mode)}")
            require(rec["identity"] == "http-agent" and rec["generation"] == 1, f"initial token record: {redact_record(rec)}")
            authority = rec["listener"]
            token1 = rec["token"]

            check_endpoint_and_protocol_matrix(authority, token1)
            token2 = exercise_remote_jobs_cancel_and_rotation(fixture, authority, token_path, token1)
            exercise_http_context_and_exec(fixture, authority, token2)
            exercise_application_identity_isolation(fixture, root)

            # Clean SIGTERM shutdown removes token file and emits zero stdout and zero token leakage.
            proc.send_signal(signal.SIGTERM)
            stdout_b, stderr_b = proc.communicate(timeout=5.0)
            stderr_s = stderr_b.decode("utf-8", errors="replace")
            require(proc.returncode == 0, f"SIGTERM exit={proc.returncode} stderr={redact_text(stderr_s, token1, token2)!r}")
            require(stdout_b == b"", f"expected empty stdout, got {redact_text(stdout_b.decode('utf-8', errors='replace'), token1, token2)!r}")
            require(token1 not in stderr_s and token2 not in stderr_s, "bearer token leaked to stderr")
            require(not token_path.exists(), "token file was not removed on clean shutdown")
            proc = None
        finally:
            if proc is not None and proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)
            fixture.close()
    print("PASS: native MCP loopback HTTP server verified across auth, authority/Origin, section 2.2 matrix, JSON/SSE, stream cancel, token rotation, and clean shutdown")


if __name__ == "__main__":
    main()
