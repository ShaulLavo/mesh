"""Offline checks of image binding, refusal selection and diagnostic isolation."""
import importlib.util
import json
import io
import os
from pathlib import Path
import tempfile
import select
import signal
import subprocess
import sys
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import backend
import capture


HERE = Path(__file__).resolve().parent


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DiagnosticTests(unittest.TestCase):
    def test_kernel_rows_have_backend_neutral_sequences(self):
        run = backend.KernelRun(Path("/unused"), "offline")
        first = {"event": "exec", "device": 0, "inode": 1, "bytes": 2}
        row = {"event": "return", "number": 56, "entryNumber": 56, "args": [], "enteredNs": 1,
               "clone3Flags": 0, "device": 0, "inode": 1, "bytes": 2}
        row.update(dict.fromkeys(("uid", "euid", "gid", "egid", "ppid", "pgid", "sid", "seccomp", "cgroupId", "startBootNs"), 1))
        self.assertEqual(run.record(json.dumps(first), io.StringIO())["sequence"], 1)
        self.assertEqual(run.record(json.dumps(row), io.StringIO())["sequence"], 2)

    def test_cleanup_errors_do_not_skip_known_tasks_hooks_or_descriptors(self):
        run = backend.KernelRun(Path("/unused"), "offline")
        descriptor = os.open("/dev/null", os.O_RDONLY)
        run.tasks[123] = descriptor
        actions = []
        run.target = SimpleNamespace(wait=lambda timeout: actions.append("wait"), close=lambda: actions.append("guardian"))
        run.hook = object()
        with patch.object(run, "discover", side_effect=RuntimeError("discovery bound")), patch.object(run, "settle", side_effect=lambda: actions.append("settle")), patch.object(run, "stop_hook", side_effect=lambda: actions.append("hook")), patch.object(backend, "wait_hook", side_effect=RuntimeError("hook wait")), patch.object(backend, "kill_owned", side_effect=lambda fd: actions.append("kill")), patch.object(backend, "pidfd_alive", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "discovery bound.*hook wait"):
                run.cleanup()
        self.assertEqual(actions, ["kill", "wait", "hook", "guardian", "settle"])
        self.assertEqual(run.tasks, {})
        with self.assertRaises(OSError):
            os.fstat(descriptor)

    def test_released_tree_is_removed_after_controller_sigkill(self):
        with tempfile.TemporaryDirectory(prefix="childstage-guardian-", dir="/work/tmp") as directory:
            result = exercise_controller_death(Path(directory))
        self.assertTrue(result["guardianGone"])
        self.assertTrue(result["rootGone"])
        self.assertTrue(result["detachedDescendantGone"])

    def test_preexisting_cohort_is_never_owned_or_stopped(self):
        with tempfile.TemporaryDirectory(prefix="childstage-preexisting-", dir="/work/tmp") as temporary:
            root = Path(temporary)
            cohort = root / "cohort.json"
            cohort.write_text('{"workers":[]}')
            argv = ["capture.py", "--authorized-disposable-diagnostic", "--observer", "/unused", "--control", "/unused",
                "--output", str(root / "output"), "--cohort-tool", "/unused", "state", "config", "registry", "version", "binary", str(cohort)]
            verify_preexisting(root, cohort, argv)

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


def synthetic_controller(root):
    read, write = os.pipe()
    ready_read, ready_write = os.pipe()
    code = 'import json,os,subprocess,sys,time; from pathlib import Path; child=subprocess.Popen(["/bin/sleep","60"],start_new_session=True); Path(sys.argv[1]).write_text(json.dumps({"root":os.getpid(),"child":child.pid})); time.sleep(60)'
    with (root / "child.log").open("wb") as log:
        target = backend.GuardedTarget([sys.executable, str(HERE / "barrier.py"), str(read), str(ready_write),
            sys.executable, "-c", code, str(root / "descendant.json")], (read, ready_write), log)
        os.close(read)
        os.close(ready_write)
        backend.wait_barrier(ready_read)
        os.close(ready_read)
        os.write(write, b"x")
        os.close(write)
        print(json.dumps({"guardian": target.guardian.pid, "root": target.pid}), flush=True)
        sys.stdin.buffer.read(1)


def verify_preexisting(root, cohort, argv):
    with patch.object(sys, "argv", argv), patch.object(backend, "require_disposable"), patch.object(capture, "stop_cohort") as stop:
        try:
            capture.main()
        except RuntimeError as error:
            assert "pre-existing" in str(error)
        else:
            raise AssertionError("pre-existing cohort accepted")
    stop.assert_not_called()
    assert cohort.read_text() == '{"workers":[]}'
    assert not (root / "output").exists()


def wait_json(path):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            return json.loads(path.read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            time.sleep(0.01)
    raise RuntimeError("synthetic descendant did not publish its PID")


def remove_test_tasks(controller, pids):
    if controller is not None and controller.poll() is None:
        controller.kill()
        controller.wait(timeout=3)
    for pid in pids:
        try:
            descriptor = os.pidfd_open(pid)
            backend.kill_owned(descriptor)
            os.close(descriptor)
        except ProcessLookupError:
            pass
    backend.reap_adopted()
    if controller is not None:
        controller.stdin.close()
        controller.stdout.close()


def wait_guardian(pid):
    descriptor = os.pidfd_open(pid)
    try:
        if not select.select([descriptor], [], [], 8)[0]:
            raise RuntimeError("independent guardian did not finish subtree cleanup")
        os.waitpid(pid, 0)
    finally:
        os.close(descriptor)


def exercise_controller_death(root):
    previous = backend.subreaper()
    controller, pids = None, []
    try:
        controller = subprocess.Popen([sys.executable, str(Path(__file__)), "--controller", str(root)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        if not select.select([controller.stdout], [], [], 5)[0]:
            raise RuntimeError("synthetic controller failed to publish its guardian")
        started = json.loads(controller.stdout.readline())
        descendant = wait_json(root / "descendant.json")
        pids = [started["guardian"], started["root"], descendant["child"]]
        os.kill(controller.pid, signal.SIGKILL)
        controller.wait(timeout=3)
        wait_guardian(started["guardian"])
        return {"guardianGone": not Path(f"/proc/{pids[0]}").exists(), "rootGone": not Path(f"/proc/{pids[1]}").exists(),
                "detachedDescendantGone": not Path(f"/proc/{pids[2]}").exists()}
    finally:
        remove_test_tasks(controller, pids)
        backend.subreaper(previous)


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--controller":
        synthetic_controller(Path(sys.argv[2]))
    else:
        unittest.main()
