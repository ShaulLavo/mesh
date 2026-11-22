"""Stateless MCP 2026-07-28 upstream for the M9 gateway proof.

One protocol core serves both upstream transports: stdio (run this file) and
loopback Streamable HTTP (HTTPUpstream). It offers tools, prompts, resources,
templates, completions, multi-round input on tools, reads and prompts, and
subscriptions with partial acknowledgment.
"""
import http.server
import json
import os
import socketserver
import sys
import threading

PROTO = "2026-07-28"
LONG_TOOL = "very_long_tool_name_" + "x" * 60
REFUSED_PREFIX = "file:///refused"


class Core:
    def __init__(self, label):
        self.label = label
        self.pid = os.getpid()
        self.lock = threading.Lock()
        self.calls = 0
        # listen id (JSON text) -> (filter, send function)
        self.listens = {}

    def server_info(self):
        return {"io.modelcontextprotocol/serverInfo": {"name": "gw-" + self.label, "version": "1.0"}}

    def complete(self, req_id, result, cacheable=False):
        result = dict(result, resultType="complete")
        if cacheable:
            result.update(ttlMs=60000, cacheScope="private")
        return {"jsonrpc": "2.0", "id": req_id, "result": result}

    def ask(self, req_id, state, key, message):
        return {"jsonrpc": "2.0", "id": req_id, "result": {
            "resultType": "input_required", "requestState": state,
            "inputRequests": {key: {"method": "elicitation/create",
                                    "params": {"message": message, "requestedSchema": {"type": "object", "properties": {"v": {"type": "string"}}}}}},
        }}

    def error(self, req_id, code, message):
        return {"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}}

    def tools(self):
        names = ["echo", "whoami", "a_b", "a", "b", LONG_TOOL, "ask2", "rich", "touch", "end_listens", "slow"]
        return [{"name": n, "title": n.upper(), "inputSchema": {"type": "object"},
                 "annotations": {"readOnlyHint": n in ("echo", "whoami")},
                 "x-vendor": {"label": self.label, "big": 12345678901234567890},
                 "_meta": {"vendor.example/tag": n}} for n in names]

    def handle(self, req, send):
        """Returns the terminal reply, or None for a listen that stays open."""
        method, req_id = req.get("method"), req.get("id")
        params = req.get("params") or {}
        if method == "server/discover":
            return self.complete(req_id, {
                "supportedVersions": [PROTO],
                "capabilities": {"tools": {"listChanged": True}, "prompts": {}, "resources": {"subscribe": True}, "completions": {}},
                "_meta": self.server_info(),
            }, True)
        if method == "tools/list":
            return self.complete(req_id, {"tools": self.tools()}, True)
        if method == "prompts/list":
            return self.complete(req_id, {"prompts": [{"name": "confirm"}, {"name": "greet", "arguments": [{"name": "who"}]}]}, True)
        if method == "resources/list":
            return self.complete(req_id, {"resources": [
                {"name": "readme", "uri": "file:///docs/readme.md", "x-size": 42},
                {"name": "odd", "uri": "repo://a%2Fb/x?y=1#z"},
            ]}, True)
        if method == "resources/templates/list":
            return self.complete(req_id, {"resourceTemplates": [{"name": "repo", "uriTemplate": "repo://{owner}/{name}"}]}, True)
        if method == "resources/read":
            return self.read(req_id, params)
        if method == "prompts/get":
            return self.prompt(req_id, params)
        if method == "completion/complete":
            return self.complete(req_id, {"completion": {"values": ["main", "dev"], "hasMore": False}})
        if method == "tools/call":
            return self.call(req_id, params)
        if method == "subscriptions/listen":
            return self.listen(req_id, params, send)
        return self.error(req_id, -32601, "method not found")

    def read(self, req_id, params):
        uri = params.get("uri", "")
        if uri == "file:///secret":
            if params.get("requestState") != "read-r1":
                return self.ask(req_id, "read-r1", "pin", "PIN?")
            pin = params["inputResponses"]["pin"]["content"]["v"]
            # A continuation result is never cacheable.
            return self.complete(req_id, {"contents": [{"uri": uri, "text": "secret for " + pin}], "ttlMs": 0, "cacheScope": "private"})
        return self.complete(req_id, {"contents": [{"uri": uri, "text": "content of " + uri, "x-extra": True}]}, True)

    def prompt(self, req_id, params):
        name = params.get("name")
        if name == "confirm" and params.get("requestState") != "prompt-r1":
            return self.ask(req_id, "prompt-r1", "ok", "Confirm?")
        text = "confirmed" if name == "confirm" else "hello " + str((params.get("arguments") or {}).get("who", ""))
        return self.complete(req_id, {"messages": [
            {"role": "user", "content": {"type": "text", "text": text}},
            {"role": "user", "content": {"type": "resource", "resource": {"uri": "file:///docs/readme.md", "text": "embedded"}}},
        ]})

    def call(self, req_id, params):
        name, args = params.get("name"), params.get("arguments") or {}
        with self.lock:
            self.calls += 1
            calls = self.calls
        if name in ("echo", "a_b", "a", "b", LONG_TOOL):
            return self.complete(req_id, {"content": [{"type": "text", "text": f"{self.label}:{name}:{args.get('text', '')}"}]})
        if name == "whoami":
            return self.complete(req_id, {"content": [{"type": "text", "text": f"{self.label} pid={self.pid} calls={calls}"}]})
        if name == "ask2":
            return self.ask2(req_id, params)
        if name == "rich":
            return self.complete(req_id, {
                "content": [
                    {"type": "text", "text": "see file:///docs/readme.md"},
                    {"type": "resource_link", "uri": "repo://a%2Fb/x?y=1#z", "name": "odd", "x-link": 1},
                    {"type": "resource", "resource": {"uri": "file:///docs/readme.md", "text": "inline", "mimeType": "text/plain"}},
                ],
                "structuredContent": {"uri": "file:///not-rewritten", "n": 1.50},
                "x-result-vendor": {"nested": [1, {"k": "v"}]},
                "_meta": dict(self.server_info(), **{"vendor.example/trace": "t-1"}),
            })
        if name == "touch":
            self.notify_all(args.get("uri"))
            return self.complete(req_id, {"content": [{"type": "text", "text": "touched"}]})
        if name == "end_listens":
            self.end_all()
            return self.complete(req_id, {"content": [{"type": "text", "text": "ended"}]})
        if name == "slow":
            threading.Event().wait(30)
            return self.complete(req_id, {"content": [{"type": "text", "text": "slow"}]})
        return self.error(req_id, -32602, "unknown tool")

    def ask2(self, req_id, params):
        state = params.get("requestState")
        responses = params.get("inputResponses") or {}
        if state is None:
            return self.ask(req_id, "ask2-r1", "first", "First?")
        if state == "ask2-r1":
            return self.ask(req_id, "ask2-r2:" + responses["first"]["content"]["v"], "second", "Second?")
        if state.startswith("ask2-r2:"):
            first = state.split(":", 1)[1]
            return self.complete(req_id, {"content": [{"type": "text", "text": f"hello {first} {responses['second']['content']['v']}"}]})
        return self.complete(req_id, {"content": [{"type": "text", "text": "bad state"}], "isError": True})

    def listen(self, req_id, params, send):
        wanted = params.get("notifications") or {}
        acked = {}
        if wanted.get("toolsListChanged"):
            acked["toolsListChanged"] = True
        uris = [u for u in wanted.get("resourceSubscriptions") or [] if not u.startswith(REFUSED_PREFIX)]
        if "resourceSubscriptions" in wanted:
            acked["resourceSubscriptions"] = uris
        key = json.dumps(req_id, separators=(",", ":"))
        with self.lock:
            self.listens[key] = (req_id, acked, send)
        send({"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged",
              "params": {"notifications": acked, "_meta": {"io.modelcontextprotocol/subscriptionId": req_id}}}, False)
        return None

    def notify_all(self, uri):
        with self.lock:
            listens = list(self.listens.values())
        for req_id, acked, send in listens:
            sub = {"io.modelcontextprotocol/subscriptionId": req_id}
            if acked.get("toolsListChanged"):
                send({"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": {"_meta": sub}}, False)
            if uri and uri in (acked.get("resourceSubscriptions") or []):
                send({"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": {"uri": uri, "x-rev": 2, "_meta": sub}}, False)

    def end_all(self):
        with self.lock:
            listens = list(self.listens.items())
            self.listens.clear()
        for _, (req_id, _, send) in listens:
            send({"jsonrpc": "2.0", "id": req_id, "result": {"resultType": "complete",
                  "_meta": {"io.modelcontextprotocol/subscriptionId": req_id}}}, True)

    def cancel(self, req_id):
        key = json.dumps(req_id, separators=(",", ":"))
        with self.lock:
            self.listens.pop(key, None)


def serve_stdio(label):
    core = Core(label)
    out_lock = threading.Lock()

    def send(message, _terminal):
        with out_lock:
            sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    def handle(req):
        if req.get("id") is None:
            if req.get("method") == "notifications/cancelled":
                core.cancel((req.get("params") or {}).get("requestId"))
            return
        reply = core.handle(req, send)
        if reply is not None:
            send(reply, True)

    for line in sys.stdin:
        line = line.strip()
        if line:
            threading.Thread(target=handle, args=(json.loads(line),), daemon=True).start()


class HTTPUpstream:
    """The same core behind loopback Streamable HTTP, with SSE for listens."""

    def __init__(self, label):
        self.core = Core(label)
        core = self.core

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
                if body.get("id") is None:
                    self.send_response(202)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if body.get("method") == "subscriptions/listen":
                    self.stream(body)
                    return
                reply = core.handle(body, None)
                raw = json.dumps(reply, separators=(",", ":")).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def stream(self, body):
                done = threading.Event()
                lock = threading.Lock()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()

                def send(message, terminal):
                    with lock:
                        if done.is_set():
                            return
                        try:
                            self.wfile.write(b"data: " + json.dumps(message, separators=(",", ":")).encode() + b"\n\n")
                            self.wfile.flush()
                        except OSError:
                            done.set()
                        if terminal:
                            done.set()

                core.handle(body, send)
                while not done.wait(0.2):
                    try:
                        # A closed client stream is the cancellation.
                        self.connection.settimeout(0.01)
                        if self.connection.recv(1, 2) == b"":
                            break
                    except (TimeoutError, BlockingIOError, OSError):
                        continue
                core.cancel(body.get("id"))
                self.close_connection = True

        class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
            daemon_threads = True

        self.httpd = Server(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}/mcp"
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


if __name__ == "__main__":
    serve_stdio(sys.argv[1] if len(sys.argv) > 1 else "stdio")
