#!/usr/bin/env python3
"""Check metrics advice through owned native and controlled dashboard PTYs."""

import argparse
import copy
import json
import os
import socket
import subprocess
import tempfile
import threading
import time
from pathlib import Path

from dashboard import render
from isolation import check_environment, termination_cleanup
from mesh_control import round_trip
from state_watch import read, send
from terminal_window import Terminal

UPGRADE = "Update mesh for GPU, disk and temperatures"
WAITING = "Waiting for metrics…"


def capture(terminal, screen, predicate, evidence, name):
    deadline = time.monotonic() + 8
    text = ""
    while time.monotonic() < deadline:
        terminal.drain()
        text = render(screen, terminal.output, 80, 24).decode()
        if predicate(text):
            if evidence:
                evidence.mkdir(parents=True, exist_ok=True)
                (evidence / f"{name}.ansi").write_bytes(terminal.output)
                (evidence / f"{name}.txt").write_text(text + "\n")
            return text
        assert terminal.poll() is None, "dashboard exited before the expected screen"
        time.sleep(0.05)
    raise AssertionError(f"{name} never rendered:\n{text}")


class Producer:
    def __init__(self, path, info, metrics, mode):
        self.info = info
        self.metrics = copy.deepcopy(metrics)
        self.mode = mode
        self.release = threading.Event()
        self.stop = threading.Event()
        self.errors = []
        self.connections = []
        self.threads = []
        self.server = socket.socket(socket.AF_UNIX)
        self.server.bind(path)
        self.server.listen()
        self.server.settimeout(0.1)
        self.thread = threading.Thread(target=self.accept)
        self.thread.start()

    def accept(self):
        while not self.stop.is_set():
            try:
                connection, _ = self.server.accept()
            except TimeoutError:
                continue
            self.connections.append(connection)
            thread = threading.Thread(target=self.serve, args=(connection,))
            self.threads.append(thread)
            thread.start()

    def serve(self, connection):
        try:
            self.respond(connection)
        except (RuntimeError, OSError):
            pass
        except (AssertionError, KeyError, ValueError) as error:
            self.errors.append(repr(error))

    def respond(self, connection):
        while not self.stop.is_set():
            request, _ = read(connection)
            kind = request["type"]
            response = {"type": kind, "requestId": request.get("requestId", "")}
            if kind == "host.info":
                response.update(type="host.info.result", host=self.info)
            elif kind == "state.watch" and self.mode != "unsupported":
                snapshot = {"seq": 1, "host": self.info, "sessions": [], "services": [],
                            "current": {topic: {"ageMillis": 0} for topic in
                                        ("host", "sessions", "services", "metrics")}}
                if self.mode == "legacy":
                    self.metrics["performanceVersion"] = 0
                    for key in ("gpu", "disk", "network", "cores", "battery", "temperatures"):
                        self.metrics.pop(key, None)
                    snapshot["metrics"] = self.metrics
                if self.mode == "failed":
                    snapshot["current"]["metrics"]["failing"] = True
                response.update(type="state.snapshot", stateSnapshot=snapshot)
                send(connection, response)
                self.release.wait(8)
                if self.mode == "current" and not self.stop.is_set():
                    send(connection, {"type": "state.event", "stateEvent": {
                        "seq": 2, "kind": "metrics", "payload": {"metrics": self.metrics}}})
                self.stop.wait(8)
                return
            elif kind in ("state.watch", "host.metrics"):
                response.update(type="error", message=f'daemon: unknown control "{kind}"')
            elif kind == "session.list":
                response.update(type="session.listed", sessions=[])
            elif kind == "service.list":
                response.update(type="service.listed", services=[])
            else:
                raise AssertionError(f"unexpected fixture request {kind}")
            send(connection, response)

    def close(self):
        self.stop.set()
        self.release.set()
        self.thread.join(timeout=2)
        self.server.close()
        for connection in self.connections:
            try:
                connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            connection.close()
        for thread in self.threads:
            thread.join(timeout=2)
        assert not self.thread.is_alive() and all(not thread.is_alive() for thread in self.threads)
        assert not self.errors, self.errors


def prove(binary, screen, evidence):
    check_environment()
    with tempfile.TemporaryDirectory(prefix="dashboard-metrics-", dir=os.environ.get("TMPDIR")) as temporary:
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
        path = str(state / "daemon.sock")
        with (root / "daemon.log").open("wb") as log:
            daemon = subprocess.Popen([binary, "daemon", "--tailnet-port", "0", "--ssh-port", "0"],
                                      env=environment, stdout=log, stderr=log)
            terminal = None
            try:
                deadline = time.monotonic() + 8
                while not Path(path).exists():
                    assert daemon.poll() is None and time.monotonic() < deadline
                    time.sleep(0.05)
                info = round_trip(path, {"type": "host.info", "requestId": "fixture-info"})["host"]
                terminal = Terminal([binary, "dashboard", "--wall"], environment, root)
                capture(terminal, screen, lambda text: "CPU " in text and "DISK" in text,
                        evidence, "native-current")
                metrics = round_trip(path, {"type": "host.metrics", "requestId": "fixture-metrics"})["metrics"]
                assert metrics["performanceVersion"] > 0
            finally:
                if terminal:
                    terminal.close()
                daemon.terminate()
                daemon.wait(timeout=8)
        Path(path).unlink(missing_ok=True)
        for mode in ("legacy", "unsupported", "current", "failed"):
            producer = Producer(path, info, metrics, mode)
            terminal = Terminal([binary, "dashboard", "--wall"], environment, root)
            try:
                text = capture(terminal, screen, lambda text: "reachable 1" in text and
                               "sessions 0 live" in text, evidence, f"{mode}-initial")
                if mode == "current":
                    assert UPGRADE not in text, f"current producer startup asked for an upgrade:\n{text}"
                    assert WAITING in text and "CPU pending" in text, text
                    producer.release.set()
                    text = capture(terminal, screen, lambda text: "DISK" in text,
                                   evidence, "current-received")
                    assert WAITING not in text and UPGRADE not in text, text
                elif mode == "legacy":
                    assert UPGRADE in text and WAITING not in text, text
                elif mode == "unsupported":
                    text = capture(terminal, screen, lambda text: "needs mesh v0.1.114+" in text,
                                   evidence, "unsupported-confirmed")
                    assert WAITING not in text and "CPU pending" not in text, text
                else:
                    assert "Metrics unavailable" in text and WAITING not in text and UPGRADE not in text, text
            finally:
                terminal.close()
                producer.close()
                Path(path).unlink(missing_ok=True)
        print(json.dumps({"passed": ["native-current", "current-pending", "current-received",
                                     "legacy", "unsupported", "failed"], "columns": 80, "rows": 24}))


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
