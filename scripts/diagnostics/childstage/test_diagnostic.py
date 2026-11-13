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

    def test_outer_guardian_preserves_new_session_after_controller_sigkill(self):
        with tempfile.TemporaryDirectory(prefix="childstage-outer-", dir="/work/tmp") as directory:
            result = exercise_controller_death(Path(directory), outer=True)
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

    def test_unrelated_eperm_cannot_satisfy_startup_capture(self):
        for number in (233, 54, 109):
            row = {"call": {"number": number, "args": [17]}, "tgid": 123, "tid": 124}
            self.assertFalse(capture.startup_call(row, 123, []))
        row = {"call": {"number": 56, "args": [0x5100]}, "tgid": 123, "tid": 124}
        self.assertFalse(capture.startup_call(row, 123, []))
        row["call"]["args"] = [0x4111]
        self.assertFalse(capture.startup_call(row, 123, []))
        row["context"] = {"terminals": [{"terminal": "/dev/ptmx"}, {"terminal": "/dev/pts/1"}]}
        self.assertTrue(capture.startup_call(row, 123, []))
        row = {"call": {"number": 112}, "tid": 124, "preExec": True}
        self.assertFalse(capture.startup_call(row, 123, []))
        created = {"event": "created", "tid": 124, "creatorTgid": 123, "preExec": True}
        self.assertTrue(capture.startup_call(row, 123, [created]))

    def test_vehicle_requires_exact_fresh_review_and_clean_checkout(self):
        driver = load("run-hosted")
        pins = json.loads((HERE / "driver-pins.json").read_bytes())
        with tempfile.TemporaryDirectory(prefix="childstage-source-", dir="/work/tmp") as temporary:
            output = Path(temporary)
            verify_source_gate(self, driver, pins, output)

    def test_vehicle_rejects_expired_packet_before_download(self):
        driver = load("run-hosted")
        from fixtures import installed_artifacts
        pins = json.loads((HERE / "driver-pins.json").read_bytes())
        artifact = {"id": pins["artifactId"], "name": pins["artifactName"], "size": pins["archiveBytes"],
                    "digest": "sha256:" + pins["archiveSha256"]}
        complete = {"runHead": pins["caller"], "artifacts": {"linux-amd64": artifact}}
        metadata = {"name": artifact["name"], "size_in_bytes": artifact["size"], "digest": artifact["digest"],
                    "workflow_run": {"id": pins["producerRun"], "head_sha": pins["caller"]}, "expired": True}
        with tempfile.TemporaryDirectory(prefix="childstage-expired-", dir="/work/tmp") as temporary:
            verify_expired_packet(self, driver, installed_artifacts, pins, complete, metadata, Path(temporary))

    def test_vehicle_image_bytes_cannot_be_substituted(self):
        driver = load("run-hosted")
        with tempfile.TemporaryDirectory(prefix="childstage-image-", dir="/work/tmp") as temporary:
            image = Path(temporary) / "image"
            image.write_bytes(b"original")
            expected = driver.digest(image)
            driver.verify_image(image, expected, 8)
            image.write_bytes(b"replaced")
            with self.assertRaisesRegex(RuntimeError, "selected image"):
                driver.verify_image(image, expected, 8)

    def test_selected_group_remains_the_original_31_images(self):
        import hashlib
        pins = json.loads((HERE / "driver-pins.json").read_bytes())
        raw = (HERE / "driver-selection.json").read_bytes()
        selected = json.loads(raw)
        self.assertEqual(hashlib.sha256(raw).hexdigest(), pins["selectionSha256"])
        self.assertEqual(len(selected["group"]["members"]), 31)
        self.assertEqual(selected["group"]["index"], 1)
        self.assertEqual(selected["group"]["members"][15]["version"], "v0.1.225")
        self.assertEqual(selected["group"]["members"][15]["artifact"]["binarySha256"], pins["workerDigest"])
        self.assertEqual(selected["originalBytes"] + sum(row["imageBytes"] for row in selected["group"]["members"]), 934547328)

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


def synthetic_outer_controller(root):
    driver = load("run-hosted")
    code = 'import json,os,subprocess,sys,time; from pathlib import Path; child=subprocess.Popen(["/bin/sleep","60"],start_new_session=True); Path(sys.argv[1]).write_text(json.dumps({"root":os.getpid(),"child":child.pid,"sid":os.getsid(0)})); time.sleep(60)'
    with (root / "child.log").open("wb") as log:
        return driver.execute_guarded([sys.executable, "-c", code, str(root / "descendant.json")], dict(os.environ), log, root)


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


