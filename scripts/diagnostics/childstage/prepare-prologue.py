"""Prepare a diagnostic-only prefix without executing any Mesh program."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex


ROOT_LINE = 'repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)'
COHORT_LINE = '  python3 "$cohort_tool" start "$state" "$config" "$MESH_WORKER_REGISTRY" "$source_version" "$old_binary" "$cohort"'
CUTOFF = "# Live checkpoints belong to the worker and must remain free to change."


def prepare(root, observer, control, evidence):
    source = (root / "scripts/prove-release-transition.sh").read_text()
    if source.count(ROOT_LINE) != 1 or source.count(COHORT_LINE) != 1 or source.count(CUTOFF) != 1:
        raise ValueError("reviewed diagnostic insertion or cutoff changed")
    prefix, _ = source.split(CUTOFF)
    prefix = prefix.replace(ROOT_LINE, "repo_root=" + shlex.quote(str(root)))
    driver = root / "scripts/diagnostics/childstage/capture.py"
    wrapper = "  python3 " + shlex.quote(str(driver)) + " --authorized-disposable-diagnostic"
    for name, value in (("observer", observer), ("control", control), ("output", evidence)):
        wrapper += " --" + name + " " + shlex.quote(str(value))
    wrapper += ' --cohort-tool "$cohort_tool" "$state" "$config" "$MESH_WORKER_REGISTRY" "$source_version" "$old_binary" "$cohort"'
    prefix = prefix.replace(COHORT_LINE, wrapper)
    guard = '\n[[ -n ${MESH_WORKER_GROUP:-} ]] || { echo "diagnostic requires one exact verified group" >&2; exit 2; }\n'
    prefix = prefix.replace("set -euo pipefail\n", "set -euo pipefail\n" + guard, 1)
    prefix += '\necho "diagnostic cohort stage ended; native acceptance was not run"\nexit 0\n'
    return prefix


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("root", "observer", "control", "evidence", "script"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    value = prepare(args.root.resolve(), args.observer.resolve(), args.control.resolve(), args.evidence.resolve())
    with args.script.open("x") as stream:
        stream.write(value)
    args.script.chmod(0o600)
    print(json.dumps({"scriptSha256": hashlib.sha256(value.encode()).hexdigest(), "executed": False,
                      "nativeAcceptance": False, "source": str(args.root)}))
