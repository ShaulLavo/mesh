"""Listener failure controls without a Mesh seed or native service."""
import io
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from fixtures import installed_listener_evidence as evidence
from fixtures import installed_native, installed_order, installed_order_host


HOOK = '''import json,sys,threading
from fixtures.installed_listener_evidence import Observer
observer = Observer(sys.argv[1], sys.argv[2])
observer.stage("imports")
print(json.dumps({"endpoint": observer.endpoint}), flush=True)
sys.stdin.readline()
'''


class ListenerEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="listener-control-", dir=os.environ.get("MESH_SHORT_TMP", tempfile.gettempdir()))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.marker, self.token = self.root / "listener.json", "unit-observer-token"
        self.expected = {"marker": str(self.marker), "token": self.token, "shell": 101,
                         "shellStamp": str(os.getuid()) + " Thu Nov 12 12:00:00 2026"}
        self.shell = self.row(101, 100)
        self.child = self.row(102, 101)
        Path(str(self.marker) + ".pid").write_text("102\n")

    def row(self, pid, parent, status="live"):
        return {"pid": pid, "ppid": parent, "uid": os.getuid(), "status": status,
                "lstart": "Thu Nov 12 12:00:00 2026", "stat": "T" if status == "stopped" else "S"}

    def observation(self, **changes):
        value = {"pid": 102, "ppid": 101, "uid": os.getuid(), "token": self.token,
                 "positiveControl": True, "endpoint": 54321, "phase": "imports"} | changes
        Path(str(self.marker) + ".observation").write_text(json.dumps(value))

    def test_success_uses_original_predicate_and_emits_nothing(self):
        self.marker.write_text("original readiness marker")
        seen = []
        with patch.object(evidence, "capture") as capture:
            evidence.wait(None, self.marker, self.token, lambda predicate, message: seen.append((predicate(), message)))
        self.assertEqual(seen, [(True, "session listener did not start")])
        capture.assert_not_called()
        self.assertEqual(self.marker.read_text(), "original readiness marker")

    def test_original_timeout_survives_diagnostic_failure(self):
        original = RuntimeError("session listener did not start")
        with patch.object(evidence, "capture", side_effect=ValueError("diagnostic failed")) as capture:
            with self.assertRaises(RuntimeError) as raised:
                evidence.wait(None, self.marker, self.token, lambda *_: self.raise_error(original))
        self.assertIs(raised.exception, original)
        capture.assert_called_once_with(None, self.marker, self.token)

    @staticmethod
    def raise_error(error):
        raise error

    def test_pre_hook_state_is_explicit_and_does_not_request_frames(self):
        with patch.object(evidence, "process", return_value=self.child), patch.object(evidence, "request") as request:
            result = evidence.child_observation(self.expected, self.shell)
        self.assertEqual(result["interpreter"], {"entered": "unavailable", "hook": "unavailable"})
        request.assert_not_called()

    def test_interpreter_entry_distinguishes_observer_bootstrap_from_pre_entry(self):
        Path(str(self.marker) + ".entered").write_text("\n".join((self.token, "102", "101", str(os.getuid()))))
        result = evidence.interpreter(self.marker, self.token, self.child)
        self.assertEqual(result, {"entered": "observed", "hook": "unavailable"})

    def test_stopped_child_retains_stage_and_never_requests_frames(self):
        self.observation(phase="listener-create-bind")
        with patch.object(evidence, "process", return_value=self.child | {"status": "stopped"}), patch.object(evidence, "request") as request:
            result = evidence.child_observation(self.expected, self.shell)
        self.assertEqual(result["status"], "stopped")
        self.assertEqual(result["interpreter"]["lastPhase"], "listener-create-bind")
        self.assertEqual(result["interpreter"]["current"]["status"], "unavailable-stopped")
        request.assert_not_called()

    def test_exited_child_retains_only_historical_phase(self):
        self.observation(phase="marker-write")
        with patch.object(evidence, "process", return_value={"status": "exited", "pid": 102}), patch.object(evidence, "request") as request:
            result = evidence.child_observation(self.expected, self.shell)
        self.assertEqual(result["status"], "exited")
        self.assertEqual(result["lastObservation"]["lastPhase"], "marker-write")
        request.assert_not_called()

    def test_process_unavailable_and_missing_launch_have_distinct_labels(self):
        with patch.object(evidence, "process", side_effect=TimeoutError):
            result = evidence.child_observation(self.expected, self.shell)
        self.assertEqual(result["interpreter"]["status"], "unavailable-process-state")
        Path(str(self.marker) + ".pid").unlink()
        self.assertEqual(evidence.child_observation(self.expected, self.shell)["status"], "launch-pid-unavailable")

    def test_changed_shell_lifetime_uid_or_parent_refuses_interpreter_observation(self):
        variants = (self.child | {"ppid": 99}, self.child | {"uid": os.getuid() + 1})
        for child in variants:
            self.assert_refused(self.expected, child)
        self.assert_refused(self.expected | {"shellStamp": "different lifetime"}, self.child)

    def assert_refused(self, expected, child):
        with patch.object(evidence, "process", return_value=child), patch.object(evidence, "interpreter") as interpreter:
            result = evidence.child_observation(expected, self.shell)
        self.assertEqual(result["status"], "ownership-refused")
        interpreter.assert_not_called()

    def test_unarmed_or_wrong_token_never_requests_interpreter(self):
        for changes in ({"positiveControl": False}, {"token": "another fixture"}, {"pid": 103}):
            self.observation(**changes)
            with patch.object(evidence, "request") as request:
                result = evidence.interpreter(self.marker, self.token, self.child)
            self.assertIn(result["hook"], ("unavailable", "ownership-refused"))
            request.assert_not_called()

    def test_failed_current_request_keeps_armed_phase(self):
        self.observation()
        with patch.object(evidence, "request", side_effect=TimeoutError):
            result = evidence.interpreter(self.marker, self.token, self.child)
        self.assertEqual(result["lastPhase"], "imports")
        self.assertEqual(result["current"]["status"], "unavailable-or-ownership-refused")

    def test_current_response_with_another_identity_is_refused(self):
        self.observation()
        with patch.object(evidence, "request", return_value={"pid": 999, "ppid": 101, "uid": os.getuid()}):
            result = evidence.interpreter(self.marker, self.token, self.child)
        self.assertNotIn("pid", result["current"])
        self.assertEqual(result["current"]["status"], "unavailable-or-ownership-refused")

    def test_process_adapter_retains_pty_stop_wait_channel_and_birth(self):
        output = f" {os.getuid()} 102 101 102 101 T pts/4 Thu Nov 12 12:00:00 2026\n"
        results = (SimpleNamespace(returncode=0, stdout=output), SimpleNamespace(returncode=0, stdout="do_signal_stop\n"))
        with patch.object(evidence.subprocess, "run", side_effect=results) as run:
            result = evidence.process(102)
        self.assertEqual((result["status"], result["tty"], result["pgid"], result["tpgid"]), ("stopped", "pts/4", 102, 101))
        self.assertEqual(result["wchan"], "do_signal_stop")
        self.assertEqual(result["lstart"], "Thu Nov 12 12:00:00 2026")
        self.assertEqual(run.call_args_list[0].kwargs["timeout"], 0.25)
        self.assertNotIn("args=", run.call_args_list[0].args[0])

    def test_missing_wait_channel_does_not_erase_process_or_pty_state(self):
        output = f" {os.getuid()} 102 101 102 101 t pts/4 Thu Nov 12 12:00:00 2026\n"
        results = (SimpleNamespace(returncode=0, stdout=output), SimpleNamespace(returncode=1, stdout=""))
        with patch.object(evidence.subprocess, "run", side_effect=results):
            result = evidence.process(102)
        self.assertEqual(result["status"], "stopped")
        self.assertEqual(result["tty"], "pts/4")
        self.assertEqual(result["wchan"]["status"], "unavailable")

    def test_process_adapter_reports_exited_and_rejects_malformed_output(self):
        with patch.object(evidence.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="")):
            self.assertEqual(evidence.process(102), {"status": "exited", "pid": 102})
        with patch.object(evidence.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="truncated")):
            with self.assertRaisesRegex(ValueError, "incomplete"):
                evidence.process(102)

    def case(self):
        return SimpleNamespace(root=self.root, state=self.root / "state", session="owned", shell=101,
                               process_stamps={101: self.expected["shellStamp"]})

    def test_capture_bounds_failure_and_preserves_original_timeout(self):
        failures = (subprocess.TimeoutExpired("collector", evidence.SECONDS), ValueError("bad adapter"))
        for failure in failures:
            with patch.object(evidence.subprocess, "run", side_effect=failure) as run:
                evidence.capture(self.case(), self.marker, self.token)
            record = json.loads((self.root / evidence.FAILURE).read_text())
            self.assertEqual(record["errorType"], type(failure).__name__)
            self.assertEqual(run.call_args.kwargs["timeout"], evidence.SECONDS)
            self.assertEqual(json.loads(run.call_args.kwargs["input"])["shellStamp"], self.expected["shellStamp"])

    def test_capture_rejects_exit_output_and_shape_failures(self):
        results = ((1, ""), (0, "x" * (evidence.MAXIMUM + 1)), (0, "[]"), (0, "not json"))
        for code, output in results:
            with patch.object(evidence.subprocess, "run", return_value=SimpleNamespace(returncode=code, stdout=output)):
                evidence.capture(self.case(), self.marker, self.token)
            self.assertEqual(json.loads((self.root / evidence.FAILURE).read_text())["status"], "unavailable")
            self.assertLessEqual((self.root / evidence.FAILURE).stat().st_size, evidence.MAXIMUM)

    def test_capture_real_bounded_collector_without_any_mesh_process(self):
        self.expected["workerLog"] = str(self.root / "worker.log")
        (self.root / "worker.log").write_text("__ATTACHED__owned__END__\n")
        case = self.case()
        case.shell, case.process_stamps = os.getpid(), {os.getpid(): "intentionally unavailable ownership"}
        evidence.capture(case, self.marker, self.token)
        result = json.loads((self.root / evidence.FAILURE).read_text())
        self.assertEqual(result["scope"], "original listener readiness timeout")
        self.assertFalse(result["limits"]["signals"])
        self.assertLess(result["elapsedSeconds"], evidence.SECONDS + 0.5)

    def test_private_reads_refuse_links_and_oversized_files_and_bound_pty_tail(self):
        log = self.root / "worker.log"
        log.write_bytes(b"old" + b"x" * 8192)
        self.assertEqual(evidence.tail(log), "x" * 8192)
        link = self.root / "link"
        link.symlink_to(log)
        with self.assertRaisesRegex(ValueError, "regular"):
            evidence.read_private(link)
        with self.assertRaisesRegex(ValueError, "bound"):
            evidence.read_private(log, 10)

    def test_native_fresh_and_ordered_exports_keep_private_host_names(self):
        data = '{"listener":{"status":"stopped"}}\n'
        (self.root / evidence.FAILURE).write_text(data)
        output = self.root / "output"
        output.mkdir()
        for name in ("mac-interrupt", "fresh-managed"):
            installed_native.retain_logs(self.root, output, name)
            self.assertEqual((output / (name + "-" + evidence.FAILURE)).read_text(), data)
        with patch("sys.stdout", new_callable=io.StringIO) as stream:
            installed_order_host.export_logs(self.root)
        installed_order.publish_logs(stream.getvalue(), output, "canary-ordered")
        self.assertEqual((output / ("canary-ordered-" + evidence.FAILURE)).read_text(), data)
        self.assertFalse((output / "preserved-before.json").exists())

    def test_ordered_export_keeps_existing_file_and_transfer_bounds(self):
        with self.assertRaisesRegex(RuntimeError, "file bound"):
            installed_order_host.read_log(io.BytesIO(b"x" * 262145), evidence.FAILURE)
        with self.assertRaisesRegex(RuntimeError, "transfer bound"):
            installed_order.publish_logs("x" * ((2 << 20) + 1), self.root, "host")

    def test_diagnostic_read_or_export_errors_cannot_replace_primary_failure(self):
        (self.root / evidence.FAILURE).write_bytes(b"x" * (evidence.MAXIMUM + 1))
        output = self.root / "export"
        output.mkdir()
        installed_native.retain_logs(self.root, output, "fresh-managed")
        result = json.loads((output / ("fresh-managed-" + evidence.FAILURE)).read_text())
        self.assertEqual(result, {"status": "unavailable", "errorType": "ValueError"})
        with patch.object(Path, "write_bytes", side_effect=PermissionError):
            installed_native.retain_logs(self.root, output, "fresh-managed")

    def stop_hook(self):
        if self.hook is None:
            return
        try:
            self.hook.communicate("\n", timeout=1)
        except subprocess.TimeoutExpired:
            self.hook.kill()
            self.hook.communicate(timeout=1)
        self.assertIsNotNone(self.hook.poll())

    def start_hook(self):
        self.hook = None
        self.addCleanup(self.stop_hook)
        self.hook = subprocess.Popen([sys.executable, "-c", HOOK, str(self.marker), self.token],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            env=os.environ | {"PYTHONPATH": str(Path(__file__).resolve().parents[1]), "PYTHONDONTWRITEBYTECODE": "1"})
        ready = select.select([self.hook.stdout], [], [], 5)[0]
        self.assertTrue(ready, "isolated interpreter hook did not arm")
        return json.loads(self.hook.stdout.readline())

    def test_isolated_interpreter_positive_control_and_current_main_frames(self):
        hook = self.start_hook()
        current = evidence.request(hook["endpoint"], self.token)
        self.assertEqual((current["pid"], current["ppid"], current["phase"]), (self.hook.pid, os.getpid(), "imports"))
        self.assertTrue(any(frame["file"] == "<string>" for frame in current["frames"]))
        self.assertTrue(all(set(frame) == {"file", "function", "line"} for frame in current["frames"]))
        observation = json.loads(Path(str(self.marker) + ".observation").read_text())
        self.assertTrue(observation["positiveControl"])
        with self.assertRaises(TimeoutError):
            evidence.request(hook["endpoint"], "another fixture")

    def test_bounded_collector_observes_only_the_isolated_hook_child(self):
        self.start_hook()
        Path(str(self.marker) + ".pid").write_text(str(self.hook.pid))
        case = self.case()
        case.shell = os.getpid()
        case.process_stamps = {case.shell: installed_native.process_stamp(case.shell)}
        evidence.capture(case, self.marker, self.token)
        result = json.loads((self.root / evidence.FAILURE).read_text())
        self.assertEqual(result["listener"]["pid"], self.hook.pid)
        self.assertEqual(result["listener"]["interpreter"]["hook"], "armed")
        self.assertTrue(result["listener"]["interpreter"]["current"]["frames"])
        self.assertFalse(result["markerPresentAtCapture"])

    def test_isolated_hook_failure_cannot_publish_positive_control(self):
        code = '''import sys
from unittest.mock import patch
from fixtures import installed_listener_evidence as e
with patch.object(e, "request", side_effect=TimeoutError):
    try:
        e.Observer(sys.argv[1], sys.argv[2])
    except TimeoutError:
        pass
'''
        result = subprocess.run([sys.executable, "-c", code, str(self.marker), self.token],
            capture_output=True, text=True, timeout=3,
            env=os.environ | {"PYTHONPATH": str(Path(__file__).resolve().parents[1]), "PYTHONDONTWRITEBYTECODE": "1"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(Path(str(self.marker) + ".observation").exists())
