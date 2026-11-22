"""Drive mesh mcp terminal and service tools against an isolated remote daemon.

A real person attach (mesh attach in a PTY) races agent input and resize at
the worker, and the agent's screen reads never change the person's size.
"""
import json
from pathlib import Path
import socket
import sys
import tempfile

from mcp_native import MCPClient, MCPFixture, run_cli
from terminal_window import Terminal, eventually, require

DETACH = b"\x1d"  # ctrl+], the default detach key


def data(reply):
    require(not reply["result"]["isError"], f"tool failed: {reply}")
    return reply["result"]["structuredContent"]["data"]


def error_code(reply):
    require(reply["result"]["isError"], f"tool unexpectedly succeeded: {reply}")
    return reply["result"]["structuredContent"]["errors"][0]["code"]


def screen_text(client, job, context):
    screen = data(client.call_tool("mesh_terminal_screen", {"job": job, "context": context, "cols": 120, "rows": 24}))["screen"]
    return screen, "\n".join(screen["lines"])


def wait_screen(client, job, context, marker):
    seen = {}

    def shown():
        seen["screen"], text = screen_text(client, job, context)
        return marker in text
    eventually(shown, f"terminal screen never showed {marker!r}: {seen.get('screen')}")
    return seen["screen"]


class Writer:
    """Keyed terminal input with a running offset, as an agent keeps it."""

    def __init__(self, client, job, context):
        self.client, self.job, self.context, self.offset, self.count = client, job, context, 0, 0

    def call(self, key, text, offset=None):
        args = {"job": self.job, "context": self.context, "write_key": key, "offset": str(self.offset if offset is None else offset), "data": text}
        return self.client.call_tool("mesh_terminal_input", args)

    def type(self, text):
        self.count += 1
        reply = self.call(f"w{self.count}", text)
        ack = data(reply)
        require(ack["offset"] == str(self.offset) and ack["length"] == len(text.encode()), f"bad acknowledgment: {ack}")
        self.offset += len(text.encode())
        return reply


