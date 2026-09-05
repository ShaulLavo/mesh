#!/usr/bin/env python3
"""Prove native argv, worker acknowledgements, and recovery with fake providers."""

from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
from mesh_control import round_trip
from recovery_transactions import kill_worker, websocket_request
from ssh_sessions import SSHFixture
from terminal_window import Fixture, Terminal, eventually, require, run_outside_containing_session


PROVIDER = r'''#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys, time
if sys.argv[1:] == ["--version"]:
    print("2.1.261 (Claude Code)")
    raise SystemExit(0)
root = pathlib.Path(os.environ["MESH_TEST_AGENT_ROOT"])
resumed = next((arg.split("=", 1)[1] for arg in sys.argv if arg.startswith("--resume=")), None)
identity = resumed or sys.argv[sys.argv.index("--model") + 1]
session = os.environ["MESH_SESSION_ID"]
(root / ("launch-" + session + ".json")).write_text(json.dumps({"argv":sys.argv[1:], "id":identity, "cwd":os.getcwd(), "root":os.environ.get("CLAUDE_CONFIG_DIR")}))
if resumed:
    while not (root / "allow-resume").exists():
        time.sleep(0.01)
    if identity == "missing-conversation":
        print("Saved conversation no longer exists", flush=True)
        raise SystemExit(17)
event = {"hook_event_name":"SessionStart", "session_id":identity, "cwd":os.getcwd(), "source":"resume" if resumed else "startup", "prompt":"must-not-save", "transcript_path":"/private/must-not-save"}
subprocess.run([os.environ["MESH_TEST_AGENT_BINARY"], "agent-hook", "claude"], input=json.dumps(event).encode(), check=True)
print("AGENT_READY_" + identity, flush=True)
sys.stdin.readline()
'''


def record(fixture, session_id):
    path = fixture.remote / "s" / session_id / "recovery.json"
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return {}


def recover(fixture, source, request_id, endpoint=None):
    request = {"type": "session.recover", "requestId": request_id, "sessionId": source}
    if endpoint:
        return websocket_request(endpoint, request)
    return round_trip(str(fixture.remote / "daemon.sock"), request)


def setup(fixture, ssh=False):
    providers = fixture.root / "providers"
    providers.mkdir()
    fake = providers / "claude"
    fake.write_text(PROVIDER)
    fake.chmod(0o700)
    storage = fixture.root / "claude-state"
    storage.mkdir()
    home = fixture.root / "home"
    home.mkdir()
    fixture.environment = {key: value for key, value in fixture.environment.items()
                           if not key.startswith("MESH_AGENT_")}
    fixture.environment.update(PATH=str(providers) + os.pathsep + fixture.environment["PATH"],
                               CLAUDE_CONFIG_DIR=str(storage), SHELL="/bin/bash",
                               HOME=str(home),
                               MESH_TEST_AGENT_ROOT=str(fixture.root), MESH_TEST_AGENT_BINARY=fixture.binary)
    if ssh:
        fixture.start_ssh()
        fixture.remote = fixture.local
        return
    fixture.start_remote()


def create(fixture, identity):
    response = round_trip(str(fixture.remote / "daemon.sock"), {
        "type": "session.create", "requestId": "create-" + identity,
        "command": [fixture.binary, "agent", "claude", "--", "--model", identity],
        "cwd": str(fixture.root),
    })
    require(response.get("type") == "session.created", f"create failed: {response}")
    source = response["sessionId"]
    eventually(lambda: record(fixture, source).get("agent", {}).get("conversationId") == identity,
               f"native launch did not register {identity}; {source}")
    saved = record(fixture, source)
    require(saved["agent"]["provider"] == "claude" and saved["agent"]["lifecycle"] == "active", saved)
    require("must-not-save" not in json.dumps(saved), "checkpoint retained provider context")
    require(saved["agent"]["directory"] == str(fixture.root), "provider directory changed")
    return source


def crash(fixture, source):
    leader = kill_worker(fixture.remote, source)
    try:
        os.killpg(leader, signal.SIGKILL)
    except ProcessLookupError:
        pass
    def listener_closed():
        try:
            with socket.socket(socket.AF_UNIX) as connection:
                connection.connect(str(fixture.remote / "s" / source / "sock"))
                return False
        except (FileNotFoundError, ConnectionRefusedError):
            return True
    eventually(listener_closed, "SIGKILL did not close the source worker listener")


def delayed_recovery(fixture, sources):
    endpoint = json.loads((fixture.config / "hosts.json").read_text())["hosts"][0]["endpoint"]
    replacements = []
    for source, identity in sources:
        crash(fixture, source)
        with ThreadPoolExecutor(max_workers=8) as pool:
            replies = list(pool.map(lambda index: recover(fixture, source, f"retry-{index}", endpoint if index % 2 else None), range(8)))
        require(all(reply.get("type") == "session.recovered" for reply in replies), f"recovery failed: {replies}")
        ids = {reply["recoveryResult"]["sessionId"] for reply in replies}
        require(len(ids) == 1 and source not in ids, f"multiple replacement sessions: {ids}")
        replacement = ids.pop()
        require(all(reply["recoveryResult"]["agentStatus"] == "unverified" for reply in replies), replies)
        path = fixture.root / ("launch-" + replacement + ".json")
        eventually(path.exists, "replacement never started the native helper")
        invocation = json.loads(path.read_text())
        require(invocation["id"] == identity, f"wrong conversation: {invocation}")
        require(invocation["argv"] == ["--model", identity, "--resume=" + identity], f"new prompt or wrong argv: {invocation}")
        require(invocation["cwd"] == str(fixture.root) and invocation["root"] == str(fixture.root / "claude-state"), "resume lost provider context")
        require(record(fixture, source).get("agent", {}).get("conversationId") == identity, "source record lost before acknowledgement")
        replacements.append((source, replacement, identity))
    require(len(list((fixture.remote / "s").glob("*/meta.json"))) == 4, "unexpected recovery workers")
    return replacements


