#!/usr/bin/env python3
import hashlib
import json
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest import mock

from fixtures import release_workers


class ExactWorkerSelectionTests(unittest.TestCase):
    def test_observation_retains_actual_image_observer_duration(self):
        result = subprocess.CompletedProcess([], 0, b"--- PASS: TestRetainedWorkerCohortIdentity\n", b"")
        with tempfile.TemporaryDirectory(prefix="worker-time-") as directory:
            path = Path(directory) / "cohort.json"
            worker = {"digest": "a" * 64, "session": "W000", "pid": 10, "shellPid": 11, "protocol": 1, "version": "v0.1.1"}
            path.write_text(json.dumps({"workers": [worker], "observations": []}))
            with mock.patch.dict(release_workers.os.environ, MESH_WORKER_PROBE="observer"), mock.patch.object(release_workers.subprocess, "run", return_value=result), mock.patch.object(release_workers.time, "monotonic", side_effect=[10.0, 11.25]), mock.patch.object(release_workers, "verify_one", return_value=7):
                self.assertEqual(release_workers.verify(Path("state"), path, "checkpoint"), ["a" * 64])
            observation = json.loads(path.read_bytes())["observations"][0]
            self.assertEqual(observation, {"label": "checkpoint", "workers": 1, "processes": 2,
                                           "rssKiB": 7, "originalRssKiB": 0, "observerSeconds": 1.25, "identities": [worker], "original": None})

    def test_image_observer_failure_keeps_bounded_real_output_and_rejects(self):
        result = subprocess.CompletedProcess([], 1, b"x" * 9000 + b"mapped image mismatch", b"actual stderr control")
        with mock.patch.dict(release_workers.os.environ, MESH_WORKER_PROBE="observer"), mock.patch.object(release_workers.subprocess, "run", return_value=result) as command:
            with self.assertRaisesRegex(RuntimeError, "mapped image mismatch") as error:
                release_workers.verify_images(Path("state"), Path("cohort.json"))
            self.assertIn("actual stderr control", str(error.exception))
            self.assertIn("exited 1", str(error.exception))
            self.assertLess(len(str(error.exception)), 8300)
            self.assertEqual(command.call_args.kwargs["timeout"], 30)

    def test_modern_worker_requires_the_maintained_exporter(self):
        with mock.patch.object(release_workers.subprocess, "check_output", return_value=b"--instance"):
            with self.assertRaisesRegex(ValueError, "maintained reservation exporter"):
                release_workers.reserve_worker(Path("mesh"), Path("state"), 0, {})

    def test_startup_failure_retains_bounded_worker_log_tail(self):
        with tempfile.TemporaryDirectory(prefix="worker-log-") as directory:
            root = Path(directory)
            (root / "worker.log").write_bytes(b"a" * 8192 + b"reservation missing")
            process = mock.Mock()
            process.poll.return_value = 1
            with self.assertRaisesRegex(RuntimeError, "reservation missing") as error:
                release_workers.wait_worker(root, process)
            self.assertLess(len(str(error.exception)), 4300)

    def test_source_applicability_does_not_require_future_worker_binaries(self):
        with tempfile.TemporaryDirectory(prefix="workers-") as directory:
            root = Path(directory)
            source = root / "source"
            source.write_bytes(b"original source worker")
            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            registry = {"schema": 1, "platform": {"os": "linux", "arch": "amd64"}, "workers": [
                {"version": "v0.1.249", "protocol": 1, "binary": "source", "artifact": {"binarySha256": digest}},
                {"version": "v0.1.255", "protocol": 1, "binary": "future-must-not-open", "artifact": {"binarySha256": "a" * 64}},
            ]}
            path = root / "workers.json"
            path.write_text(json.dumps(registry))
            selected = release_workers.applicable(path, "v0.1.249", source)
            self.assertEqual([row["version"] for row in selected], ["v0.1.249"])
            source.write_bytes(b"changed original")
            with self.assertRaisesRegex(ValueError, "executable changed"):
                release_workers.applicable(path, "v0.1.249", source)


if __name__ == "__main__":
    unittest.main()