def exercise_terminal(fixture):
    host_id = fixture.remote_id
    res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", "term", "--json")
    require(res.returncode == 0, f"identity create failed: {res.stdout!r} {res.stderr!r}")
    agent = MCPClient(fixture.binary, fixture.environment, args=("--identity", "term"))
    try:
        ctx = data(agent.call_tool("mesh_context_open", {"host": host_id, "request_key": "term-ctx", "cwd": str(fixture.root), "shell": "sh"}))["handle"]
        open_args = {"context": ctx, "request_key": "term-1", "argv": ["/bin/sh"], "env": {"PS1": "$ "}, "cols": 100, "rows": 20}
        opened = data(agent.call_tool("mesh_terminal_open", open_args))
        job = opened["job"]
        require(data(agent.call_tool("mesh_terminal_open", open_args))["job"] == job, "terminal_open replay returned another job")
        session_id = job.split(".")[2]
        instance = job.split(".")[3]

        writer = Writer(agent, job, ctx)
        writer.type("printf 'MARK-%s\\n' one\r")
        wait_screen(agent, job, ctx, "MARK-one")

        # A lost acknowledgment: the exact retry and the lookup return the
        # original receipt and never type the keys twice.
        retry = data(writer.call("w1", "printf 'MARK-%s\\n' one\r", offset=0))
        require(retry["offset"] == "0", f"retry acknowledgment: {retry}")
        lookup = data(agent.call_tool("mesh_terminal_input", {"job": job, "context": ctx, "write_key": "w1"}))
        require(lookup == retry, f"lookup {lookup} != retry {retry}")
        require(error_code(writer.call("w1", "rm -rf /tmp/never\r", offset=0)) == "input.key_conflict", "conflicting key was not refused")
        require(error_code(writer.call("stale", "echo stale\r", offset=0)) == "input.offset_mismatch", "stale offset was not refused")
        writer.type("printf 'COUNT-%s\\n' $(history 2>/dev/null | wc -l)\r")

        # Resize while detached, then confirm the program sees it.
        size = data(agent.call_tool("mesh_terminal_resize", {"job": job, "context": ctx, "cols": 120, "rows": 40}))
        require(size["size"]["cols"] == 120 and size["size"]["rows"] == 40, f"resize result: {size}")
        writer.type("printf 'SIZE-%s\\n' \"$(stty size)\"\r")
        wait_screen(agent, job, ctx, "SIZE-40 120")

        # Kinds never mix: pipe stdin refuses a terminal job, terminal keys
        # refuse a pipe job.
        pipe_tool = agent.call_tool("mesh_job_input", {"job": job, "context": ctx, "write_key": "p1", "offset": "0", "data": "x"})
        require(error_code(pipe_tool) == "usage.invalid", f"pipe input reached a terminal job: {pipe_tool}")
        pipe_job = data(agent.call_tool("mesh_exec", {"context": ctx, "request_key": "pipe-1", "argv": ["/bin/cat"], "stdin": "open", "wait_ms": 0}))["job"]
        keys_to_pipe = agent.call_tool("mesh_terminal_input", {"job": pipe_job, "context": ctx, "write_key": "t1", "offset": "0", "data": "x"})
        require(error_code(keys_to_pipe) == "usage.invalid", f"terminal input reached a pipe job: {keys_to_pipe}")
        data(agent.call_tool("mesh_job_cancel", {"job": pipe_job, "context": ctx}))

        exercise_human_attach(fixture, agent, writer, job, ctx, session_id)

        # CLI parity: the same screen and resize envelopes from mesh session.
        cli = run_cli(fixture.binary, fixture.environment, "session", "screen", "--host", host_id, session_id, "--instance", instance,
                      "--identity", "term", "--context", ctx, "--cols", "120", "--rows", "24", "--json")
        require(cli.returncode == 0, f"mesh session screen failed: {cli.stdout!r} {cli.stderr!r}")
        cli_screen = json.loads(cli.stdout)["data"]
        mcp_screen, _ = screen_text(agent, job, ctx)
        require(cli_screen["lines"] == mcp_screen["lines"] and cli_screen["instance"] == instance, f"CLI screen {cli_screen} != MCP {mcp_screen}")
        cli = run_cli(fixture.binary, fixture.environment, "session", "resize", "--host", host_id, session_id, "--instance", instance,
                      "--identity", "term", "--context", ctx, "--cols", "90", "--rows", "30", "--json")
        require(cli.returncode == 0 and json.loads(cli.stdout)["data"]["cols"] == 90, f"mesh session resize failed: {cli.stdout!r} {cli.stderr!r}")

        # A second identity reaches none of it.
        res = run_cli(fixture.binary, fixture.environment, "mcp", "identity", "create", "other", "--json")
        require(res.returncode == 0, f"identity create failed: {res.stdout!r}")
        other = MCPClient(fixture.binary, fixture.environment, args=("--identity", "other"))
        try:
            for tool, args in (("mesh_terminal_screen", {"job": job}), ("mesh_terminal_resize", {"job": job, "cols": 80, "rows": 24}),
                               ("mesh_terminal_input", {"job": job, "write_key": "x", "offset": str(writer.offset), "data": "x"})):
                require(error_code(other.call_tool(tool, args)) == "access.denied", f"{tool} reached another identity's job")
            # The human-session tools do not reach it either.
            inspect = other.call_tool("mesh_session_inspect", {"host": host_id, "session_id": session_id, "preview": True})
            require(error_code(inspect) == "session.not_found", f"session inspect reached another identity's job: {inspect}")
            listed = data(other.call_tool("mesh_sessions_list", {"host": host_id, "all": True}))
            require(all(row.get("id") != session_id for row in listed.get("sessions", [])), f"session list shows a terminal job: {listed}")
            other.close_eof()
        finally:
            other.terminate()

        # Passive discovery mode (no identity) never acts with the account's
        # authority: terminal and mutating tools refuse before any host.
        passive = MCPClient(fixture.binary, fixture.environment)
        try:
            for tool, args in (("mesh_terminal_screen", {"job": job}), ("mesh_terminal_resize", {"job": job, "cols": 80, "rows": 24}),
                               ("mesh_terminal_input", {"job": job, "write_key": "p", "offset": str(writer.offset), "data": "x"}),
                               ("mesh_service_start", {"host": host_id, "name": "/m5"}),
                               ("mesh_session_hibernate", {"host": host_id, "session_id": session_id})):
                require(error_code(passive.call_tool(tool, args)) == "usage.invalid", f"{tool} ran without an identity")
            passive.close_eof()
        finally:
            passive.terminate()

        writer.type("printf 'LAST-%s\\n' screen; exit\r")
        waited = data(agent.call_tool("mesh_job_wait", {"job": job, "context": ctx, "wait_ms": 10000}))
        require(waited["state"] == "exited", f"terminal job did not exit: {waited}")
        # The last screen stays readable after exit, marked as saved.
        retained = data(agent.call_tool("mesh_terminal_screen", {"job": job, "context": ctx}))["screen"]
        require(retained["source"] == "saved" and any("LAST-screen" in line for line in retained["lines"]), f"retained screen: {retained}")
        require(error_code(writer.call("late", "echo late\r")) in ("terminal.closed", "input.outcome_unknown"), "input to an exited terminal was accepted")
        rc, _ = agent.close_eof()
        require(rc == 0, f"mesh mcp exit = {rc}")
    finally:
        agent.terminate()


