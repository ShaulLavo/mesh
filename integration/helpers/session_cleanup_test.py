#!/usr/bin/env python3
"""Shell fixture deletion must follow the worker's final writes."""

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


class SessionCleanupTest(unittest.TestCase):
    @unittest.skipUnless(sys.platform == "linux", "Linux process identity contract")
    def test_process_identity_distinguishes_live_child_from_unreaped_zombie(self):
        child = subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"], stdin=subprocess.PIPE)
        descriptor = None
        try:
            descriptor = os.pidfd_open(child.pid)
            self.assertIsNotNone(process_identity(child.pid))
            self.assertEqual(self.app_update_process_status(child.pid, "running"), 0)
            self.assertEqual(self.app_update_process_status(child.pid, "stopped"), 1)
            child.stdin.close()
            self.assertTrue(select.select([descriptor], [], [], 2)[0], "child did not exit")
            os.kill(child.pid, 0)
            self.assertEqual(Path(f"/proc/{child.pid}/stat").read_text().rsplit(")", 1)[1].split()[0], "Z")
            self.assertIsNone(process_identity(child.pid))
            self.assertEqual(self.app_update_process_status(child.pid, "running"), 1)
            self.assertEqual(self.app_update_process_status(child.pid, "stopped"), 0)
        finally:
            if descriptor is not None:
                os.close(descriptor)
            child.stdin.close()
            if child.poll() is None:
                child.kill()
            child.wait(timeout=2)

    def app_update_process_status(self, pid, state):
        entry = (HELPERS.parent / "temporary_app_update.sh").read_text()
        start = entry.index("process_is() {")
        predicate = entry[start:entry.index("\n}", start) + 2]
        result = subprocess.run(["bash", "-c", predicate + '\nprocess_is "$1" "$2"', "fixture", str(pid), state],
                                env=os.environ | {"REPO_ROOT": str(HELPERS.parent.parent)},
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=2)
        self.assertEqual(result.stderr, "")
        return result.returncode

    def test_client_cleanup_waits_for_final_worker_writes(self):
        self.check_final_worker_writes("client_signal_detaches.sh")

    def test_terminal_signal_cleanup_waits_for_final_worker_writes(self):
        self.check_final_worker_writes("terminal_signals_reach_the_child.sh")

    def check_final_worker_writes(self, script):
        with tempfile.TemporaryDirectory(prefix="m-cleanup-") as temporary:
            root = Path(temporary)
            tree = root / "fixture"
            worker = tree / "state/s/7K3D"
            worker.mkdir(parents=True)
            socket = worker / "sock"
            socket.touch()
            gate = root / "gate"
            os.mkfifo(gate)
            read_fd, write_fd = os.pipe()
            self.addCleanup(os.close, read_fd)
            self.addCleanup(os.close, write_fd)
            mesh = root / "mesh"
            mesh.write_text('#!/bin/bash\nif [[ $1 == ls ]]; then\n'
                            f'  printf polled >&{write_fd}\n'
                            f'  read -r _ < {shlex.quote(str(gate))}\nfi\n')
            mesh.chmod(0o700)
            entry = (HELPERS.parent / script).read_text()
            start = entry.find("cleanup() {")
            if start >= 0:
                cleanup = entry[start:entry.index("\n}", start) + 2] + "\ncleanup"
            else:
                trap = next(line for line in entry.splitlines() if line.startswith("trap "))
                cleanup = shlex.split(trap)[1]
            environment = os.environ | {"T": str(tree), "MESH_STATE_DIR": str(tree / "state"),
                                        "MESH": str(mesh), "SID": "7K3D", "CLIENT1": "", "CLIENT2": ""}
            command = f"source {shlex.quote(str(HELPERS / 'session_cleanup.sh'))}\n{cleanup}"
            process = subprocess.Popen(["bash", "-c", command], env=environment, pass_fds=(write_fd,),
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
            try:
                ready, _, _ = select.select([read_fd], [], [], 3)
                self.assertTrue(tree.exists(), "cleanup removed state before the worker finished writing")
                self.assertTrue(ready, "cleanup never observed the still-finishing worker")
                self.assertIsNone(process.poll())
                (worker / "final-data").write_text("checkpoint complete")
                socket.unlink()
                with gate.open("w") as release:
                    release.write("finished\n")
                stdout, stderr = process.communicate(timeout=3)
                self.assertEqual(process.returncode, 0, stdout + stderr)
                self.assertFalse(tree.exists())
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=3)

    def test_non_errexit_exit_trap_propagates_cleanup_failure(self):
        entry = (HELPERS.parent / "logs_does_not_attach.sh").read_text()
        start = entry.index("cleanup() {")
        cleanup = entry[start:entry.index("\n}", start) + 2]
        for test_status in (0, 7):
            with self.subTest(test_status=test_status), tempfile.TemporaryDirectory(prefix="m-cleanup-trap-") as temporary:
                tree = Path(temporary)
                state = tree / "state"
                socket = state / "s/7K3D/sock"
                socket.parent.mkdir(parents=True)
                socket.touch()
                command = (f"source {shlex.quote(str(HELPERS / 'session_cleanup.sh'))}\n"
                           "set -uo pipefail\nfixture_mesh() { SECONDS=$((SECONDS + 20)); }\n"
                           f"{cleanup}\ntrap cleanup EXIT\nexit {test_status}")
                environment = os.environ | {"T": str(tree), "MESH_STATE_DIR": str(state),
                                            "MESH": "fixture_mesh", "SID": "", "CLIENT": ""}
                result = subprocess.run(["bash", "-c", command], env=environment,
                                        capture_output=True, text=True, timeout=3)
                self.assertEqual(result.returncode, test_status or 1, result.stdout + result.stderr)
                self.assertIn(str(socket), result.stderr)
                self.assertTrue(socket.exists())

    def test_deadline_reports_worker_and_preserves_state(self):
        with tempfile.TemporaryDirectory(prefix="m-cleanup-bound-") as temporary:
            state = Path(temporary)
            socket = state / "s/7K3D/sock"
            socket.parent.mkdir(parents=True)
            socket.touch()
            command = (f"source {shlex.quote(str(HELPERS / 'session_cleanup.sh'))}\n"
                       "fixture_mesh() { SECONDS=$((SECONDS + 20)); }\nMESH=fixture_mesh\n"
                       f"wait_for_fixture_workers {shlex.quote(str(state))}")
            result = subprocess.run(["bash", "-c", command], capture_output=True, text=True, timeout=3)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn(str(socket), result.stderr)
            self.assertIn("did not finish writing session data", result.stderr)
            self.assertTrue(socket.exists())


if __name__ == "__main__":
    unittest.main()
