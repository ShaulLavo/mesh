"""Portable daemon plist rendering and native Unix socket proof controls."""
import ast
import importlib
import json
import os
import plistlib
import select
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import Mock, patch

from fixtures import helper_recovery_paths as paths

HELPERS = Path(__file__).resolve().parents[1] / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))
retained_process = importlib.import_module("retained_process")


class HelperRecoveryDaemonTemplateTest(unittest.TestCase):
    def test_darwin_installation_renders_every_daemon_placeholder(self):
        self.assert_darwin_template("prove-helper-recovery.py", "Installation")

    def test_darwin_cutover_renders_every_daemon_placeholder(self):
        self.assert_darwin_template("prove-device-auth-cutover.py", "Host")

    def assert_darwin_template(self, script, class_name):
        repo = Path(__file__).resolve().parents[1]
        source = repo / "scripts" / script
        tree = ast.parse(source.read_text(), filename=str(source))
        installation = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == class_name)
        installation.body = [node for node in installation.body if isinstance(node, ast.FunctionDef) and node.name == "__init__"]
        with tempfile.TemporaryDirectory(prefix="helper-plist-") as directory:
            root = Path(directory)
            original = root / "original-mesh"
            original.write_bytes(b"fixture executable; never started")
            scope = {
                "ROOT": root, "REPO": repo, "HELPERS": repo / "integration" / "helpers",
                "HOSTS": {}, "NATIVE": {"os": "darwin", "arch": "arm64"},
                "OLD_BUILD": {"platform": {"os": "darwin", "arch": "arm64"}},
                "Path": Path, "sys": sys, "socket": socket, "os": os, "json": json,
                "tls": types.SimpleNamespace(certificate_environment=lambda _: {}),
                "require": self.assertTrue,
            }
            # Loading the proof module would acquire releases and start services.
            exec(compile(ast.Module(body=[installation], type_ignores=[]), str(source), "exec"), scope)
            host = scope[class_name]("render", {"executable": original, "build": scope["OLD_BUILD"]}, 0, 0)
            path = host.home / "Library" / "LaunchAgents" / "dev.shaulavo.mesh.plist"
            rendered = path.read_bytes()
            self.assertNotIn(b"@MESH_", rendered)
            plist = plistlib.loads(rendered)
            command = plist["ProgramArguments"][2]
            self.assertIn('PATH="${HOME}/.local/bin:$PATH" exec ', command)
            self.assertIn('"${HOME}/.local/bin/mesh" daemon ', command)


