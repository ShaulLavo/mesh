#!/usr/bin/env python3
"""A served fixture must join even workers that have not published their PID."""
import os
from pathlib import Path
import select
import shlex
import signal
import subprocess
import sys
import tempfile
import unittest

from failed_fixture import process_identity

HELPERS = Path(__file__).resolve().parent


def stop_child(child):
    if child.poll() is None:
        child.kill()
    child.communicate(timeout=3)


@unittest.skipUnless(sys.platform == "linux", "Linux fixture process identities")
class ServedCleanupTest(unittest.TestCase):
    def fixture(self):
        temporary = tempfile.TemporaryDirectory(prefix="m-kept-cleanup-")
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name) / "fixture"
        (root / "kept").mkdir(parents=True)
        (root / "finite").mkdir()
        (root / "origin-state/s").mkdir(parents=True)
        return root

    def child(self, command, root):
        child = subprocess.Popen(command, cwd=root, stdin=subprocess.PIPE,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(stop_child, child)
        return child

    def cleanup(self, root, status, interrupted=False):
        entry = (HELPERS.parent / "serve_kept.sh").read_text()
        start = entry.index("cleanup() {")
        cleanup = entry[start:entry.index("\n}", start) + 2]
        traps = "\n".join(line for line in entry.splitlines() if line.startswith("trap "))
        action = "printf 'armed\\n'\nread -r _" if interrupted else f"exit {status}"
        command = (f"source {shlex.quote(str(HELPERS / 'session_cleanup.sh'))}\n"
                   f"{cleanup}\n{traps}\n{action}\n")
        environment = os.environ | {"ORIGIN_PID": "", "TEST_ROOT": str(root),
                                    "ORIGIN_STATE": str(root / "origin-state"),
                                    "KEPT": str(root / "kept"), "FINITE": str(root / "finite"),
                                    "MESH_INTEGRATION": sys.executable,
                                    "REPO_ROOT": str(HELPERS.parent.parent), "MESH": sys.executable}
        process = subprocess.Popen(["bash", "-c", command], env=environment, stdin=subprocess.PIPE,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(stop_child, process)
        return process

    def check_unpublished_child(self, status, interrupted=False):
        root = self.fixture()
        child = self.child([sys.executable, "-c", "import sys; print('ready', flush=True); sys.stdin.read()"], root / "kept")
        self.assertEqual(child.stdout.readline(), "ready\n")
        self.assertIsNotNone(process_identity(child.pid), "fixture observer did not see its live child")
        cleanup = self.cleanup(root, status, interrupted)
        if interrupted:
            self.assertEqual(cleanup.stdout.readline(), "armed\n")
            cleanup.send_signal(signal.SIGTERM)
        stdout, stderr = cleanup.communicate(timeout=15)
        self.assertEqual(cleanup.returncode, status, stdout + stderr)
        self.assertIsNone(process_identity(child.pid), "cleanup left an unpublished fixture command running")
        child.wait(timeout=2)
        self.assertFalse(root.exists())

    def test_unpublished_child_is_joined_on_every_exit(self):
        for status in (0, 7, 143):
            with self.subTest(status=status):
                self.check_unpublished_child(status)

    def test_signal_teardown_joins_unpublished_child(self):
        self.check_unpublished_child(143, interrupted=True)

    def test_finishing_worker_keeps_state_until_final_writes(self):
        root = self.fixture()
        worker_dir = root / "origin-state/s/7K3D"
        worker_dir.mkdir()
        socket = worker_dir / "sock"
        socket.touch()
        code = ("import pathlib,sys; print('ready', flush=True); sys.stdin.read(1); "
                "p=pathlib.Path(sys.argv[1]); (p/'final-data').write_text('finished'); "
                "(p/'sock').unlink(); print('finished', flush=True)")
        worker = self.child([sys.executable, "-c", code, str(worker_dir), "session-worker"], root / "kept")
        self.assertEqual(worker.stdout.readline(), "ready\n")
        cleanup = self.cleanup(root, 0)
        readable, _, _ = select.select([cleanup.stdout], [], [], 3)
        self.assertTrue(readable, "cleanup did not reach its worker join")
        line = cleanup.stdout.readline()
        self.assertTrue(root.exists(), "cleanup erased state before the worker's final writes")
        self.assertIn("waiting for served fixture workers", line)
        self.assertIsNone(worker.poll())
        worker.stdin.write("x")
        worker.stdin.flush()
        self.assertEqual(worker.stdout.readline(), "finished\n")
        worker.wait(timeout=3)
        stdout, stderr = cleanup.communicate(timeout=5)
        self.assertEqual(cleanup.returncode, 0, stdout + stderr)
        self.assertFalse(root.exists())


if __name__ == "__main__":
    unittest.main()
