"""End-to-end M9 proof: the aggregate gateway over two separately hosted
machines (each its own daemon and device key) plus native tools.

Covers names and URIs, lists, routing, multi-round input over stdio and HTTP
upstreams, continuation rejections, fresh frontends, subscription fan-in,
saturation, cancellation, graceful close, revocation and profile edits."""
import base64
import json
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

from mcp_connect import run_mesh, setup_client_device
from mcp_gateway_upstream import LONG_TOOL, HTTPUpstream
from pipe_execution import PipeFixture
from terminal_window import print_diagnostics, require

PROTO = "2026-07-28"
CAPS = {"elicitation": {}}
META = {"io.modelcontextprotocol/protocolVersion": PROTO, "io.modelcontextprotocol/clientCapabilities": CAPS}
ROUTE = "com.github.shaullavo.mesh/route"
UPSTREAM_INFO = "com.github.shaullavo.mesh/upstreamServerInfo"
SUB = "io.modelcontextprotocol/subscriptionId"


def b32(raw):
    return base64.b32encode(raw).decode().rstrip("=").lower()


def exposed_name(host_id, service_id, kind, name):
    """Independent implementation of the deterministic h_ name."""
    host = base64.urlsafe_b64decode(host_id + "=")
    return f"h_{b32(host)}_{b32(bytes.fromhex(service_id))}_{kind}_{b32(name.encode())}"


def exposed_uri(host_id, service_id, uri):
    host = base64.urlsafe_b64decode(host_id + "=")
    return f"mesh-mcp://{b32(host)}/{b32(bytes.fromhex(service_id))}/{uri}"


