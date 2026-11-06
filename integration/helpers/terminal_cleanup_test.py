#!/usr/bin/env python3
"""Cancellation must enter a fixture's real cleanup once, inside one bounded allowance."""

import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest

from terminal_window import CleanupDeadline, Fixture, close_fixture

HELPERS = Path(__file__).resolve().parent
LINUX = sys.platform == "linux"
# A coordinator whose fixture cleanup blocks on a final-write barrier until the test releases it.
COORDINATOR = r'''
import os
from pathlib import Path
import sys
import time
sys.path.insert(0, sys.argv[1])
import terminal_window

out, mode = Path(sys.argv[2]), sys.argv[3]


class Recorded(terminal_window.Fixture):
    def close(self, allowance=terminal_window.CLEANUP_SECONDS):
        with (out / "entered").open("a") as entered:
            entered.write("x")
        (out / "root").write_text(str(self.root))
        while not (out / "release").exists():
            time.sleep(0.01)
        super().close(allowance)
        (out / "finished").write_text("final writes done")


def scenario(fixture):
    (out / "scenario").write_text(str(os.getpid()))
    if mode == "cancel":
        time.sleep(60)


terminal_window.run_fixture(Recorded, "/bin/true", "mesh-window-", scenario, "fixture limit")
'''
# A containing worker stand-in: its arguments match the product's ancestry walk.
SESSION_WORKER = r'''
import ctypes
import json
from pathlib import Path
import signal
import subprocess
import sys
import time

out, helpers, subreaper = Path(sys.argv[5]), sys.argv[6], sys.argv[7] == "subreaper"
for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(signum, lambda *_: (out / "ancestor-signalled").touch())
if subreaper:
    ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0)
program = (
    "import json, os, sys\n"
    "sys.path.insert(0, sys.argv[1])\n"
    "from pathlib import Path\n"
    "from terminal_window import run_outside_containing_session\n"
    "out = Path(sys.argv[2])\n"
    "def runner():\n"
    "    chain, pid = [], os.getpid()\n"
    "    while pid > 1:\n"
    "        chain.append(pid)\n"
    "        pid = int(Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[1])\n"
    "    (out / 'chain').write_text(json.dumps(chain))\n"
    "    return 0\n"
    "sys.exit(run_outside_containing_session(runner))\n"
)
child = subprocess.run([sys.executable, "-B", "-c", program, helpers, str(out)], stderr=subprocess.PIPE, text=True)
(out / "result").write_text(json.dumps({"code": child.returncode, "stderr": child.stderr}))
while not (out / "release").exists():
    time.sleep(0.01)
'''


def wait_until(predicate, description, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.01)
    raise AssertionError("timed out waiting for " + description)


class Scratch(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="m-cleanup-test-"))
        self.processes = []
        self.addCleanup(self.teardown_scratch)

    def teardown_scratch(self):
        (self.root / "release").touch()
        for process in self.processes:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
        shutil.rmtree(self.root)


class CancelledCleanupTest(Scratch):
    def coordinator(self, mode):
        tmp = self.root / "tmp"
        tmp.mkdir()
        environment = {"PATH": os.defpath, "HOME": str(self.root), "TMPDIR": str(tmp), "PYTHONDONTWRITEBYTECODE": "1"}
        process = subprocess.Popen([sys.executable, "-B", "-c", COORDINATOR, str(HELPERS), str(self.root), mode],
                                   env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.processes.append(process)
        return process, tmp

    def assert_cleanup_held_then_finished(self, process, tmp, status):
        root = Path(wait_until(lambda: (self.root / "root").exists() and (self.root / "root").read_text(),
                               "cleanup to start"))
        for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            process.send_signal(signum)
        time.sleep(0.2)
        self.assertIsNone(process.poll(), "a later signal interrupted cleanup")
        self.assertEqual((self.root / "entered").read_text(), "x", "cleanup was entered more than once")
        self.assertTrue(root.is_dir(), "the fixture root was removed before its final writes")
        (self.root / "release").touch()
        _, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, status, stderr)
        self.assertEqual((self.root / "finished").read_text(), "final writes done")
        self.assertEqual(list(tmp.iterdir()), [], "the fixture root survived a finished cleanup")

    def test_termination_enters_fixture_cleanup_once(self):
        process, tmp = self.coordinator("cancel")
        wait_until(lambda: (self.root / "scenario").exists(), "the scenario to start")
        process.send_signal(signal.SIGTERM)
        self.assert_cleanup_held_then_finished(process, tmp, 128 + signal.SIGTERM)

    def test_cancellation_during_active_cleanup_lets_it_finish(self):
        process, tmp = self.coordinator("normal")
        self.assert_cleanup_held_then_finished(process, tmp, 0)

    def test_escaped_coordinator_parent_outlives_its_cleanup(self):
        program = (
            "import os, sys, time\n"
            "sys.path.insert(0, sys.argv[1])\n"
            "from pathlib import Path\n"
            "from isolation import termination_cleanup\n"
            "from terminal_window import run_outside_containing_session\n"
            "out = Path(sys.argv[2])\n"
            "def final_writes():\n"
            "    (out / 'cleanup').touch()\n"
            "    while not (out / 'release').exists():\n"
            "        time.sleep(0.01)\n"
            "def runner():\n"
            "    with termination_cleanup():\n"
            "        try:\n"
            "            (out / 'escaped').write_text(str(os.getpid()))\n"
            "            time.sleep(60)\n"
            "        finally:\n"
            "            final_writes()\n"
            "sys.exit(run_outside_containing_session(runner))\n"
        )
        parent = subprocess.Popen([sys.executable, "-B", "-c", program, str(HELPERS), str(self.root)])
        self.processes.append(parent)
        escaped = int(wait_until(lambda: (self.root / "escaped").exists() and (self.root / "escaped").read_text(),
                                 "the escaped coordinator"))
        # The job owner signals every process that existed at cancellation, the waiting parent included.
        for pid in (parent.pid, escaped):
            os.kill(pid, signal.SIGTERM)
        wait_until(lambda: (self.root / "cleanup").exists(), "the escaped coordinator's cleanup")
        time.sleep(0.2)
        self.assertIsNone(parent.poll(), "the parent exited while the escaped cleanup was still running")
        (self.root / "release").touch()
        self.assertEqual(parent.wait(timeout=10), 1)


