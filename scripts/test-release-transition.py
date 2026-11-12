#!/usr/bin/env python3
"""Exercise the release proof's inventory assertions with a bounded CLI fixture."""

import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parent
SOURCE = (ROOT / "prove-release-transition.sh").read_text()
SPEC = importlib.util.spec_from_file_location("transition_fixture", ROOT / "fixtures/release_transition.py")
TRANSITION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TRANSITION)


class CandidateWritePathTest(unittest.TestCase):
    def test_symlinked_state_keeps_exact_physical_target_check(self):
        with tempfile.TemporaryDirectory(prefix="proof-path-") as directory:
            physical = Path(directory) / "physical"
            physical.mkdir()
            (physical / "rollback-site").mkdir()
            alias = Path(directory) / "alias"
            alias.symlink_to(physical, target_is_directory=True)
            with sqlite3.connect(physical / "mesh.db") as database:
                database.execute("CREATE TABLE services(name,kind,target,display_name,isolate)")
                database.execute("INSERT INTO services VALUES(?,?,?,?,?)", ("rollback-proof", "static",
                    str((physical / "rollback-site").resolve()), "Candidate rollback proof", 1))
            (physical / "machine-name.json").write_bytes(b"retained name")
            (physical / "rollback-name.before").write_bytes(b"retained name")
            response = {"services": [{"name": "rollback-proof"}], "host": {"machineName": "rollback-proof"}}
            with mock.patch.object(TRANSITION, "request", return_value=response):
                TRANSITION.verify_writes(alias)
            with sqlite3.connect(physical / "mesh.db") as database:
                database.execute("UPDATE services SET target='different target'")
            with self.assertRaises(AssertionError):
                TRANSITION.verify_writes(alias)


def inventory_assertions(restart):
    start = r'^\[\[ \$\(read_state_version\)' if restart else r'^candidate_written_state='
    match = re.search(start + r'.*?(?=^assert_session_processes$)', SOURCE, re.M | re.S)
    assert match, "missing release inventory assertions"
    return match.group(0)


def failure_functions():
    match = re.search(r"^dump_diagnostics\(\) \{.*?(?=^wait_for_daemon\(\))", SOURCE, re.M | re.S)
    assert match, "missing release failure diagnostics"
    return match.group(0)


FIXTURE = '''import hashlib
import json
import os
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


class ProofAssemblyTest(unittest.TestCase):
    def test_pending_migrations_without_fault_input_emit_no_rollback(self):
        guard = re.search(r"^require_startup_fault_input\(\) \{.*?^\}", SOURCE, re.M | re.S)
        self.assertIsNotNone(guard)
        with tempfile.TemporaryDirectory(prefix="mesh-proof-input-") as directory:
            output = Path(directory) / "rollback.json"
            script = ('set -euo pipefail\nfail() { printf "%s\\n" "$*" >&2; exit 1; }\n'
                      + guard.group(0) + '\nrequire_startup_fault_input\nprintf "unexpected proof" > "$PROOF_OUTPUT"\n')
            environment = dict(os.environ, old_state_version="13", candidate_state_version="16", PROOF_OUTPUT=str(output))
            environment.pop("MESH_STARTUP_FAULT_BINARY", None)
            result = subprocess.run(["bash", "-c", script], env=environment, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("requires MESH_STARTUP_FAULT_BINARY", result.stderr)
            self.assertFalse(output.exists())

    def test_separate_rollback_asset_preserves_forward_assembly(self):
        with tempfile.TemporaryDirectory(prefix="mesh-proof-assembly-") as directory:
            root = Path(directory)
            receipt = {
                "schema": 1, "platform": {"os": "linux", "arch": "amd64"},
                "fromDigest": "a" * 64, "toDigest": "b" * 64,
                "stateReadMin": 14, "stateReadMax": 16, "stateWrite": 16,
                "workerMin": 1, "workerMax": 1, "workerWrite": 1, "journalVersion": 1,
                "candidateOpenedRetainedState": True, "sessionsPreserved": True,
                "recoveryRecordsPreserved": True,
            }
            contents = json.dumps(receipt, separators=(",", ":"), sort_keys=True).encode()
            digest = hashlib.sha256(contents).hexdigest()
            forward = root / (digest + ".json")
            forward.write_bytes(contents)
            (root / "rollback").mkdir()
            rollback = {"schema": 1, "forward": digest, "boundaries": []}
            rollback_bytes = json.dumps(rollback).encode()
            rollback_digest = hashlib.sha256(rollback_bytes).hexdigest()
            (root / "rollback" / (rollback_digest + ".rollback.json")).write_bytes(rollback_bytes)
            result = subprocess.run(["python3", str(ROOT / "assemble-release-compatibility.py"),
                                     str(root), str(root / "compatibility")],
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            assembled = json.loads((root / "compatibility").read_bytes())
            self.assertEqual([transition["proof"] for transition in assembled["transitions"]], [digest])
            self.assertEqual(forward.read_bytes(), contents)


if __name__ == "__main__":
    unittest.main()
