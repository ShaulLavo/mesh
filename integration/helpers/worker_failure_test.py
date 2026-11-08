#!/usr/bin/env python3
"""Prove failure diagnostics against the same real window worker as normal CI."""
from contextlib import redirect_stderr
import io
import json
from pathlib import Path
import shutil
import socket
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from failed_fixture import owned_processes, stop_owned
from terminal_window import CleanupDeadline, Fixture, close_fixture, run_outside_containing_session
from worker_failure import STACK_LINES, WorkerFailure, stack_summary


BINARY = str(Path(sys.argv.pop(1)).resolve()) if len(sys.argv) > 1 else None


class WorkerFailureTest(unittest.TestCase):
    def test_summary_keeps_stages_without_arguments_registers_or_absolute_paths(self):
        dump = (b"SIGQUIT: quit\nPC=0x123\n\ngoroutine 23 gp=0xDEAD m=3 [select]:\n"
                b"github.com/shaul/mesh/internal/worker.(*Worker).kill(0x123, 0x456)\n"
                b"\t/private/build/internal/worker/serve.go:337 +0x111\nrax 0x789\n")
        self.assertEqual(stack_summary(dump), ["goroutine 23 [select]:",
                         "github.com/shaul/mesh/internal/worker.(*Worker).kill", "/internal/worker/serve.go:337"])
        self.assertEqual(len(stack_summary(dump * 1000)), STACK_LINES)

    def test_uninstrumented_peer_and_diagnostic_error_keep_original_failure(self):
        with tempfile.TemporaryDirectory(prefix="m-wdiag-unit-") as temporary:
            fixture = Fixture("/bin/true", temporary)
            connection = socket.socket(socket.AF_UNIX)
            self.addCleanup(connection.close)
            observer = WorkerFailure(fixture, fixture.local / "s/TEST/sock", connection, CleanupDeadline(1))
            original = RuntimeError("original completion failure")
            self.assert_original_error(observer, original)

    def assert_original_error(self, observer, original):
        with redirect_stderr(io.StringIO()), patch.object(observer, "capture", side_effect=OSError("diagnostic failure")):
            with self.assertRaises(RuntimeError) as raised, observer:
                raise original
        self.assertIs(raised.exception, original)


@unittest.skipUnless(BINARY and sys.platform == "linux", "pass a real Mesh binary on Linux")
class RealWindowWorkerFailureTest(unittest.TestCase):
    def fixture(self):
        root = Path(tempfile.mkdtemp(prefix="m-wdiag-"))
        # Register teardown before any terminal or daemon can start.
        self.addCleanup(self.cleanup_fixture, root)
        self.fixture_instance = Fixture(BINARY, root)
        return self.fixture_instance

    def cleanup_fixture(self, root):
        fixture = self.fixture_instance
        stop_owned(root, fixture.binary, {})
        for terminal in fixture.terminals:
            terminal.poll()
        self.assertFalse(owned_processes(root, fixture.binary), "diagnostic fixture still has owned processes")
        shutil.rmtree(root)

    def test_success_is_silent_and_does_not_dump_or_stop_a_worker(self):
        fixture = self.fixture()
        terminal = fixture.window()
        output = io.StringIO()
        with redirect_stderr(output):
            fixture.close()
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(list(fixture.root.glob("failure-worker-*.json")), [])
        self.assertIsNotNone(terminal.poll())
        for log in fixture.local.glob("s/*/worker.log"):
            self.assertNotIn("SIGQUIT", log.read_text())
        print("PASS: real window worker successful cleanup emitted no diagnostic", flush=True)

    def test_real_kill_timeout_dumps_the_connected_worker_and_keeps_failure(self):
        fixture = self.fixture()
        terminal = fixture.window()
        boundary = len(terminal.drain())
        terminal.send("trap '' HUP; printf '__HUP_IGNORED__\\n'\n")
        terminal.expect(b"__HUP_IGNORED__\r\n", since=boundary)

        class ShortExchange(CleanupDeadline):
            def within(self, seconds):
                return super().within(min(seconds, 0.1))

        started = time.monotonic()
        output = io.StringIO()
        with patch("terminal_window.CleanupDeadline", ShortExchange), redirect_stderr(output), \
                self.assertRaisesRegex(RuntimeError, "answered its kill") as raised:
            close_fixture(fixture, fixture.root, allowance=2)
        self.assertLess(time.monotonic() - started, 2)
        evidence, = fixture.root.glob("failure-worker-*.json")
        record = json.loads(evidence.read_text())
        self.assertEqual(record["failure"], str(raised.exception))
        self.assertIsNone(record["unavailable"])
        self.assertEqual(record["child_at_arm"]["parent_pid"], record["worker"]["pid"])
        self.assertTrue(record["child_still_live"])
        self.assertEqual(record["signal"], "SIGQUIT")
        self.assertTrue(record["worker_exited"], "runtime dump did not complete in the original allowance")
        stack = "\n".join(record["stack"])
        for stage in (".kill", ".killAndAcknowledge", ".waitChild", ".pump"):
            self.assertIn(stage, stack, f"real worker stack omitted {stage}")
        self.assertNotIn("0x", stack)
        self.assertIn("fixture.worker_failure", output.getvalue())
        self.assertTrue(fixture.root.exists(), "failure incorrectly authorized state deletion")
        print("PASS: real kill timeout kept its exception and captured handler, child wait and PTY pump", flush=True)
        print(json.dumps(record, sort_keys=True), flush=True)


def main():
    suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    raise SystemExit(run_outside_containing_session(main))
