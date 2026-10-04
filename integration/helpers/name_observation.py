#!/usr/bin/env python3
"""Observe unchanged owner names through native watch frames and a dashboard PTY."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time

from dashboard import render
from isolation import check_environment, termination_cleanup
from machine_naming import read_watch, watch
from mesh_control import round_trip
from terminal_window import Terminal


def save(directory, stage, terminal, screen, frames):
    lines = render(screen, terminal.output, 80, 24).decode()
    if directory:
        directory.mkdir(parents=True, exist_ok=True)
        (directory / f"{stage}.ansi").write_bytes(terminal.output)
        (directory / f"{stage}.txt").write_text(lines + "\n")
        (directory / f"{stage}-watch.json").write_text(json.dumps(frames, indent=2) + "\n")
    return lines


def current(message):
    if message["type"] == "state.snapshot":
        return message["stateSnapshot"]["current"]["host"]
    return message.get("stateCurrent", {}).get("sections", {}).get("host")


def dashboard_until(terminal, predicate, screen, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        terminal.drain()
        if predicate(render(screen, terminal.output, 80, 24)):
            return
        assert terminal.poll() is None, "fixture dashboard exited before the expected screen"
        time.sleep(0.1)
    raise AssertionError("fixture dashboard did not render the expected screen within its deadline")


def prove(binary, screen, evidence):
    check_environment()
    if evidence:
        evidence.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="name-observation-", dir=os.environ.get("TMPDIR")) as temporary:
        root = Path(temporary)
        state = root / "state"
        tools = root / "bin"
        tools.mkdir()
        tailscale = tools / "tailscale"
        tailscale.write_text("#!/bin/sh\nexit 1\n")
        tailscale.chmod(0o700)
        environment = dict(os.environ, MESH_STATE_DIR=str(state),
                           MESH_CONFIG_DIR=str(root / "config"), TERM="xterm-256color",
                           PATH=str(tools) + os.pathsep + os.environ["PATH"])
        socket = str(state / "daemon.sock")
        with (root / "daemon.log").open("wb") as log:
            daemon = subprocess.Popen([binary, "daemon", "--tailnet-port", "0", "--ssh-port", "0"],
                                      env=environment, stdout=log, stderr=log)
            terminal = None
            connection = None
            stopped = False
            try:
                deadline = time.monotonic() + 8
                while not Path(socket).exists():
                    if daemon.poll() is not None or time.monotonic() >= deadline:
                        raise AssertionError("fixture daemon did not create its socket")
                    time.sleep(0.05)
                info = round_trip(socket, {"type": "host.info", "requestId": "fixture-info"})["host"]
                renamed = round_trip(socket, {"type": "host.rename", "requestId": "fixture-rename",
                                              "rename": {"targetId": info["id"], "machineName": "fixture-owner",
                                                         "expectedRevision": info["nameRevision"]}})
                assert renamed["type"] == "host.renamed", renamed.get("type")
                declaration = renamed["host"]
                durable = (state / "machine-name.json").read_bytes()
                # The Unix socket and state directory are owned by this disposable daemon.
                fixture = argparse.Namespace(remote=state)
                connection, initial = watch(fixture)
                connection.settimeout(12)
                frames = [initial]
                assert current(initial)["ageMillis"] < 30000 and not current(initial).get("failing", False)
                terminal = Terminal([binary, "dashboard", "--wall"], environment, root)
                dashboard_until(terminal, lambda data: b"fixture-owner" in data and b"reachable 1" in data
                                and b"last known name" not in data, screen)
                fresh = save(evidence, "known-good", terminal, screen, frames)
                assert "last known name" not in fresh, "fresh owner name is not observable"
                started = time.monotonic()
                while time.monotonic() - started < 32:
                    message = read_watch(connection)
                    frames.append(message)
                    if message.get("stateEvent", {}).get("payload", {}).get("host"):
                        raise AssertionError("unchanged declaration manufactured a rename event")
                assert (state / "machine-name.json").read_bytes() == durable, "observation changed durable name/revision"
                dashboard_until(terminal, lambda data: b"fixture-owner" in data, screen)
                unchanged = save(evidence, "unchanged-past-threshold", terminal, screen, frames)
                observation = current(frames[-1])
                assert observation and observation["ageMillis"] < 30000 and not observation.get("failing", False), observation
                assert "last known name" not in unchanged, "healthy unchanged owner observation became retained"
                daemon.send_signal(signal.SIGSTOP)
                stopped = True
                dashboard_until(terminal, lambda data: b"last known name" in data, screen, timeout=38)
                retained = save(evidence, "source-paused-retained", terminal, screen, frames)
                assert "last known name" in retained
                daemon.send_signal(signal.SIGCONT)
                stopped = False
                dashboard_until(terminal, lambda data: b"fixture-owner" in data and b"last known name" not in data,
                                screen, timeout=15)
                renewed = save(evidence, "source-resumed-fresh", terminal, screen, frames)
                assert "last known name" not in renewed
                daemon.terminate()
                daemon.wait(timeout=8)
                dashboard_until(terminal, lambda data: b"last known name" in data, screen)
                save(evidence, "disconnected-retained", terminal, screen, frames)
                assert (state / "machine-name.json").read_bytes() == durable
                result = {"platform": os.uname().sysname, "transport": "fixture-owned Unix socket",
                          "unchangedSeconds": time.monotonic() - started,
                          "durableSHA256": hashlib.sha256(durable).hexdigest(),
                          "nameRevision": declaration["nameRevision"], "watchFrames": len(frames),
                          "lastHealthyHostObservation": observation,
                          "passed": ["known-good", "unchanged-past-threshold", "source-paused-retained",
                                     "source-resumed-fresh", "disconnected-retained"]}
                if evidence:
                    (evidence / "result.json").write_text(json.dumps(result, indent=2) + "\n")
                print(json.dumps(result, sort_keys=True))
            finally:
                if connection:
                    connection.close()
                if terminal:
                    terminal.drain()
                    save(evidence, "final", terminal, screen, frames)
                    terminal.close()
                if stopped:
                    daemon.send_signal(signal.SIGCONT)
                if daemon.poll() is None:
                    daemon.terminate()
                    daemon.wait(timeout=8)
                if evidence:
                    (evidence / "daemon.txt").write_bytes((root / "daemon.log").read_bytes())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("binary")
    parser.add_argument("--screen", required=True)
    parser.add_argument("--evidence-dir", type=Path)
    args = parser.parse_args()
    with termination_cleanup():
        prove(args.binary, args.screen, args.evidence_dir)


if __name__ == "__main__":
    main()
