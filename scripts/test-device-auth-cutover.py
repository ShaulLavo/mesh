"""Cutover event boundaries with fixture-owned service processes."""
import ast
import contextlib
import hashlib
import importlib
import io
import json
import os
import plistlib
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
HELPERS = Path(__file__).resolve().parents[1] / "integration" / "helpers"
sys.path.insert(0, str(HELPERS))
terminals = importlib.import_module("terminal_window")
eventually, require = terminals.eventually, terminals.require

SOURCE = Path(__file__).with_name("prove-device-auth-cutover.py")


class CutoverEventsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.release = threading.Event()
        self.entered = threading.Event()
        self.finished = threading.Event()
        self.timers = []
        owner = self

        class JoiningTimer(threading.Timer):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, **kwargs)
                owner.timers.append(self)

            def join(self, timeout=None):
                owner.release.set()
                return super().join(timeout)

        self.scope = {
            "ROOT": self.root, "EVENTS": [], "LOCK": threading.RLock(), "hosts": [],
            "OLD_BUILD": {"platform": {"os": "darwin"}}, "threading": types.SimpleNamespace(Timer=JoiningTimer),
            "os": os, "json": json, "plistlib": plistlib, "subprocess": subprocess, "time": time,
            "require": require, "eventually": eventually, "Path": Path, "hashlib": hashlib,
        }
        # Importing the proof starts release acquisition and services.
        tree = ast.parse(SOURCE.read_text(), filename=str(SOURCE))
        names = {"Host", "event", "digest", "local_cutover", "wait_for_restarts"}
        definitions = [node for node in tree.body if isinstance(node, (ast.FunctionDef, ast.ClassDef))
                       and node.name in names]
        exec(compile(ast.Module(body=definitions, type_ignores=[]), str(SOURCE), "exec"), self.scope)
        self.hosts = [self.host("h0"), self.host("h1")]
        self.scope["hosts"] = self.hosts
        self.addCleanup(self.close)
        self.target_bytes = b"isolated candidate installation"
        self.target = {"version": "v0.1.160", "digest": hashlib.sha256(self.target_bytes).hexdigest()}
        for host in self.hosts:
            host.command = lambda *args, host=host: self.update(host)

    def host(self, name):
        host = self.scope["Host"].__new__(self.scope["Host"])
        host.name = name
        host.root = self.root / name
        host.root.mkdir()
        host.state = host.root / "state"
        host.binary = host.root / "mesh"
        host.binary.write_bytes(b"unchanged original installation")
        host.environment = dict(os.environ)
        host.processes, host.logs, host.terminals, host.workers, host.restarts = {}, [], [], [], []
        host.service_plists = {}
        for unit in ("mesh.service", "mesh-update-helper.service"):
            path = host.root / (unit + ".plist")
            path.write_bytes(plistlib.dumps({"AbandonProcessGroup": True, "ProgramArguments":
                [sys.executable, "-c", "import signal; signal.pause()"]}))
            host.service_plists[unit] = path
            host.start(unit)
        host.id = name
        host.worker_pid = host.processes["mesh.service"].pid
        host.shell_pid = host.worker_pid
        host.ready = lambda: host.processes["mesh.service"].poll() is None
        host.check_retained = lambda: None
        host.prove_loaded_image = lambda target: None
        host.check_io = lambda: None
        return host

    def close(self):
        self.release.set()
        for timer in self.timers:
            timer.join(timeout=5)
            self.assertFalse(timer.is_alive())
        for host in reversed(self.hosts):
            host.close()

    def update(self, host):
        host.binary.write_bytes(self.target_bytes)
        host.restart("mesh.service")
        response = {
            "fleet": {"members": [{"endpoint": "unix://" + str(host.state / "daemon.sock")}]},
            "coordinator": host.id,
            "targets": [{"state": "updated", "workers": [{"pid": host.worker_pid, "shellPid": host.shell_pid}]}],
        }
        return types.SimpleNamespace(stdout=json.dumps(response).encode())

    def cutover(self, host):
        with contextlib.redirect_stdout(io.StringIO()):
            self.scope["local_cutover"](host, self.target)

    def kickstart(self, host):
        host.launchctl(["kickstart", "-k", "gui/" + str(os.getuid()) + "/dev.shaulavo.mesh-update-helper"])

    def test_failed_listing_retains_bounded_nonsecret_config_diagnostic(self):
        host = self.hosts[0]
        host.config = host.root / "config"
        host.config.mkdir()
        config = host.config / "hosts.json"
        config.write_text('{"version":1,"hosts":[]}')
        config.chmod(0o644)
        host.binary.write_text("#!" + sys.executable + "\nimport sys\nsys.stderr.write('config refusal ' + 'x' * 5000)\nsys.exit(1)\n")
        host.binary.chmod(0o755)
        with self.assertRaisesRegex(RuntimeError, "h0 fixture ls failed with status 1"):
            self.scope["Host"].command(host, "ls")
        failure = json.loads((host.root / "cli-failure.log").read_text())
        self.assertEqual(failure["configMode"], "0o644")
        self.assertTrue(failure["configOwned"])
        self.assertEqual(failure["configLinks"], 1)
        self.assertEqual(len(failure["stderr"]), 4096)
        self.assertTrue(failure["stderr"].startswith("config refusal "))
        self.assertEqual(config.read_text(), '{"version":1,"hosts":[]}')

    def test_previous_helper_restart_finishes_before_next_cutover(self):
        first, second = self.hosts
        restart = first.restart

        def blocked_restart(unit):
            self.entered.set()
            require(self.release.wait(timeout=5), "fixture callback was not released")
            restart(unit)
            self.finished.set()

        first.restart = blocked_restart

        def first_update(*args):
            first.restart = restart
            result = self.update(first)
            first.restart = blocked_restart
            self.kickstart(first)
            require(self.entered.wait(timeout=5), "fixture callback did not start")
            return result

        def second_update(*args):
            self.release.set()
            require(self.finished.wait(timeout=5), "previous fixture callback did not finish")
            return self.update(second)

        first.command, second.command = first_update, second_update
        self.cutover(first)
        self.cutover(second)
        self.assertTrue(self.finished.is_set())
        rows = [json.loads(line) for line in (self.root / "events.jsonl").read_text().splitlines()]
        self.assertEqual(rows, self.scope["EVENTS"])

    def test_true_unrelated_installation_change_is_rejected(self):
        target, unrelated = self.hosts
        original_bytes = unrelated.binary.read_bytes()
        original_pid = unrelated.processes["mesh.service"].pid

        def corrupt_other_installation(*args):
            result = self.update(target)
            unrelated.binary.write_bytes(b"genuine unrelated installation mutation")
            unrelated.restart("mesh.service")
            return result

        target.command = corrupt_other_installation
        with self.assertRaisesRegex(RuntimeError, "local update changed another installation"):
            self.cutover(target)
        self.assertNotEqual(unrelated.binary.read_bytes(), original_bytes)
        self.assertNotEqual(unrelated.processes["mesh.service"].pid, original_pid)

    def test_unrelated_deferred_restart_is_rejected(self):
        target, unrelated = self.hosts

        def wrong_restart(*args):
            result = self.update(target)
            self.kickstart(unrelated)
            return result

        target.command = wrong_restart
        with self.assertRaisesRegex(RuntimeError, "local update changed another installation"):
            self.cutover(target)


class CutoverTerminalArgumentsTest(unittest.TestCase):
    def test_remote_identity_follows_option_delimiter(self):
        tree = ast.parse(SOURCE.read_text(), filename=str(SOURCE))
        calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)
                 and isinstance(node.func, ast.Name) and node.func.id == "Terminal"
                 and isinstance(node.args[0], ast.List)
                 and any(isinstance(item, ast.Attribute) and item.attr == "id" for item in node.args[0].elts)]
        self.assertEqual(len(calls), 3)
        for call in calls:
            with self.subTest(line=call.lineno):
                arguments = call.args[0].elts
                self.assertEqual(len(arguments), 4)
                self.assertEqual([item.value for item in arguments[1:3] if isinstance(item, ast.Constant)],
                                 ["-r", "--"])
                self.assertIsInstance(arguments[3], ast.Attribute)
                self.assertEqual(arguments[3].attr, "id")


if __name__ == "__main__":
    unittest.main()
