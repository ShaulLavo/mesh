"""End-to-end M8 proof: two distinct Mesh client devices connecting to one
hosted installation (both stdio and external HTTP upstreams), exact stdio/HTTP
cancellation, daemon-loss outcome_unknown, safe catalog projection, mi1 handle
validation (including 32-char service names), private scoped cache, and invalidation."""
import http.server
import json
import os
from pathlib import Path
import select
import socket
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

SECRET = os.environ.get("HOST_API_KEY", "")
TRACE_FILE = os.environ.get("STDIO_TRACE_FILE", "")
if SECRET:
    sys.stderr.write(f"startup secret={SECRET}\n")
    sys.stderr.flush()

PID = os.getpid()
LOCK = threading.Lock()
CALLS = 0
PENDING = {}   # canonical_id_json -> {"ev": Event, "tag": str, "cancelled": bool}
BY_TAG = {}    # tag -> canonical_id_json


def trace(line):
    if not TRACE_FILE:
        return
    with LOCK:
        with open(TRACE_FILE, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
            fh.flush()


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
            with LOCK:
                rec = PENDING.pop(target, None)
                if rec is not None:
                    BY_TAG.pop(rec["tag"], None)
                    rec["cancelled"] = True
                    rec["ev"].set()
            tag = rec["tag"] if rec is not None else "unknown"
            trace(f"cancelled {target} {tag}")
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
                        "x-hidden": "SYNTHETIC_SERVERINFO_SENTINEL",
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
                    {"name": "slow", "inputSchema": {"type": "object", "properties": {"tag": {"type": "string"}}}},
                    {"name": "release", "inputSchema": {"type": "object", "properties": {"tag": {"type": "string"}}}},
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
        if name == "release":
            tag = str(args.get("tag", ""))
            with LOCK:
                key = BY_TAG.pop(tag, None)
                rec = PENDING.pop(key, None) if key else None
                if rec is not None:
                    rec["ev"].set()
            send({
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "resultType": "complete",
                    "content": [{"type": "text", "text": f"released:{tag}"}],
                },
            })
            return
        if name == "slow":
            tag = str(args.get("tag", "default"))
            key = json.dumps(req_id, separators=(",", ":"))
            ev = threading.Event()
            rec = {"ev": ev, "tag": tag, "cancelled": False}
            with LOCK:
                PENDING[key] = rec
                BY_TAG[tag] = key
            trace(f"entered {tag} {key}")
            ev.wait(timeout=30)
            with LOCK:
                PENDING.pop(key, None)
                if BY_TAG.get(tag) == key:
                    BY_TAG.pop(tag, None)
                was_cancelled = rec["cancelled"]
            if not was_cancelled:
                send({
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {"resultType": "complete", "content": [{"type": "text", "text": f"slow-done:{tag}"}]},
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


class ThreadingHTTPServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True


class ExternalHTTPMCPServer:
    """Stateless 2026-07-28 loopback HTTP MCP server with x-mcp-header mirroring and cancellation tracking."""

    def __init__(self, expected_auth):
        self.expected_auth = expected_auth
        self.seen_headers = []
        self.discover_calls = 0
        self.list_calls = 0
        self.tool_calls = 0
        self.http_slow_entered = threading.Event()
        self.http_slow_cancelled = threading.Event()
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, _fmt, *_args):
                pass

            def do_POST(self):
                if self.path != "/mcp":
                    self.send_response(404)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if self.headers.get("Authorization") != outer.expected_auth:
                    self.send_response(401)
                    self.send_header("Content-Length", "0")
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
                            "tools": [
                                {
                                    "name": "scoped_lookup",
                                    "inputSchema": {
                                        "type": "object",
                                        "properties": {
                                            "scope": {"type": "string", "x-mcp-header": "Scope"},
                                        },
                                        "required": ["scope"],
                                    },
                                },
                                {
                                    "name": "slow_http",
                                    "inputSchema": {"type": "object"},
                                },
                            ],
                            "ttlMs": 60000,
                            "cacheScope": "private",
                        },
                    }
                elif method == "tools/call":
                    tool_name = params.get("name")
                    if tool_name == "slow_http":
                        outer.http_slow_entered.set()
                        # Wait until the Mesh host closes this outbound HTTP request on cancellation.
                        deadline = time.monotonic() + 20.0
                        sock = self.connection
                        while time.monotonic() < deadline:
                            ready, _, _ = select.select([sock], [], [], 0.05)
                            if ready:
                                try:
                                    peek = sock.recv(1, socket.MSG_PEEK)
                                except OSError:
                                    peek = b""
                                if not peek:
                                    outer.http_slow_cancelled.set()
                                    return
                        reply = {
                            "jsonrpc": "2.0",
                            "id": req_id,
                            "result": {"resultType": "complete", "content": [{"type": "text", "text": "http-slow-done"}]},
                        }
                        self._send_json(200, reply)
                        return

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

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
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
        buf = getattr(self, "_stdout_buf", b"")
        deadline = time.monotonic() + timeout
        fd = self.proc.stdout.fileno()
        while True:
            nl = buf.find(b"\n")
            if nl >= 0:
                line = buf[: nl + 1]
                self._stdout_buf = buf[nl + 1 :]
                return json.loads(line.decode("utf-8"))
            remaining = deadline - time.monotonic()
            require(remaining > 0, f"timed out reading MCP stdout after {timeout}s")
            ready, _, _ = select.select([fd], [], [], remaining)
            require(bool(ready), f"timed out reading MCP stdout after {timeout}s")
            chunk = os.read(fd, 65536)
            require(bool(chunk), "MCP connect closed stdout unexpectedly")
            buf += chunk

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


