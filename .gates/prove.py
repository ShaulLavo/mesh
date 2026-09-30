#!/usr/bin/env python3
"""Plant real violations in an isolated module and exercise the gates command."""

import argparse
import contextlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import textwrap


ROOT = Path(__file__).resolve().parent.parent
CLEAN = "package main\n\nfunc main() {}\n"


def run_gates(root, evidence, name, *arguments):
    reports = evidence / name
    result = subprocess.run(
        ["bash", "scripts/gates.sh", "--report-dir", str(reports), *arguments],
        cwd=root, capture_output=True, text=True, check=False,
    )
    output = result.stdout + result.stderr
    (evidence / f"{name}.txt").write_text(output)
    return result.returncode, output, reports


def expect(result, status, message):
    code, output, _ = result
    if code != status or message not in output:
        raise RuntimeError(f"expected exit {status} and {message!r}, got exit {code}\n{output}")
    print(f"PASS: {message} (exit {code})")
    for line in output.splitlines():
        if line.startswith(("NEW:", "REFUSED:", "baseline:", "golangci:", "deadcode:", "shellcheck:", "ruff:")):
            print(f"  {line}")


def write_go(root, content):
    path = root / "cmd/mesh/main.go"
    path.write_text(textwrap.dedent(content))
    subprocess.run(["gofmt", "-w", str(path)], check=True)


