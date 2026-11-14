#!/usr/bin/env python3
"""Owned subprocess controls for fixture startup, not a kernel EPERM reproducer."""
from contextlib import closing
import hashlib
import json
import math
import os
from pathlib import Path
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

from fixtures import installed_clock, release_worker_eperm as eperm, release_workers as workers


LOG = 'worker: start /bin/sh: child-start path="/bin/sh" errno=1 stage=unavailable: fork/exec /bin/sh: operation not permitted'


class OwnedStartup(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mesh-startup-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.processes = []
        self.addCleanup(self.reap)
        self.binary = Path(sys.executable).resolve()
        self.script = self.root / "synthetic-worker.py"
        self.script.write_text(self.program())
        self.arguments = [str(self.binary), str(self.script), str(self.root)]
        self.path = self.root / "cohort.json"
        self.row = {"session": "OWNED", "pid": 0, "shellPid": 0, "digest": hashlib.sha256(self.binary.read_bytes()).hexdigest(),
                    "protocol": 1, "version": "synthetic"}
        self.record = {"workers": [], "observations": []}

    def program(self):
        return (
            "import json, pathlib, signal, socket, subprocess, sys\n"
            "signal.signal(signal.SIGTERM, lambda *_: sys.exit(2))\n"
            "root = pathlib.Path(sys.argv[1])\n"
            "counter = root / 'attempts'\n"
            "attempt = len(counter.read_bytes()) + 1 if counter.exists() else 1\n"
            "counter.write_bytes(b'x' * attempt)\n"
            "if attempt == 1:\n"
            f"    print({LOG!r}, file=sys.stderr)\n"
            "    sys.exit(1)\n"
            "server = socket.socket(socket.AF_UNIX)\n"
            "server.bind(str(root / 'sock'))\n"
            "server.listen(1)\n"
            "server.settimeout(120)\n"
            "shell = subprocess.Popen(['/bin/sh', '-c', 'read value'], stdin=subprocess.PIPE)\n"
            "try:\n"
            "    (root / 'meta.json').write_text(json.dumps({'pid': shell.pid}))\n"
            "    connection, _ = server.accept()\n"
            "    with connection:\n"
            "        shell.communicate(connection.recv(1024), timeout=120)\n"
            "finally:\n"
            "    if shell.poll() is None:\n"
            "        shell.kill()\n"
            "    shell.wait(timeout=120)\n"
            "    server.close()\n"
        )

    def spawn(self, *args, **kwargs):
        process = self.real_spawn(*args, **kwargs, start_new_session=True)
        self.processes.append(process)
        return process

    def reap(self):
        meta = self.root / "meta.json"
        if meta.exists():
            self.finish_shell()
        for process in self.processes:
            self.finish_process(process)
        self.assertTrue(all(process.poll() is not None for process in self.processes))
        if meta.exists():
            self.assertFalse(workers.process_running(json.loads(meta.read_text())["pid"]))

    def finish_shell(self):
        try:
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(120)
                connection.connect(str(self.root / "sock"))
                connection.sendall(b"finish\n")
        except OSError:
            self.processes[-1].terminate()

    def finish_process(self, process):
        try:
            process.wait(timeout=120)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=120)

    @unittest.skipUnless(sys.platform == "linux" and os.uname().machine == "x86_64", "Linux AMD64 fixture policy")
    def test_real_children_retry_once_keep_image_and_ownership_and_are_reaped(self):
        self.real_spawn = subprocess.Popen
        item = {"version": "synthetic", "artifact": {"binarySha256": self.row["digest"]}}
        environment = {installed_clock.HANDOFF_ENV: str(time.monotonic_ns() + 120 * installed_clock.SECOND)}
        with mock.patch.dict(os.environ, environment, clear=True), installed_clock.inherited_handoff(watchdog=True), \
                mock.patch.object(workers.subprocess, "Popen", side_effect=self.spawn), \
                mock.patch("fixtures.release_worker_snapshot.capture", return_value={"cleanupConfirmed": True}), eperm.ledger(self.path) as ledger:
            workers.launch_selected(self.binary, self.arguments, {}, self.root, item, self.record, self.row, self.path, ledger)
        self.assertEqual(len(self.processes), 2)
        self.assertIsNotNone(self.processes[0].returncode)
        self.assertEqual((self.root / "attempts").read_bytes(), b"xx")
        self.assertEqual([attempt["outcome"] for attempt in self.row["startupAttempts"]], ["EPERM", "started"])
        self.assertEqual(self.row["startupImage"]["sha256"], self.row["digest"])
        failure = self.row["startupAttempts"][0]
        self.assertEqual(failure["cleanup"]["worker"], "reaped")
        self.assertEqual(failure["startupFailure"]["failingCall"], "unavailable")
        shell = json.loads((self.root / "meta.json").read_text())["pid"]
        self.assertEqual(workers.process_info(shell)[0], self.processes[1].pid)
        self.assertIsNone(self.processes[1].poll())