def verify_source_gate(test, driver, pins, output):
    source = "a" * 40
    with patch.dict(os.environ, {"REVIEWED_DIAGNOSTIC_SOURCE": source}), patch.object(driver.subprocess, "check_output", side_effect=[source + "\n", b""]):
        driver.validate_source(output, pins)
    with patch.dict(os.environ, {"REVIEWED_DIAGNOSTIC_SOURCE": source}), patch.object(driver.subprocess, "check_output", side_effect=[source + "\n", b"?? unreviewed.py"]):
        with test.assertRaisesRegex(RuntimeError, "not clean"):
            driver.validate_source(output, pins)
    with patch.dict(os.environ, {"REVIEWED_DIAGNOSTIC_SOURCE": "b" * 40}), patch.object(driver.subprocess, "check_output", return_value=source + "\n"):
        with test.assertRaisesRegex(RuntimeError, "fresh review pin"):
            driver.validate_source(output, pins)


def verify_expired_packet(test, driver, artifacts, pins, complete, metadata, output):
    with patch.object(artifacts, "verify_set", return_value=complete), patch.object(artifacts, "api", return_value=metadata), patch.object(artifacts, "fetch_archive") as fetch:
        with test.assertRaisesRegex(RuntimeError, "exact live pin"):
            driver.acquire(output, output, pins)
    fetch.assert_not_called()


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


def ptrace_controller(root, observer):
    code = 'import json,os,subprocess,sys,time; from pathlib import Path; child=subprocess.Popen(["/bin/sleep","60"],start_new_session=True); Path(sys.argv[1]).write_text(json.dumps({"root":os.getpid(),"child":child.pid})); time.sleep(60)'
    return capture.ptrace(observer, root, "death", [sys.executable, "-c", code, str(root / "descendant.json")], 25)


def exercise_ptrace_death(root, observer, victim):
    previous = backend.subreaper()
    controller, pids = None, []
    try:
        controller = subprocess.Popen([sys.executable, str(Path(__file__)), "--ptrace-controller", str(root), str(observer)],
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        started = wait_json(root / "death.context.json")
        descendant = wait_json(root / "descendant.json")
        pids = [started["guardian"], started["observer"], descendant["root"], descendant["child"]]
        os.kill(controller.pid if victim == "controller" else started["observer"], signal.SIGKILL)
        controller.wait(timeout=12)
        if victim == "controller":
            wait_guardian(started["guardian"])
        backend.reap_adopted()
        remaining = [pid for pid in pids if Path(f"/proc/{pid}").exists()]
        if remaining or controller.returncode == 0:
            raise RuntimeError("ptrace death control left owned tasks or reported success")
        return {"victim": victim, "pids": pids, "remainingOwnedTasks": remaining,
                "controllerExit": controller.returncode, "passed": True}
    finally:
        remove_test_tasks(controller, pids)
        backend.subreaper(previous)


def exercise_controller_death(root, outer=False):
    previous = backend.subreaper()
    controller, pids = None, []
    try:
        flag = "--outer-controller" if outer else "--controller"
        controller = subprocess.Popen([sys.executable, str(Path(__file__)), flag, str(root)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        started = controller_identity(controller, root, outer)
        descendant = wait_json(root / "descendant.json")
        if outer and descendant["sid"] != descendant["root"]:
            raise RuntimeError("outer guardian changed the original new-session setting")
        pids = [started["guardian"], started["root"], descendant["child"]]
        os.kill(controller.pid, signal.SIGKILL)
        controller.wait(timeout=3)
        wait_guardian(started["guardian"])
        return {"guardianGone": not Path(f"/proc/{pids[0]}").exists(), "rootGone": not Path(f"/proc/{pids[1]}").exists(),
                "detachedDescendantGone": not Path(f"/proc/{pids[2]}").exists()}
    finally:
        remove_test_tasks(controller, pids)
        backend.subreaper(previous)


def controller_identity(controller, root, outer):
    if outer:
        return wait_json(root / "outer-guardian.json")
    if not select.select([controller.stdout], [], [], 5)[0]:
        raise RuntimeError("synthetic controller failed to publish its guardian")
    return json.loads(controller.stdout.readline())


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--controller":
        synthetic_controller(Path(sys.argv[2]))
    elif len(sys.argv) == 3 and sys.argv[1] == "--outer-controller":
        raise SystemExit(synthetic_outer_controller(Path(sys.argv[2])))
    elif len(sys.argv) == 4 and sys.argv[1] == "--ptrace-controller":
        raise SystemExit(ptrace_controller(Path(sys.argv[2]), Path(sys.argv[3])))
    else:
        unittest.main()