class Client:
    """One MCP stdio client of mesh mcp (gateway or direct connect). A reader
    thread sorts replies by ID and notifications by subscription."""

    def __init__(self, binary, env, args):
        self.proc = subprocess.Popen([binary, "mcp", *args], env=env, stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.replies = {}
        self.events = {}
        self.cond = threading.Condition()
        self.next_id = 1
        threading.Thread(target=self.read, daemon=True).start()

    def read(self):
        for line in self.proc.stdout:
            msg = json.loads(line)
            with self.cond:
                if "id" in msg and "method" not in msg:
                    self.replies[json.dumps(msg["id"])] = msg
                else:
                    sub = ((msg.get("params") or {}).get("_meta") or {}).get(SUB)
                    self.events.setdefault(json.dumps(sub), []).append(msg)
                self.cond.notify_all()

    def send(self, obj):
        self.proc.stdin.write((json.dumps(obj, separators=(",", ":")) + "\n").encode())
        self.proc.stdin.flush()

    def start(self, method, params=None, req_id=None):
        if req_id is None:
            req_id = self.next_id
            self.next_id += 1
        body = {"_meta": dict(META)}
        body.update(params or {})
        self.send({"jsonrpc": "2.0", "id": req_id, "method": method, "params": body})
        return req_id

    def reply(self, req_id, timeout=30):
        key = json.dumps(req_id)
        with self.cond:
            require(self.cond.wait_for(lambda: key in self.replies, timeout), f"no reply to {req_id}")
            return self.replies.pop(key)

    def call(self, method, params=None, timeout=30):
        return self.reply(self.start(method, params), timeout)

    def tool(self, name, arguments=None, **extra):
        return self.call("tools/call", dict({"name": name, "arguments": arguments or {}}, **extra))

    def wait_events(self, sub, count, timeout=20):
        key = json.dumps(sub)
        with self.cond:
            require(self.cond.wait_for(lambda: len(self.events.get(key, [])) >= count, timeout),
                    f"listen {sub}: wanted {count} events, have {self.events.get(key)}")
            return list(self.events[key])

    def cancel(self, req_id):
        self.send({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": req_id}})

    def close(self):
        self.proc.stdin.close()
        rc = self.proc.wait(timeout=10)
        return rc, self.proc.stderr.read().decode(errors="replace")

    def kill(self):
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(timeout=5)


def wait_for(client, sub, match, what, timeout=20):
    deadline = time.monotonic() + timeout
    with client.cond:
        while True:
            if any(match(e) for e in client.events.get(json.dumps(sub), [])):
                return
            remaining = deadline - time.monotonic()
            require(remaining > 0, f"{what}: {client.events.get(json.dumps(sub))}")
            client.cond.wait(remaining)


def list_all(client, method, field):
    items, cursor = [], None
    while True:
        reply = client.call(method, {"cursor": cursor} if cursor else {})
        require("result" in reply, f"{method}: {reply}")
        items += reply["result"][field]
        cursor = reply["result"].get("nextCursor")
        if not cursor:
            return items, reply


def text_of(reply):
    require("result" in reply, f"expected a result: {reply}")
    return reply["result"]["content"][0]["text"]


def mesh_code(reply):
    return ((reply.get("error") or {}).get("data") or {}).get("meshCode")


def strip_route(entry):
    entry = json.loads(json.dumps(entry))
    meta = entry.get("_meta") or {}
    meta.pop(ROUTE, None)
    if not meta:
        entry.pop("_meta", None)
    entry.pop("name", None)
    return entry


def answer(key, value):
    return {key: {"action": "accept", "content": {"v": value}}}


def publish(binary, env, name, *args):
    out = run_mesh(binary, env, "mcp", "publish", name, "--json", *args, check_rc=0).stdout
    return json.loads(out)["data"]["id"]


class World:
    def __init__(self, binary, root):
        self.binary, self.root = binary, root
        self.http = None
        self.fixtures = []

    def host(self, label):
        (self.root / label).mkdir()
        fixture = PipeFixture(self.binary, self.root / label)
        self.fixtures.append(fixture)
        fixture.start_remote()
        entry = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]
        claim = json.loads((fixture.config / "machine-names" / f"{fixture.remote_id}.json").read_text())
        env = dict(fixture.environment, MESH_CONFIG_DIR=str(fixture.root / "remote-config"), MESH_STATE_DIR=str(fixture.remote))
        return fixture, entry, claim, env

    def close(self):
        if self.http:
            self.http.close()
        for f in self.fixtures:
            f.close()


def stdio_command(label):
    helpers = Path(__file__).resolve().parent
    return ["--", sys.executable, "-B", str(helpers / "mcp_gateway_upstream.py"), label]


def main():
    binary = sys.argv[1]
    root = Path(os.environ["HOME"]).parent / "mcp-gateway"
    root.mkdir()
    world = World(binary, root)
    try:
        run(world, binary, root)
    except BaseException:
        for f in world.fixtures:
            print_diagnostics(f)
        raise
    finally:
        world.close()
    print("PASS: M9 aggregate gateway end-to-end acceptance")


def run(world, binary, root):
    fa, entry_a, claim_a, env_a = world.host("a")
    fb, entry_b, claim_b, env_b = world.host("b")
    world.http = HTTPUpstream("http-a")
    host_a, host_b = fa.remote_id, fb.remote_id
    require(host_a != host_b, "the two hosts must have separate identities")

    repo_a = publish(binary, env_a, "repo", *stdio_command("stdio-a"))
    search = publish(binary, env_a, "search", "--url", world.http.url, "--shared")
    notes = publish(binary, env_a, "notes", "--shared", *stdio_command("notes"))
    repo_b = publish(binary, env_b, "repo", "--shared", *stdio_command("stdio-b"))

    env, cfg, _state, device = setup_client_device(binary, root, "laptop", [entry_a, entry_b],
                                                   {host_a: claim_a, host_b: claim_b})
    env_c, _cfg_c, _state_c, device_c = setup_client_device(binary, root, "phone", [entry_a], {host_a: claim_a})
    for host_env in (env_a, env_b):
        run_mesh(binary, host_env, "device", "approve", "--allow-root", "--", device, check_rc=0)
    run_mesh(binary, env_a, "device", "approve", "--allow-root", "--", device_c, check_rc=0)
    for name in ("agent", "other"):
        run_mesh(binary, env, "mcp", "identity", "create", name, "--json", check_rc=0)
    run_mesh(binary, env_c, "mcp", "identity", "create", "agent", "--json", check_rc=0)

    profiles(binary, env, env_c, host_a, host_b)
    listing(binary, env, host_a, host_b, repo_a, search, repo_b)
    fidelity(binary, env, host_a, host_b, repo_b, claim_b)
    resources(binary, env, host_a, repo_a)
    rounds(binary, env, host_a, repo_a, search)
    rejections(binary, env, env_c, env_a, env_b, host_a, host_b, repo_a, repo_b)
    subscriptions(binary, env, host_a, host_b, repo_a, search, repo_b)
    native_hosted(binary, env, host_b)
    two_callers(binary, env, host_b, repo_b)
    host_revisions(binary, env, env_b, host_b)
    edits(binary, env, env_a, cfg, host_a, host_b, repo_a, notes)
    revocation(binary, env, env_b, host_a, host_b, search, repo_b, device)


def gateway(binary, env, identity="agent"):
    return Client(binary, env, ["--profile", "dev", "--identity", identity])


def profiles(binary, env, env_c, host_a, host_b):
    created = json.loads(run_mesh(binary, env, "mcp", "profile", "add", "dev", "--native", "hosts,services", "--expected-revision", "0", "--json", check_rc=0).stdout)
    require(created["data"]["revision"] == 1 and created["data"]["profiles"][0]["native"] == ["hosts", "services"], created)
    stale = run_mesh(binary, env, "mcp", "profile", "add", "dev", "--native", "all", "--expected-revision", "0", "--json", check_rc=1)
    require(json.loads(stale.stdout)["errors"][0]["code"] == "fleet.revision_conflict", stale.stdout)
    for target, alias in ((f"{host_a}/repo", "ra"), (f"{host_a}/search", "search"), (f"{host_b}/repo", "rb")):
        run_mesh(binary, env, "mcp", "profile", "add", "--alias", alias, "--identity", "agent", "--json", "--", "dev", target, check_rc=0)
    dup = run_mesh(binary, env, "mcp", "profile", "add", "--alias", "again", "--json", "--", "dev", f"{host_b}/repo", check_rc=2)
    require("already selected" in dup.stdout, dup.stdout)
    shown = json.loads(run_mesh(binary, env, "mcp", "profile", "show", "dev", "--identity", "agent", "--json", check_rc=0).stdout)
    entries = shown["data"]["profiles"][0]["services"]
    require([e["alias"] for e in entries] == ["ra", "search", "rb"], entries)
    require(all(e["state"] in ("stopped", "ready") for e in entries), entries)
    mode = (Path(json.loads(run_mesh(binary, env, "mcp", "profile", "show", "--cached", "--json", check_rc=0).stdout)["data"]["path"]).stat().st_mode) & 0o777
    require(mode == 0o600, f"profiles file mode {oct(mode)}")
    run_mesh(binary, env_c, "mcp", "profile", "add", "--alias", "ra", "--json", "--", "dev", f"{host_a}/repo", check_rc=0)
    print("PASS: profiles bind stable IDs with compare-and-swap, unique aliases and passive states", flush=True)


def listing(binary, env, host_a, host_b, repo_a, search, repo_b):
    gw = gateway(binary, env)
    try:
        discover = gw.call("server/discover")
        caps = discover["result"]["capabilities"]
        require(caps["tools"]["listChanged"] and "prompts" in caps and caps["resources"].get("subscribe") and "completions" in caps, caps)
        require("extensions" not in caps, f"tasks or extensions leaked into gateway capabilities: {caps}")
        tools, reply = list_all(gw, "tools/list", "tools")
        require(reply["result"]["cacheScope"] == "private", reply)
        names = [t["name"] for t in tools]
        require(names[0].startswith("mesh_") and "mesh_hosts_list" in names and "mesh_exec" not in names, names[:10])
        hosted = [t for t in tools if not t["name"].startswith("mesh_")]
        aliases = [t["_meta"][ROUTE]["alias"] for t in hosted]
        require(aliases == sorted(aliases, key=["ra", "search", "rb"].index), f"services out of profile order: {aliases}")
        for host, svc in ((host_a, repo_a), (host_b, repo_b)):
            for tool in ("echo", "a_b", "a", "b"):
                require(exposed_name(host, svc, "t", tool) in names, f"missing {tool} on {host}")
        require(exposed_name(host_a, repo_a, "t", "echo") != exposed_name(host_b, repo_b, "t", "echo"), "collision across hosts")
        overflow = [t for t in hosted if t["name"].startswith("hx_")]
        require(len(overflow) == 3 and len({t["name"] for t in overflow}) == 3, f"overflow names {overflow}")
        require(all(len(n) <= 128 for n in names), "a name exceeds 128 characters")
        echo = next(t for t in hosted if t["name"] == exposed_name(host_a, repo_a, "t", "echo"))
        require(echo["x-vendor"]["big"] == 12345678901234567890 and echo["_meta"]["vendor.example/tag"] == "echo", echo)
        require(text_of(gw.tool(exposed_name(host_b, repo_b, "t", "a_b"), {"text": "x"})) == "stdio-b:a_b:x", "a_b routed wrong")
        long_name = next(t["name"] for t in overflow if t["_meta"][ROUTE]["alias"] == "rb")
        require(text_of(gw.tool(long_name, {"text": "y"})) == f"stdio-b:{LONG_TOOL}:y", "hx_ name routed wrong")
        again = [t["name"] for t in list_all(gw, "tools/list", "tools")[0]]
        require(again == names, "tools/list order changed between calls")
        require(text_of(gw.tool("mesh_skill", {})) != "", "native route failed")
        prompts, _ = list_all(gw, "prompts/list", "prompts")
        require(exposed_name(host_a, search, "p", "confirm") in [p["name"] for p in prompts], prompts)
        rc, _ = gw.close()
        require(rc == 0, f"gateway exit {rc}")
    finally:
        gw.kill()
    print("PASS: merged lists are stable, collision-free, overflow-safe and keep unknown fields", flush=True)


def fidelity(binary, env, host_a, host_b, repo_b, claim_b):
    """A fresh gateway calls a rich tool it never listed; the result matches
    the direct connection's apart from documented identifier rewrites."""
    gw = gateway(binary, env)
    direct = Client(binary, env, ["connect", "--identity", "agent", "--", f"{host_b}/repo"])
    try:
        via_gw = gw.tool(exposed_name(host_b, repo_b, "t", "rich"))
        via_direct = direct.tool("rich")
        g, d = via_gw["result"], via_direct["result"]
        require(g["content"][1]["uri"] == exposed_uri(host_b, repo_b, "repo://a%2Fb/x?y=1#z"), g["content"][1])
        require(g["content"][2]["resource"]["uri"] == exposed_uri(host_b, repo_b, "file:///docs/readme.md"), g["content"][2])
        g["content"][1]["uri"] = d["content"][1]["uri"]
        g["content"][2]["resource"]["uri"] = d["content"][2]["resource"]["uri"]
        require(g == d, f"gateway and direct results differ:\n{g}\n{d}")
        require(g["_meta"][UPSTREAM_INFO]["name"] == "gw-stdio-b", g["_meta"])
        direct_tools = {t["name"]: t for t in direct.call("tools/list")["result"]["tools"]}
        gw_tools = {t["name"]: t for t in list_all(gw, "tools/list", "tools")[0]}
        for name, tool in direct_tools.items():
            if name == LONG_TOOL:
                continue
            mine = gw_tools[exposed_name(host_b, repo_b, "t", name)]
            require(strip_route(mine) == strip_route(tool), f"{name}: listing differs\n{mine}\n{tool}")
    finally:
        gw.kill()
        direct.kill()
    print("PASS: rich results without a prior list match direct results semantically", flush=True)


def resources(binary, env, host_a, repo_a):
    gw = gateway(binary, env)
    try:
        odd = exposed_uri(host_a, repo_a, "repo://a%2Fb/x?y=1#z")
        read = gw.call("resources/read", {"uri": odd})
        require("result" in read, f"read {odd}: {read}")
        require(read["result"]["contents"][0]["uri"] == odd and read["result"]["contents"][0]["text"] == "content of repo://a%2Fb/x?y=1#z", read)
        unlisted = exposed_uri(host_a, repo_a, "file:///never/listed%20here")
        require(gw.call("resources/read", {"uri": unlisted})["result"]["contents"][0]["uri"] == unlisted, "unlisted read")
        templates, _ = list_all(gw, "resources/templates/list", "resourceTemplates")
        template = next(t["uriTemplate"] for t in templates if t["_meta"][ROUTE]["alias"] == "ra")
        expanded = template.replace("{owner}", "me").replace("{name}", "proj")
        require(gw.call("resources/read", {"uri": expanded})["result"]["contents"][0]["text"] == "content of repo://me/proj", "template read")
        listed, _ = list_all(gw, "resources/list", "resources")
        require(exposed_uri(host_a, repo_a, "file:///docs/readme.md") in [r["uri"] for r in listed], listed)
        greet = gw.call("prompts/get", {"name": exposed_name(host_a, repo_a, "p", "greet"), "arguments": {"who": "ann"}})
        require(greet["result"]["messages"][0]["content"]["text"] == "hello ann", greet)
        require(greet["result"]["messages"][1]["content"]["resource"]["uri"].startswith("mesh-mcp://"), greet)
        done = gw.call("completion/complete", {"ref": {"type": "ref/resource", "uri": template}, "argument": {"name": "owner", "value": "m"}})
        require(done["result"]["completion"]["values"] == ["main", "dev"], done)
        bad = gw.call("resources/read", {"uri": "mesh-mcp://nothere/x/file:///y"})
        require(bad["error"]["code"] == -32602, bad)
    finally:
        gw.kill()
    print("PASS: resource URIs, reserved encodings, templates, prompts and completions route exactly", flush=True)


def multi_round(gw, name):
    first = gw.tool(name)
    require(first["result"]["resultType"] == "input_required", first)
    second = gw.tool(name, requestState=first["result"]["requestState"], inputResponses=answer("first", "ada"))
    require(second["result"]["resultType"] == "input_required", second)
    third = gw.tool(name, requestState=second["result"]["requestState"], inputResponses=answer("second", "lovelace"))
    require(text_of(third) == "hello ada lovelace", third)


def rounds(binary, env, host_a, repo_a, search):
    gw = gateway(binary, env)
    try:
        for svc in (repo_a, search):
            multi_round(gw, exposed_name(host_a, svc, "t", "ask2"))
            secret = exposed_uri(host_a, svc, "file:///secret")
            first = gw.call("resources/read", {"uri": secret})
            require(first["result"]["resultType"] == "input_required" and "ttlMs" not in first["result"], first)
            done = gw.call("resources/read", {"uri": secret, "requestState": first["result"]["requestState"], "inputResponses": answer("pin", "42")})
            require(done["result"]["contents"][0]["text"] == "secret for 42" and done["result"]["ttlMs"] == 0, done)
            confirm = exposed_name(host_a, svc, "p", "confirm")
            first = gw.call("prompts/get", {"name": confirm})
            require(first["result"]["resultType"] == "input_required", first)
            done = gw.call("prompts/get", {"name": confirm, "requestState": first["result"]["requestState"], "inputResponses": answer("ok", "yes")})
            require(done["result"]["messages"][0]["content"]["text"] == "confirmed", done)
    finally:
        gw.kill()
    print("PASS: multiple input rounds for tools, reads and prompts over stdio and HTTP upstreams", flush=True)


def first_round(gw, name, args=None):
    reply = gw.tool(name, args)
    require(reply["result"]["resultType"] == "input_required", reply)
    return reply["result"]["requestState"]


def require_invalid(reply, what):
    require(reply.get("error", {}).get("code") == -32602 and mesh_code(reply) == "mcp.state_invalid", f"{what} was accepted: {reply}")


def rejections(binary, env, env_c, env_a, env_b, host_a, host_b, repo_a, repo_b):
    ask_a = exposed_name(host_a, repo_a, "t", "ask2")
    ask_b = exposed_name(host_b, repo_b, "t", "ask2")
    gw = gateway(binary, env)
    other = gateway(binary, env, "other")
    phone = gateway(binary, env_c)
    try:
        state = first_round(gw, ask_a)
        ok = gw.tool(ask_a, requestState=state, inputResponses=answer("first", "x"))
        require(ok["result"]["resultType"] == "input_required", ok)
        require_invalid(gw.tool(ask_a, requestState=state, inputResponses=answer("first", "x")), "a replayed reference")
        require_invalid(other.tool(ask_a, requestState=first_round(gw, ask_a), inputResponses=answer("first", "x")), "another caller's reference")
        require_invalid(phone.tool(ask_a, requestState=first_round(gw, ask_a), inputResponses=answer("first", "x")), "another device's reference")
        require_invalid(gw.tool(ask_a, {"changed": True}, requestState=first_round(gw, ask_a), inputResponses=answer("first", "x")), "a reference for different arguments")
        echo_state = first_round(gw, ask_a)
        require_invalid(gw.call("prompts/get", {"name": exposed_name(host_a, repo_a, "p", "confirm"), "requestState": echo_state,
                                                "inputResponses": answer("ok", "y")}), "a reference for another operation")
        stale = first_round(gw, ask_a)
        publish(binary, env_a, "repo", "--env", "REV=2", *stdio_command("stdio-a"))
        require_invalid(gw.tool(ask_a, requestState=stale, inputResponses=answer("first", "x")), "a reference from an older revision")
        before_restart = first_round(gw, ask_b)
        run_mesh(binary, env_b, "mcp", "restart", "repo", "--json", check_rc=0)
        require_invalid(gw.tool(ask_b, requestState=before_restart, inputResponses=answer("first", "x")), "a reference from a replaced instance")
        carried = first_round(gw, ask_a)
    finally:
        for c in (gw, other, phone):
            c.kill()
    fresh = gateway(binary, env)
    try:
        cont = fresh.tool(ask_a, requestState=carried, inputResponses=answer("first", "z"))
        require(cont["result"]["resultType"] == "input_required", f"a fresh frontend could not continue: {cont}")
    finally:
        fresh.kill()
    print("PASS: continuations reject replay, caller, device, operation, revision and instance changes; a fresh frontend continues", flush=True)


def subscriptions(binary, env, host_a, host_b, repo_a, search, repo_b):
    gw = gateway(binary, env)
    try:
        readme_a = exposed_uri(host_a, repo_a, "file:///docs/readme.md")
        refused = exposed_uri(host_a, repo_a, "file:///refused/x")
        readme_b = exposed_uri(host_b, repo_b, "file:///docs/readme.md")
        readme_http = exposed_uri(host_a, search, "file:///docs/readme.md")
        gw.start("subscriptions/listen", {"notifications": {"toolsListChanged": True, "resourceSubscriptions": [readme_a, refused, readme_b]}}, "active")
        ack = gw.wait_events("active", 1)[0]
        acked = ack["params"]["notifications"]
        require(ack["method"] == "notifications/subscriptions/acknowledged" and acked.get("toolsListChanged") is True, ack)
        require(sorted(acked["resourceSubscriptions"]) == sorted([readme_a, readme_b]), f"acknowledged subset {acked}")
        gw.start("subscriptions/listen", {"notifications": {"resourceSubscriptions": [readme_http]}}, "quiet")
        require(gw.wait_events("quiet", 1)[0]["params"]["notifications"]["resourceSubscriptions"] == [readme_http], "quiet ack")
        gw.tool(exposed_name(host_a, repo_a, "t", "touch"), {"uri": "file:///docs/readme.md"})
        # The upstream's own list change and the host's listing revision can
        # each report the change, so wait for each event rather than a count.
        wait_for(gw, "active", lambda e: e["method"] == "notifications/resources/updated" and e["params"]["uri"] == readme_a and e["params"]["x-rev"] == 2,
                 "resource update with its own fields")
        wait_for(gw, "active", lambda e: e["method"] == "notifications/tools/list_changed", "tools list change")
        time.sleep(0.5)
        require(len(gw.events.get('"quiet"', [])) == 1, "the quiet listen received another stream's events")
        gw.tool(exposed_name(host_a, search, "t", "touch"), {"uri": "file:///docs/readme.md"})
        require(gw.wait_events("quiet", 2)[1]["params"]["uri"] == readme_http, "HTTP upstream update")
        # Saturation: the frontend carries at most four listens at once.
        extra = [gw.start("subscriptions/listen", {"notifications": {"toolsListChanged": True}}, f"extra-{i}") for i in range(2)]
        for sub in extra:
            gw.wait_events(sub, 1)
        refused_listen = gw.call("subscriptions/listen", {"notifications": {"toolsListChanged": True}})
        require(mesh_code(refused_listen) == "resource.limit", f"a fifth listen was admitted: {refused_listen}")
        gw.cancel("extra-0")
        time.sleep(0.3)
        gw.tool(exposed_name(host_b, repo_b, "t", "touch"), {"uri": "file:///docs/readme.md"})
        wait_for(gw, "active", lambda e: e["params"].get("uri") == readme_b, "independent cancellation stopped another listen")
        require('"extra-0"' not in gw.replies, "a cancelled listen got a reply")
        gw.tool(exposed_name(host_b, repo_b, "t", "end_listens"))
        closed = gw.reply("active")
        require(closed["result"]["resultType"] == "complete" and closed["result"]["_meta"][SUB] == "active", f"graceful close {closed}")
        watch = gw.start("subscriptions/listen", {"notifications": {"toolsListChanged": True}}, "watch")
        gw.wait_events(watch, 1)
        run_alias = run_mesh(binary, env, "mcp", "profile", "alias", "dev", "search", "lookup", "--json", check_rc=0)
        require(json.loads(run_alias.stdout)["data"]["changed"], run_alias.stdout)
        wait_for(gw, "watch", lambda e: e["method"] == "notifications/tools/list_changed", "profile edit not surfaced")
        run_mesh(binary, env, "mcp", "profile", "alias", "dev", "lookup", "search", "--json", check_rc=0)
        for sub in ("quiet", "extra-1", "watch"):
            gw.cancel(sub)
        rc, _ = gw.close()
        require(rc == 0, f"gateway exit {rc}")
    finally:
        gw.kill()
    print("PASS: listens acknowledge backed subsets, keep quiet and active streams apart, saturate, cancel independently and close gracefully", flush=True)


def native_hosted(binary, env, host_b):
    gw = gateway(binary, env)
    try:
        listed = gw.tool("mesh_services_list", {"host": host_b})
        hosted = listed["result"]["structuredContent"]["data"]["hostedMcp"]
        require(any(s["name"] == "repo" for s in hosted), listed)
        inspect = gw.tool("mesh_service_inspect", {"host": host_b, "name": "repo"})
        require(inspect["result"]["structuredContent"]["data"]["hostedMcp"]["name"] == "repo", inspect)
        lease = gw.tool("mesh_service_lease", {"host": host_b, "service": "repo", "lease_id": "gw-proof", "ttl_seconds": 600})
        data = lease["result"]["structuredContent"]["data"]
        require(data["leaseId"] == "gw-proof" and data["instance"]["leases"] == 1, lease)
        # A repeat after a lost reply names the same lease.
        again = gw.tool("mesh_service_lease", {"host": host_b, "service": "repo", "lease_id": "gw-proof", "ttl_seconds": 600})
        require(again["result"]["structuredContent"]["data"]["instance"]["leases"] == 1, again)
        release = gw.tool("mesh_service_lease", {"host": host_b, "service": "repo", "lease_id": "gw-proof", "ttl_seconds": 0})
        require(release["result"]["structuredContent"]["data"]["released"], release)
    finally:
        gw.kill()
    print("PASS: mesh_services_list and mesh_service_inspect show hosted servers; mesh_service_lease holds and releases", flush=True)


def host_revisions(binary, env, env_b, host_b):
    """A listen whose upstream cannot report list changes still learns of one
    from the owning host's listing revision on the mcp topic of state.watch."""
    static = publish(binary, env_b, "static", "--shared", "--env", "GW_STATIC=1", *stdio_command("static"))
    run_mesh(binary, env, "mcp", "profile", "add", "--alias", "static", "--identity", "agent", "--json", "--", "dev", f"{host_b}/static", check_rc=0)
    gw = gateway(binary, env)
    direct = Client(binary, env, ["connect", "--identity", "agent", "--", f"{host_b}/static"])
    try:
        before = hosted_names(gw, "static")
        gw.start("subscriptions/listen", {"notifications": {"toolsListChanged": True}}, "revisions")
        gw.wait_events("revisions", 1)
        direct.tool("grow")
        require("result" in direct.call("tools/list"), "direct list")
        wait_for(gw, "revisions", lambda e: e["method"] == "notifications/tools/list_changed", "a host listing revision did not reach the listen")
        after = hosted_names(gw, "static")
        require(exposed_name(host_b, static, "t", "grown_0") in after and len(after) == len(before) + 1, f"relist after the revision: {after}")
        gw.cancel("revisions")
    finally:
        gw.kill()
        direct.kill()
    run_mesh(binary, env, "mcp", "profile", "rm", "dev", "static", "--json", check_rc=0)
    print("PASS: a host-published listing revision reaches a listen whose upstream cannot report changes", flush=True)


def hosted_names(gw, alias):
    return [t["name"] for t in list_all(gw, "tools/list", "tools")[0] if (t.get("_meta") or {}).get(ROUTE, {}).get("alias") == alias]


def edits(binary, env, env_a, cfg, host_a, host_b, repo_a, notes):
    old_echo = exposed_name(host_a, repo_a, "t", "echo")
    gw = gateway(binary, env)
    try:
        rb_before = hosted_names(gw, "rb")
        require(len(rb_before) == 14, rb_before)
    finally:
        gw.kill()
    run_mesh(binary, env, "mcp", "profile", "rm", "dev", "ra", "--json", check_rc=0)
    run_mesh(binary, env, "mcp", "profile", "add", "--alias", "ra", "--json", "--", "dev", f"{host_a}/notes", check_rc=0)
    gw = gateway(binary, env)
    try:
        listed = [t["name"] for t in list_all(gw, "tools/list", "tools")[0]]
        require(old_echo not in listed and exposed_name(host_a, notes, "t", "echo") in listed, "alias reuse kept the old service's names")
        require(gw.tool(old_echo)["error"]["code"] == -32602, "a removed service's name still routes")
    finally:
        gw.kill()
    run_mesh(binary, env_a, "mcp", "rm", "notes", "--json", check_rc=0)
    # A new service named after the retired ID never receives the old route.
    publish(binary, env_a, notes, "--shared", *stdio_command("impostor"))
    gw = gateway(binary, env)
    try:
        stray = gw.tool(exposed_name(host_a, notes, "t", "echo"), {"text": "x"})
        require(stray["result"]["isError"] and stray["result"]["structuredContent"]["errors"][0]["code"] == "service.not_found", f"a retired ID reached the service named after it: {stray}")
    finally:
        gw.kill()
    run_mesh(binary, env_a, "mcp", "rm", notes, "--json", check_rc=0)
    replaced = publish(binary, env_a, "notes", "--shared", *stdio_command("notes2"))
    require(replaced != notes, "a re-published service kept its ID")
    shown = json.loads(run_mesh(binary, env, "mcp", "profile", "show", "dev", "--json", check_rc=0).stdout)
    require(next(e for e in shown["data"]["profiles"][0]["services"] if e["alias"] == "ra")["state"] == "not_found", shown)
    proposal = run_mesh(binary, env, "mcp", "profile", "repair", "dev", "ra", "--json", check_rc=2)
    require(json.loads(proposal.stdout)["data"]["newServiceId"] == replaced, proposal.stdout)
    run_mesh(binary, env, "mcp", "profile", "repair", "dev", "ra", "--confirm", "--json", check_rc=0)
    # Rename host B on this client: routes and names stay.
    hosts_path = cfg / "hosts.json"
    hosts = json.loads(hosts_path.read_text())
    for h in hosts["hosts"]:
        if h["id"] == host_b:
            h["tailscaleName"] = "renamed.fixture.test"
    hosts_path.write_text(json.dumps(hosts))
    claim = cfg / "machine-names" / f"{host_b}.json"
    claim.unlink()
    gw = gateway(binary, env)
    try:
        listed = [t["name"] for t in list_all(gw, "tools/list", "tools")[0]]
        require(exposed_name(host_a, replaced, "t", "echo") in listed and exposed_name(host_a, notes, "t", "echo") not in listed, "repair")
        require(hosted_names(gw, "rb") == rb_before, "host rename changed exposed names")
        echo_b = next(n for n in rb_before if n.endswith("_t_" + b32(b"echo")))
        require(text_of(gw.tool(echo_b, {"text": "r"})) == "stdio-b:echo:r", "renamed host route")
    finally:
        gw.kill()
    print("PASS: alias reuse, service replacement with confirmed repair, and host rename keep names exact", flush=True)


def revocation(binary, env, env_b, host_a, host_b, search, repo_b, device):
    gw = gateway(binary, env)
    try:
        gw.start("subscriptions/listen", {"notifications": {"resourceSubscriptions": [exposed_uri(host_b, repo_b, "file:///docs/readme.md")]}}, "held")
        gw.wait_events("held", 1)
        # A write in flight when the grant is retired: its outcome is unknown
        # to the caller, and the gateway never replays it.
        gw.start("tools/call", {"name": exposed_name(host_b, repo_b, "t", "write"), "arguments": {"seconds": 3}}, "write")
        time.sleep(0.5)
        run_mesh(binary, env_b, "device", "revoke", "--", device, check_rc=0)
        ended = gw.reply("held", timeout=30)
        require(mesh_code(ended) == "access.denied", f"revocation did not retire the listen: {ended}")
        cut = gw.reply("write", timeout=30)
        require(cut["result"]["isError"] and cut["result"]["structuredContent"]["errors"][0]["code"] in ("access.denied", "request.outcome_unknown", "host.unreachable"), f"a live call outlived revocation: {cut}")
        denied = gw.tool(exposed_name(host_b, repo_b, "t", "echo"))
        # A Mesh failure inside tools/call is a tool execution error (section 2.4).
        require(denied["result"]["isError"] and denied["result"]["structuredContent"]["errors"][0]["code"] in ("access.denied", "host.unreachable"), denied)
        survivor = gw.tool(exposed_name(host_a, search, "t", "echo"), {"text": "still"})
        require(text_of(survivor) == "http-a:echo:still", survivor)
    finally:
        gw.kill()
    run_mesh(binary, env_b, "device", "approve", "--allow-root", "--", device, check_rc=0)
    fresh = gateway(binary, env)
    try:
        count = text_of(fresh.tool(exposed_name(host_b, repo_b, "t", "count")))
        require(count == "writes=1", f"the interrupted write ran again: {count}")
    finally:
        fresh.kill()
    print("PASS: authorization retirement ends listens and live calls on that host only, and an ambiguous write is never replayed", flush=True)


def two_callers(binary, env, host_b, repo_b):
    """Progress reaches only its own caller, and cancelling one caller's call
    leaves another caller's call on the same shared instance running."""
    one, two = gateway(binary, env), gateway(binary, env, "other")
    name = exposed_name(host_b, repo_b, "t", "progress")
    try:
        def meta(token):
            return dict(META, progressToken=token)

        one.start("tools/call", {"name": name, "arguments": {"steps": 8}, "_meta": meta("p1")}, "c1")
        two.start("tools/call", {"name": name, "arguments": {"steps": 8}, "_meta": meta("p2")}, "c2")
        wait_for(one, None, lambda e: e["params"]["progressToken"] == "p1", f"progress for the first caller: {one.replies}")
        one.cancel("c1")
        done = two.reply("c2", timeout=30)
        require(text_of(done) == "progressed 8", done)
        progress = two.events.get("null", [])
        require(progress and all(e["params"]["progressToken"] == "p2" for e in progress), f"progress crossed callers: {progress}")
        time.sleep(1)
        require('"c1"' not in one.replies, "a cancelled call got a reply")
        require(all(e["params"]["progressToken"] == "p1" for e in one.events.get("null", [])), "progress crossed callers")
    finally:
        one.kill()
        two.kill()
    print("PASS: progress and cancellation stay with their own caller on a shared instance", flush=True)


if __name__ == "__main__":
    main()