class Unresponsive:
    """A daemon that ignores TERM and KILL, the way a process stuck in the kernel does."""

    def terminate(self):
        pass

    def kill(self):
        pass

    def wait(self, timeout):
        time.sleep(timeout)
        raise subprocess.TimeoutExpired("daemon", timeout)


class BoundedCleanupTest(Scratch):
    def fixture(self):
        fixture = Fixture("/bin/true", self.root / "fixture")
        return fixture

    def setUp(self):
        super().setUp()
        (self.root / "fixture").mkdir()

    def silent_workers(self, fixture, count):
        listeners = []
        for index in range(count):
            directory = fixture.local / "s" / f"W{index}"
            directory.mkdir(parents=True)
            listener = socket.socket(socket.AF_UNIX)
            listener.bind(str(directory / "sock"))
            listener.listen()
            listeners.append(listener)
            self.addCleanup(listener.close)
        return listeners

    def test_serial_worker_close_shares_one_deadline(self):
        fixture = self.fixture()
        self.silent_workers(fixture, 3)
        started = time.monotonic()
        with self.assertRaisesRegex(RuntimeError, "ran out of time"):
            fixture.close(allowance=0.6)
        # Three workers would allow 42 seconds one after another; the shared deadline ends it.
        self.assertLess(time.monotonic() - started, 2)

    def test_daemon_surviving_kill_is_reported_within_the_allowance(self):
        fixture = self.fixture()
        fixture.daemon = Unresponsive()
        started = time.monotonic()
        with self.assertRaises((RuntimeError, subprocess.TimeoutExpired)):
            fixture.close(allowance=0.5)
        self.assertLess(time.monotonic() - started, 1.5)
        self.assertIsNotNone(fixture.daemon, "an unproved daemon exit was recorded as stopped")

    def test_incomplete_cleanup_keeps_the_fixture_root(self):
        fixture = self.fixture()
        self.silent_workers(fixture, 1)
        fixture.close = lambda: Fixture.close(fixture, allowance=0.3)
        with self.assertRaisesRegex(RuntimeError, "ran out of time"):
            close_fixture(fixture, fixture.root)
        self.assertTrue(fixture.root.is_dir(), "a fixture root was deleted after unproved cleanup")

    def test_clock_injected_deadline_never_extends(self):
        now = [0.0]
        deadline = CleanupDeadline(25, clock=lambda: now[0])
        exchange = deadline.within(10)
        now[0] = 9.0
        self.assertEqual(exchange.remaining(10, "frame"), 1.0)
        self.assertEqual(deadline.remaining(4, "socket removal"), 4)
        now[0] = 24.5
        late = deadline.within(10)
        self.assertEqual(late.remaining(10, "frame"), 0.5)
        now[0] = 25.0
        with self.assertRaisesRegex(RuntimeError, "ran out of time before socket removal"):
            deadline.remaining(4, "socket removal")


@unittest.skipUnless(LINUX, "ancestry is read from /proc")
class AncestryTest(Scratch):
    def run_under_session_worker(self, topology):
        script = self.root / "session-worker"
        script.write_text(SESSION_WORKER)
        ancestor = subprocess.Popen([sys.executable, "session-worker", "--id", "7K3D", "--dir", "/x/7K3D",
                                     str(self.root), str(HELPERS), topology], cwd=self.root)
        self.processes.append(ancestor)
        result = wait_until(lambda: (self.root / "result").exists() and (self.root / "result").read_text(),
                            "the escaped coordinator's result")
        self.assertIsNone(ancestor.poll(), "the containing ancestor did not survive")
        self.assertFalse((self.root / "ancestor-signalled").exists(), "the containing ancestor was signalled")
        return ancestor.pid, json.loads(result)

    def test_ordinary_escape_leaves_the_containing_worker(self):
        ancestor, result = self.run_under_session_worker("ordinary")
        self.assertEqual(result["code"], 0, result["stderr"])
        chain = json.loads((self.root / "chain").read_text())
        self.assertNotIn(ancestor, chain)

    def test_intervening_subreaper_refuses_before_the_scenario(self):
        _, result = self.run_under_session_worker("subreaper")
        self.assertEqual(result["code"], 1)
        self.assertIn("ancestry-sensitive fixtures refuse to run there", result["stderr"])
        self.assertFalse((self.root / "chain").exists(), "the scenario ran with the wrong ancestry")


if __name__ == "__main__":
    unittest.main()
