#!/usr/bin/env python3
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest

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
