#!/usr/bin/env python3
"""Lean lists, legacy requests, explicit previews, and optional old-binary peers."""

import json
import socket
import struct
import subprocess
import sys
import tempfile
from datetime import UTC, datetime, timedelta
from pathlib import Path

from mesh_control import receive, round_trip
from terminal_window import Fixture, Terminal, eventually, require


def listing(fixture, lean=None):
    request = {"type": "session.list", "requestId": "lean-proof"}
    if lean is not None:
        request["lean"] = lean
    payload = json.dumps(request).encode()
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(10)
        connection.connect(str(fixture.remote / "daemon.sock"))
        connection.sendall(struct.pack(">BI", 1, len(payload)) + payload)
        kind, size = struct.unpack(">BI", receive(connection, 5))
        require(kind == 1 and size <= 4 << 20, "invalid catalog frame")
        return json.loads(receive(connection, size)), size


def seed(fixture, index):
    alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
    session_id = "000" + alphabet[index]
    directory = fixture.remote / "s" / session_id
    directory.mkdir(parents=True)
    timestamp = (
        (datetime.now(UTC) - timedelta(days=1))
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )
    (directory / "meta.json").write_text(
        json.dumps(
            {
                "id": session_id,
                "pid": 1,
                "command": ["/bin/sh"],
                "cwd": "/tmp",
                "state": "exited",
                "createdAt": timestamp,
                "exitedAt": timestamp,
                "exitCode": 0,
            }
        )
    )
    (directory / "recovery.json").write_text(
        json.dumps(
            {
                "version": 1,
                "hostId": fixture.remote_id,
                "sessionId": session_id,
                "checkpointAt": timestamp,
                "lastOutputAt": timestamp,
                "shell": "/bin/sh",
                "shellDirectory": "/tmp",
                "directorySource": "shell",
                "title": "Lean preview proof",
                "command": ["/bin/sh"],
                "lines": [
                    f"SAVED-SCREEN-{line:02d} ".ljust(160, "x") for line in range(24)
                ],
            }
        )
    )
    return session_id


def settled(fixture, count):
    def check():
        response, size = listing(fixture)
        return (response, size) if len(response.get("sessions", [])) == count else None

    try:
        return eventually(
            check, f"catalog did not settle at {count} sessions", timeout=8
        )
    except RuntimeError as error:
        raise RuntimeError(
            f"{error}; response={listing(fixture)[0]!r}; daemon={Path(fixture.root / 'daemon.log').read_text()}"
        ) from error


def check_picker(fixture, binary):
    terminal = Terminal([binary], fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect("pc")
    terminal.send("\x1b[B\r")
    terminal.expect("Previous output")
    terminal.expect("SAVED-SCREEN-")
    start = len(terminal.drain())
    terminal.send(" ")
    terminal.expect("SAVED-SCREEN-10", since=start)
    print("PICKER: selected and expanded saved screen visible", flush=True)
    terminal.send("\x1b\x1bq")
    terminal.close()
    fixture.terminals.remove(terminal)


def cli_listing(fixture, binary):
    result = subprocess.run(
        [binary, "ls", "--all"],
        env=fixture.environment,
        stdin=subprocess.DEVNULL,
        capture_output=True,
        timeout=10,
        check=False,
    )
    require(result.returncode == 0, f"list failed: {result.stderr!r}")
    return result.stdout


def check_new_daemon(binary, baseline):
    with tempfile.TemporaryDirectory(prefix="mesh-lean-") as temporary:
        fixture = Fixture(binary, temporary)
        fixture.daemon_args = ["--app-data-root", str(fixture.root / "apps")]
        try:
            fixture.start_remote()
            for index in range(20):
                session_id = seed(fixture, index)
                if index + 1 not in (1, 5, 20):
                    continue
                full, full_bytes = settled(fixture, index + 1)
                explicit_full, _ = listing(fixture, False)
                lean, lean_bytes = listing(fixture, True)
                require(
                    full == explicit_full, "absent option changed the legacy response"
                )
                require(
                    len(lean["sessions"]) == index + 1, "lean catalog lost a session"
                )
                for row in lean["sessions"]:
                    require(
                        row.get("recoveryDetailsOmitted"),
                        "lean row lost its omission marker",
                    )
                    require(
                        not row["recovery"].get("lines")
                        and not row["recovery"].get("command"),
                        "lean row retained screen or launch argv",
                    )
                saved = round_trip(
                    str(fixture.remote / "daemon.sock"),
                    {
                        "type": "session.inspect",
                        "requestId": "preview-proof",
                        "sessionId": session_id,
                        "previewCols": 80,
                        "previewRows": 8,
                    },
                )
                original = next(
                    row for row in full["sessions"] if row["id"] == session_id
                )
                require(
                    saved.get("recovery") == original["recovery"],
                    "explicit saved inspection changed the preview",
                )
                print(
                    f"PAYLOAD sessions={index + 1} full={full_bytes} lean={lean_bytes}"
                )
                if index == 0:
                    check_picker(fixture, binary)
            if baseline:
                require(
                    cli_listing(fixture, baseline) == cli_listing(fixture, binary),
                    "old client against new daemon changed ls output",
                )
                check_picker(fixture, baseline)
            print(
                "PASS: legacy request, lean catalogs, selected saved previews, and picker"
            )
        finally:
            fixture.close()


def check_old_daemon(binary, baseline):
    with tempfile.TemporaryDirectory(prefix="mesh-lean-old-") as temporary:
        fixture = Fixture(baseline, temporary)
        fixture.daemon_args = ["--app-data-root", str(fixture.root / "apps")]
        try:
            fixture.start_remote()
            seed(fixture, 0)
            full, _ = settled(fixture, 1)
            ignored, _ = listing(fixture, True)
            require(
                full == ignored, "baseline daemon did not ignore the additive option"
            )
            require(
                cli_listing(fixture, binary) == cli_listing(fixture, baseline),
                "new client against old daemon changed ls output",
            )
            check_picker(fixture, binary)
            print(
                "PASS: actual old daemon ignores lean; new client lists and previews it"
            )
        finally:
            fixture.close()


def main():
    binary = str(Path(sys.argv[1]).resolve())
    baseline = str(Path(sys.argv[2]).resolve()) if len(sys.argv) > 2 else None
    check_new_daemon(binary, baseline)
    if baseline:
        check_old_daemon(binary, baseline)


if __name__ == "__main__":
    main()
