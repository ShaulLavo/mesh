#!/usr/bin/env python3
"""Idle agents hibernate, plain shells stay, and attach resumes the exact conversation."""

import json
import sys
import tempfile
import time

sys.dont_write_bytecode = True
from agent_recovery import create, record, recover, setup
from mesh_control import round_trip
from terminal_window import Fixture, Terminal, eventually, require, run_outside_containing_session

IDLE_SECONDS = 2


def listing(fixture):
    response = round_trip(str(fixture.remote / "daemon.sock"), {"type": "session.list", "requestId": "t27-list"})
    require(response.get("type") == "session.listed", f"listing failed: {response}")
    return {session["id"]: session for session in response.get("sessions", [])}


def meta(fixture, session_id):
    return json.loads((fixture.remote / "s" / session_id / "meta.json").read_text())


def attach_and_detach(fixture, session_id, ready):
    environment = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote)}
    terminal = Terminal([fixture.binary, "attach", session_id], environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect(ready)
    terminal.send("\x1d")
    terminal.expect(b"detached")
    eventually(lambda: meta(fixture, session_id).get("state") == "detached" and meta(fixture, session_id).get("detachedAt"),
               f"{session_id} never recorded its detach time")


def create_shell(fixture):
    response = round_trip(str(fixture.remote / "daemon.sock"), {
        "type": "session.create", "requestId": "create-shell",
        "command": ["/bin/bash", "--norc", "-i"], "cwd": str(fixture.root),
    })
    require(response.get("type") == "session.created", f"create shell failed: {response}")
    return response["sessionId"]


def hibernate(fixture, session_id, request_id):
    return round_trip(str(fixture.remote / "daemon.sock"), {
        "type": "session.hibernate", "requestId": request_id, "sessionId": session_id})


def idle_policy(fixture):
    agent = create(fixture, "idle-conversation")
    shell = create_shell(fixture)
    attach_and_detach(fixture, agent, b"AGENT_READY_idle-conversation")
    attach_and_detach(fixture, shell, b"$")

    eventually(lambda: listing(fixture)[agent].get("hibernated"), "idle agent was not hibernated", timeout=IDLE_SECONDS * 4)
    row = listing(fixture)[agent]
    require(row["state"] == "exited" and row["hibernated"]["reason"] == "idle", row)
    require(row["hibernated"]["conversationId"] == "idle-conversation" and row["hibernated"]["provider"] == "claude", row)
    saved = record(fixture, agent)["agent"]
    require(saved["lifecycle"] == "active", f"hibernation closed the conversation: {saved}")

    # The shell had the same idle time and more; it has nothing to resume, so
    # stopping it would lose work.
    time.sleep(IDLE_SECONDS)
    row = listing(fixture)[shell]
    require(row["state"] == "detached" and not row.get("hibernated"), f"plain shell was touched: {row}")
    require(row.get("memoryBytes", 0) > 0, f"live shell reported no memory: {row}")
    refused = hibernate(fixture, shell, "shell-refused")
    require(refused.get("type") == "error" and "no running agent" in refused.get("message", ""), refused)
    print("PASS: an idle detached agent hibernates with its conversation kept active; a plain shell is left running")
    return agent


def wake(fixture, source):
    reply = recover(fixture, source, "wake")
    require(reply.get("type") == "session.recovered", f"wake failed: {reply}")
    replacement = reply["sessionId"]
    require(replacement != source, reply)
    path = fixture.root / ("launch-" + replacement + ".json")
    eventually(path.exists, "waking did not start the provider")
    invocation = json.loads(path.read_text())
    require(invocation["argv"] == ["--model", "idle-conversation", "--resume=idle-conversation"], f"wrong resume: {invocation}")
    (fixture.root / "allow-resume").touch()
    eventually(lambda: record(fixture, replacement).get("agentResume", {}).get("conversationId") == "idle-conversation",
               "resumed provider did not acknowledge the exact conversation")
    rows = listing(fixture)
    require(not rows[source].get("hibernated") and rows[source]["replacementId"] == replacement,
            f"woken source still reads as hibernated: {rows[source]}")
    print("PASS: the default recovery action wakes a hibernated session into its exact conversation")
    return replacement


def explicit(fixture, replacement):
    eventually(lambda: record(fixture, replacement).get("agent", {}).get("lifecycle") == "active",
               "replacement never registered its conversation")
    reply = hibernate(fixture, replacement, "explicit")
    require(reply.get("type") == "ok" and reply.get("sessionId") == replacement, reply)
    eventually(lambda: listing(fixture)[replacement].get("hibernated"), "explicit hibernation was not listed")
    require(listing(fixture)[replacement]["hibernated"]["reason"] == "request", listing(fixture)[replacement])
    again = recover(fixture, replacement, "wake-again")
    require(again.get("type") == "session.recovered" and again["sessionId"] != replacement, again)
    print("PASS: explicit hibernation of a resumed conversation wakes again through recovery")


def run():
    with tempfile.TemporaryDirectory(prefix="m-h-") as temporary:
        fixture = Fixture(sys.argv[1], temporary)
        fixture.daemon_args = ["--hibernate-idle", f"{IDLE_SECONDS}s"]
        try:
            setup(fixture)
            agent = idle_policy(fixture)
            replacement = wake(fixture, agent)
            explicit(fixture, replacement)
        except Exception:
            print((fixture.root / "daemon.log").read_text()[-5000:], file=sys.stderr)
            for terminal in fixture.terminals:
                print(terminal.drain()[-1500:], file=sys.stderr)
            raise
        finally:
            fixture.close()


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(run))
