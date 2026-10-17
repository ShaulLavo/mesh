#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
root=$(mktemp -d "${TMPDIR:-/tmp}/mesh-terminal-definitions.XXXXXX")
trap 'rm -rf -- "$root"' EXIT
mesh=${MESH_INTEGRATION_BINARY:-$root/mesh}
if [[ -z ${MESH_INTEGRATION_BINARY:-} ]]; then
  (cd "$repo_root" && go build -o "$mesh" ./cmd/mesh)
fi
python3 - "$mesh" "$repo_root/integration/helpers" <<'PY'
import os
from pathlib import Path
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
sys.path.insert(0, sys.argv[2])
from terminal_window import Fixture, Terminal, PROMPT, eventually, require

with tempfile.TemporaryDirectory(prefix="mesh-terminal-definitions-") as directory:
    fixture = Fixture(sys.argv[1], Path(directory))
    try:
        fixture.start_remote()
        source = fixture.root / "client-terminfo"
        source.mkdir()
        subprocess.run(["tic", "-x", "-o", str(source), "-"], check=True,
                       input=b"mesh-fixture|Mesh fixture terminal,\n am, cols#80, lines#24, clear=\\E[H\\E[2J, setrgbf=\\E[38:2:%p1%d:%p2%d:%p3%dm,\n")
        client = fixture.environment | {"TERM": "mesh-fixture", "TERMINFO": str(source)}
        remote = fixture.environment | {"TERM": "mesh-fixture"}
        require(subprocess.run(["infocmp", "mesh-fixture"], env=remote, capture_output=True).returncode != 0,
                "remote already had the client definition")
        original = Terminal([fixture.binary, "pc"], client, fixture.root)
        fixture.terminals.append(original)
        original.expect(PROMPT)
        session_id, shell_pid = fixture.shell_identity(original)
        original.send("clear; printf 'FIRST_CLEAR_%s\\n' \"$?\"\n")
        original.expect(b"FIRST_CLEAR_0")
        require(b"unknown terminal" not in original.drain(), "fresh remote shell could not clear")
        definition = subprocess.run(["infocmp", "-x", "mesh-fixture"], env=remote, capture_output=True, check=True)
        require(b"setrgbf=" in definition.stdout, "extended terminal capabilities were lost")
        original.send(b"\x1d")
        original.expect_exit()
        for entry in (Path(remote["HOME"]) / ".terminfo").glob("*/mesh-fixture"):
            entry.unlink()
        require(subprocess.run(["infocmp", "mesh-fixture"], env=remote, capture_output=True).returncode != 0,
                "remote definition was not removed before attachment")
        resumed = Terminal([fixture.binary, session_id], client, fixture.root)
        fixture.terminals.append(resumed)
        eventually(lambda: fixture.attached(fixture.remote, session_id), "retained shell was not reattached")
        require(fixture.shell_identity(resumed) == (session_id, shell_pid), "definition repair replaced the shell")
        resumed.send("clear; printf 'RESUMED_CLEAR_%s\\n' \"$?\"\n")
        resumed.expect(b"RESUMED_CLEAR_0")
        require(b"unknown terminal" not in resumed.drain(), "attachment did not repair the retained terminal")
        print("PASS: fresh remote creation and retained-session attachment install the caller's terminal and extended capabilities; clear succeeds; shell survives")
    finally:
        fixture.close()
PY
