#!/usr/bin/env python3
"""Print docs/reference/cli.md from the help text of a built mesh binary.

Usage: python3 scripts/gen-cli-reference.py [--binary PATH] > docs/reference/cli.md

Without --binary the script builds ./cmd/mesh into a temporary directory.
"""

import argparse
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

MAX_DEPTH = 3
SKIPPED_SECTIONS = {"USAGE", "EXAMPLES", "FLAGS"}
SKIPPED_COMMANDS = {"help"}
SHALLOW_COMMANDS = {"completion"}


def run_help(binary, words):
    env = dict(os.environ, NO_COLOR="1", TERM="dumb", HOME=tempfile.gettempdir())
    result = subprocess.run(
        [binary, *words, "--help"],
        capture_output=True,
        text=True,
        env=env,
        stdin=subprocess.DEVNULL,
        check=False,
    )
    text = result.stdout or result.stderr
    lines = [line.rstrip() for line in text.splitlines()]
    while lines and not lines[0]:
        lines.pop(0)
    while lines and not lines[-1]:
        lines.pop()
    return lines


def subcommands(lines):
    """Return (name, description) pairs from a fang or cobra help page."""
    found = []
    section = None
    for line in lines:
        fang_header = re.fullmatch(r" {2}([A-Z]+)", line)
        if fang_header:
            section = fang_header.group(1)
            continue
        if line == "Available Commands:":
            section = "COBRA"
            continue
        if section == "COBRA":
            entry = re.match(r" {2}(\S+)\s{2,}(.*)", line)
            if entry:
                found.append((entry.group(1), entry.group(2)))
            elif line and not line.startswith(" "):
                section = None
            continue
        if section and section not in SKIPPED_SECTIONS:
            entry = re.match(r" {4}(\S+)(.*)", line)
            if entry:
                description = (
                    re.split(r"\s{2,}", entry.group(2).strip())[-1]
                    if "  " in entry.group(2)
                    else ""
                )
                found.append((entry.group(1), description))
    return [(name, text) for name, text in found if name not in SKIPPED_COMMANDS]


def collect(binary, words, depth, pages):
    lines = run_help(binary, words)
    pages.append((words, lines))
    if depth >= MAX_DEPTH or (words and words[-1] in SHALLOW_COMMANDS):
        return
    for name, _ in subcommands(lines):
        collect(binary, [*words, name], depth + 1, pages)


def render(binary):
    pages = []
    collect(binary, [], 0, pages)
    top = subcommands(pages[0][1])
    out = [
        "# CLI reference",
        "",
        "Generated from `--help`. Do not edit by hand. Regenerate with:",
        "",
        "```bash",
        "python3 scripts/gen-cli-reference.py > docs/reference/cli.md",
        "```",
        "",
        "Run `mesh COMMAND --help` for the same text. A bare name is a host or a session, so",
        "`mesh pc`, `mesh pc -r` and `mesh 7K3D` are not subcommands.",
        "",
        "| Command | Job |",
        "|---|---|",
    ]
    for name, description in top:
        out.append(f"| [`mesh {name}`](#mesh-{name}) | {description} |")
    out.append("")
    for words, lines in pages:
        title = " ".join(["mesh", *words])
        out.extend(
            [
                f"## {title}",
                "",
                "```text",
                *[line.strip("\n") for line in dedent(lines)],
                "```",
                "",
            ]
        )
    return "\n".join(out).rstrip() + "\n"


def dedent(lines):
    margin = min((len(line) - len(line.lstrip()) for line in lines if line), default=0)
    return [line[margin:] for line in lines]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", help="path to a built mesh binary")
    args = parser.parse_args()
    if args.binary:
        sys.stdout.write(render(args.binary))
        return
    root = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory() as scratch:
        binary = str(Path(scratch) / "mesh")
        subprocess.run(
            ["go", "build", "-o", binary, "./cmd/mesh"], cwd=root, check=True
        )
        sys.stdout.write(render(binary))


if __name__ == "__main__":
    main()
