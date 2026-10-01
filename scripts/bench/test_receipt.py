"""Fault provenance on an actual built Mesh binary; no daemon is needed."""
import argparse
import copy
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from receipt import inspect_binary, verify_receipt


def rejected(binary, receipt, **labels):
    try:
        verify_receipt(binary, receipt, **labels)
    except ValueError:
        return
    raise AssertionError("mismatched provenance was accepted")


def check(binary, receipt_path, parent):
    receipt = json.loads(receipt_path.read_text())
    assert verify_receipt(binary, receipt) == receipt
    rejected(binary, receipt, commit="incorrect-source-label")
    rejected(binary, receipt, go_version="incorrect-toolchain-label")
    rejected(binary, receipt, profiled=not receipt["binary"]["profiled"])
    changed = copy.deepcopy(receipt)
    changed["binary"]["build_settings"]["GOARCH"] = "incorrect-target"
    rejected(binary, changed)
    changed = copy.deepcopy(receipt)
    changed["production_source"]["files"]["go.mod"] = "0" * 64
    rejected(binary, changed)
    changed = copy.deepcopy(receipt)
    changed["production_source"].pop("target")
    rejected(binary, changed)
    changed = copy.deepcopy(receipt)
    changed["production_equivalent_to_vcs_revision"] = not changed["production_equivalent_to_vcs_revision"]
    rejected(binary, changed)
    changed = copy.deepcopy(receipt)
    changed["embedded_vcs_revision"] = "incorrect-source-label"
    rejected(binary, changed)
    with tempfile.TemporaryDirectory(prefix="mesh-m5-receipt-", dir=parent) as directory:
        root = Path(directory)
        altered = root / "mesh"
        shutil.copy2(binary, altered)
        with altered.open("ab") as output:
            output.write(b"replaced artifact")
        assert inspect_binary(altered)["sha256"] != receipt["binary"]["sha256"]
        rejected(altered, receipt)
        # This is the original false-label invocation, now rejected before any root/process.
        result = subprocess.run([sys.executable, str(Path(__file__).with_name("run.py")),
                                 "--binary", str(binary), "--receipt", str(receipt_path),
                                 "--commit", "incorrect-source-label", "--go-version", "incorrect-toolchain-label",
                                 "--scratch-parent", str(root), "--output", str(root / "incorrect.json")],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        assert result.returncode != 0
        assert not list(root.glob("mesh-m5-*")) and not (root / "incorrect.json").exists()
    print("PASS: actual binary, source/compiler/target/profile labels and artifact mismatch receipts")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--scratch-parent", type=Path, required=True)
    args = parser.parse_args()
    check(args.binary.resolve(), args.receipt.resolve(), args.scratch_parent)