def exercise_human_attach(fixture, agent, writer, job, ctx, session_id):
    # mesh attach is local to the job's host: attach the way a person at
    # that machine does.
    at_host = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote), "MESH_CONFIG_DIR": str(fixture.root / "remote-config")}
    person = Terminal([fixture.binary, "attach", session_id], at_host, fixture.root)
    fixture.terminals.append(person)
    try:
        eventually(lambda: screen_text(agent, job, ctx)[0]["attached"], "screen never reported the person attached")
    except RuntimeError as error:
        raise RuntimeError(f"{error}; attach output={person.drain()[-2000:]!r}") from error
    person.send("printf 'HUMAN-%s\\n' \"$(stty size)\"\r")
    person.expect("HUMAN-24 80")

    # The person holds the terminal: agent keys and resizes are refused and
    # write nothing; screen reads leave the person's size alone.
    refused = writer.call("during", "printf 'AGENT-WROTE\\n'\r")
    require(error_code(refused) == "terminal.attached", f"agent input while attached: {refused}")
    resize = agent.call_tool("mesh_terminal_resize", {"job": job, "context": ctx, "cols": 132, "rows": 50})
    require(error_code(resize) == "terminal.attached", f"agent resize while attached: {resize}")
    for _ in range(3):
        screen_text(agent, job, ctx)
    start = len(person.drain())
    person.send("printf 'AFTER-%s\\n' \"$(stty size)\"\r")
    person.expect("AFTER-24 80", since=start)
    require(b"AGENT-WROTE" not in person.drain(), "refused agent input reached the terminal")
    # An admitted write keeps its acknowledgment while the person is attached.
    require(data(writer.call("w1", "printf 'MARK-%s\\n' one\r", offset=0))["offset"] == "0", "acknowledged write lost while attached")

    person.send(DETACH)
    person.expect_exit()
    eventually(lambda: not screen_text(agent, job, ctx)[0]["attached"], "screen still reports the person attached")
    writer.type("printf 'BACK-%s\\n' agent\r")
    wait_screen(agent, job, ctx, "BACK-agent")


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def exercise_services(fixture):
    host_id = fixture.remote_id
    agent = MCPClient(fixture.binary, fixture.environment, args=("--identity", "term"))
    try:
        port = free_port()
        site = fixture.root / "m5-site"
        site.mkdir()
        (site / "index.html").write_text("m5\n")
        run = f"exec {sys.executable} -m http.server {port} --bind 127.0.0.1"
        published = data(agent.call_tool("mesh_service_publish", {"host": host_id, "target": str(port), "route": "/m5", "run": run, "cwd": str(site), "listen": [f"{free_port()}={port}"]}))
        name = published["service"]["name"]
        require(published["warnings"] == [] or all(len(w) <= 512 for w in published["warnings"]), f"unbounded warnings: {published}")
        relative = agent.call_tool("mesh_service_publish", {"host": host_id, "target": str(port), "route": "/m5b", "run": run, "cwd": "site"})
        require(error_code(relative) == "usage.invalid", f"relative cwd accepted: {relative}")

        started = data(agent.call_tool("mesh_service_start", {"host": host_id, "name": "/m5", "timeout_ms": 60000}, timeout=75))
        require(started["sessionId"] != "" and "running" in started["state"], f"start result: {started}")
        stopped = data(agent.call_tool("mesh_service_stop", {"host": host_id, "name": "/m5"}))
        require(stopped.get("sessionId", "") == "" and "stopped" in stopped["state"], f"stop result: {stopped}")

        removed = data(agent.call_tool("mesh_service_unpublish", {"host": host_id, "name": "/m5"}))
        require(removed["removed"] and removed["name"] == name, f"unpublish result: {removed}")
        missing = agent.call_tool("mesh_service_unpublish", {"host": host_id, "name": "/m5"})
        message = missing["result"]["structuredContent"]["errors"][0]["message"]
        require(error_code(missing) == "service.not_found" and len(message) <= 512, f"second unpublish: {missing}")

        # Lifecycle controls keep the host's restrictions and never attach.
        shell = run_cli(fixture.binary, fixture.environment, "session", "create", "--host", host_id, "--kind", "pty", "--cwd", str(fixture.root),
                        "--request-key", "plain-shell", "--json", "--", "/bin/sh")
        require(shell.returncode == 0, f"plain shell create failed: {shell.stdout!r} {shell.stderr!r}")
        plain = json.loads(shell.stdout)["data"]["sessionId"]
        hibernate = agent.call_tool("mesh_session_hibernate", {"host": host_id, "session_id": plain})
        require(hibernate["result"]["isError"] and len(hibernate["result"]["structuredContent"]["errors"][0]["message"]) <= 512, f"plain shell hibernated: {hibernate}")
        recover = agent.call_tool("mesh_session_recover", {"host": host_id, "session_id": plain, "action": "shell"})
        require(error_code(recover) == "usage.invalid", f"running session recovered: {recover}")

        # App owner operations stay local to their host; this host offers no
        # Mesh SSH, so the listing fails with a short diagnostic.
        apps = agent.call_tool("mesh_apps_list", {"host": host_id, "timeout_ms": 3000})
        require(apps["result"]["isError"] and len(apps["result"]["structuredContent"]["errors"][0]["message"]) <= 512, f"apps list: {apps}")
        rc, _ = agent.close_eof()
        require(rc == 0, f"mesh mcp exit = {rc}")
    finally:
        agent.terminate()


def main():
    binary = sys.argv[1]
    with tempfile.TemporaryDirectory(prefix="mesh-mcp-terminal-") as directory:
        root = Path(directory)
        fixture = MCPFixture(binary, root)
        # Route run commands go through the daemon's login shell, which must
        # run -c; the terminal-window fixture shell is interactive only.
        fixture.environment["SHELL"] = "/bin/sh"
        try:
            fixture.start_remote()
            exercise_terminal(fixture)
            exercise_services(fixture)
        finally:
            fixture.close()
    print("PASS: mesh mcp terminal jobs with keyed input, attach races, passive screens, CLI parity, and service and lifecycle tools")


if __name__ == "__main__":
    main()