def verify_receipts(fixture, replacements):
    (fixture.root / "allow-resume").touch()
    for source, replacement, identity in replacements:
        eventually(lambda: record(fixture, replacement).get("agentResume", {}).get("conversationId") == identity,
                   "provider startup did not durably acknowledge the exact ID")
        reply = recover(fixture, source, "after-ack")
        require(reply["recoveryResult"]["agentStatus"] == "verified" and reply["sessionId"] == replacement, reply)
        receipt = record(fixture, replacement)["agentResume"]
        require(receipt["sourceSessionId"] == source and receipt["conversationId"] == identity, receipt)
        require(record(fixture, source)["agent"]["invocationToken"] != receipt["invocationToken"], "old token reused for resume")
        require(record(fixture, source), "verified recovery deleted original checkpoint")


def missing_conversation(fixture):
    source = create(fixture, "missing-conversation")
    crash(fixture, source)
    environment = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
    terminal = Terminal([fixture.binary, "recover", source], environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect(b"Press Enter to open a shell")
    reply = recover(fixture, source, "failed-native-retry")
    require(reply["recoveryResult"]["agentStatus"] == "unverified", reply)
    replacement = reply["sessionId"]
    terminal.send("\n")
    terminal.expect(b"Opening the saved shell")
    terminal.send("printf 'T25_SHELL:%s\\n' \"$PWD\"\n")
    terminal.expect(("T25_SHELL:" + str(fixture.root)).encode())
    require(not record(fixture, replacement).get("agentResume"), "failed lookup claimed verified resume")
    retry = recover(fixture, source, "after-open-shell")
    require(retry["sessionId"] == replacement and retry["recoveryResult"]["agentStatus"] == "unverified", retry)
    require(record(fixture, source)["agent"]["conversationId"] == "missing-conversation", "failure erased saved source")
    print("PASS: missing conversation retains one unverified replacement and offers a working shell")


def ssh_agent_recovery():
    with tempfile.TemporaryDirectory(prefix="m-as-") as temporary:
        fixture = SSHFixture(sys.argv[2], temporary)
        try:
            setup(fixture, ssh=True)
            source = create(fixture, "ssh-conversation")
            crash(fixture, source)
            terminal = fixture.ssh_terminal(["recover", source, "--agent"])
            with ThreadPoolExecutor(max_workers=4) as pool:
                responses = list(pool.map(lambda index: recover(fixture, source, f"ssh-race-{index}"), range(4)))
            require(all(reply.get("type") == "session.recovered" for reply in responses), responses)
            ids = {reply["sessionId"] for reply in responses}
            require(len(ids) == 1 and source not in ids, f"SSH and daemon chose different replacements: {responses}")
            replacement = ids.pop()
            (fixture.root / "allow-resume").touch()
            terminal.expect(b"AGENT_READY_ssh-conversation")
            eventually(lambda: record(fixture, replacement).get("agentResume", {}).get("conversationId") == "ssh-conversation", "SSH native identity was not acknowledged")
            terminal.send("\n")
            terminal.expect_exit()
            require(terminal.status == 0, f"SSH changed native exit status: {terminal.status}")
            require(len(list((fixture.remote / "s").glob("*/meta.json"))) == 2, "SSH created extra recovery attempts")
            print("PASS: stock OpenSSH conversation recovery shares one daemon claim and preserves native input/exit")
        except Exception:
            print((fixture.root / "daemon.log").read_text()[-5000:], file=sys.stderr)
            for terminal in fixture.terminals:
                print(terminal.drain()[-2500:], file=sys.stderr)
            raise
        finally:
            fixture.close()


def run():
    with tempfile.TemporaryDirectory(prefix="m-a-") as temporary:
        fixture = Fixture(sys.argv[1], temporary)
        try:
            setup(fixture)
            sources = [(create(fixture, identity), identity) for identity in ("conversation one", "conversation two")]
            replacements = delayed_recovery(fixture, sources)
            verify_receipts(fixture, replacements)
            missing_conversation(fixture)
            print("PASS: two exact agent identities, durable hook registration, native resume argv, and concurrent Unix/WebSocket retries")
        except Exception:
            print((fixture.root / "daemon.log").read_text()[-5000:], file=sys.stderr)
            for path in (fixture.remote / "s").glob("*/recovery.json"):
                saved = json.loads(path.read_text())
                print(path, saved.get("lines"), saved.get("agent"), file=sys.stderr)
            for path in (fixture.remote / "s").glob("*/worker.log"):
                print(path, path.read_text()[-2000:], file=sys.stderr)
            raise
        finally:
            fixture.close()
    ssh_agent_recovery()


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(run))
