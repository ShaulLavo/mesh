"""Prove real MCP clients against `mesh mcp --identity NAME` over stdio.

Usage (from the repository root, with a built binary):

    python3 -B .internal/bench-results/mcp-clients/harness/prove.py MESH_BINARY ROOT OUT

ROOT is an empty, short scratch directory (Unix socket paths must stay short).
OUT receives the redacted traces and summary.json.

It starts the `mcp_native` integration fixture (a client state directory plus a
second daemon with its own state, reached over its authenticated WebSocket),
creates one application identity per client, and drives each installed client
non-interactively with the owner's existing logins:

- Claude Code: `claude -p --strict-mcp-config --mcp-config FILE` (run1, run2) and a
  project `.mcp.json` (run3).
- Codex CLI: `codex exec --ignore-user-config -c mcp_servers.mesh.*` with
  `--enable mcp_2026_07_28` and `CODEX_MCP_PROTOCOL_VERSION=2026-07-28` (run1, run2),
  then the three legacy combinations and the missing approval mode.

Each server runs under mcp_tee.py, which records every stdio line. run1 lists
hosts, opens a context and runs one command; run2 is a fresh client and a fresh
`mesh mcp` process that looks the job up by request key and reads its output.
"""
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[3]
sys.path.insert(0, str(REPO / "integration" / "helpers"))

from mcp_native import MCPFixture, run_cli  # noqa: E402

CLAUDE_MODEL = "claude-sonnet-5-5"
CODEX_MODEL = "gpt-6.1-terra"
COMMAND = "printf 'hello from %s\\n' \"$(uname -s)\"; pwd"
# A nested run from inside another Claude Code session must not inherit its session.
CLAUDE_SESSION_VARS = ("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID",
                       "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_SOCKET",
                       "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_EXECPATH", "CLAUDE_PID",
                       "CLAUDE_AGENT_SDK_VERSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_EFFORT")
HANDLE = re.compile(r"mc1\.[A-Za-z0-9_-]+\.[0-9a-f]{32}")


def fail(message):
    raise SystemExit(f"FAIL: {message}")


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
    if result.returncode != 0:
        fail(f"claude {run} exited {result.returncode}: {result.stderr[-2000:]}")
    answer = ""
    for line in result.stdout.splitlines():
        event = json.loads(line)
        if event.get("type") == "result":
            answer = event.get("result", "")
    return {"rc": result.returncode, "answer": answer, "trace": trace}


def run_codex(fixture, binary, root, run, prompt, feature=True, protocol_env=True, approve=True):
    work = root / "codex"
    work.mkdir(parents=True, exist_ok=True)
    trace = work / f"trace-{run}.jsonl"
    env_map = mesh_env(fixture)
    if protocol_env:
        env_map["CODEX_MCP_PROTOCOL_VERSION"] = "2026-07-28"
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
    if result.returncode != 0:
        fail(f"codex {run} exited {result.returncode}: {result.stderr[-2000:]}")
    answer, failures = "", []
    for line in result.stdout.splitlines():
        item = json.loads(line).get("item") or {}
        if item.get("type") == "agent_message":
            answer = item.get("text", "")
        if item.get("type") == "mcp_tool_call" and item.get("error"):
            failures.append(item["error"].get("message", ""))
    return {"rc": result.returncode, "answer": answer, "toolErrors": failures, "trace": trace}


def read_trace(path):
    messages = []
    if not path.exists():
        return messages
    for raw in path.read_text().splitlines():
        record = json.loads(raw)
        if record["dir"] == "err":
            continue
        try:
            messages.append((record["dir"], json.loads(record["line"]), record["t"]))
        except json.JSONDecodeError:
            continue
    return messages


def calls(messages):
    """Pair each tools/call with its reply: [(name, arguments, result, seconds)]."""
    pending, paired = {}, []
    for direction, message, at in messages:
        if direction == "c2s" and message.get("method") == "tools/call":
            pending[json.dumps(message["id"])] = (message["params"]["name"], message["params"].get("arguments"), at)
        if direction == "s2c" and "id" in message and json.dumps(message["id"]) in pending:
            name, arguments, start = pending.pop(json.dumps(message["id"]))
            paired.append((name, arguments, message.get("result") or message.get("error"), round(at - start, 3)))
    return paired


def methods(messages):
    return [message["method"] for direction, message, _ in messages if direction == "c2s" and "method" in message]


def check_run1(messages):
    seen = methods(messages)
    if "initialize" in seen or not seen or seen[0] != "server/discover":
        fail(f"run1 did not start with server/discover alone: {seen}")
    paired = {name: (arguments, result, took) for name, arguments, result, took in calls(messages)}
    for name in ("mesh_hosts_list", "mesh_context_open", "mesh_exec"):
        if name not in paired or paired[name][1].get("isError"):
            fail(f"run1 {name} missing or failed: {paired.get(name)}")
    data = paired["mesh_exec"][1]["structuredContent"]["data"]
    if data["state"] != "exited" or data["result"]["termination"] != {"kind": "exit", "code": 0, "signal": None}:
        fail(f"run1 mesh_exec did not exit 0: {data['state']} {data.get('result')}")
    if not data["stdout"]["data"].startswith("hello from "):
        fail(f"run1 stdout: {data['stdout']}")
    handle = paired["mesh_context_open"][1]["structuredContent"]["data"]["handle"]
    return handle, data["job"], {name: took for name, (_, _, took) in paired.items()}


