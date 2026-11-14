#!/usr/bin/env python3
"""Positive controls for the native fixture's bounded startup observer."""
import ast
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from fixtures import release_worker_evidence as evidence, release_workers


@unittest.skipUnless(sys.platform == "linux", "Linux procfs startup evidence")
class StartupEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="worker-evidence-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def child(self, executable=sys.executable):
        process = subprocess.Popen([str(executable), "-c", "import sys; sys.stdin.buffer.read(1)", "private-argument"],
                                   stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   env=os.environ | {"PRIVATE_DIAGNOSTIC_CONTROL": "private-environment"})
        self.addCleanup(evidence.stop_control, process)
        return process

    def test_exact_launcher_context_control_retains_image_after_exit(self):
        before = set(Path("/proc/self/fd").iterdir())
        result = evidence.positive_control()
        self.assertTrue(result["passed"])
        self.assertTrue(result["mappedImageRetainedAfterExit"])
        self.assertEqual(result["launcherPid"], os.getpid())
        self.assertFalse(Path(f"/proc/{result['childPid']}").exists())
        self.assertEqual(set(Path("/proc/self/fd").iterdir()), before)

    def test_failed_control_stops_before_cohort_launch(self):
        (self.root / "s/seed").mkdir(parents=True)
        (self.root / "s/seed/meta.json").write_text('{"bootId":"unit-control"}')
        with sqlite3.connect(self.root / "mesh.db") as database:
            database.execute("CREATE TABLE hosts(id TEXT)")
            database.execute("INSERT INTO hosts VALUES('unit-control')")
        database.close()
        before = set(Path("/proc/self/fd").iterdir())
        with mock.patch.object(evidence, "verify_control_context", side_effect=RuntimeError("control failed")), mock.patch.object(release_workers, "start_one") as launch:
            with self.assertRaisesRegex(RuntimeError, "control failed"):
                release_workers.start(self.root, self.root, [{"unit": "never launched"}], self.root / "cohort.json")
        launch.assert_not_called()
        self.assertEqual(set(Path("/proc/self/fd").iterdir()), before)

    def test_mapped_image_is_distinct_from_replaced_selected_path_and_survives_exit(self):
        executable = self.root / "interpreter"
        shutil.copy2(sys.executable, executable)
        original_digest = hashlib.sha256(executable.read_bytes()).hexdigest()
        process = self.child(executable)
        with evidence.Startup(process.pid) as observed:
            self.assertEqual(observed.before["stat"]["parentPid"], os.getpid())
            process.communicate(b"x", timeout=3)
            executable.unlink()
            executable.write_bytes(b"selected path replaced after this child exited")
            record = observed.failure({"version": "unit-control", "sha256": "f" * 64}, "bounded startup log")
        self.assertEqual(record["mappedImage"]["sha256"], original_digest)
        self.assertNotEqual(record["mappedImage"]["sha256"], record["selectedArtifact"]["sha256"])
        self.assertEqual(record["failingCall"], "unavailable")
        self.assertNotIn("private-argument", json.dumps(record))
        self.assertNotIn("private-environment", json.dumps(record))
        self.assertNotIn("PRIVATE_DIAGNOSTIC_CONTROL", json.dumps(record))

    def test_unchanged_wait_failure_publishes_actual_pid_image_and_partial_cohort(self):
        process = self.child()
        row = {"pid": process.pid, "shellPid": 0, "session": "W000"}
        record, path = {"workers": [row], "observations": []}, self.root / "cohort.json"
        item = {"version": "unit-control", "artifact": {"binarySha256": "e" * 64}}
        (self.root / "worker.log").write_bytes(b"x" * 10000 + b"child-start errno=1 stage=unavailable")
        failure = RuntimeError("original startup failure")
        with mock.patch.object(release_workers, "wait_worker", side_effect=failure):
            with self.assertRaisesRegex(RuntimeError, "original startup failure") as raised:
                release_workers.wait_startup(self.root, process, item, record, row, path)
        self.assertIs(raised.exception, failure)
        actual = json.loads(path.read_text())["workers"][0]["startupFailure"]
        self.assertEqual(actual["pid"], process.pid)
        self.assertEqual(actual["workerAtObservation"]["stat"]["parentPid"], os.getpid())
        self.assertEqual(actual["mappedImage"]["sha256"], hashlib.sha256(Path(sys.executable).read_bytes()).hexdigest())
        self.assertEqual(len(actual["workerLogTail"]), 4096)
        self.assertTrue(actual["workerLogTail"].endswith("child-start errno=1 stage=unavailable"))
        self.assertLess(len(json.dumps(actual)), 65536)

    def test_diagnostic_write_failure_preserves_original_start_failure_and_closes_handles(self):
        process = self.child()
        before = set(Path("/proc/self/fd").iterdir())
        failure = RuntimeError("original startup failure")
        with mock.patch.object(release_workers, "wait_worker", side_effect=failure), mock.patch.object(release_workers, "save", side_effect=OSError("disk full")):
            with self.assertRaisesRegex(RuntimeError, "original startup failure") as raised:
                release_workers.wait_startup(self.root, process, {"version": "unit", "artifact": {"binarySha256": "a"}}, {}, {}, self.root / "record")
        self.assertIs(raised.exception, failure)
        self.assertEqual(set(Path("/proc/self/fd").iterdir()), before)

    def test_disappeared_process_is_explicit_and_does_not_lookup_another_pid(self):
        process = self.child()
        process.communicate(b"x", timeout=3)
        with evidence.Startup(process.pid) as observed:
            record = observed.failure({"version": "unit"}, "original failure")
        self.assertIn("unavailable", record["workerAtObservation"])
        self.assertIn("unavailable", record["mappedImage"])

    def test_successful_wait_closes_observer_without_hashing_or_publishing(self):
        process = self.child()
        before = set(Path("/proc/self/fd").iterdir())
        with mock.patch.object(release_workers, "wait_worker"), mock.patch.object(evidence, "image_digest", side_effect=AssertionError("unnecessary image hash")):
            release_workers.wait_startup(self.root, process, {}, {}, {}, self.root / "absent")
        self.assertFalse((self.root / "absent").exists())
        self.assertEqual(set(Path("/proc/self/fd").iterdir()), before)


class FailureRetentionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="worker-export-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def run_cleanup(self, output):
        script = (Path(__file__).parent / "prove-release-transition.sh").read_text()
        cleanup = script[script.index("cleanup() {"):script.index("trap cleanup EXIT")]
        scratch = self.root / "scratch"
        scratch.mkdir()
        (scratch / "cohort.json").write_text('{"workers":[{"pid":42,"startupFailure":{"failingCall":"unavailable"}}]}')
        stop = self.root / "stop.py"
        stop.write_text("from pathlib import Path; import sys; Path(sys.argv[0]).with_suffix('.stopped').write_bytes(Path(sys.argv[2]).read_bytes())")
        environment = os.environ | {"test_root": str(scratch), "cohort": str(scratch / "cohort.json"),
            "cohort_tool": str(stop), "output_directory": str(output), "source_version": "v0.1.9003"}
        prelude = "set -euo pipefail\nhelper_pid= client= candidate_daemon= old_daemon= shell_pid= worker_pid= original_worker= original_shell=\ndiscover_owned_session_processes() { :; }\n"
        result = subprocess.run(["bash", "-c", prelude + cleanup + "trap cleanup EXIT\nexit 7"], env=environment,
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(json.loads(stop.with_suffix('.stopped').read_text())["workers"][0]["pid"], 42)
        self.assertFalse(scratch.exists())

    def test_exit_trap_retains_partial_record_before_actual_scratch_teardown(self):
        output = self.root / "output"
        self.run_cleanup(output)
        record = json.loads((output / "failures/v0.1.9003.cohort.json").read_text())
        self.assertEqual(record["workers"][0]["pid"], 42)
        self.assertEqual(record["workers"][0]["startupFailure"]["failingCall"], "unavailable")

    def test_export_failure_still_tears_down_scratch_and_preserves_exit_status(self):
        output = self.root / "blocked-output"
        output.write_text("unwritable output directory control")
        self.run_cleanup(output)

    def test_new_fixture_blocks_have_at_most_three_levels(self):
        paths = (Path(evidence.__file__), Path(__file__), Path(release_workers.__file__))
        for path in paths:
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))


def depth(node, nesting=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        nesting = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        nesting += 1
    return max((depth(child, nesting) for child in ast.iter_child_nodes(node)), default=nesting)


if __name__ == "__main__":
    unittest.main()
