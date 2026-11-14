#!/usr/bin/env python3
"""Owned subprocess controls for fixture startup, not a kernel EPERM reproducer."""
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
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
        with mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(workers.subprocess, "Popen", side_effect=self.spawn), \
                mock.patch.object(installed_clock, "parent_deadline", side_effect=lambda: time.monotonic_ns() + 1200_000_000_000), \
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


if __name__ == "__main__":
    unittest.main()
