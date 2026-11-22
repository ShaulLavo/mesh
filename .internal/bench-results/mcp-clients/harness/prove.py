"""Prove real MCP clients against `mesh mcp --identity NAME` over stdio.

Two modes, from the repository root:

    python3 -B .internal/bench-results/mcp-clients/harness/prove.py run MESH_BINARY ROOT OUT
    python3 -B .internal/bench-results/mcp-clients/harness/prove.py check OUT [--record]

`run` starts the `mcp_native` integration fixture (a client state directory plus
a second daemon with its own state, reached over its authenticated WebSocket),
creates one application identity per client, and drives each installed client
non-interactively with the owner's existing logins. ROOT is an empty, short
scratch directory (Unix socket paths must stay short); raw client output, model
replies and server stderr stay there. OUT receives only redacted protocol traces
and summary.json, and then `run` performs the same validation as `check`.

`check` validates the published traces and summary.json alone, so a reviewer can
re-verify the stored evidence without model accounts. It fails unless:

- every modern run starts each server process with a successful
  `server/discover` for 2026-07-28 and a `tools/list` that includes mesh_exec,
  sends no `initialize`, and gets exactly one reply for every request;
- run1 lists hosts, opens a context and runs one command that exits 0;
- run2 (a new client and a new `mesh mcp`) starts no job, finds run1's exact job
  handle by run1's context and request key, and reads run1's exact stdout;
- each legacy Codex run sends only `initialize` and gets a -32601 refusal for
  every one of them;
- the Codex run without an approval mode never sends mesh_exec on the wire and
  Codex reported its approval refusal;
- the Claude project run connected the server from `.mcp.json`.
"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[3]

PROTOCOL = "2026-07-28"
CLAUDE_MODEL = "claude-sonnet-5-5"
CODEX_MODEL = "gpt-6.1-terra"
COMMAND = "printf 'hello from %s\\n' \"$(uname -s)\"; pwd"
APPROVAL_ERROR = "MCP tool call requires approval, but approval policy is never"
EXEC_KEY = {"claude-code": "claude-proof-exec-1", "codex": "codex-proof-exec-1"}
# A nested run from inside another Claude Code session must not inherit its session.
CLAUDE_SESSION_VARS = ("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID",
                       "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_SOCKET",
                       "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_EXECPATH", "CLAUDE_PID",
                       "CLAUDE_AGENT_SDK_VERSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_EFFORT")
LEGACY_RUNS = (("legacy-none", False, False), ("legacy-feature-only", True, False), ("legacy-env-only", False, True))


class CheckFailed(Exception):
    pass


# Refused or timed-out attempts a model retried, by trace and tool; recorded, not failed.
FAILED_ATTEMPTS = {}


def require(condition, message):
    if not condition:
        raise CheckFailed(message)


# ---- checks on published traces ----

def load(path):
    require(path.exists(), f"missing trace {path.name}")
    return [json.loads(line) for line in path.read_text().splitlines()]


def exchanges(records, name):
    """Pair every request with exactly one reply, per server process."""
    pending, paired = {}, []
    for record in records:
        message, key = record["message"], (record["pid"], json.dumps(record["message"].get("id")))
        if record["dir"] == "c2s" and "method" in message and "id" in message:
            require(key not in pending, f"{name}: duplicate request id {key}")
            pending[key] = {"pid": record["pid"], "t": record["t"], "method": message["method"],
                            "params": message.get("params") or {}, "reply": None}
            paired.append(pending[key])
            continue
        if record["dir"] != "s2c" or "method" in message:
            continue
        require(key in pending, f"{name}: reply {key} matches no request")
        require(pending[key]["reply"] is None, f"{name}: second reply for {key}")
        pending[key]["reply"], pending[key]["seconds"] = message, round(record["t"] - pending[key]["t"], 3)
    missing = [item["method"] for item in paired if item["reply"] is None]
    require(not missing, f"{name}: requests without a reply: {missing}")
    return paired


def succeeded(exchange):
    result = exchange["reply"].get("result")
    return isinstance(result, dict) and not result.get("isError")


def data(exchange):
    return exchange["reply"]["result"]["structuredContent"]["data"]


def check_modern(path):
    paired = exchanges(load(path), path.name)
    require(paired, f"{path.name}: no requests")
    for exchange in paired:
        require(exchange["method"] != "initialize", f"{path.name}: sent initialize")
        meta = exchange["params"].get("_meta") or {}
        require(meta.get("io.modelcontextprotocol/protocolVersion") == PROTOCOL,
                f"{path.name}: {exchange['method']} without protocol {PROTOCOL}")
    for pid in {exchange["pid"] for exchange in paired}:
        mine = [exchange for exchange in paired if exchange["pid"] == pid]
        first = mine[0]
        require(first["method"] == "server/discover" and succeeded(first)
                and PROTOCOL in first["reply"]["result"].get("supportedVersions", []),
                f"{path.name}: server {pid} did not start with a successful discover")
        listed = [e for e in mine if e["method"] == "tools/list" and succeeded(e)]
        require(listed and "mesh_exec" in listed[0]["reply"]["result"]["tools"]["names"],
                f"{path.name}: server {pid} has no tools/list with mesh_exec")
    return [e for e in paired if e["method"] == "tools/call"]


def only_call(calls, name, path):
    """The tool's successful call. A model may retry after a refused or timed-out
    attempt; every success must then return the same data."""
    found = [call for call in calls if call["params"].get("name") == name]
    successes = [call for call in found if succeeded(call)]
    require(successes, f"{path.name}: no successful {name} among {len(found)} calls")
    payloads = {json.dumps(call["reply"]["result"].get("structuredContent", {}).get("data", {}).get("job")
                           or call["reply"]["result"].get("structuredContent", {}).get("data", {}).get("handle"))
                for call in successes}
    require(len(payloads) == 1, f"{path.name}: {name} succeeded with different results {payloads}")
    FAILED_ATTEMPTS[f"{path.parent.name}/{path.name}:{name}"] = len(found) - len(successes)
    return successes[-1]


def check_run1(path, key):
    calls = check_modern(path)
    only_call(calls, "mesh_hosts_list", path)
    opened = data(only_call(calls, "mesh_context_open", path))
    executed = only_call(calls, "mesh_exec", path)
    arguments, result = executed["params"]["arguments"], data(executed)
    require(arguments.get("context") == opened["handle"] and arguments.get("request_key") == key,
            f"{path.name}: mesh_exec arguments {arguments}")
    require(result["state"] == "exited" and result["requestKey"] == key, f"{path.name}: mesh_exec state {result['state']}")
    require(result["result"]["termination"] == {"kind": "exit", "code": 0, "signal": None},
            f"{path.name}: termination {result['result']['termination']}")
    stdout = result["stdout"]
    require(stdout["eof"] and stdout["data"].startswith("hello from "), f"{path.name}: stdout {stdout}")
    seconds = {call["params"]["name"]: call["seconds"] for call in calls}
    return {"context": opened["handle"], "job": result["job"], "stdout": stdout["data"], "callSeconds": seconds}


def check_run2(path, first, key):
    calls = check_modern(path)
    started = [c["params"]["name"] for c in calls if c["params"].get("name") in ("mesh_exec", "mesh_terminal_open")]
    require(not started, f"{path.name}: started a job: {started}")
    receipt = only_call(calls, "mesh_job_receipt", path)
    arguments = receipt["params"]["arguments"]
    require(arguments.get("context") == first["context"] and arguments.get("request_key") == key,
            f"{path.name}: receipt lookup arguments {arguments}")
    found = data(receipt)
    require(found["job"] == first["job"] and found["requestKey"] == key,
            f"{path.name}: receipt returned {found['job']} for {found['requestKey']}")
    output = only_call(calls, "mesh_job_output", path)
    require(output["params"]["arguments"].get("job") == first["job"], f"{path.name}: output read another job")
    page = data(output)
    require(page["job"] == first["job"] and page["data"] == first["stdout"],
            f"{path.name}: output {page['data']!r} differs from run1 {first['stdout']!r}")
    return {"job": found["job"], "callSeconds": {call["params"]["name"]: call["seconds"] for call in calls}}


def check_legacy(path):
    paired = exchanges(load(path), path.name)
    require(paired, f"{path.name}: no requests")
    for exchange in paired:
        require(exchange["method"] == "initialize", f"{path.name}: sent {exchange['method']}")
        error = exchange["reply"].get("error") or {}
        require(error.get("code") == -32601, f"{path.name}: initialize answered {exchange['reply']}")
    return {"initializeRequests": len(paired), "refusedWith": -32601}


def check(out):
    summary = json.loads((out / "summary.json").read_text())
    results = {}
    for client in ("claude-code", "codex"):
        first = check_run1(out / client / "trace-run1.jsonl", EXEC_KEY[client])
        results[f"{client}/run1"] = first
        results[f"{client}/run2"] = check_run2(out / client / "trace-run2.jsonl", first, EXEC_KEY[client])
    project = check_modern(out / "claude-code" / "trace-run3.jsonl")
    only_call(project, "mesh_hosts_list", out / "claude-code" / "trace-run3.jsonl")
    require(summary["clientEvents"]["claude-code/run3"] == {"name": "mesh", "status": "connected", "source": "project"},
            f"project server event {summary['clientEvents']['claude-code/run3']}")
    results["claude-code/run3"] = {"server": "connected from .mcp.json"}
    for run, _, _ in LEGACY_RUNS:
        results[f"codex/{run}"] = check_legacy(out / "codex" / f"trace-{run}.jsonl")
    calls = check_modern(out / "codex" / "trace-noapprove.jsonl")
    require(not [c for c in calls if c["params"].get("name") == "mesh_exec"], "noapprove: mesh_exec reached the server")
    require(summary["clientEvents"]["codex/noapprove"] == [APPROVAL_ERROR],
            f"noapprove: Codex reported {summary['clientEvents']['codex/noapprove']}")
    results["codex/noapprove"] = {"mesh_execOnWire": False, "codexError": APPROVAL_ERROR}
    results["retriedAttempts"] = {name: count for name, count in FAILED_ATTEMPTS.items() if count}
    return results


def record(out):
    """Validate the published evidence and store the results in summary.json."""
    summary = json.loads((out / "summary.json").read_text())
    summary["checks"] = check(out)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")


# ---- running the clients ----

def mesh_env(fixture):
    return {"MESH_STATE_DIR": str(fixture.local), "MESH_CONFIG_DIR": str(fixture.config),
            "HOME": fixture.environment["HOME"]}


def server_args(binary, trace, identity):
    return ["-B", str(HERE / "mcp_tee.py"), str(trace), "--", binary, "mcp", "--identity", identity]


def run1_prompt(host, cwd, prefix):
    return (f"This is an automated test of the 'mesh' MCP server. Do not run shell commands. "
            f"Use only mesh MCP tools, exactly in this order: (1) mesh_hosts_list with no arguments. "
            f"(2) mesh_context_open with host \"{host}\", cwd \"{cwd}\", shell \"login\", "
            f"request_key \"{prefix}-ctx-1\". (3) mesh_exec with the returned context handle, "
            f"request_key \"{prefix}-exec-1\", command {json.dumps(COMMAND)}. "
            f"Then reply with exactly two lines: the context handle, and the job's stdout.")


def run2_prompt(context, prefix):
    return (f"This is an automated test of the 'mesh' MCP server, in a new session. Do not run shell "
            f"commands and do not run any new job. Use only mesh MCP tools, exactly in this order: "
            f"(1) mesh_job_receipt with context \"{context}\" and request_key \"{prefix}-exec-1\". "
            f"(2) mesh_job_output with the job handle from that receipt, stream \"stdout\". Then reply "
            f"with exactly three lines: the job handle, its termination (kind and code), and the stdout text.")


PROBE_PROMPT = ("Automated test. Do not run shell commands. Call the mesh MCP tool mesh_hosts_list once "
                "if it is available and reply with the number of hosts, or reply UNAVAILABLE if no mesh "
                "tools are available.")


def events(text):
    return [json.loads(line) for line in text.splitlines() if line.startswith("{")]


def run_claude(fixture, binary, root, run, prompt, project=False):
    work = root / "claude" / ("project" if project else "")
    work.mkdir(parents=True, exist_ok=True)
    trace = root / "claude" / f"trace-{run}.jsonl"
    config = {"mcpServers": {"mesh": {"type": "stdio", "command": "python3",
                                      "args": server_args(binary, trace, "claude-proof"),
                                      "env": mesh_env(fixture)}}}
    # --mcp-config, --allowedTools and --tools take variadic values, so each is
    # followed by another flag and the prompt goes last.
    args = ["claude", "-p"]
    if project:
        (work / ".mcp.json").write_text(json.dumps(config, indent=2))
        args += ["--settings", json.dumps({"enableAllProjectMcpServers": True})]
    else:
        path = root / "claude" / f"mcp-{run}.json"
        path.write_text(json.dumps(config, indent=2))
        args += ["--mcp-config", str(path), "--strict-mcp-config"]
    args += ["--allowedTools", "mcp__mesh", "--tools", "", "--model", CLAUDE_MODEL,
             "--no-session-persistence", "--output-format", "stream-json", "--verbose"]
    env = {key: value for key, value in os.environ.items() if key not in CLAUDE_SESSION_VARS}
    result = subprocess.run(args + [prompt], cwd=work, env=env, stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, timeout=300, check=False)
    (root / "claude" / f"{run}.stream.jsonl").write_text(result.stdout)
    (root / "claude" / f"{run}.stderr").write_text(result.stderr)
    require(result.returncode == 0, f"claude {run} exited {result.returncode}; see {root}/claude/{run}.stderr")
    servers = [event.get("mcp_servers") for event in events(result.stdout)
               if event.get("type") == "system" and event.get("subtype") == "init"]
    return next((server for server in (servers[0] if servers else []) if server.get("name") == "mesh"), None)


def run_codex(fixture, binary, root, run, prompt, feature=True, protocol_env=True, approve=True):
    work = root / "codex"
    work.mkdir(parents=True, exist_ok=True)
    trace = work / f"trace-{run}.jsonl"
    env_map = mesh_env(fixture)
    if protocol_env:
        env_map["CODEX_MCP_PROTOCOL_VERSION"] = PROTOCOL
    toml_env = "{" + ",".join(f"{key}={json.dumps(value)}" for key, value in env_map.items()) + "}"
    args = ["codex", "exec", "--ignore-user-config", "--ephemeral", "--skip-git-repo-check",
            "--sandbox", "read-only", "--json", "-m", CODEX_MODEL,
            "-c", 'model_reasoning_effort="medium"',
            "-c", 'mcp_servers.mesh.command="python3"',
            "-c", "mcp_servers.mesh.args=" + json.dumps(server_args(binary, trace, "codex-proof")),
            "-c", "mcp_servers.mesh.env=" + toml_env]
    if approve:
        args += ["-c", 'mcp_servers.mesh.default_tools_approval_mode="approve"']
    if feature:
        args += ["--enable", "mcp_2026_07_28"]
    result = subprocess.run(args + [prompt], cwd=work, stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, timeout=400, check=False)
    (work / f"{run}.events.jsonl").write_text(result.stdout)
    (work / f"{run}.stderr").write_text(result.stderr)
    require(result.returncode == 0, f"codex {run} exited {result.returncode}; see {work}/{run}.stderr")
    items = [event.get("item") or {} for event in events(result.stdout)]
    return [item["error"].get("message") for item in items
            if item.get("type") == "mcp_tool_call" and item.get("tool") == "mesh_exec" and item.get("error")]


def handle_from(trace):
    """The context handle mesh_context_open returned, read from the raw trace."""
    for line in trace.read_text().splitlines():
        record = json.loads(line)
        if record["dir"] != "s2c" or '"handle":"mc1.' not in record["line"]:
            continue
        return json.loads(record["line"])["result"]["structuredContent"]["data"]["handle"]
    raise CheckFailed(f"{trace.name}: no context handle")


def version(command):
    return subprocess.run(command, capture_output=True, text=True, check=True, timeout=30).stdout.strip()


def drive(fixture, binary, root):
    from mcp_native import run_cli  # noqa: PLC0415 (needs the fixture's sys.path)
    for name in ("claude-proof", "codex-proof"):
        created = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        require(created.returncode == 0, f"identity create {name} exited {created.returncode}")
    host, cwd = "pc", str(fixture.root)
    client_events = {}
    run_claude(fixture, binary, root, "run1", run1_prompt(host, cwd, "claude-proof"))
    run_claude(fixture, binary, root, "run2", run2_prompt(handle_from(root / "claude" / "trace-run1.jsonl"), "claude-proof"))
    client_events["claude-code/run3"] = run_claude(fixture, binary, root, "run3", PROBE_PROMPT, project=True)
    run_codex(fixture, binary, root, "run1", run1_prompt(host, cwd, "codex-proof"))
    context = handle_from(root / "codex" / "trace-run1.jsonl")
    run_codex(fixture, binary, root, "run2", run2_prompt(context, "codex-proof"))
    for run, feature, protocol_env in LEGACY_RUNS:
        run_codex(fixture, binary, root, run, PROBE_PROMPT, feature=feature, protocol_env=protocol_env)
    client_events["codex/noapprove"] = run_codex(
        fixture, binary, root, "noapprove",
        f"Automated test. Do not run shell commands. Call the mesh MCP tool mesh_exec once with context "
        f"\"{context}\", request_key \"codex-proof-exec-2\", command \"echo second\". Reply with its stdout, "
        f"or with the exact error if the call was refused or cancelled.", approve=False)
    return client_events


def publish(root, out, binary, client_events):
    for client, target in (("claude", "claude-code"), ("codex", "codex")):
        (out / target).mkdir(parents=True, exist_ok=True)
        for stale in (out / target).glob("trace-*.jsonl"):
            stale.unlink()
        for trace in sorted((root / client).glob("trace-*.jsonl")):
            subprocess.run([sys.executable, "-I", str(HERE / "redact.py"), str(trace), str(out / target / trace.name),
                            str(root), str(HERE), str(Path(binary).parent)], check=True)
    summary = {"mesh": version([binary, "version"]).split()[1:3], "claudeCode": version(["claude", "--version"]),
               "codex": version(["codex", "--version"]), "transport": "stdio",
               "models": {"claude-code": CLAUDE_MODEL, "codex": CODEX_MODEL},
               "runAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "clientEvents": client_events}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    record(out)


def run(binary, root, out):
    sys.path.insert(0, str(REPO / "integration" / "helpers"))
    from mcp_native import MCPFixture  # noqa: PLC0415
    fleet = root / "fleet"
    fleet.mkdir()
    fixture = MCPFixture(binary, fleet)
    try:
        fixture.start_remote()
        client_events = drive(fixture, binary, root)
    finally:
        fixture.close()
    publish(root, out, binary, client_events)


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    try:
        if mode == "run" and len(sys.argv) == 5:
            run(str(Path(sys.argv[2]).resolve()), Path(sys.argv[3]).resolve(), Path(sys.argv[4]).resolve())
        elif mode == "check" and len(sys.argv) == 3:
            check(Path(sys.argv[2]).resolve())
        elif mode == "check" and len(sys.argv) == 4 and sys.argv[3] == "--record":
            record(Path(sys.argv[2]).resolve())
        else:
            raise SystemExit(__doc__)
    except CheckFailed as failure:
        raise SystemExit(f"FAIL: {failure}") from None
    print("PASS: Claude Code and Codex (both opt-ins) used stateless 2026-07-28 for discover, list, mesh_exec "
          "and a fresh-process lookup of the same job; Codex without both opt-ins sent only initialize, "
          "each refused with -32601; without an approval mode Codex kept mesh_exec off the wire")


if __name__ == "__main__":
    main()
