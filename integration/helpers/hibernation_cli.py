#!/usr/bin/env python3
"""Hibernate agents from the CLI, list them, wake them by ID, and reclaim with gc."""

import json
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True
from agent_recovery import create, setup
from mesh_control import round_trip
from terminal_window import Fixture, Terminal, eventually, require, run_outside_containing_session


def cli(fixture, environment, *arguments):
    return subprocess.run([fixture.binary, *arguments], env=environment, stdin=subprocess.DEVNULL,
                          capture_output=True, text=True, timeout=15)


def meta(fixture, session_id):
    return json.loads((fixture.remote / "s" / session_id / "meta.json").read_text())


def listing_row(listing, session_id):
    for line in listing.splitlines():
        fields = line.split()
        if fields and fields[0] == session_id:
            return fields
    raise RuntimeError(f"mesh ls omitted {session_id}:\n{listing}")


def create_shell(fixture):
    response = round_trip(str(fixture.remote / "daemon.sock"), {
        "type": "session.create", "requestId": "create-shell",
        "command": ["/bin/sh", "-c", "exec sleep 300"], "cwd": str(fixture.root),
    })
    require(response.get("type") == "session.created", f"shell create failed: {response}")
    return response["sessionId"]


def detached(fixture, session_id):
    current = meta(fixture, session_id)
    return current["state"] == "detached" and current.get("detachedAt")


def visit(fixture, environment, session_id):
    # Idle is measured from the last detach, and a session nobody ever
    # attached has none, so attach once and leave.
    terminal = Terminal([fixture.binary, "attach", session_id], environment, fixture.root)
    fixture.terminals.append(terminal)
    eventually(lambda: meta(fixture, session_id)["state"] == "running" and meta(fixture, session_id).get("lastAttachedAt"),
               f"{session_id} did not record the attachment")
    terminal.send(b"\x1d")
    terminal.expect_exit()
    eventually(lambda: detached(fixture, session_id), f"{session_id} did not record when it was detached")


def hibernated(fixture, session_id):
    return (fixture.remote / "s" / session_id / "hibernation.json").exists() and meta(fixture, session_id)["state"] == "exited"


def run_checks(fixture):
    setup(fixture)
    local_config = fixture.root / "local-config"
    local_config.mkdir()
    # The CLI reads the daemon host's own state, as on that host itself.
    on_host = fixture.environment | {"MESH_STATE_DIR": str(fixture.remote), "MESH_CONFIG_DIR": str(local_config)}

    shell = create_shell(fixture)
    visit(fixture, on_host, shell)
    refused = cli(fixture, on_host, "hibernate", shell)
    require(refused.returncode != 0 and f"session {shell}: no running agent conversation is registered in this session" in refused.stderr,
            f"plain shell hibernation: {refused}")
    require(meta(fixture, shell)["state"] == "detached", "a refused hibernation stopped the shell")

    agent = create(fixture, "sleepy")
    visit(fixture, on_host, agent)
    result = cli(fixture, on_host, "hibernate", agent)
    require(result.returncode == 0 and result.stdout.strip() == f"hibernated {agent}", f"hibernate: {result}")
    eventually(lambda: hibernated(fixture, agent), "hibernated agent left no marker or kept running")

    listing = cli(fixture, on_host, "ls")
    require(listing.returncode == 0, f"ls failed: {listing}")
    require(listing.stdout.splitlines()[0].split()[:5] == ["ID", "STATE", "AGE", "IDLE", "MEM"], listing.stdout)
    require(listing_row(listing.stdout, agent)[1] == "hibernated", listing.stdout)
    shell_row = listing_row(listing.stdout, shell)
    require(shell_row[1] == "detached" and shell_row[4] != "-", f"live shell has no memory figure:\n{listing.stdout}")

    # Through the adopted-host path the daemon forwards the request.
    remote_agent = create(fixture, "remote-sleepy")
    visit(fixture, on_host, remote_agent)
    result = cli(fixture, fixture.environment, "hibernate", remote_agent)
    require(result.returncode == 0 and f"hibernated {remote_agent}" in result.stdout, f"remote hibernate: {result}")
    eventually(lambda: hibernated(fixture, remote_agent), "remote hibernation did not stop the agent")

    terminal = Terminal([fixture.binary, agent], on_host, fixture.root, timeout=8)
    fixture.terminals.append(terminal)
    terminal.expect(b"resuming hibernated claude conversation")
    (fixture.root / "allow-resume").touch()
    terminal.expect(b"AGENT_READY_sleepy")
    resumed = [json.loads(path.read_text()) for path in fixture.root.glob("launch-*.json")]
    require(any(entry["id"] == "sleepy" and "--resume=sleepy" in entry["argv"] for entry in resumed), f"wake did not resume the conversation: {resumed}")
    listing = cli(fixture, on_host, "ls", "--all").stdout
    require(listing_row(listing, agent)[1] == "exited", f"a woken session still reads as hibernated:\n{listing}")

    idle_agent = create(fixture, "idle-agent")
    visit(fixture, on_host, idle_agent)
    time.sleep(1.2)
    plan = cli(fixture, on_host, "gc", "--idle", "1s")
    require(plan.returncode == 0, f"gc plan failed: {plan}")
    require(any(line.split()[:3] == ["this", "host", idle_agent] and "hibernate" in line for line in plan.stdout.splitlines()), plan.stdout)
    require(any(shell in line and "left running (plain shell)" in line for line in plan.stdout.splitlines()), plan.stdout)
    require("reclaimable:" in plan.stdout and meta(fixture, idle_agent)["state"] == "detached", f"gc acted without --yes:\n{plan.stdout}")

    applied = cli(fixture, on_host, "gc", "--idle", "1s", "--shells", "--yes")
    require(applied.returncode == 0, f"gc --yes failed: {applied}")
    require(f"hibernated {idle_agent} on this host" in applied.stdout and f"killed {shell} on this host" in applied.stdout, applied.stdout)
    eventually(lambda: hibernated(fixture, idle_agent), "gc did not hibernate the idle agent")
    require(meta(fixture, shell)["state"] == "exited", "gc --shells left the idle shell running")
    require(terminal.poll() is None, "gc touched the attached, woken session")
    terminal.send("\n")
    terminal.expect_exit()
    print("PASS: hibernate refuses shells, ls shows hibernated rows and memory, attach by ID resumes the conversation, gc plans before it acts")


def run():
    with tempfile.TemporaryDirectory(prefix="m-h-") as temporary:
        fixture = Fixture(sys.argv[1], temporary)
        try:
            run_checks(fixture)
        except Exception:
            print((fixture.root / "daemon.log").read_text()[-5000:], file=sys.stderr)
            for terminal in fixture.terminals:
                print(terminal.drain()[-2500:], file=sys.stderr)
            raise
        finally:
            fixture.close()


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(run))
