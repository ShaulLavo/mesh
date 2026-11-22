"""End-to-end M8 proof: two distinct Mesh client devices connecting to one
hosted installation (both stdio and external HTTP upstreams) plus fleet-wide
mesh mcp ls states, private scoped cache, invalidation, and agent envelopes."""
import http.server
import json
import os
from pathlib import Path
import select
import socketserver
import subprocess
import sys
import threading
import time

from pipe_execution import PipeFixture
from terminal_window import print_diagnostics, require

PROTO = "2026-07-28"
META = {
    "io.modelcontextprotocol/protocolVersion": PROTO,
    "io.modelcontextprotocol/clientCapabilities": {},
}

# Stateless 2026-07-28 stdio MCP server hosted on the owning machine only.
STDIO_SERVER = r"""#!/usr/bin/env python3
import json
import os
import sys
import threading
import time

SECRET = os.environ.get("HOST_API_KEY", "")
if SECRET:
    sys.stderr.write(f"startup secret={SECRET}\n")
    sys.stderr.flush()

PID = os.getpid()
LOCK = threading.Lock()
CALLS = 0
PENDING = {}


def send(obj):
    with LOCK:
        sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
        sys.stdout.flush()


def handle(req):
    global CALLS
    method = req.get("method")
    req_id = req.get("id")
    params = req.get("params") or {}
    if req_id is None:
        if method == "notifications/cancelled":
            target = json.dumps(params.get("requestId"), separators=(",", ":"))
            ev = PENDING.pop(target, None)
            if ev is not None:
                ev.set()
                sys.stderr.write(f"cancelled {target}\n")
                sys.stderr.flush()
        return

    if method == "server/discover":
        send({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "resultType": "complete",
                "supportedVersions": ["2026-07-28"],
                "capabilities": {"tools": {"listChanged": False}},
                "ttlMs": 60000,
                "cacheScope": "private",
                "_meta": {
                    "io.modelcontextprotocol/serverInfo": {
                        "name": "host-stdio-srv",
                        "version": "2.1.0",
                    }
                },
            },
        })
        return

    if method == "tools/list":
        send({
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "resultType": "complete",
                "tools": [
                    {"name": "whoami", "inputSchema": {"type": "object"}},
                    {"name": "echo", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
                    {"name": "slow", "inputSchema": {"type": "object"}},
                ],
                "ttlMs": 60000,
                "cacheScope": "private",
            },
        })
        return

    if method == "tools/call":
        with LOCK:
            CALLS += 1
            call_num = CALLS
        name = params.get("name")
        args = params.get("arguments") or {}
        if name == "whoami":
            send({
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "resultType": "complete",
                    "content": [{"type": "text", "text": f"pid={PID} calls={call_num} has_secret={bool(SECRET)}"}],
                },
            })
            return
        if name == "echo":
            send({
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "resultType": "complete",
                    "content": [{"type": "text", "text": str(args.get("text", ""))}],
                },
            })
            return
        if name == "slow":
            key = json.dumps(req_id, separators=(",", ":"))
            ev = threading.Event()
            PENDING[key] = ev
            if not ev.wait(timeout=30):
                PENDING.pop(key, None)
                send({
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {"resultType": "complete", "content": [{"type": "text", "text": "slow-done"}]},
                })
            return

    send({"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": "method not found"}})


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    threading.Thread(target=handle, args=(req,), daemon=True).start()
"""

# Legacy server that refuses server/discover to exercise incompatible state.
LEGACY_SERVER = r"""#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\("[^"]*"\).*/\1/p')
  printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
done
"""


