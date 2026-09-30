#!/usr/bin/env python3
"""Match tool reports against counted, reasoned exceptions without line-number keys."""

import argparse
from collections import Counter
import json
from pathlib import Path
import re
import sys


GATES = ("golangci", "deadcode", "shellcheck", "ruff")
FIELDS = ("gate", "file", "rule", "text", "source")


def file_name(name, root):
    path = Path(name)
    if path.is_absolute():
        path = path.relative_to(root)
    if ".." in path.parts:
        raise ValueError(f"report path escapes repository: {name}")
    return path.as_posix().removeprefix("./")


def source_line(root, name, line):
    lines = (root / name).read_text().splitlines()
    if not 1 <= line <= len(lines):
        raise ValueError(f"invalid report location: {name}:{line}")
    return lines[line - 1].strip()


def findings(reports, root):
    result = Counter()

    def add(gate, name, rule, text, line=None):
        name = file_name(name, root)
        if name == "third_party" or name.startswith("third_party/"):
            return
        # Dupl embeds locations in its message even though Position is separate.
        text = re.sub(r"(?<=\.go):\d+(?:-\d+)?", ":<location>", text)
        source = source_line(root, name, line) if line is not None else ""
        result[(gate, name, str(rule), text, source)] += 1

    lint = json.loads((reports / "golangci.json").read_text())
    for issue in lint["Issues"] or []:
        pos = issue["Pos"]
        add("golangci", pos["Filename"], issue["FromLinter"], issue["Text"], pos["Line"])
    dead = json.loads((reports / "deadcode.json").read_text())
    for package in dead or []:
        for function in package["Funcs"]:
            add("deadcode", function["Position"]["File"], "unreachable", function["Name"])
    for issue in json.loads((reports / "shellcheck.json").read_text()):
        add("shellcheck", issue["file"], f"SC{issue['code']}", issue["message"], issue["line"])
    for issue in json.loads((reports / "ruff.json").read_text()):
        add("ruff", issue["filename"], issue["code"], issue["message"], issue["location"]["row"])
    return result


def baseline_entries(path):
    data = json.loads(path.read_text())
    if data["version"] != 1:
        raise ValueError("unsupported baseline version")
    result = {}
    for entry in data["entries"]:
        key = tuple(entry[field] for field in FIELDS)
        if key[0] not in GATES or not all(isinstance(value, str) for value in key):
            raise ValueError(f"invalid baseline key: {key}")
        if key in result:
            raise ValueError(f"duplicate baseline entry: {key}")
        if type(entry["count"]) is not int or entry["count"] < 1:
            raise ValueError(f"invalid baseline count: {key}")
        if not isinstance(entry["reason"], str) or not entry["reason"].strip():
            raise ValueError(f"baseline entry needs a reason: {key}")
        if re.search(r"\.go:\d+", entry["text"]):
            raise ValueError(f"line-number key is forbidden: {key}")
        result[key] = entry
    return result


def in_scope(key, scope):
    if scope is None:
        return True
    return key[0] == "golangci" and str(Path(key[1]).parent) in scope


def compare(actual, entries, scope=None):
    expected = Counter({key: entry["count"] for key, entry in entries.items() if in_scope(key, scope)})
    measured = Counter({key: count for key, count in actual.items() if in_scope(key, scope)})
    return measured - expected, expected - measured


def print_finding(kind, key, count):
    gate, name, rule, text, source = key
    print(f"{kind}: {gate} {name} [{rule}] {text} (count {count})", file=sys.stderr)
    if source:
        print(f"  {source}", file=sys.stderr)


def check(reports, root, baseline, update=False, scope=None):
    actual = findings(reports, root)
    entries = baseline_entries(baseline)
    new, stale = compare(actual, entries, scope)
    for key, count in sorted(new.items()):
        print_finding("NEW", key, count)
    if update and new:
        print("REFUSED: --update-baseline cannot add findings; no baseline was changed", file=sys.stderr)
    elif update:
        for key in list(entries):
            if in_scope(key, scope):
                if actual[key]:
                    entries[key]["count"] = actual[key]
                else:
                    del entries[key]
        baseline.write_text(json.dumps({"version": 1, "entries": [entries[key] for key in sorted(entries)]}, indent=2) + "\n")
        print(f"baseline: removed {sum(stale.values())} findings; added 0")
    else:
        for key, count in sorted(stale.items()):
            print_finding("STALE (run scripts/gates.sh --update-baseline)", key, count)
    for gate in GATES:
        if scope is not None and gate != "golangci":
            continue
        total = sum(count for key, count in actual.items() if key[0] == gate and in_scope(key, scope))
        added = sum(count for key, count in new.items() if key[0] == gate)
        removed = sum(count for key, count in stale.items() if key[0] == gate)
        status = "FAIL" if added or (removed and not update) else "PASS"
        print(f"{gate}: {status} ({total} findings, {added} new, {removed} stale)")
    return 1 if new or (stale and not update) else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reports", type=Path, required=True)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--baseline", type=Path, default=Path(".gates/baseline.json"))
    parser.add_argument("--update-baseline", action="store_true")
    parser.add_argument("--scope", type=Path)
    args = parser.parse_args()
    scope = set(args.scope.read_text().splitlines()) if args.scope else None
    try:
        return check(args.reports, args.root.resolve(), args.baseline, args.update_baseline, scope)
    except (KeyError, TypeError, ValueError, OSError) as error:
        print(f"gates: invalid baseline or tool report: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
