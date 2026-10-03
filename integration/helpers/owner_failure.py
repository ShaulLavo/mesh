#!/usr/bin/env python3
import ast
from pathlib import Path
import re
import sys


GO_QUOTED = re.compile(r'"(?:[^"\\\x00-\x1f\x7f]|\\(?:[abfnrtv"\\]|x[0-9a-fA-F]{2}|u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}))*"')


def escaped_owner_output(log, reference, marker):
    prefix = f"daemon: on-demand failure {reference}: "
    records = [line.partition(prefix)[2] for line in log.split("\n") if prefix in line]
    if len(records) != 1 or GO_QUOTED.fullmatch(records[0]) is None:
        return False
    # Go's %q permits hex escapes; its quoted-string escapes also decode as Python literals.
    try:
        diagnostic = ast.literal_eval(records[0])
    except (SyntaxError, ValueError):
        return False
    output = diagnostic.partition("\n\nlast output:\n")[2]
    return marker in output.splitlines()


if __name__ == "__main__":
    sys.exit(0 if escaped_owner_output(Path(sys.argv[1]).read_text(encoding="utf-8"), sys.argv[2], sys.argv[3]) else 1)
