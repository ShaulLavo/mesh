"""Pure and mock controls for original attachment failure evidence."""
import base64
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import installed_attachment_evidence as evidence, installed_native


class AttachmentEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="attachment-control-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.case = SimpleNamespace(root=self.root, state=self.root / "state", session="ABCD",
            worker=102, shell=103, listener={"pid": 104}, process_stamps={102: "worker-birth", 103: "shell-birth"},
            terminal=SimpleNamespace(pid=101, snapshot=Mock(return_value=b"disconnected"), poll=Mock(return_value=0), expect=Mock()))

    def test_success_preserves_marker_and_performs_no_capture(self):
        with patch.object(evidence, "capture") as capture:
            evidence.expect(self.case, "exact-token", 42)
        self.case.terminal.expect.assert_called_once_with("__RESUMED__exact-token__", since=42)
        capture.assert_not_called()

    def test_failure_preserves_original_exception_despite_collector_error(self):
        original = RuntimeError("client exited with status 0")
        self.case.terminal.expect.side_effect = original
        with patch.object(evidence, "capture", side_effect=ValueError("capture failed")) as capture:
            with self.assertRaises(RuntimeError) as raised:
                evidence.expect(self.case, "token", 42)
        self.assertIs(raised.exception, original)
        capture.assert_called_once_with(self.case, "token", 42)

    def test_capture_retains_original_client_and_uses_bounded_child(self):
        result = SimpleNamespace(returncode=0, stdout='{"schema":1,"client":{"pid":101,"exitStatus":0}}')
        with patch.object(evidence.subprocess, "run", return_value=result) as run:
            evidence.capture(self.case, "token", 42)
        expected = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(expected["processes"], {"client": 101, "worker": 102, "shell": 103, "listener": 104})
        self.assertEqual(expected["challenge"], {"token": "token", "clientOutputStart": 42})
        self.assertEqual(base64.b64decode(expected["transcript"]), b"disconnected")
        self.assertTrue(any(frame["function"] == "capture" for frame in expected["resumeStack"]))
        self.assertGreater(run.call_args.kwargs["timeout"], 0)
        self.assertLessEqual(run.call_args.kwargs["timeout"], 2)
        self.assertEqual(run.call_args.args[0][1:], ["-m", "fixtures.installed_attachment_evidence"])
        self.assertEqual(run.call_args.kwargs["cwd"], Path(evidence.__file__).resolve().parents[1])
        self.assertEqual(json.loads((self.root / evidence.FAILURE).read_text())["client"]["exitStatus"], 0)
        self.case.terminal.snapshot.assert_called_once_with()
        self.case.terminal.poll.assert_called_once_with()

    def test_expired_clock_skips_child_and_records_unavailable(self):
        with patch.object(evidence.installed_clock, "timeout", side_effect=evidence.installed_clock.ClockExpired("expired")), \
                patch.object(evidence.subprocess, "run") as run:
            evidence.capture(self.case, "token", 42)
        run.assert_not_called()
        self.assertEqual(json.loads((self.root / evidence.FAILURE).read_text())["errorType"], "ClockExpired")

    def test_oversize_child_output_is_not_retained(self):
        result = SimpleNamespace(returncode=0, stdout="x" * (evidence.MAXIMUM + 1))
        with patch.object(evidence.subprocess, "run", return_value=result):
            evidence.capture(self.case, "token", 42)
        self.assertLess((self.root / evidence.FAILURE).stat().st_size, 256)

    def test_collect_retains_live_metadata_and_bounded_pty_without_commands(self):
        session = self.case.state / "s" / self.case.session
        session.mkdir(parents=True)
        (session / "meta.json").write_text(json.dumps({"id": "ABCD", "pid": 103, "state": "detached", "command": ["private"]}))
        (session / "worker.log").write_bytes(b"x" * 10000 + b"__RESUMED__token__")
        update = self.case.state / "update"
        update.mkdir()
        (update / "installation.json").write_text(json.dumps({"phase": "committed", "request": {
            "manifest": {"version": "v0.1.9002"}, "private": "omitted"}}))
        expected = {"state": str(self.case.state), "session": "ABCD", "client": {"pid": 101, "exitStatus": 0},
            "processes": {"client": 101, "worker": 102}, "stamps": {}, "challenge": {"token": "token"},
            "transcript": base64.b64encode(b"client EOF").decode(), "resumeStack": []}
        with patch.object(evidence.evidence, "process", side_effect=lambda pid: {"pid": pid, "status": "live"}):
            record = evidence.collect(expected)
        self.assertEqual(record["workerMeta"], {"id": "ABCD", "pid": 103, "state": "detached"})
        self.assertEqual(record["journal"], {"phase": "committed", "version": "v0.1.9002"})
        self.assertEqual(len(record["workerPtyTail"]), 8192)
        self.assertTrue(record["workerPtyTail"].endswith("__RESUMED__token__"))
        self.assertEqual(record["clientPtyTail"], "client EOF")
        self.assertFalse(record["limits"]["signals"])
        self.assertFalse(record["limits"]["controlRequests"])

    def test_native_resume_replaces_challenge_once_and_keeps_original_terminal(self):
        terminal = self.case.terminal
        self.case.challenge = self.root / "listener.challenge"
        terminal.drain = Mock(return_value=b"old-output")
        with patch.object(evidence, "expect") as expect:
            installed_native.Installation.wait_attachment_resume(self.case)
        self.assertIs(self.case.terminal, terminal)
        expect.assert_called_once_with(self.case, self.case.challenge.read_text(), 10)
        self.assertFalse(self.case.challenge.with_suffix(".tmp").exists())

    def test_retention_uses_existing_native_log_path(self):
        (self.root / evidence.FAILURE).write_text('{"schema":1}')
        output = self.root / "output"
        output.mkdir()
        installed_native.retain_logs(self.root, output, "pc-chain")
        self.assertEqual((output / ("pc-chain-" + evidence.FAILURE)).read_text(), '{"schema":1}')

    def test_child_entry_point_reads_only_synthetic_fixture_files(self):
        expected = {"state": str(self.root / "absent-state"), "session": "ABCD", "client": {"exitStatus": 0},
            "processes": {}, "stamps": {}, "challenge": {"token": "token"},
            "transcript": base64.b64encode(b"original client transcript").decode(), "resumeStack": []}
        result = subprocess.run([sys.executable, "-m", "fixtures.installed_attachment_evidence"],
            cwd=Path(evidence.__file__).resolve().parents[1], input=json.dumps(expected),
            capture_output=True, text=True, timeout=2, check=True)
        record = json.loads(result.stdout)
        self.assertEqual(record["clientPtyTail"], "original client transcript")
        self.assertEqual(record["processes"], {})
        self.assertEqual(record["workerMeta"]["errorType"], "FileNotFoundError")
