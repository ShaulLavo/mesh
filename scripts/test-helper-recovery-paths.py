"""Portable daemon plist rendering and native Unix socket proof controls."""
import ast
import importlib
import json
import os
import plistlib
import re
import select
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import Mock, patch

from fixtures import helper_recovery_paths as paths

HELPERS = Path(__file__).resolve().parents[1] / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))
retained_process = importlib.import_module("retained_process")
sessions = importlib.import_module("retained_session")
controls = importlib.import_module("mesh_control")


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


def serve_fixture_worker(directory, command):
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    with subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"], stdin=subprocess.PIPE) as shell:
        try:
            serve_fixture_shell(directory, command, shell)
        finally:
            shell.stdin.close()


def serve_fixture_shell(directory, command, shell):
    session = directory.name
    meta = {"id": session, "command": command, "pid": shell.pid, "state": "running"}
    metadata = directory / "meta.json"
    metadata.write_text(json.dumps(meta))
    path = directory / "sock"
    try:
        with socket.socket(socket.AF_UNIX) as listener:
            listener.bind(str(path))
            listener.listen()
            listener.settimeout(15)
            print("ready", flush=True)
            serve_fixture_connections(listener, session, shell)
        meta.update(state="exited", exitCode=shell.returncode)
        metadata.write_text(json.dumps(meta))
    finally:
        path.unlink(missing_ok=True)


def serve_fixture_connections(listener, session, shell):
    while True:
        with listener.accept()[0] as peer:
            finished = serve_fixture_attachment(peer, session, shell)
        if finished:
            return


def serve_fixture_attachment(peer, session, shell):
    deadline = time.monotonic_ns() + 10_000_000_000

    def send(kind, body):
        sessions.send_frame(peer, deadline, kind, body)

    try:
        kind, request = sessions.read_frame(peer, deadline)
    except controls.EmptyResponse:
        return False
    assert kind == 1 and json.loads(request) == {"type": "session.attach", "sessionId": session}
    send(1, json.dumps({"type": "session.attached", "sessionId": session}).encode())
    header = session.encode().ljust(8, b"\0")
    kind, data = sessions.read_frame(peer, deadline)
    assert kind == 3 and data.startswith(header)
    challenge = re.search(rb"[0-9a-f]{32}(?=:%s:ready)", data).group()
    reply = challenge + b":" + str(shell.pid).encode() + b":ready"
    send(2, header + bytes(8) + reply[:12])
    send(2, header + bytes(8) + reply[12:])
    kind, data = sessions.read_frame(peer, deadline)
    assert kind == 3 and data == header + b"exit 0\n"
    shell.stdin.close()
    status = shell.wait(timeout=10)
    send(1, json.dumps({"type": "session.exit", "sessionId": session, "exitCode": status}).encode())
    return True


class RetainedSessionTeardownTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="rps-", dir=paths.selected_base().resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.session = sessions.RetainedSession(self.root, self.root / "mesh", HELPERS / "window_shell.sh")
        self.session.check_empty()
        self.directory = self.root / "s" / "TEST"
        self.directory.mkdir(parents=True)

    def test_helper_recovery_closes_after_failed_capture(self):
        self.assert_cooperative_close("prove-helper-recovery.py", "Installation")

    def test_cutover_closes_after_failed_capture(self):
        self.assert_cooperative_close("prove-device-auth-cutover.py", "Host")

    def fixture(self, script, class_name):
        source = Path(__file__).resolve().parents[1] / "scripts" / script
        tree = ast.parse(source.read_text(), filename=str(source))
        fixture = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == class_name)
        methods = {"close", "drain_restarts", "seed"}
        fixture.body = [node for node in fixture.body if isinstance(node, ast.FunctionDef) and node.name in methods]
        scope = {"ExitStack": ExitStack, "LOCK": threading.RLock(), "require": sessions.require,
                 "HELPERS": HELPERS, "retained_session": sessions}
        # Loading the proof module would acquire releases and start services.
        exec(compile(ast.Module(body=[fixture], type_ignores=[]), str(source), "exec"), scope)
        host = scope[class_name].__new__(scope[class_name])
        host.root, host.state, host.binary, host.environment = self.root, self.root, self.root / "mesh", {}
        host.terminals, host.logs, host.restarts = [], [], []
        host.retained_session = None
        host.stop = lambda unit: None
        host.control = lambda command: {"host": {"id": "fixture"}}
        return host, scope

    def start_worker(self):
        args = [sys.executable, str(Path(__file__).resolve()), "--fixture-worker", str(self.directory)] + self.session.command
        worker = subprocess.Popen(args, stdout=subprocess.PIPE)
        self.addCleanup(self.join_worker, worker)
        self.session.worker_command_matches = lambda command: str(self.directory) in command and self.session.command[-1] in command
        self.assertTrue(select.select([worker.stdout], [], [], 10)[0], "worker did not report readiness")
        self.assertEqual(worker.stdout.readline(), b"ready\n")
        return worker

    def join_worker(self, worker):
        try:
            worker.wait(timeout=30)
        finally:
            worker.stdout.close()

    def assert_cooperative_close(self, script, class_name):
        worker = self.start_worker()
        host, _ = self.fixture(script, class_name)
        host.retained_session = self.session
        shell = json.loads((self.directory / "meta.json").read_text())["pid"]
        with self.assertRaisesRegex(RuntimeError, "shell PID differs from fixture metadata"):
            self.session.capture(self.directory.name, shell + 1)
        worker.terminate()
        host.close()
        self.assertEqual(worker.wait(timeout=10), 0)
        self.assertFalse((self.directory / "sock").exists())
        self.assertEqual(json.loads((self.directory / "meta.json").read_text())["exitCode"], 0)

    def test_both_proofs_register_cleanup_before_starting_the_terminal(self):
        self.directory.rmdir()
        for script, name in (("prove-helper-recovery.py", "Installation"), ("prove-device-auth-cutover.py", "Host")):
            self.assert_registration(script, name)

    def assert_registration(self, script, name):
        host, scope = self.fixture(script, name)

        def fail_start(*args, **kwargs):
            self.assertIsInstance(host.retained_session, sessions.RetainedSession)
            raise RuntimeError("terminal startup failed")

        scope["Terminal"] = fail_start
        with self.assertRaisesRegex(RuntimeError, "terminal startup failed"):
            host.seed()
        host.close()

    def test_teardown_refuses_recycled_or_unverifiable_worker_before_sending(self):
        worker = self.start_worker()
        shell = json.loads((self.directory / "meta.json").read_text())["pid"]
        self.session.capture(self.directory.name, shell)
        self.addCleanup(self.session.release_references)
        original = retained_process.process_info(worker.pid)
        for current in (original._replace(birth=("replacement",)), None):
            self.assert_worker_refused(current)
        self.assertIsNone(worker.poll())
        self.session.close()

    def assert_worker_refused(self, current):
        real_info = retained_process.process_info

        def process_info(pid):
            return current if pid == self.session.worker.pid else real_info(pid)

        with (
            patch.object(retained_process, "process_info", side_effect=process_info),
            patch.object(sessions, "send_frame") as send,
            self.assertRaisesRegex(RuntimeError, "could not verify ownership"),
        ):
            self.session.exit_shell()
        send.assert_not_called()

    def test_unverifiable_cleanup_keeps_root_and_closes_other_resources(self):
        host, _ = self.fixture("prove-helper-recovery.py", "Installation")
        host.retained_session = self.session
        log = Mock()
        host.logs.append(log)
        other_fixture = Mock()
        proof = self.root / "proof"
        proof.mkdir()
        pending = proof / "cleanup-pending"
        pending.touch()
        with self.assertRaises(FileNotFoundError), sessions.confirm_cleanup(proof) as cleanup:
            cleanup.callback(other_fixture.close)
            cleanup.callback(host.close)
        self.assertTrue(pending.exists())
        other_fixture.close.assert_called_once_with()
        log.close.assert_called_once_with()
        helper = Path(__file__).resolve().parent / "fixtures" / "update-proof-cleanup.sh"
        command = ["bash", "-c", 'source "$1"; update_proof_cleanup "$2"', "_", str(helper), str(self.root)]
        result = subprocess.run(command, capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn(f"Retained {self.root}", result.stderr)
        self.assertTrue(self.directory.is_dir())
        with sessions.confirm_cleanup(proof):
            pass
        subprocess.run(command, check=True)
        self.assertFalse(self.root.exists())


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
    if len(sys.argv) > 1 and sys.argv[1] == "--fixture-worker":
        serve_fixture_worker(Path(sys.argv[2]), sys.argv[3:])
    else:
        unittest.main()
