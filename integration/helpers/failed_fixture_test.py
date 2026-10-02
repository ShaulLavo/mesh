#!/usr/bin/env python3
import json
import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from failed_fixture import owned_processes, retain_evidence, stop_owned


@unittest.skipUnless(sys.platform == "linux", "Linux process ownership contract")
class FailedFixtureTest(unittest.TestCase):
    def test_partial_launch_children_are_stopped_after_reparenting(self):
        with tempfile.TemporaryDirectory(prefix="mesh-failed-fixture-") as directory:
            root = Path(directory)
            program = "import subprocess,sys,time; child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],cwd='/',stdout=subprocess.DEVNULL); print(child.pid,flush=True); time.sleep(60)"
            parent = subprocess.Popen([sys.executable, "-c", program], cwd=root, stdout=subprocess.PIPE)
            captured = {}
            try:
                child = int(parent.stdout.readline())
                captured = owned_processes(root, sys.executable)
                self.assertIn(parent.pid, captured)
                self.assertIn(child, captured)
                parent.terminate()
                parent.wait(timeout=2)
                stop_owned(root, sys.executable, captured)
                self.assertFalse(owned_processes(root, sys.executable))
            finally:
                if parent.poll() is None:
                    parent.kill()
                    parent.wait(timeout=2)
                stop_owned(root, sys.executable, captured)
                parent.stdout.close()

    def test_ownership_read_rechecks_the_seed_process_start(self):
        with tempfile.TemporaryDirectory(prefix="mesh-failed-fixture-") as directory:
            root = Path(directory)
            parent = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"], cwd=root)
            read_text = Path.read_text
            reads = 0
            try:
                parent_stat = Path(f"/proc/{parent.pid}/stat")

                def kernel_read(path, *args, **kwargs):
                    nonlocal reads
                    data = read_text(path, *args, **kwargs)
                    if path != parent_stat:
                        return data
                    reads += 1
                    if reads > 1:
                        prefix, suffix = data.rsplit(")", 1)
                        fields = suffix.split()
                        fields[19] = str(int(fields[19]) + 1)
                        return prefix + ") " + " ".join(fields)
                    return data

                with patch.object(Path, "read_text", kernel_read):
                    captured = owned_processes(root, sys.executable)
                self.assertNotIn(parent.pid, captured)
            finally:
                parent.terminate()
                parent.wait(timeout=2)

    def test_reused_parent_snapshot_cannot_adopt_a_foreign_child(self):
        with tempfile.TemporaryDirectory(prefix="mesh-failed-fixture-") as directory:
            root = Path(directory)
            program = "import subprocess,sys,time; child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],cwd='/',stdout=subprocess.DEVNULL); print(child.pid,flush=True); time.sleep(60)"
            parent = subprocess.Popen([sys.executable, "-c", program], cwd=root, stdout=subprocess.PIPE)
            read_text = Path.read_text
            parent_changed = False
            child_descriptor = None
            try:
                child = int(parent.stdout.readline())
                child_descriptor = os.pidfd_open(child)
                self.assertEqual(int(Path(f"/proc/{child}/stat").read_text().rsplit(")", 1)[1].split()[1]), parent.pid)
                parent_stat = Path(f"/proc/{parent.pid}/stat")
                child_stat = Path(f"/proc/{child}/stat")

                def kernel_read(path, *args, **kwargs):
                    nonlocal parent_changed
                    data = read_text(path, *args, **kwargs)
                    if path == child_stat:
                        parent_changed = True
                    if path == parent_stat and parent_changed:
                        prefix, suffix = data.rsplit(")", 1)
                        fields = suffix.split()
                        fields[19] = str(int(fields[19]) + 1)
                        return prefix + ") " + " ".join(fields)
                    return data

                with patch.object(Path, "read_text", kernel_read):
                    captured = owned_processes(root, sys.executable)
                self.assertIn(parent.pid, captured)
                self.assertNotIn(child, captured)
            finally:
                if child_descriptor is not None:
                    signal.pidfd_send_signal(child_descriptor, signal.SIGTERM)
                    os.close(child_descriptor)
                if parent.poll() is None:
                    parent.terminate()
                parent.wait(timeout=2)
                parent.stdout.close()

    def test_pre_cleanup_state_and_terminal_output_are_retained(self):
        with tempfile.TemporaryDirectory(prefix="mesh-failed-fixture-") as directory:
            root = Path(directory)
            metadata = root / "local/s/7K3D/meta.json"
            metadata.parent.mkdir(parents=True)
            metadata.write_text('{"state":"running"}')
            (metadata.parent / "worker.log").write_text("startup stage\n")
            (root / "daemon.log").write_text("daemon stage\n")
            fixture = SimpleNamespace(root=root, local=root / "local", remote=root / "remote", binary=sys.executable,
                                      terminals=[SimpleNamespace(output=b"raw terminal error")])
            retain_evidence(fixture)
            metadata.write_text('{"state":"exited"}')
            retain_evidence(fixture)
            evidence = json.loads((root / "failure-evidence.json").read_text())
            self.assertEqual(evidence["terminals"], ["raw terminal error"])
            self.assertEqual((root / "failure-state/local/s/7K3D/meta.json").read_text(), '{"state":"running"}')
            self.assertEqual((root / "failure-state/daemon.log").read_text(), "daemon stage\n")
            self.assertTrue(root.exists())


if __name__ == "__main__":
    unittest.main()