class StartupFinalizer(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mesh-startup-finalizer-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        with closing(sqlite3.connect(self.root / "mesh.db")) as database, database:
            database.execute("CREATE TABLE hosts (id TEXT)")
            database.execute("INSERT INTO hosts VALUES ('owned-host')")
        self.path = self.root / "cohort.json"
        self.log = self.root / "worker.log"
        self.primary = installed_clock.ClockExpired("original startup deadline failure")
        self.now = time.monotonic_ns()
        self.work_end = self.now + 120 * installed_clock.SECOND
        self.endpoints = []
        self.real_stop_one = workers.stop_one
        self.cleanup_error = None

    def fail_start(self, _state, _config, _item, _index, _host, record, path):
        record["workers"] = [{"session": "first", "pid": 0, "shellPid": 0},
                             {"session": "second", "pid": 0, "shellPid": 0}]
        record["startupFailure"] = "original diagnostic"
        workers.save(path, record)
        self.log.write_text("original worker failure\n")
        self.now = self.work_end + installed_clock.SECOND
        # A delivered one-shot work alarm is already disarmed on entry to cleanup.
        signal.setitimer(signal.ITIMER_REAL, 0)
        raise self.primary

    def stop_one(self, worker):
        end = installed_clock.cleanup_deadline()
        self.endpoints.append(end)
        self.assertGreater(end, self.now)
        self.assertEqual(installed_clock.parent_deadline(), end)
        self.assertGreater(installed_clock.timeout(), 0)
        self.assertEqual(installed_clock.handoff_environment(), {installed_clock.HANDOFF_ENV: str(end)})
        with installed_clock.cleanup_handoff() as nested:
            self.assertEqual(nested, end)
            seconds = end / installed_clock.SECOND
            self.assertLessEqual(workers.hang_deadline(), seconds)
            self.assertAlmostEqual(workers.hang_deadline(), seconds, delta=math.ulp(seconds))
            self.real_stop_one(worker)
        if self.cleanup_error:
            raise self.cleanup_error

    def run_failure(self):
        environment = {installed_clock.HANDOFF_ENV: str(self.work_end)}
        with mock.patch.dict(os.environ, environment, clear=True), \
                mock.patch.object(installed_clock.time, "monotonic_ns", side_effect=lambda: self.now), \
                mock.patch.object(workers.time, "monotonic", side_effect=lambda: self.now / installed_clock.SECOND), \
                mock.patch("fixtures.release_worker_evidence.positive_control", return_value={"controlled": True}), \
                mock.patch.object(workers, "start_one", side_effect=self.fail_start), \
                mock.patch.object(workers, "stop_one", side_effect=self.stop_one), \
                installed_clock.inherited_handoff(watchdog=True):
            self.assert_start_failure()
            with self.assertRaises(installed_clock.ClockExpired):
                installed_clock.timeout()
        self.assertIsNone(installed_clock.HANDOFF.get())
        self.assertIsNone(installed_clock.CLEANUP_HANDOFF.get())
        self.assertEqual(json.loads(self.path.read_text())["startupFailure"], "original diagnostic")
        self.assertEqual(self.log.read_text(), "original worker failure\n")

    def assert_start_failure(self):
        try:
            workers.start(self.root, self.root, [{"selected": "original"}], self.path)
        except BaseException as error:
            self.assertIs(error, self.primary)
        else:
            self.fail("startup failure was suppressed")

    def test_expired_inherited_work_gets_one_shared_cleanup_endpoint(self):
        for origin in (89_133_810_986, 100 * installed_clock.SECOND, 2 ** 55 + 123):
            with self.subTest(origin=origin):
                self.check_shared_cleanup_endpoint(origin)

    def check_shared_cleanup_endpoint(self, origin):
        self.now = origin
        self.work_end = origin + 120 * installed_clock.SECOND
        self.endpoints = []
        self.run_failure()
        self.assertEqual(self.endpoints, [self.now + installed_clock.CLEANUP_SECONDS * installed_clock.SECOND] * 2)
        self.assertFalse(getattr(self.primary, "__notes__", []))

    def test_cleanup_base_exception_keeps_identical_startup_error_and_diagnostics(self):
        self.cleanup_error = KeyboardInterrupt("cleanup interrupted")
        self.run_failure()
        self.assertEqual(len(self.endpoints), 1)
        self.assertIn("KeyboardInterrupt: cleanup interrupted", self.primary.__notes__[0])
        self.assertIn("resources unconfirmed", self.primary.__notes__[0])
        self.assertIn(str(self.path), self.primary.__notes__[0])

    def test_startup_base_exception_also_cleans_up_without_replacement(self):
        self.primary = KeyboardInterrupt("original startup interruption")
        self.run_failure()
        self.assertEqual(len(self.endpoints), 2)


if __name__ == "__main__":
    unittest.main()
