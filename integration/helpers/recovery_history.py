#!/usr/bin/env python3
"""Explicit command recipes and previous attempts remain usable after exits."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
from terminal_window import run_outside_containing_session, Fixture, Terminal, PROMPT, eventually, require


def run(fixture, *args, cwd=None):
    result = subprocess.run([fixture.binary, *args], env=fixture.environment, cwd=cwd or fixture.root,
                            input=b"", stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=8)
    require(result.returncode == 0, f"{args} failed: {result.stderr!r} {result.stdout!r}")
    return result


def exercise(fixture):
    project = fixture.root / "deleted project"
    project.mkdir()
    run(fixture, "local", "--raw", "--", "/bin/sh", "-c", "printf 'completed-output\\n'", cwd=project)
    paths = list((fixture.local / "s").glob("*/meta.json"))
    require(len(paths) == 1, "fixture did not create exactly one completed session")
    source = json.loads(paths[0].read_text())
    session_id = source["id"]
    require(source["state"] == "exited", "completed program was not recorded as exited")
    # A completed command is never resumed by a window's detached-only claim.
    window = fixture.window(take=True)
    window.expect(PROMPT)
    unrelated_id, _ = fixture.shell_identity(window)
    require(unrelated_id != session_id, "--take restarted completed work")
    window.send(b"\x1d")
    window.expect_exit()
    argument = "literal space ' $(touch should-not-exist)"
    run(fixture, "recovery-command", session_id, "--", "/bin/sh", "-c", "printf 'recipe:%s\\n' \"$1\"", "--", argument, cwd=project)
    result = run(fixture, "recover", session_id, "--command", "--raw")
    require(("recipe:" + argument).encode() in result.stdout, "restart recipe changed literal argv")
    require(not (project / "should-not-exist").exists(), "recipe was reconstructed through a shell")
    link = json.loads((fixture.local / "s" / session_id / "recovery-intent.json").read_text())
    replacement_id = link["launch"]["ID"]
    replacement = fixture.metadata(replacement_id)
    require(replacement.get("recoveredFrom") == session_id, "recipe replacement lost its source")
    require((fixture.local / "s" / session_id).exists(), "recipe deleted the previous attempt")
    run(fixture, "recovery-command", replacement_id, "--", "/bin/echo", "must-not-run", cwd=project)
    run(fixture, "recovery-command", replacement_id, "--clear")
    override = json.loads((fixture.local / "s" / replacement_id / "recovery-command.json").read_text())
    require(override.get("command") is None, "--clear retained the explicit recipe")
    project.rmdir()
    failed = subprocess.run([fixture.binary, "recover", replacement_id, "--command", "--raw"],
                            env=fixture.environment, input=b"", capture_output=True, timeout=5)
    require(failed.returncode != 0, "explicit command ran from a different directory")
    restored = Terminal([fixture.binary, "recover", replacement_id, "--shell"], fixture.environment, fixture.root)
    fixture.terminals.append(restored)
    restored.expect("Saved directory")
    restored.expect(PROMPT)
    current_id, _ = fixture.shell_identity(restored)
    require(fixture.metadata(current_id)["cwd"] == str(fixture.root), "missing directory did not use nearest existing parent")
    run(fixture, "rm", session_id)
    eventually(lambda: not (fixture.local / "s" / session_id).exists(), "explicit forgetting retained source data")
    require((fixture.local / "s" / current_id).exists(), "forgetting a previous attempt deleted current work")
    print("PASS: explicit recovery recipes, history, directory fallback and forgetting")


def main():
    with tempfile.TemporaryDirectory(prefix="mesh-recovery-history-") as root:
        fixture = Fixture(sys.argv[1], root)
        try:
            exercise(fixture)
        finally:
            fixture.close()


if __name__ == "__main__":
    sys.exit(run_outside_containing_session(main))
