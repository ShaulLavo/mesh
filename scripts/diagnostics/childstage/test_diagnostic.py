"""Offline checks of image binding, refusal selection and diagnostic isolation."""
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

import backend
import capture


HERE = Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DiagnosticTests(unittest.TestCase):
    def test_missing_call_stays_unavailable(self):
        worker = {"pid": 123, "digest": "same"}
        failure = {"selectedArtifact": {"sha256": "same"}, "mappedImage": {"sha256": "same", "device": 1, "inode": 2, "bytes": 3}}
        executed = {"event": "exec", "tid": 123, "image": "1:2:3"}
        row = {"event": "return", "result": -1, "owner": 124, "image": "1:2:3"}
        self.assertEqual(capture.worker_calls(worker, failure, [executed, row])["calls"], [])
        row["owner"], row["image"] = 123, "wrong"
        self.assertEqual(capture.worker_calls(worker, failure, [executed, row])["calls"], [])
        row["image"] = "1:2:3"
        self.assertEqual(capture.worker_calls(worker, failure, [executed, row])["calls"], [row])
        failure["mappedImage"]["sha256"] = "swapped"
        with self.assertRaisesRegex(RuntimeError, "image binding"):
            capture.worker_calls(worker, failure, [executed, row])

    def test_parent_probe_is_not_actual_start(self):
        checker = load("check-controls")
        self.assertFalse(checker.real_launch({"call": {"args": [0x5100]}}, "parent"))
        self.assertTrue(checker.real_launch({"call": {"args": [0x4111]}}, "parent"))

    def test_retained_image_survives_missing_original_snapshot(self):
        worker = {"pid": 123, "digest": "same"}
        image = {"sha256": "same", "device": 1, "inode": 2, "bytes": 3}
        failure = {"selectedArtifact": {"sha256": "same"}, "mappedImage": {"unavailable": True}}
        executed = {"event": "exec", "tid": 123, "image": "1:2:3"}
        row = {"event": "return", "result": -1, "owner": 123, "image": "1:2:3", "mappedImage": image}
        result = capture.worker_calls(worker, failure, [executed, row])
        self.assertEqual(result["observerMappedImage"], image)
        self.assertEqual(result["mappedImage"], {"unavailable": True})
        with self.assertRaisesRegex(RuntimeError, "exec identity"):
            capture.worker_calls(worker, failure, [executed, executed, row])

    def test_start_time_mismatch_is_not_attributed(self):
        failure = {"workerAtObservation": {"stat": {"startTicks": 123}}}
        with self.assertRaisesRegex(RuntimeError, "different start times"):
            capture.verify_start_ticks(failure, {"context": {"stat": ["124"]}})

    def test_kernel_device_encoding_matches_proc_stat(self):
        run = backend.KernelRun(Path("/unused"), "offline")
        row = {"event": "exec", "device": (253 << 20) | 5, "inode": 22, "bytes": 33}
        run.remember(row)
        self.assertEqual(row["image"], f"{os.makedev(253, 5)}:22:33")

    def test_prepare_only_stops_at_original_cohort(self):
        source = load("prepare-prologue")
        root = HERE.parents[2]
        result = source.prepare(root, Path("/observer"), Path("/control"), Path("/evidence"))
        self.assertIn(" --cohort-tool ", result)
        self.assertNotIn(source.COHORT_LINE, result)
        self.assertNotIn("saved_session=$(", result)
        self.assertIn("MESH_WORKER_GROUP", result)
        self.assertTrue(result.endswith("exit 0\n"))

    def test_summary_never_accepts_a_trace(self):
        with tempfile.TemporaryDirectory(prefix="mesh-childstage-test-", dir="/work/tmp") as directory:
            output = Path(directory)
            (output / "capture.jsonl").write_text(json.dumps({"event": "teardown", "remainingTasks": 0, "observerError": ""}) + "\n")
            result = capture.summarize(output)
            self.assertFalse(result["actualErrno1Captured"])
            self.assertFalse(result["nativeAcceptance"])
            self.assertFalse(result["oldCauseProved"])


if __name__ == "__main__":
    unittest.main()