def prove(root, evidence):
    for directory in ("cmd/mesh", "internal", "scripts/install", "integration", ".gates"):
        (root / directory).mkdir(parents=True, exist_ok=True)
    (root / "go.mod").write_text("module github.com/shaul/mesh\n\ngo 1.27.0\n")
    for name in ("check.go", "check_test.go"):
        shutil.copyfile(ROOT / ".gates" / name, root / ".gates" / name)
    shutil.copyfile(ROOT / "scripts/gates.sh", root / "scripts/gates.sh")
    shutil.copyfile(ROOT / ".golangci.yml", root / ".golangci.yml")
    baseline = root / ".gates/baseline.json"
    baseline.write_text(json.dumps({"version": 2, "entries": []}))
    write_go(root, CLEAN)
    expect(run_gates(root, evidence, "clean"), 0, "golangci: PASS")

    # Goconst ignores call arguments by default; composite literals are analyzed.
    write_go(root, 'package main\nfunc main() { values := []string{"stable", "stable", "stable"}; println(values) }\n')
    expect(run_gates(root, evidence, "constant-new"), 1, "[goconst]")
    baseline.write_text(json.dumps({"version": 2, "entries": [{
        "gate": "golangci", "file": "cmd/mesh/main.go", "rule": "goconst",
        "text": "string `stable`", "function": "", "count": 1,
        "reason": "Deliberate fixture exception: diagnostic occurrence counts do not identify a finding.",
    }]}))
    expect(run_gates(root, evidence, "constant-baselined"), 0, "golangci: PASS")
    write_go(root, 'package main\nfunc main() { values := []string{"stable", "stable", "stable", "stable"}; println(values) }\n')
    expect(run_gates(root, evidence, "constant-more-occurrences"), 0, "golangci: PASS")
    write_go(root, 'package main\nfunc main() { values := []string{"stable", "stable", "stable", "stable", "new-literal", "new-literal", "new-literal"}; println(values) }\n')
    expect(run_gates(root, evidence, "constant-different-literal"), 1, "[goconst] string `new-literal`")
    baseline.write_text(json.dumps({"version": 2, "entries": []}))
    write_go(root, CLEAN)

    fixtures = {
        "dead-code": ("deadcode", CLEAN + "\nfunc abandonedValue() int { return 7 }\n"),
        "nesting": ("nestif", """\
            package main
            func main() { println(nestedValue(8)) }
            func nestedValue(n int) int {
                if n > 1 { if n > 2 { if n > 3 { if n > 4 { if n > 5 { return n } } } } }
                return 0
            }
        """),
        "unwrapped-error": ("wrapcheck", """\
            package main
            import "os"
            func main() { println(readConfig()) }
            func readConfig() error { _, err := os.ReadFile("fixture"); return err }
        """),
        "revive": ("revive", """\
            package main
            func main() { println(decrement(4)) }
            func decrement(value int) int { value -= 1; return value }
        """),
    }
    body = "\n".join(f"value += {number}" for number in range(1, 45))
    fixtures["duplication"] = ("dupl", f"package main\nfunc main() {{ println(cloneOne(1), cloneTwo(2)) }}\nfunc cloneOne(value int) int {{\n{body}\nreturn value\n}}\nfunc cloneTwo(value int) int {{\n{body}\nreturn value\n}}\n")
    for name, (rule, content) in fixtures.items():
        write_go(root, content)
        result = run_gates(root, evidence, name)
        expect(result, 1, f"[{rule}]" if rule != "deadcode" else "NEW: deadcode")
        write_go(root, CLEAN)

    planted_shell = root / "scripts/planted.sh"
    planted_shell.write_text("#!/usr/bin/env bash\nvalue='fixture value'\necho $value\n")
    expect(run_gates(root, evidence, "shellcheck"), 1, "[SC2086]")
    planted_shell.unlink()

    # Keep exported-environment exceptions tied to their statements, not the file's count.
    exported_shell = root / "scripts/install/linux.sh"
    original_shell = """#!/usr/bin/env bash
remote_uid=1000
source_binary=source
binary_tmp=destination
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$remote_uid}
export DBUS_SESSION_BUS_ADDRESS=${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}
install -m 0755 "$source_binary" "$binary_tmp"
"""
    exported_shell.write_text(original_shell)
    shell_new = run_gates(root, evidence, "shell-statements-new")
    expect(shell_new, 1, "[SC2086]")
    issues = json.loads((shell_new[2] / "shellcheck.json").read_text())
    if len(issues) != 2 or any(issue["code"] != 2086 for issue in issues):
        raise RuntimeError("replacement fixture must produce exactly two SC2086 findings")
    baseline.write_text(json.dumps({"version": 2, "entries": [{
        "gate": "shellcheck", "file": issue["file"], "rule": "SC2086",
        "text": issue["message"], "function": "",
        "source": original_shell.splitlines()[issue["line"] - 1].strip(), "count": 1,
        "reason": "Deliberate fixture exception for this exported-environment statement only.",
    } for issue in issues]}))
    expect(run_gates(root, evidence, "shell-statements-baselined"), 0, "shellcheck: PASS")
    replacement_shell = original_shell.replace(
        'export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$remote_uid}',
        'export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$remote_uid}"',
    ).replace('install -m 0755 "$source_binary"', 'install -m 0755 $source_binary')
    exported_shell.write_text(replacement_shell)
    expect(run_gates(root, evidence, "shell-statement-replacement"), 1,
           "shellcheck: FAIL (2 findings, 1 new, 1 stale)")
    before = baseline.read_bytes()
    expect(run_gates(root, evidence, "shell-replacement-refuse", "--update-baseline"), 1,
           "REFUSED: --update-baseline cannot add findings")
    if baseline.read_bytes() != before:
        raise RuntimeError("replacement changed the baseline despite refusal")
    exported_shell.unlink()
    baseline.write_text(json.dumps({"version": 2, "entries": []}))

    planted_python = root / "scripts/planted.py"
    planted_python.write_text("import os\n")
    expect(run_gates(root, evidence, "ruff"), 1, "[F401]")
    planted_python.unlink()

    write_go(root, fixtures["dead-code"][1])
    before = baseline.read_bytes()
    refused = run_gates(root, evidence, "refuse-growth", "--update-baseline")
    expect(refused, 1, "REFUSED: --update-baseline cannot add findings")
    if baseline.read_bytes() != before:
        raise RuntimeError("update changed the baseline while refusing growth")
    entries = []
    for issue in json.loads((refused[2] / "golangci.json").read_text())["Issues"] or []:
        file = issue["Pos"]["Filename"]
        function = "abandonedValue"
        entries.append({"gate": "golangci", "file": file, "rule": issue["FromLinter"], "text": issue["Text"], "function": function, "count": 1})
    for package in json.loads((refused[2] / "deadcode.json").read_text()) or []:
        for function in package["Funcs"]:
            entries.append({"gate": "deadcode", "file": function["Position"]["File"], "rule": "unreachable", "text": function["Name"], "function": "", "count": 1})
    for entry in entries:
        entry["reason"] = "Deliberate test fixture exception; removed in the next assertion."
    baseline.write_text(json.dumps({"version": 2, "entries": entries}))
    expect(run_gates(root, evidence, "reasoned-exception"), 0, "deadcode: PASS")
    write_go(root, CLEAN)
    expect(run_gates(root, evidence, "stale-exception"), 1, "STALE (run scripts/gates.sh --update-baseline)")
    expect(run_gates(root, evidence, "remove-fixed", "--update-baseline"), 0, "baseline: removed")
    if json.loads(baseline.read_text())["entries"]:
        raise RuntimeError("fixed entries remain in the baseline")
    expect(run_gates(root, evidence, "restored"), 0, "golangci: PASS")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    evidence = args.evidence.resolve()
    evidence.mkdir(parents=True, exist_ok=True)
    with contextlib.ExitStack() as stack:
        temporary = stack.enter_context(tempfile.TemporaryDirectory(prefix="mesh-gates-proof-"))
        root = Path(temporary)
        os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
        prove(root, evidence)


if __name__ == "__main__":
    main()
