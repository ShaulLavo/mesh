#!/usr/bin/env python3
"""Exercise prompt composition, saved directories and history through real PTYs."""

import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
from terminal_window import run_outside_containing_session, Fixture, Terminal, eventually, require


def checkpoint(fixture, session_id):
    path = fixture.local / "s" / session_id / "recovery.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text())


def exercise(binary, shell, mode):
    with tempfile.TemporaryDirectory(prefix="mesh-shell-recovery-") as directory:
        fixture = Fixture(binary, directory)
        try:
            run_shell(fixture, shell, mode)
        finally:
            fixture.close()


def run_shell(fixture, shell, mode):
    project = fixture.root / "project with spaces"
    project.mkdir()
    history = fixture.root / "history"
    kind = "zsh" if mode == "zsh" else "bash"
    snippet = subprocess.check_output([fixture.binary, "shell-init", kind], env=fixture.environment).decode()
    common = "PS1='RECOVERY_PROMPT> '\nHISTFILE=" + shlex.quote(str(history)) + "\n"
    if mode == "zsh":
        setup = common + "SAVEHIST=1000\nHISTSIZE=1000\nsetopt HIST_IGNORE_SPACE\n"
        setup += "old_prompt() { print -r -- __STATUS__${?}; }\nprecmd_functions=(old_prompt)\n"
    else:
        setup = common + "HISTSIZE=1000\nHISTCONTROL=ignorespace\n"
        hook = "printf '__STATUS__%s\\n' \"$?\""
        setup += "PROMPT_COMMAND=" + ("(" + shlex.quote(hook) + ")" if mode == "array" else shlex.quote(hook)) + "\n"
    rc = fixture.root / (".zshrc" if mode == "zsh" else ".bashrc")
    rc.write_text(setup + snippet + snippet)
    fixture.environment.update({"HOME": str(fixture.root), "ZDOTDIR": str(fixture.root), "SHELL": shell,
                                "PATH": str(Path(shell).parent) + os.pathsep + fixture.environment["PATH"]})
    argv = [shell, "-d", "-i"] if mode == "zsh" else [shell, "--noprofile", "--rcfile", str(rc), "-i"]
    terminal = Terminal([fixture.binary, "local", "--"] + argv, fixture.environment, fixture.root)
    fixture.terminals.append(terminal)
    terminal.expect("RECOVERY_PROMPT> ")
    session_id, shell_pid = fixture.shell_identity(terminal)
    start = len(terminal.drain())
    terminal.send("false\n")
    terminal.expect("__STATUS__1", since=start)
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    start = len(terminal.drain())
    terminal.send("cd " + shlex.quote(str(project)) + "\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    eventually(lambda: checkpoint(fixture, session_id).get("shellDirectory") == str(project), "prompt did not save shell directory")
    terminal.send(" echo excluded_from_history\n")
    terminal.expect("excluded_from_history")
    start = len(terminal.drain())
    terminal.send("printf 'retained-work-output\\n'\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    eventually(lambda: history.exists() and "retained-work-output" in history.read_text(), "completed command was not appended to history")
    require("excluded_from_history" not in history.read_text(), "prompt hook bypassed history exclusion")
    eventually(lambda: any("retained-work-output" in line for line in checkpoint(fixture, session_id).get("lines", [])), "rendered checkpoint omitted completed output")
    require(checkpoint(fixture, session_id)["directorySource"] == "shell", "shell directory was not authoritative")
    # An interactive child shell is not the registered session leader.
    nested = [shell, "-d", "-i"] if mode == "zsh" else [shell, "--noprofile", "--rcfile", str(rc), "-i"]
    start = len(terminal.drain())
    terminal.send(shlex.join(nested) + "\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    start = len(terminal.drain())
    terminal.send("cd /\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    require(checkpoint(fixture, session_id)["shellDirectory"] == str(project), "subshell replaced the registered shell directory")
    start = len(terminal.drain())
    terminal.send("exit\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    start = len(terminal.drain())
    terminal.send("for mesh_i in {1..50}; do printf 'later-line-%s\\n' \"$mesh_i\"; done\n")
    terminal.expect("RECOVERY_PROMPT> ", since=start)
    eventually(lambda: any("later-line-50" in line for line in checkpoint(fixture, session_id).get("lines", [])), "final checkpoint did not retain completed output")
    fixture.crash_worker(terminal, session_id, shell_pid)
    previous = subprocess.check_output([fixture.binary, "logs", session_id, "--previous"], env=fixture.environment)
    require(b"Previous output" in previous and b"retained-work-output" in previous, "full saved output is inaccessible after worker crash")
    restored = Terminal([fixture.binary, "recover", session_id], fixture.environment, fixture.root)
    fixture.terminals.append(restored)
    restored.expect("RECOVERY_PROMPT> ")
    replacement_id, _ = fixture.shell_identity(restored)
    metadata = fixture.metadata(replacement_id)
    require(metadata.get("recoveredFrom") == session_id, "recovered shell has no previous-attempt link")
    require(metadata["cwd"] == str(project), "recovered shell opened in the wrong directory")
    require((fixture.local / "s" / session_id / "recovery.json").exists(), "recovery deleted the saved output")
    start = len(restored.drain())
    restored.send("history\n")
    restored.expect("retained-work-output", since=start)
    print("PASS: shell recovery " + mode)


def main():
    binary = str(Path(sys.argv[1]).resolve())
    bash = shutil.which("bash")
    require(bash is not None, "bash is required")
    for mode in ("scalar", "array"):
        exercise(binary, bash, mode)
    zsh = os.environ.get("MESH_TEST_ZSH") or shutil.which("zsh")
    if zsh:
        exercise(binary, zsh, "zsh")
    else:
        print("SKIP: zsh is not installed; set MESH_TEST_ZSH to exercise it")


if __name__ == "__main__":
    sys.exit(run_outside_containing_session(main))