class RetainedSessionTeardownTest(unittest.TestCase):
    """Teardown must end retained sessions even though workers hold SIGTERM."""

    def test_helper_recovery_close_ends_a_worker_that_holds_sigterm(self):
        self.assert_close_ends_workers("prove-helper-recovery.py", "Installation")

    def test_cutover_close_ends_a_worker_that_holds_sigterm(self):
        self.assert_close_ends_workers("prove-device-auth-cutover.py", "Host")

    def fixture(self, script, class_name):
        source = Path(__file__).resolve().parents[1] / "scripts" / script
        tree = ast.parse(source.read_text(), filename=str(source))
        fixture = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == class_name)
        fixture.body = [node for node in fixture.body if isinstance(node, ast.FunctionDef) and node.name == "close"]
        scope = {"ExitStack": ExitStack, "LOCK": threading.RLock()}
        # Loading the proof module would acquire releases and start services.
        exec(compile(ast.Module(body=[fixture], type_ignores=[]), str(source), "exec"), scope)
        host = scope[class_name].__new__(scope[class_name])
        host.terminals, host.logs, host.retained_processes = [], [], []
        host.stop = lambda unit: None
        return host

    def start_worker(self):
        worker = subprocess.Popen([sys.executable, "-c", "import signal, sys; signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                                   "print('ready', flush=True); sys.stdin.buffer.read(1)"],
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        self.addCleanup(self.join_worker, worker)
        self.assertTrue(select.select([worker.stdout], [], [], 10)[0], "worker did not report readiness")
        self.assertEqual(worker.stdout.readline(), b"ready\n")
        return worker

    def join_worker(self, worker):
        try:
            if worker.poll() is None:
                worker.kill()
            worker.wait(timeout=10)
        finally:
            worker.stdin.close()
            worker.stdout.close()

    def track(self, worker):
        process = retained_process.RetainedProcess(worker.pid)
        self.addCleanup(process.kill)
        return process

    def assert_close_ends_workers(self, script, class_name):
        worker = self.start_worker()
        host = self.fixture(script, class_name)
        host.retained_processes.append(self.track(worker))
        worker.terminate()
        host.close()
        self.assertEqual(worker.wait(timeout=10), -signal.SIGKILL)

    def test_exited_reference_cannot_signal_recycled_pid(self):
        worker = self.start_worker()
        process = self.track(worker)
        worker.stdin.close()
        self.assertEqual(worker.wait(timeout=10), 0)
        with patch.object(retained_process, "process_info") as lookup, patch.object(process.reference, "kill") as send:
            process.kill()
        lookup.assert_not_called()
        send.assert_not_called()

    def test_teardown_refuses_changed_or_unverifiable_process(self):
        for script, class_name in (("prove-helper-recovery.py", "Installation"), ("prove-device-auth-cutover.py", "Host")):
            with self.subTest(script=script):
                self.assert_close_refuses_process(script, class_name)

    def assert_close_refuses_process(self, script, class_name):
        worker = self.start_worker()
        original = retained_process.process_info(worker.pid)
        for current in (original._replace(birth=("replacement",)), original._replace(uid=original.uid + 1), None):
            self.assert_denied(script, class_name, worker, current)

    def assert_denied(self, script, class_name, worker, current):
        process = self.track(worker)
        host = self.fixture(script, class_name)
        host.retained_processes.append(process)
        log = Mock()
        host.logs.append(log)
        with patch.object(retained_process, "process_info", return_value=current), patch.object(process.reference, "kill") as send:
            with self.assertRaisesRegex(RuntimeError, "could not verify ownership"):
                host.close()
        send.assert_not_called()
        log.close.assert_called_once_with()
        self.assertIsNone(worker.poll())

    def test_capture_rechecks_socket_peer_and_shell_parent(self):
        worker = self.start_worker()
        peer = Mock(side_effect=[worker.pid, worker.pid + 1])
        with self.assertRaisesRegex(RuntimeError, "no longer owns the fixture socket"):
            retained_process.RetainedProcess.from_socket_peer(peer)
        original = self.track(worker)
        sibling = self.start_worker()
        with self.assertRaisesRegex(RuntimeError, "is not a child of the fixture worker"):
            retained_process.RetainedProcess(sibling.pid, parent=original)
        self.assertIsNone(worker.poll())
        self.assertIsNone(sibling.poll())


class HelperRecoveryPathsTest(unittest.TestCase):
    def setUp(self):
        self.short_base = paths.selected_base().resolve()
        self.temp = tempfile.TemporaryDirectory(prefix="hp-", dir=self.short_base)
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.long = self.base / ("long-native-tmpdir-" * 7)
        self.long.mkdir()

    def test_default_base_ignores_native_tmpdir(self):
        environment = os.environ | {"TMPDIR": str(self.long)}
        environment.pop("MESH_SHORT_TMP", None)
        with patch.dict(os.environ, environment, clear=True):
            self.assertEqual(paths.selected_base(), Path("/tmp"))

    def test_previous_tmpdir_root_fails_before_short_root_binds(self):
        with tempfile.TemporaryDirectory(prefix="mesh-helper-proof.", dir=self.long) as old:
            old_root = Path(old) / "proof"
            with self.assertRaisesRegex(ValueError, "socket path requires"):
                paths.validate_socket_root(old_root)
            old_socket = old_root / "repair" / "state" / ".d-4294967295"
            old_socket.parent.mkdir(parents=True)
            with socket.socket(socket.AF_UNIX) as connection, self.assertRaises(OSError):
                connection.bind(str(old_socket))
            print(json.dumps({"control": "previous-long-TMPDIR-root-rejected",
                              "socketBytes": len(os.fsencode(old_socket))}), flush=True)
        with patch.dict(os.environ, {"TMPDIR": str(self.long), "MESH_SHORT_TMP": str(self.short_base)}):
            root = paths.create_root()
        self.addCleanup(shutil.rmtree, root)
        evidence = paths.validate_socket_root(root / "proof")
        self.assertEqual(root.parent, self.short_base)
        self.assertLessEqual(evidence["longestSocketBytes"], paths.MAX_SOCKET_BYTES)
        for row in evidence["socketPaths"]:
            address = Path(row["path"])
            address.parent.mkdir(parents=True, exist_ok=True)
            with socket.socket(socket.AF_UNIX) as connection:
                connection.bind(str(address))
        print(json.dumps({"control": "short-root-native-sockets-bound", **evidence}), flush=True)

    def test_overlong_configured_base_fails_closed_and_cleans_owned_root(self):
        before = set(self.long.iterdir())
        with (
            patch.dict(os.environ, {"TMPDIR": str(self.base), "MESH_SHORT_TMP": str(self.long)}),
            self.assertRaisesRegex(ValueError, "socket path requires"),
        ):
            paths.create_root()
        self.assertEqual(set(self.long.iterdir()), before)

    def test_configured_symlink_uses_resolved_path_bound(self):
        alias = self.base / "a"
        alias.symlink_to(self.long, target_is_directory=True)
        with (
            patch.dict(os.environ, {"MESH_SHORT_TMP": str(alias)}),
            self.assertRaisesRegex(ValueError, "socket path requires"),
        ):
            paths.create_root()
        self.assertEqual(list(self.long.iterdir()), [])

    def test_exact_portable_limit_is_accepted_and_next_byte_rejected(self):
        base = Path("/tmp").resolve()
        suffix_bytes = max(len(os.fsencode(path)) for path in paths.socket_paths(base))
        root = base / ("a" * (paths.MAX_SOCKET_BYTES - suffix_bytes - 1))
        self.assertEqual(paths.validate_socket_root(root)["longestSocketBytes"], paths.MAX_SOCKET_BYTES)
        with self.assertRaisesRegex(ValueError, "socket path requires"):
            paths.validate_socket_root(root.with_name(root.name + "a"))

    def test_filesystem_bytes_and_longest_retained_worker_path_are_bounded(self):
        root = Path("/tmp") / ("é" * 30)
        longest = max(paths.socket_paths(root), key=lambda path: len(os.fsencode(path)))
        self.assertLessEqual(len(str(longest)), paths.MAX_SOCKET_BYTES)
        self.assertGreater(len(os.fsencode(longest)), paths.MAX_SOCKET_BYTES)
        self.assertEqual(longest.name, "sock")
        with self.assertRaisesRegex(ValueError, "socket path requires"):
            paths.validate_socket_root(root)


if __name__ == "__main__":
    unittest.main()
