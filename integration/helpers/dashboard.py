#!/usr/bin/env python3
"""Exercise the passive dashboard on an owned PTY and isolated daemon."""

import argparse
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import subprocess
import termios
import time

from state_watch import connect, watch


def resize(master, process, columns, rows):
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))
    process.send_signal(signal.SIGWINCH)


def collect(master, process, predicate, timeout, screen=None, columns=80, rows=24):
    output = bytearray()
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 0.1)
        if ready:
            try:
                value = os.read(master, 65536)
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                break
            if not value:
                break
            output.extend(value)
            assert len(output) <= 1 << 20, "terminal output exceeded fixture bound"
            if predicate(render(screen, output, columns, rows)):
                return bytes(output)
        if process.poll() is not None:
            break
    assert predicate(render(screen, output, columns, rows)), (process.poll(), output.decode(errors="replace"))
    return bytes(output)


def render(screen, output, columns, rows):
    if not screen:
        return output
    lines = json.loads(subprocess.check_output([screen, str(columns), str(rows)], input=bytes(output)))
    return "\n".join(lines).encode()


def released(socket):
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        connection, initial = watch(socket, ["sessions", "services"])
        connection.close()
        if initial["type"] == "state.snapshot":
            return
        assert initial.get("errorCode") == "state.subscriber_limit", initial
        time.sleep(0.05)
    raise AssertionError("dashboard left its subscriber reserved")


def exercise(binary, socket, stop_signal, evidence, screen):
    master, slave = pty.openpty()
    before = termios.tcgetattr(slave)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    environment = dict(os.environ, TERM="xterm-256color")
    process = subprocess.Popen([binary, "dashboard", "--wall"], stdin=subprocess.DEVNULL,
                               stdout=slave, stderr=slave, env=environment, start_new_session=True)
    try:
        output = collect(master, process,
                         lambda data: bool(re.search(rb"CPU [0-9]+%.*RAM [0-9.]+\s*/\s*[0-9.]+", data)), 8, screen)
        assert b"\x1b[?1049h" in output, "fullscreen view never entered alternate screen"
        connection, initial = watch(socket, ["sessions"])
        connection.close()
        assert initial.get("errorCode") == "state.subscriber_limit", "dashboard never subscribed"
        memory = {}
        rollup = Path(f"/proc/{process.pid}/smaps_rollup")
        if rollup.exists():
            for line in rollup.read_text().splitlines():
                field = line.split()
                if field[0] in ("Rss:", "Pss:"):
                    memory[field[0][:-1] + "KiB"] = int(field[1])
        if evidence:
            evidence.mkdir(parents=True, exist_ok=True)
            (evidence / f"{stop_signal.name}-80x24.ansi").write_bytes(output)
        resize(master, process, 50, 12)
        output += collect(master, process, lambda data: b"current 50" in data, 3, screen, 50, 12)
        resize(master, process, 160, 48)
        large = collect(master, process, lambda data: b"Hosts 1 / 1 visible" in data, 3, screen, 160, 48)
        output += large
        if evidence:
            (evidence / f"{stop_signal.name}-160x48.ansi").write_bytes(large)
        process.send_signal(stop_signal)
        output += collect(master, process, lambda data: b"\x1b[?1049l" in data, 3)
        assert process.wait(timeout=3) == 0, output.decode(errors="replace")
        assert termios.tcgetattr(slave) == before, "dashboard changed terminal attributes"
        released(socket)
        return {"signal": stop_signal.name, "inputBytes": 0, "outputBytes": len(output),
                "subscriberReleased": True, "terminalRestored": True, **memory}
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)
        os.close(slave)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("binary")
    parser.add_argument("socket")
    parser.add_argument("--evidence-dir", type=Path)
    parser.add_argument("--screen", required=True)
    args = parser.parse_args()
    connection = connect(args.socket)
    connection.close()
    results = [exercise(args.binary, args.socket, stop, args.evidence_dir, args.screen)
               for stop in (signal.SIGTERM, signal.SIGINT)]
    print(json.dumps({"dashboardPTY": results}, sort_keys=True))


if __name__ == "__main__":
    main()