class ExternalHTTPMCPServer:
    """Stateless 2026-07-28 loopback HTTP MCP server with x-mcp-header mirroring."""

    def __init__(self, expected_auth):
        self.expected_auth = expected_auth
        self.seen_headers = []
        self.discover_calls = 0
        self.list_calls = 0
        self.tool_calls = 0
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, _fmt, *_args):
                pass

            def do_POST(self):
                if self.path != "/mcp":
                    self.send_response(404)
                    self.end_headers()
                    return
                if self.headers.get("Authorization") != outer.expected_auth:
                    self.send_response(401)
                    self.end_headers()
                    return
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length).decode("utf-8"))
                req_id = body.get("id")
                method = body.get("method")
                params = body.get("params") or {}

                if method == "server/discover":
                    outer.discover_calls += 1
                    reply = {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "resultType": "complete",
                            "supportedVersions": [PROTO],
                            "capabilities": {"tools": {"listChanged": False}},
                            "ttlMs": 60000,
                            "cacheScope": "private",
                            "_meta": {
                                "io.modelcontextprotocol/serverInfo": {
                                    "name": "host-http-srv",
                                    "version": "3.0.0",
                                }
                            },
                        },
                    }
                elif method == "tools/list":
                    outer.list_calls += 1
                    reply = {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "resultType": "complete",
                            "tools": [{
                                "name": "scoped_lookup",
                                "inputSchema": {
                                    "type": "object",
                                    "properties": {
                                        "scope": {"type": "string", "x-mcp-header": "Scope"},
                                    },
                                    "required": ["scope"],
                                },
                            }],
                            "ttlMs": 60000,
                            "cacheScope": "private",
                        },
                    }
                elif method == "tools/call":
                    outer.tool_calls += 1
                    mirrored = self.headers.get("Mcp-Param-Scope", "")
                    outer.seen_headers.append(mirrored)
                    scope_arg = (params.get("arguments") or {}).get("scope", "")
                    if mirrored != scope_arg:
                        reply = {
                            "jsonrpc": "2.0",
                            "id": req_id,
                            "error": {"code": -32020, "message": f"Mcp-Param-Scope mismatch: {mirrored!r} != {scope_arg!r}"},
                        }
                        self._send_json(400, reply)
                        return
                    reply = {
                        "jsonrpc": "2.0",
                        "id": req_id,
                        "result": {
                            "resultType": "complete",
                            "content": [{"type": "text", "text": f"http-ok scope={scope_arg} header={mirrored}"}],
                        },
                    }
                else:
                    reply = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": "method not found"}}
                self._send_json(200, reply)

            def _send_json(self, status, obj):
                raw = json.dumps(obj, separators=(",", ":")).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        self.httpd = socketserver.TCPServer(("127.0.0.1", 0), Handler)
        self.port = self.httpd.server_address[1]
        self.url = f"http://127.0.0.1:{self.port}/mcp"
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class ConnectClient:
    def __init__(self, binary, env, target, identity_name, extra=()):
        self.proc = subprocess.Popen(
            [binary, "mcp", "connect", target, "--identity", identity_name, *extra],
            env=env,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.next_id = 1

    def send_raw(self, obj):
        self.proc.stdin.write((json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8"))
        self.proc.stdin.flush()

    def request(self, method, params=None, req_id=None):
        if req_id is None:
            req_id = self.next_id
            self.next_id += 1
        body = {"_meta": dict(META)}
        if params:
            body.update(params)
        self.send_raw({"jsonrpc": "2.0", "id": req_id, "method": method, "params": body})
        return req_id

    def read_message(self, timeout=20.0):
        ready, _, _ = select.select([self.proc.stdout], [], [], timeout)
        require(bool(ready), f"timed out reading MCP stdout after {timeout}s")
        line = self.proc.stdout.readline()
        require(bool(line), "MCP connect closed stdout unexpectedly")
        return json.loads(line.decode("utf-8"))

    def call_tool(self, name, arguments=None, timeout=20.0):
        req_id = self.request("tools/call", {"name": name, "arguments": arguments or {}})
        while True:
            msg = self.read_message(timeout=timeout)
            if "method" in msg:
                continue
            require(msg.get("id") == req_id, f"unexpected reply ID: {msg}")
            return msg

    def close(self):
        if self.proc.stdin and not self.proc.stdin.closed:
            self.proc.stdin.close()
        rc = self.proc.wait(timeout=5)
        stderr = self.proc.stderr.read().decode("utf-8", errors="replace")
        return rc, stderr

    def terminate(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(timeout=5)


def run_mesh(binary, env, *args, check_rc=None):
    res = subprocess.run(
        [binary, *args],
        env=env,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    if check_rc is not None:
        require(
            res.returncode == check_rc,
            f"mesh {args}: rc={res.returncode} (want {check_rc})\nstdout={res.stdout}\nstderr={res.stderr}",
        )
    return res


def setup_client_device(binary, root, name, hosts_list, machine_names=None):
    """Create an isolated Mesh client device with its own device key and NO server files."""
    dev_root = root / f"client-{name}"
    cfg = dev_root / "config"
    state = dev_root / "state"
    dev_root.mkdir(parents=True, mode=0o700)
    dev_root.chmod(0o700)
    cfg.mkdir(mode=0o700)
    cfg.chmod(0o700)
    state.mkdir(mode=0o700)
    state.chmod(0o700)
    env = dict(os.environ, HOME=str(dev_root), MESH_CONFIG_DIR=str(cfg), MESH_STATE_DIR=str(state))
    # Generate this device's own unique Ed25519 key via mesh device identity.
    id_res = run_mesh(binary, env, "device", "identity", "--json", check_rc=0)
    device_id = json.loads(id_res.stdout)["id"]
    hosts_path = cfg / "hosts.json"
    hosts_path.write_text(json.dumps({"version": 1, "hosts": hosts_list}))
    hosts_path.chmod(0o600)
    if machine_names:
        names_dir = cfg / "machine-names"
        names_dir.mkdir(exist_ok=True, mode=0o700)
        names_dir.chmod(0o700)
        for host_id, claim in machine_names.items():
            claim_path = names_dir / f"{host_id}.json"
            claim_path.write_text(json.dumps(claim))
            claim_path.chmod(0o600)
    return env, cfg, state, device_id


def main():
    root = Path(os.environ["HOME"]).parent / "mcp-connect"
    root.mkdir()
    binary = sys.argv[1]
    fixture = PipeFixture(binary, root)

    # Host-side server scripts live strictly under host_only/ (never in client dirs).
    host_only = root / "host-only"
    host_only.mkdir()
    stdio_script = host_only / "stdio_srv.py"
    stdio_script.write_text(STDIO_SERVER)
    stdio_script.chmod(0o700)

    legacy_script = host_only / "legacy_srv.sh"
    legacy_script.write_text(LEGACY_SERVER)
    legacy_script.chmod(0o700)

    secret_val = "super-secret-host-token-999"
    secret_file = host_only / "api.secret"
    secret_file.write_text(secret_val)
    secret_file.chmod(0o600)

    http_secret_val = "Bearer http-secret-token-777"
    http_secret_file = host_only / "http.secret"
    http_secret_file.write_text(http_secret_val)
    http_secret_file.chmod(0o600)

    http_server = ExternalHTTPMCPServer(http_secret_val)
    try:
        # Start hosting daemon on a loopback tailnet control port via Fixture.start_remote().
        fixture.start_remote()
        remote_hosts = json.loads((fixture.config / "hosts.json").read_text())["hosts"]
        host_entry = remote_hosts[0]
        remote_claim = json.loads((fixture.config / "machine-names" / f"{fixture.remote_id}.json").read_text())
        remote_name = remote_claim["machineName"]

        host_env = dict(
            fixture.environment,
            MESH_CONFIG_DIR=str(root / "remote-config"),
            MESH_STATE_DIR=str(fixture.remote),
        )

        # Publish stdio (isolated), stdio (shared), external HTTP, incompatible, and failing services on the host.
        pub_iso = json.loads(run_mesh(
            binary, host_env, "mcp", "publish", "repo-iso",
            "--cwd", str(host_only),
            "--account-mode", "host-account", "--account-label", "host-bot",
            "--secret-env", f"HOST_API_KEY={secret_file}",
            "--json", "--", sys.executable, str(stdio_script),
            check_rc=0,
        ).stdout)
        require(pub_iso["operation"] == "mcp.publish" and pub_iso["status"] == "ok", pub_iso)
        require(secret_val not in json.dumps(pub_iso) and str(secret_file) not in json.dumps(pub_iso), "secret leaked in publish")

        run_mesh(
            binary, host_env, "mcp", "publish", "repo-shared",
            "--cwd", str(host_only), "--shared",
            "--json", "--", sys.executable, str(stdio_script),
            check_rc=0,
        )
        run_mesh(
            binary, host_env, "mcp", "publish", "search-http",
            "--url", http_server.url, "--shared",
            "--account-mode", "host-account", "--account-label", "search-bot",
            "--header-secret", f"Authorization={http_secret_file}",
            "--json",
            check_rc=0,
        )
        run_mesh(
            binary, host_env, "mcp", "publish", "legacy-srv",
            "--cwd", str(host_only), "--shared", "--json", "--", "/bin/sh", str(legacy_script),
            check_rc=0,
        )
        run_mesh(
            binary, host_env, "mcp", "publish", "missing-rt",
            "--cwd", str(host_only), "--shared", "--json", "--", "/bin/sh", "-c", "exit 127",
            check_rc=0,
        )

        # Trigger incompatible and failed states once so ls observes them without starting anything itself.
        run_mesh(binary, host_env, "mcp", "start", "legacy-srv", "--json", check_rc=1)
        run_mesh(binary, host_env, "mcp", "start", "missing-rt", "--json", check_rc=1)

        # Configure TWO distinct client devices (Device A and Device B) with separate device keys.
        offline_entry = {
            "id": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            "meshIdentity": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            "tailscaleName": "offlinebox.fixture.test",
            "addresses": ["127.0.0.1"],
            "endpoint": "ws://127.0.0.1:1/mesh",
        }
        env_a, _cfg_a, state_a, dev_a_id = setup_client_device(
            binary, root, "dev-a", [host_entry, offline_entry],
            {fixture.remote_id: remote_claim},
        )
        env_b, _cfg_b, _state_b, dev_b_id = setup_client_device(
            binary, root, "dev-b", [host_entry],
            {fixture.remote_id: remote_claim},
        )
        require(dev_a_id != dev_b_id, "client devices must have distinct Mesh device keys")

        # Approve both client devices on the hosting daemon.
        run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", dev_a_id, check_rc=0)
        run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", dev_b_id, check_rc=0)

        # Create per-agent application identities on each client device.
        run_mesh(binary, env_a, "mcp", "identity", "create", "codex-a", "--json", check_rc=0)
        run_mesh(binary, env_b, "mcp", "identity", "create", "codex-b", "--json", check_rc=0)

        # 1. Passive fleet-wide `mesh mcp ls` from Device A before any calls:
        #    - hostpc online, offlinebox offline -> partial exit 3
        #    - repo-iso, repo-shared, search-http are STILL stopped (ls never starts services!)
        #    - legacy-srv is incompatible, missing-rt is failed
        #    - no secrets in output
        t0 = time.monotonic()
        ls_a = run_mesh(binary, env_a, "mcp", "ls", "--identity", "codex-a", "--json", check_rc=3)
        elapsed = time.monotonic() - t0
        require(elapsed < 2.5, f"fleet mcp ls exceeded 2s budget: {elapsed:.2f}s")
        ls_env = json.loads(ls_a.stdout)
        require(ls_env["operation"] == "mcp.list" and ls_env["status"] == "partial", ls_env)
        require(secret_val not in ls_a.stdout and http_secret_val not in ls_a.stdout, "secret leaked in mcp ls")
        require(str(secret_file) not in ls_a.stdout and str(http_secret_file) not in ls_a.stdout, "secret path leaked in mcp ls")

        host_states = {h["hostId"]: h["state"] for h in ls_env["data"]["hosts"]}
        require(host_states[fixture.remote_id] == "online", host_states)
        require(host_states[offline_entry["id"]] == "offline", host_states)
        svc_states = {s["name"]: s["state"] for s in ls_env["data"]["services"]}
        require(svc_states["repo-iso"] == "stopped", svc_states)
        require(svc_states["repo-shared"] == "stopped", svc_states)
        require(svc_states["search-http"] == "stopped", svc_states)
        require(svc_states["legacy-srv"] == "incompatible", svc_states)
        require(svc_states["missing-rt"] == "failed", svc_states)
        require(http_server.discover_calls == 0 and http_server.list_calls == 0, "ls contacted HTTP upstream")
        print("PASS: fleet-wide mesh mcp ls shows online/offline/stopped/incompatible/failed passively", flush=True)

        # 2. Fresh direct tool call from Device A AND Device B on isolated stdio service (NO prior discover/list).
        client_a = ConnectClient(binary, env_a, f"{remote_name}/repo-iso", "codex-a")
        client_b = ConnectClient(binary, env_b, f"{remote_name}/repo-iso", "codex-b")
        try:
            rep_a = client_a.call_tool("whoami")
            rep_b = client_b.call_tool("whoami")
            text_a = rep_a["result"]["content"][0]["text"]
            text_b = rep_b["result"]["content"][0]["text"]
            require("has_secret=True" in text_a and "has_secret=True" in text_b, (text_a, text_b))
            pid_a = text_a.split()[0]
            pid_b = text_b.split()[0]
            require(pid_a != pid_b, f"two distinct devices shared an isolated process: {text_a} vs {text_b}")

            # Verify upstreamServerInfo is preserved even on a cold first tools/call!
            meta_a = rep_a["result"]["_meta"]
            require(meta_a["io.modelcontextprotocol/serverInfo"]["name"] == "mesh", meta_a)
            require(meta_a["com.github.shaullavo.mesh/upstreamServerInfo"]["name"] == "host-stdio-srv", meta_a)

            # Verify cancellation over H3 cancels the slow call cleanly.
            slow_id = client_a.request("tools/call", {"name": "slow", "arguments": {}})
            time.sleep(0.1)
            client_a.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": slow_id}})
            # Follow-up echo on the same connection still works.
            echo_rep = client_a.call_tool("echo", {"text": "still-alive"})
            require(echo_rep["result"]["content"][0]["text"] == "still-alive", echo_rep)

            # Legacy initialize is rejected with -32601.
            client_a.send_raw({"jsonrpc": "2.0", "id": 999, "method": "initialize", "params": {"protocolVersion": "2025-11-25", "capabilities": {}}})
            legacy_rep = client_a.read_message()
            require(legacy_rep["error"]["code"] == -32601, legacy_rep)

            rc_a, _ = client_a.close()
            rc_b, _ = client_b.close()
            require(rc_a == 0 and rc_b == 0, (rc_a, rc_b))
        finally:
            client_a.terminate()
            client_b.terminate()
        print("PASS: two distinct devices reached isolated hosted stdio server with fresh direct tool calls & cancellation", flush=True)

        # 3. Shared stdio service: Device A and Device B share ONE process on the host.
        shared_a = ConnectClient(binary, env_a, f"{remote_name}/repo-shared", "codex-a")
        shared_b = ConnectClient(binary, env_b, f"{remote_name}/repo-shared", "codex-b")
        try:
            sa = shared_a.call_tool("whoami")["result"]["content"][0]["text"]
            sb = shared_b.call_tool("whoami")["result"]["content"][0]["text"]
            require(sa.split()[0] == sb.split()[0], f"shared service started two processes: {sa} vs {sb}")
            require(shared_a.close()[0] == 0 and shared_b.close()[0] == 0, "shared connect exit non-zero")
        finally:
            shared_a.terminate()
            shared_b.terminate()
        print("PASS: two distinct devices shared one --shared hosted stdio process", flush=True)

        # 4. External HTTP upstream: cold fresh direct tool call triggers private cold schema discovery
        #    and mirrors x-mcp-header (X-Repo-Scope) + sends secret Authorization header.
        http_a = ConnectClient(binary, env_a, f"{remote_name}/search-http", "codex-a")
        http_b = ConnectClient(binary, env_b, f"{remote_name}/search-http", "codex-b")
        try:
            hr_a = http_a.call_tool("scoped_lookup", {"scope": "alpha"})
            hr_b = http_b.call_tool("scoped_lookup", {"scope": "beta"})
            require(hr_a["result"]["content"][0]["text"] == "http-ok scope=alpha header=alpha", hr_a)
            require(hr_b["result"]["content"][0]["text"] == "http-ok scope=beta header=beta", hr_b)
            require(hr_a["result"]["_meta"]["com.github.shaullavo.mesh/upstreamServerInfo"]["name"] == "host-http-srv", hr_a)
            require(http_server.discover_calls >= 1 and http_server.list_calls >= 1 and http_server.tool_calls == 2,
                    (http_server.discover_calls, http_server.list_calls, http_server.tool_calls))
            require(http_a.close()[0] == 0 and http_b.close()[0] == 0, "http connect exit non-zero")
        finally:
            http_a.terminate()
            http_b.terminate()
        print("PASS: external HTTP upstream performed private cold schema discovery and header mirroring for both devices", flush=True)

        # 5. Refresh Device A and Device B caches while remote is online, then test DENIED (revoked) -> immediate invalidation!
        run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=0)
        run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--json", check_rc=0)

        # Revoke Device B -> Device B gets unauthorized AND its private cache is immediately invalidated!
        run_mesh(binary, host_env, "device", "revoke", "--", dev_b_id, check_rc=0)
        denied_ls = run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--json", check_rc=1)
        denied_env = json.loads(denied_ls.stdout)
        require(denied_env["data"]["hosts"][0]["state"] == "unauthorized", denied_env)
        require(denied_env["data"]["services"] == [], denied_env)
        # Verify cached read on Device B also sees nothing (cache was discarded on denial!).
        cached_after_deny = run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--cached", "--json", check_rc=1)
        cached_deny_env = json.loads(cached_after_deny.stdout)
        require(cached_deny_env["data"]["hosts"][0]["state"] == "offline" and cached_deny_env["data"]["services"] == [], cached_deny_env)
        print("PASS: revoked device gets unauthorized and its private cache is immediately invalidated", flush=True)

        # 6. Stop hosting daemon -> Device A sees STALE (<24h) first, then EXPIRED (>24h)!
        fixture.daemon.terminate()
        fixture.daemon.wait(timeout=10)
        stale_ls = run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=3)
        stale_env = json.loads(stale_ls.stdout)
        require(stale_env["status"] == "partial", stale_env)
        require(stale_env["data"]["hosts"][0]["state"] == "stale", stale_env)
        require(any(s["name"] == "repo-iso" and s["state"] == "stale" for s in stale_env["data"]["services"]), stale_env)

        # Backdate Device A's cache file by >24 hours -> must become EXPIRED and not advertise active services!
        cache_dir = state_a / "mcp-catalog-cache"
        for p in cache_dir.glob("*.json"):
            data = json.loads(p.read_text())
            data["receivedAt"] = "2020-01-01T00:00:00Z"
            p.write_text(json.dumps(data))
        expired_ls = run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=1)
        expired_env = json.loads(expired_ls.stdout)
        require(expired_env["data"]["hosts"][0]["state"] == "expired", expired_env)
        require(expired_env["data"]["services"] == [], expired_env)
        print("PASS: unreachable host shows stale (<24h) and expired (>24h) distinctly", flush=True)

    except BaseException:
        print_diagnostics(fixture)
        raise
    finally:
        http_server.close()
        fixture.close()

    print("PASS: M8 direct connect and fleet catalog end-to-end acceptance")


if __name__ == "__main__":
    main()