def wait_trace(trace_path, predicate, description, timeout=15.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if trace_path.exists():
            lines = trace_path.read_text(encoding="utf-8").splitlines()
            res = predicate(lines)
            if res is not None:
                return res
        time.sleep(0.05)
    existing = trace_path.read_text(encoding="utf-8") if trace_path.exists() else "<missing>"
    require(False, f"timed out waiting for {description}; trace=\n{existing}")


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

    host_only = root / "host-only"
    host_only.mkdir()
    stdio_script = host_only / "stdio_srv.py"
    stdio_script.write_text(STDIO_SERVER)
    stdio_script.chmod(0o700)

    trace_file = host_only / "stdio.trace"

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

    argv_sentinel = "SYNTHETIC_ARGV_SENTINEL_901"
    env_sentinel = "SYNTHETIC_ENV_SENTINEL_902"

    http_server = ExternalHTTPMCPServer(http_secret_val)
    try:
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

        # Publish stdio (isolated), stdio (shared), 32-char named stdio (isolated), external HTTP, incompatible, and failing services.
        pub_iso = json.loads(run_mesh(
            binary, host_env, "mcp", "publish", "repo-iso",
            "--cwd", str(host_only),
            "--env", f"STDIO_TRACE_FILE={trace_file}",
            "--env", f"PLAIN_SENTINEL={env_sentinel}",
            "--account-mode", "host-account", "--account-label", "host-bot",
            "--secret-env", f"HOST_API_KEY={secret_file}",
            "--json", "--", sys.executable, str(stdio_script), f"--marker={argv_sentinel}",
            check_rc=0,
        ).stdout)
        require(pub_iso["operation"] == "mcp.publish" and pub_iso["status"] == "ok", pub_iso)
        for forbidden in (secret_val, str(secret_file), argv_sentinel, env_sentinel, str(stdio_script)):
            require(forbidden not in json.dumps(pub_iso), f"publish leaked {forbidden}")

        name32 = "service-name-exact-32-characters"
        require(len(name32) == 32, f"expected 32-char service name, got {len(name32)}")
        run_mesh(
            binary, host_env, "mcp", "publish", name32,
            "--cwd", str(host_only),
            "--json", "--", sys.executable, str(stdio_script),
            check_rc=0,
        )

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

        run_mesh(binary, host_env, "mcp", "start", "legacy-srv", "--json", check_rc=1)
        run_mesh(binary, host_env, "mcp", "start", "missing-rt", "--json", check_rc=1)

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

        run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", dev_a_id, check_rc=0)
        run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", dev_b_id, check_rc=0)

        run_mesh(binary, env_a, "mcp", "identity", "create", "codex-a", "--json", check_rc=0)
        run_mesh(binary, env_b, "mcp", "identity", "create", "codex-b", "--json", check_rc=0)

        # 1. Passive fleet-wide `mesh mcp ls` from Device A before any calls + safe catalog projection check.
        t0 = time.monotonic()
        ls_a = run_mesh(binary, env_a, "mcp", "ls", "--identity", "codex-a", "--json", check_rc=3)
        elapsed = time.monotonic() - t0
        require(elapsed < 2.5, f"fleet mcp ls exceeded 2s budget: {elapsed:.2f}s")
        ls_env = json.loads(ls_a.stdout)
        require(ls_env["operation"] == "mcp.list" and ls_env["status"] == "partial", ls_env)
        forbidden_catalog = (
            secret_val, http_secret_val, str(secret_file), str(http_secret_file),
            argv_sentinel, env_sentinel, str(stdio_script), str(host_only), http_server.url,
        )
        for token in forbidden_catalog:
            require(token not in ls_a.stdout, f"mcp ls leaked {token}")
        cache_dir = state_a / "mcp-catalog-cache"
        for cache_file in cache_dir.glob("*.json"):
            if cache_file.name.startswith("."):
                continue
            cached_text = cache_file.read_text(encoding="utf-8")
            for token in forbidden_catalog:
                require(token not in cached_text, f"catalog cache {cache_file.name} persisted {token}")

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
        print("PASS: fleet-wide mesh mcp ls shows safe allowlisted catalog and passive states", flush=True)

        # 2. Fresh direct tool calls from Device A & Device B + EXACT upstream request ID cancellation on stdio.
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

            meta_a = rep_a["result"]["_meta"]
            require(meta_a["io.modelcontextprotocol/serverInfo"]["name"] == "mesh", meta_a)
            require(meta_a["com.github.shaullavo.mesh/upstreamServerInfo"]["name"] == "host-stdio-srv", meta_a)

            # Prove exact upstream request ID cancellation: start TWO concurrent blocking calls,
            # wait until both enter upstream, cancel ONLY cancel-me, verify upstream received
            # notifications/cancelled with cancel-me's EXACT host-assigned ID while keep-me completes.
            cancel_req_id = client_a.request("tools/call", {"name": "slow", "arguments": {"tag": "cancel-me"}})
            keep_req_id = client_a.request("tools/call", {"name": "slow", "arguments": {"tag": "keep-me"}})

            def both_entered(lines):
                found = {}
                for ln in lines:
                    parts = ln.split(" ", 2)
                    if len(parts) == 3 and parts[0] == "entered":
                        found[parts[1]] = parts[2]
                if "cancel-me" in found and "keep-me" in found:
                    return found["cancel-me"], found["keep-me"]
                return None

            host_id_cancel, host_id_keep = wait_trace(trace_file, both_entered, "both concurrent slow calls to enter upstream")
            require(host_id_cancel != host_id_keep, (host_id_cancel, host_id_keep))

            client_a.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": cancel_req_id}})

            wait_trace(
                trace_file,
                lambda lines: True if f"cancelled {host_id_cancel} cancel-me" in lines else None,
                f"exact upstream cancellation for {host_id_cancel}",
            )

            # Release keep-me and collect both keep-me and release replies.
            rel_req_id = client_a.request("tools/call", {"name": "release", "arguments": {"tag": "keep-me"}})
            replies = {}
            while len(replies) < 2:
                msg = client_a.read_message(timeout=15.0)
                if "method" in msg:
                    continue
                replies[msg["id"]] = msg
            require(replies[keep_req_id]["result"]["content"][0]["text"] == "slow-done:keep-me", replies)
            require(replies[rel_req_id]["result"]["content"][0]["text"] == "released:keep-me", replies)
            trace_lines = trace_file.read_text(encoding="utf-8").splitlines()
            require(not any(ln.startswith(f"cancelled {host_id_keep}") for ln in trace_lines), trace_lines)

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
        print("PASS: exact upstream stdio cancellation isolated to target request ID + unknown serverInfo stripped", flush=True)

        # Verify unknown upstream serverInfo key was NOT leaked into ls or cache after ready probe.
        ls_after = run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=0)
        require("SYNTHETIC_SERVERINFO_SENTINEL" not in ls_after.stdout, ls_after.stdout)

        # 3. 32-char service name mi1 handle reconnect + wrong-service & other-caller handle refusal.
        c32 = ConnectClient(binary, env_a, f"{remote_name}/{name32}", "codex-a")
        try:
            pid32 = c32.call_tool("whoami")["result"]["content"][0]["text"].split()[0]
            require(c32.close()[0] == 0, "c32 close failed")
        finally:
            c32.terminate()

        ls32 = json.loads(run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=0).stdout)
        svc32 = next(s for s in ls32["data"]["services"] if s["name"] == name32)
        handle32 = svc32["instances"][0]["handle"]
        require(handle32.startswith(f"mi1.{fixture.remote_id}."), handle32)

        # Authorized caller reconnects using full mi1 handle to the exact same process.
        c32_re = ConnectClient(binary, env_a, f"{remote_name}/{name32}", "codex-a", extra=("--instance", handle32))
        try:
            pid32_re = c32_re.call_tool("whoami")["result"]["content"][0]["text"].split()[0]
            require(pid32_re == pid32, f"handle reconnect hit wrong process: {pid32_re} != {pid32}")
            require(c32_re.close()[0] == 0, "c32_re close failed")
        finally:
            c32_re.terminate()

        # Wrong service with valid mi1 handle for name32 -> refused as tool error with service.not_found.
        c32_wrong_svc = ConnectClient(binary, env_a, f"{remote_name}/repo-iso", "codex-a", extra=("--instance", handle32))
        try:
            wrong_rep = c32_wrong_svc.call_tool("whoami")
            require(wrong_rep["result"].get("isError") is True, wrong_rep)
            require(wrong_rep["result"]["structuredContent"]["errors"][0]["code"] == "service.not_found", wrong_rep)
            require(c32_wrong_svc.close()[0] == 0, "c32_wrong_svc close failed")
        finally:
            c32_wrong_svc.terminate()

        # Other caller (Device B / codex-b) with Device A's mi1 handle -> refused as tool error with access.denied.
        c32_other_caller = ConnectClient(binary, env_b, f"{remote_name}/{name32}", "codex-b", extra=("--instance", handle32))
        try:
            denied_rep = c32_other_caller.call_tool("whoami")
            require(denied_rep["result"].get("isError") is True, denied_rep)
            require(denied_rep["result"]["structuredContent"]["errors"][0]["code"] == "access.denied", denied_rep)
            require(c32_other_caller.close()[0] == 0, "c32_other_caller close failed")
        finally:
            c32_other_caller.terminate()
        print("PASS: mi1 handle validated end-to-end for 32-char service name, wrong service, and other caller", flush=True)

        # 4. Shared stdio service: Device A and Device B share ONE process on the host.
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

        # 5. External HTTP upstream: cold discovery, header mirroring, AND HTTP cancellation through H1!
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

            # HTTP cancellation through H1: start slow_http, wait for upstream entry, cancel on H1,
            # assert upstream HTTP handler observed cancellation, then run another tool call cleanly.
            slow_http_id = http_a.request("tools/call", {"name": "slow_http", "arguments": {}})
            require(http_server.http_slow_entered.wait(timeout=15.0), "slow_http did not reach external HTTP upstream")
            http_a.send_raw({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": slow_http_id}})
            require(http_server.http_slow_cancelled.wait(timeout=15.0), "HTTP upstream did not observe H1 cancellation")

            hr_after = http_a.call_tool("scoped_lookup", {"scope": "gamma"})
            require(hr_after["result"]["content"][0]["text"] == "http-ok scope=gamma header=gamma", hr_after)
            require(http_a.close()[0] == 0 and http_b.close()[0] == 0, "http connect exit non-zero")
        finally:
            http_a.terminate()
            http_b.terminate()
        print("PASS: external HTTP upstream discovery, header mirroring, and H1 cancellation verified", flush=True)

        # 6. Revoked device -> unauthorized AND immediate cache invalidation across caller scopes!
        run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=0)
        run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--json", check_rc=0)

        run_mesh(binary, host_env, "device", "revoke", "--", dev_b_id, check_rc=0)
        denied_ls = run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--json", check_rc=1)
        denied_env = json.loads(denied_ls.stdout)
        require(denied_env["data"]["hosts"][0]["state"] == "unauthorized", denied_env)
        require(denied_env["data"]["services"] == [], denied_env)
        cached_after_deny = run_mesh(binary, env_b, "mcp", "ls", "--host", remote_name, "--identity", "codex-b", "--cached", "--json", check_rc=1)
        cached_deny_env = json.loads(cached_after_deny.stdout)
        require(cached_deny_env["data"]["hosts"][0]["state"] == "offline" and cached_deny_env["data"]["services"] == [], cached_deny_env)
        print("PASS: revoked device gets unauthorized and its private cache is immediately invalidated", flush=True)

        # 7. Daemon loss mid-call through H1 -> tools/call returns isError:true with request.outcome_unknown (retryable:false)!
        loss_client = ConnectClient(binary, env_a, f"{remote_name}/repo-iso", "codex-a")
        try:
            loss_id = loss_client.request("tools/call", {"name": "slow", "arguments": {"tag": "daemon-loss"}})
            wait_trace(
                trace_file,
                lambda lines: True if any(ln.startswith("entered daemon-loss ") for ln in lines) else None,
                "daemon-loss call to enter upstream",
            )
            fixture.daemon.terminate()
            fixture.daemon.wait(timeout=10)
            loss_rep = loss_client.read_message(timeout=15.0)
            require(loss_rep.get("id") == loss_id, loss_rep)
            res = loss_rep.get("result") or {}
            require(res.get("isError") is True, loss_rep)
            sc = res.get("structuredContent") or {}
            errs = sc.get("errors") or []
            require(len(errs) == 1 and errs[0]["code"] == "request.outcome_unknown" and errs[0]["retryable"] is False, loss_rep)
            loss_client.close()
        finally:
            loss_client.terminate()
        print("PASS: daemon loss during in-flight tools/call reports request.outcome_unknown (retryable:false) on H1", flush=True)

        # 8. Unreachable host -> STALE (<24h) first, then EXPIRED (>24h)!
        stale_ls = run_mesh(binary, env_a, "mcp", "ls", "--host", remote_name, "--identity", "codex-a", "--json", check_rc=3)
        stale_env = json.loads(stale_ls.stdout)
        require(stale_env["status"] == "partial", stale_env)
        require(stale_env["data"]["hosts"][0]["state"] == "stale", stale_env)
        require(any(s["name"] == "repo-iso" and s["state"] == "stale" for s in stale_env["data"]["services"]), stale_env)

        for p in cache_dir.glob("*.json"):
            if p.name.startswith("."):
                continue
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
