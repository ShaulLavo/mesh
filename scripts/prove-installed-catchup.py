#!/usr/bin/env python3
"""Installed catch-up proof, gated first on an actual native service manager."""
import argparse
import json
import shutil
import signal
from pathlib import Path

from fixtures import installed_paths, native_services


def interrupted(number, _frame):
    raise InterruptedError(f"installed proof interrupted by signal {number}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("--native-preflight", action="store_true", required=True)
    arguments = parser.parse_args()
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    root = installed_paths.create()
    try:
        (output / "socket-paths.json").write_text(json.dumps(installed_paths.validate(root), indent=2) + "\n")
        native_services.positive_control(root, output / "native-manager.json")
    finally:
        shutil.rmtree(root)
        (output / "scratch-cleanup.json").write_text(json.dumps({"root": str(root), "absent": not root.exists()}) + "\n")


if __name__ == "__main__":
    main()