def check_run2(messages, job):
    seen = methods(messages)
    if "initialize" in seen or seen[0] != "server/discover":
        fail(f"run2 did not start with server/discover alone: {seen}")
    paired = {name: (arguments, result, took) for name, arguments, result, took in calls(messages)}
    for name in ("mesh_job_receipt", "mesh_job_output"):
        if name not in paired or paired[name][1].get("isError"):
            fail(f"run2 {name} missing or failed: {paired.get(name)}")
    if "mesh_exec" in paired:
        fail("run2 started a new job")
    output = paired["mesh_job_output"][1]["structuredContent"]["data"]
    text = json.dumps(output)
    if job.split(".")[2] not in text or "hello from " not in text:
        fail(f"run2 output does not belong to job {job}: {text[:400]}")
    return {name: took for name, (_, _, took) in paired.items()}


def check_legacy(messages):
    seen = methods(messages)
    replies = [message for direction, message, _ in messages if direction == "s2c"]
    refused = all(reply.get("error", {}).get("code") == -32601 for reply in replies)
    if seen and set(seen) == {"initialize"} and refused:
        return {"requests": seen, "refusedWith": -32601}
    fail(f"legacy mode did not send only initialize refused with -32601: {seen} {replies}")


def redact(trace, out, root):
    subprocess.run([sys.executable, "-I", str(HERE / "redact.py"), str(trace), str(out), str(root),
                    str(HERE), os.environ.get("USER", "")], check=True)


def version(command):
    return subprocess.run(command, capture_output=True, text=True, check=True, timeout=30).stdout.strip()


def prove(fixture, binary, root, out):
    host, cwd = "pc", str(fixture.root)
    summary = {"mesh": version([binary, "version"]), "claudeCode": version(["claude", "--version"]),
               "codex": version(["codex", "--version"]), "transport": "stdio",
               "startedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "runs": {}}
    for name in ("claude-proof", "codex-proof"):
        created = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", name, "--json")
        if created.returncode != 0:
            fail(f"identity create {name}: {created.stdout} {created.stderr}")

    first = run_claude(fixture, binary, root, "run1", run1_prompt(host, cwd, "claude-proof"))
    context, job, took = check_run1(read_trace(first["trace"]))
    second = run_claude(fixture, binary, root, "run2", run2_prompt(context, "claude-proof"))
    summary["runs"]["claude-code/run1"] = {"answer": first["answer"], "context": context, "job": job, "callSeconds": took}
    summary["runs"]["claude-code/run2"] = {"answer": second["answer"], "callSeconds": check_run2(read_trace(second["trace"]), job)}
    third = run_claude(fixture, binary, root, "run3", PROBE_PROMPT, project=True)
    if "mesh_hosts_list" not in [name for name, *_ in calls(read_trace(third["trace"]))]:
        fail(f"claude-code project .mcp.json run did not call mesh_hosts_list: {third['answer']}")
    summary["runs"]["claude-code/run3-project-mcp-json"] = {"answer": third["answer"]}

    first = run_codex(fixture, binary, root, "run1", run1_prompt(host, cwd, "codex-proof"))
    context, job, took = check_run1(read_trace(first["trace"]))
    second = run_codex(fixture, binary, root, "run2", run2_prompt(context, "codex-proof"))
    summary["runs"]["codex/run1"] = {"answer": first["answer"], "context": context, "job": job, "callSeconds": took}
    summary["runs"]["codex/run2"] = {"answer": second["answer"], "callSeconds": check_run2(read_trace(second["trace"]), job)}
    for run, feature, protocol_env in (("legacy-none", False, False), ("legacy-feature-only", True, False),
                                       ("legacy-env-only", False, True)):
        result = run_codex(fixture, binary, root, run, PROBE_PROMPT, feature=feature, protocol_env=protocol_env)
        summary["runs"][f"codex/{run}"] = {"answer": result["answer"], **check_legacy(read_trace(result["trace"]))}
    no_approval = run_codex(fixture, binary, root, "noapprove",
                            f"Automated test. Do not run shell commands. Call the mesh MCP tool mesh_exec once with "
                            f"context \"{context}\", request_key \"codex-proof-exec-2\", command \"echo second\". "
                            f"Reply with its stdout, or with the exact error if the call was refused or cancelled.",
                            approve=False)
    summary["runs"]["codex/noapprove"] = {"answer": no_approval["answer"], "toolErrors": no_approval["toolErrors"]}

    for client in ("claude", "codex"):
        target = out / ("claude-code" if client == "claude" else "codex")
        target.mkdir(parents=True, exist_ok=True)
        for trace in sorted((root / client).glob("trace-*.jsonl")):
            redact(trace, target / trace.name, root)
    text = json.dumps(summary, indent=2).replace(str(root), "$ROOT")
    (out / "summary.json").write_text(text + "\n")
    print(text)


def main():
    binary, root, out = str(Path(sys.argv[1]).resolve()), Path(sys.argv[2]).resolve(), Path(sys.argv[3]).resolve()
    fleet = root / "fleet"
    fleet.mkdir()
    fixture = MCPFixture(binary, fleet)
    try:
        fixture.start_remote()
        prove(fixture, binary, root, out)
    finally:
        fixture.close()
    print("PASS: Claude Code and Codex (opt-in) used stateless 2026-07-28 for discover, list, mesh_exec "
          "and a fresh-process result lookup; Codex without both opt-ins sent only initialize and was refused")


if __name__ == "__main__":
    main()
