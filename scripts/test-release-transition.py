#!/usr/bin/env python3
"""Exercise the release proof's inventory assertions with a bounded CLI fixture."""

import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent
SOURCE = (ROOT / "prove-release-transition.sh").read_text()


def inventory_assertions(restart):
    start = r'^\[\[ \$\(read_state_version\)' if restart else r'^candidate_written_state='
    match = re.search(start + r'.*?(?=^assert_session_processes$)', SOURCE, re.M | re.S)
    assert match, "missing release inventory assertions"
    return match.group(0)


def failure_functions():
    match = re.search(r"^dump_diagnostics\(\) \{.*?(?=^wait_for_daemon\(\))", SOURCE, re.M | re.S)
    assert match, "missing release failure diagnostics"
    return match.group(0)


FIXTURE = '''import os
import signal
import sys
signal.signal(signal.SIGPIPE, signal.SIG_DFL)
mode = os.environ["INVENTORY_MODE"]
if mode == "missing-live":
    os.write(1, b"HOST ID STATE\\nfixture NONE exited\\n")
    sys.exit(0)
os.write(1, b"HOST ID STATE\\nfixture LIVE detached\\n")
if "--all" in sys.argv and mode != "missing-saved":
    os.write(1, b"fixture 0000 exited\\n")
if "--all" in sys.argv or mode != "saved-tail":
    os.write(1, b"trailing inventory data\\n" * 65536)
if mode == "failed":
    os.write(2, b"fixture inventory command failed\\n")
    sys.exit(42)
'''


class InventoryTest(unittest.TestCase):
    def run_inventory(self, mode, restart):
        with tempfile.TemporaryDirectory(prefix="mesh-inventory-") as directory:
            root = Path(directory)
            fixture = root / "fixture.py"
            fixture.write_text(FIXTURE)
            script = '''set -euo pipefail
mesh() { python3 "$FIXTURE" "$@"; }
read_state_version() { echo 14; }
''' + failure_functions() + inventory_assertions(restart)
            environment = dict(os.environ, FIXTURE=str(fixture), INVENTORY_MODE=mode,
                               test_root=directory, state=str(root / "state"), candidate_binary="fixture", session="LIVE",
                               saved_session="0000", candidate_state_version="14",
                               candidate_written_state="14")
            result = subprocess.run(["bash", "-c", script], env=environment,
                                    capture_output=True, text=True, timeout=5)
            snapshots = {path.name: path.read_text() for path in root.glob("*.inventory.*")}
            return result, snapshots

    def test_complete_inventory_with_early_match(self):
        cases = ((False, "complete"), (True, "complete"),
                 (False, "saved-tail"), (True, "saved-tail"))
        for restart, mode in cases:
            with self.subTest(restart=restart, mode=mode):
                result, snapshots = self.run_inventory(mode, restart)
                self.assertEqual(result.returncode, 0, result.stderr)
                outputs = [text for name, text in snapshots.items() if name.endswith(".out")]
                self.assertEqual(len(outputs), 2)
                self.assertEqual(sum(text.endswith("trailing inventory data\n") for text in outputs),
                                 1 if mode == "saved-tail" else 2)
                self.assertTrue(any("fixture 0000 exited" in text for text in outputs))

    def test_missing_session_still_fails(self):
        for restart in (False, True):
            with self.subTest(restart=restart):
                result, _ = self.run_inventory("missing-live", restart)
                self.assertNotEqual(result.returncode, 0)

    def test_missing_saved_session_still_fails(self):
        for restart in (False, True):
            with self.subTest(restart=restart):
                result, _ = self.run_inventory("missing-saved", restart)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("saved recovery session", result.stderr)

    def test_missing_saved_session_prints_observed_inventory(self):
        for restart in (False, True):
            with self.subTest(restart=restart):
                result, _ = self.run_inventory("missing-saved", restart)
                self.assertNotEqual(result.returncode, 0)
                phase = "restart" if restart else "candidate"
                self.assertIn(phase + "-all.inventory.out (last 4096 bytes):", result.stderr)

    def test_matching_output_does_not_hide_command_failure(self):
        for restart in (False, True):
            with self.subTest(restart=restart):
                result, snapshots = self.run_inventory("failed", restart)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("inventory command failed", result.stderr)
                self.assertTrue(any(text == "fixture inventory command failed\n"
                                    for name, text in snapshots.items() if name.endswith(".err")))


if __name__ == "__main__":
    unittest.main()
