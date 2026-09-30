from pathlib import Path
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "integration" / "helpers"))
import shell_recovery
from terminal_window import Terminal

original_expect = Terminal.expect
seen = set()


def diagnose(terminal, label):
    command = (
        "printf '\\nSNAPSHOT " + label + "\\n'; "
        "print -r -- HOME=$HOME ZDOTDIR=$ZDOTDIR SHELL=$SHELL PATH=$PATH; "
        "printf 'umask='; umask; "
        "print -r -- GLOBAL_RCS=$options[globalrcs]; "
        "for mesh_dir in $fpath; do print -r -- FPATH=$mesh_dir; "
        "stat -c '%a %U:%G %n' -- $mesh_dir ${mesh_dir:h}; done; "
        "autoload -Uz compaudit; print -r -- COMPAUDIT; compaudit; "
        "printf '\\n__DIAG_DONE__\\n'\n"
    )
    start = len(terminal.drain())
    terminal.send(command)
    original_expect(terminal, "__DIAG_DONE__\r\n", since=start)
    print(terminal.drain()[start:].decode(errors="replace"), flush=True)


def expect(terminal, marker, since=0):
    try:
        result = original_expect(terminal, marker, since)
    except RuntimeError:
        output = terminal.drain()
        print("FAILING RECOVERY OUTPUT", output.decode(errors="replace"), flush=True)
        if b"abort compinit" in output:
            terminal.send("n\n")
            original_expect(terminal, "RECOVERY_PROMPT> ")
            diagnose(terminal, "restored-after-declining-compinit")
        raise
    if marker == "RECOVERY_PROMPT> " and terminal.pid not in seen:
        seen.add(terminal.pid)
        diagnose(terminal, "original" if len(seen) == 1 else "restored")
    return result


Terminal.expect = expect
shell_recovery.exercise(str(Path(sys.argv[1]).resolve()), "/usr/bin/zsh", "zsh")
