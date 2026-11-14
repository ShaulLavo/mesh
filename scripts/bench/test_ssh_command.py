#!/usr/bin/env python3
"""The benchmark's decision rule and real owned OpenSSH endpoint."""

from contextlib import ExitStack
import json
import os
from pathlib import Path
import shutil
import shlex
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import ssh_command as bench  # noqa: E402 - adjacent benchmark module

SCRATCH_PARENT = os.environ.get("MESH_SHORT_TMP", "/work/tmp")
# StrictModes checks every key ancestor; /tmp is world-writable on hosted Ubuntu.
OPENSSH_SCRATCH_PARENT = os.environ.get("RUNNER_TEMP", SCRATCH_PARENT)


def pairs(mesh, ssh):
    return [{"mesh": {"seconds": left}, "ssh": {"seconds": right}}
            for left, right in zip(mesh, ssh)]


class DecisionTests(unittest.TestCase):
    def test_repeated_clear_slowdown_fails(self):
        result = bench.summarize(pairs([0.3] * 40, [0.1] * 40))
        self.assertTrue(result["clear_slowdown"])
        self.assertEqual(result["pairs_exceeding_both_margins"], 40)
        self.assertLess(result["one_sided_sign_p"], 0.01)

    def test_shared_runner_outliers_do_not_fail(self):
        result = bench.summarize(pairs([0.1] * 30 + [2] * 10, [0.1] * 40))
        self.assertFalse(result["clear_slowdown"])
        self.assertEqual(result["mesh_seconds"]["median"], 0.1)
        self.assertEqual(result["mesh_seconds"]["p90"], 2)

    def test_both_generous_margins_are_required(self):
        for mesh, ssh in ((0.02, 0.001), (1.4, 1), (0.15, 0.1), (0.05, 0.1)):
            result = bench.summarize(pairs([mesh] * 40, [ssh] * 40))
            self.assertFalse(result["clear_slowdown"], (mesh, ssh))

    def test_eighty_percent_and_confidence_required(self):
        self.assertFalse(bench.summarize(pairs([0.3] * 31 + [0.1] * 9, [0.1] * 40))["clear_slowdown"])
        self.assertTrue(bench.summarize(pairs([0.3] * 32 + [0.1] * 8, [0.1] * 40))["clear_slowdown"])
        self.assertGreater(bench.summarize(pairs([0.3], [0.1]))["one_sided_sign_p"], 0.01)

    def test_order_is_balanced_and_repeatable(self):
        order = bench.orders(40, 435)
        self.assertEqual(order, bench.orders(40, 435))
        self.assertEqual(sum(pair[0] == "mesh" for pair in order), 20)
        self.assertNotEqual(order, bench.orders(40, 436))

    def test_every_command_checks_all_output_and_status(self):
        good = subprocess.CompletedProcess([], 0, bench.PAYLOAD, bench.STDERR)
        with patch.object(bench, "checked_run", return_value=good):
            self.assertEqual(bench.sample([], {})["stdout_bytes"], 65536)
        for result in (subprocess.CompletedProcess([], 7, bench.PAYLOAD, bench.STDERR),
                       subprocess.CompletedProcess([], 0, b"", bench.STDERR),
                       subprocess.CompletedProcess([], 0, bench.PAYLOAD, b"")):
            self.assert_rejected(result)

    def assert_rejected(self, result):
        with patch.object(bench, "checked_run", return_value=result):
            with self.assertRaisesRegex(RuntimeError, "mismatch"):
                bench.sample([], {})

    def test_partial_samples_survive_error(self):
        recorded = []
        with patch.object(bench, "sample", side_effect=[{"seconds": 0.1}, RuntimeError("bad output")]):
            with self.assertRaisesRegex(RuntimeError, "bad output"):
                bench.collect({"mesh": [], "ssh": []}, {}, 20, 435, recorded)
        self.assertEqual(len(recorded), 1)
        self.assertEqual(len(recorded[0]), 2)

    def test_openssh_has_no_user_config_agent_or_reuse(self):
        argv = bench.openssh_options(1234, Path("/fixture/key"), Path("/fixture/known"))
        for value in ("ControlMaster=no", "ControlPath=none", "ControlPersist=no", "IdentityAgent=none",
                      "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=/fixture/known"):
            self.assertIn(value, argv)
        self.assertIn("/dev/null", argv)
        self.assertIn("-T", argv)

    def test_errors_are_not_reported_as_performance_failures(self):
        with tempfile.TemporaryDirectory(prefix="ms-ssh-report-", dir=SCRATCH_PARENT) as directory:
            output = Path(directory) / "report.json"
            args = SimpleNamespace(output=output)
            with patch.object(bench, "arguments", return_value=args), patch.object(bench, "environment_record", side_effect=RuntimeError("prerequisite")):
                self.assertEqual(bench.main([]), 2)
            self.assertEqual(json.loads(output.read_text())["status"], "error")

    def test_report_write_failure_is_an_execution_error(self):
        args = SimpleNamespace(output=Path("/not-written/report.json"))
        with patch.object(bench, "arguments", return_value=args), patch.object(bench, "environment_record", return_value={}):
            self.check_report_write_failure()

    def check_report_write_failure(self):
        with patch.object(bench, "execute"), patch.object(Path, "write_text", side_effect=OSError("disk full")):
            self.assertEqual(bench.main([]), 2)

    @unittest.skipUnless(sys.platform == "linux", "needs Linux child subreaping")
    def test_failure_and_termination_join_children_before_removing_root(self):
        for terminate in (False, True):
            self.check_cleanup(terminate)

    @unittest.skipUnless(sys.platform == "linux", "needs Linux child subreaping")
    def test_termination_during_allocation_records_and_removes_root(self):
        allocate = bench.tempfile.mkdtemp
        roots = []

        def interrupted_allocation(*args, **kwargs):
            root = allocate(*args, **kwargs)
            roots.append(Path(root))
            os.kill(os.getpid(), bench.signal.SIGTERM)
            return root

        args = SimpleNamespace(scratch_parent=Path(SCRATCH_PARENT))
        report = {}
        with patch.object(bench.tempfile, "mkdtemp", side_effect=interrupted_allocation):
            self.check_allocation_termination(args, report)
        self.assertEqual(report["scratch_root"], str(roots[0]))
        self.assertEqual(report["cleanup"], {"descendants_joined": True, "scratch_removed": True})
        self.assertFalse(roots[0].exists())

    def check_allocation_termination(self, args, report):
        with bench.termination_cleanup(), patch.object(bench, "exercise") as exercise:
            with self.assertRaises(SystemExit) as failure:
                bench.execute(args, report)
        self.assertEqual(failure.exception.code, 143)
        exercise.assert_not_called()

    def check_cleanup(self, terminate):
        processes, roots = [], []

        def failed_exercise(_args, _report, root):
            roots.append(root)
            (root / "sshd.log").write_text("x" * 9000 + "\nAuthentication refused\n")
            processes.append(subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"]))
            if terminate:
                os.kill(os.getpid(), bench.signal.SIGTERM)
            raise RuntimeError("fixture failed")

        args = SimpleNamespace(scratch_parent=Path(SCRATCH_PARENT))
        report = {}
        with patch.object(bench, "exercise", side_effect=failed_exercise), bench.termination_cleanup():
            with self.assertRaises((RuntimeError, SystemExit)):
                bench.execute(args, report)
        self.assertEqual(report["cleanup"], {"descendants_joined": True, "scratch_removed": True})
        self.assertFalse(roots[0].exists())
        self.assertFalse(Path(f"/proc/{processes[0].pid}").exists())
        self.assertEqual(len(report["logs"]["sshd.log"]), 8000)
        self.assertTrue(report["logs"]["sshd.log"].endswith("\nAuthentication refused\n"))
        processes[0].poll()


@unittest.skipUnless(sys.platform == "linux" and shutil.which("sshd"), "needs installed Linux OpenSSH server")
class OpenSSHTests(unittest.TestCase):
    def test_preexisting_child_is_preserved(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"])
        try:
            with self.assertRaisesRegex(RuntimeError, "pre-existing children"):
                bench.Descendants()
            self.assertIsNone(process.poll())
        finally:
            bench.stop_process(process)

    def test_real_command_and_cleanup(self):
        self.check_command(OPENSSH_SCRATCH_PARENT)

    def test_insecure_key_ancestor_is_rejected(self):
        with tempfile.TemporaryDirectory(prefix="ms-ssh-insecure-", dir=SCRATCH_PARENT) as parent:
            Path(parent).chmod(0o1777)
            with self.assertRaisesRegex(RuntimeError, "Authentication refused: bad ownership or modes for directory"):
                self.check_command(parent)

    def check_command(self, parent):
        owner = bench.Descendants()
        root = Path(tempfile.mkdtemp(prefix="ms-ssh-test-", dir=parent))
        port = None
        try:
            port = self.command(root)
        finally:
            owner.close()
            shutil.rmtree(root)
        self.assertFalse(root.exists())
        self.assertFalse(bench.listening(port))

    def command(self, root):
        state = root / "state"
        state.mkdir()
        environment = {"PATH": os.defpath, "HOME": str(root), "LC_ALL": "C"}
        key = state / "identity.key"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)], check=True)
        fixture = SimpleNamespace(root=root, local=state, environment=environment)
        try:
            return self.sample_command(fixture)
        except Exception as error:
            raise RuntimeError(f"{error}\nOpenSSH fixture logs: {bench.fixture_logs(root)}") from error

    def sample_command(self, fixture):
        with ExitStack() as stack:
            argv, port = bench.start_openssh(fixture, shutil.which("sshd"), stack)
            bench.sample(argv + [shlex.join(bench.COMMAND)], fixture.environment)
            return port


if __name__ == "__main__":
    unittest.main()
