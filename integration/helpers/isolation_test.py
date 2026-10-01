#!/usr/bin/env python3
"""Standalone integration entry points must not read the caller's address book."""

import ctypes
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time
from types import SimpleNamespace
import sys
import tempfile
import unittest
from unittest.mock import Mock, call, patch

from isolation import refuse_live_paths, run_process
from terminal_window import Fixture


class EntryPointContractTest(unittest.TestCase):
    def test_every_script_sources_the_prelude_first(self):
        scripts = Path(__file__).resolve().parents[1].glob("*.sh")
        expected = 'source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1'
        for script in scripts:
            with self.subTest(script=script.name):
                self.assertEqual(script.read_text().splitlines()[1], expected)

    def test_gate_requires_fail_closed_sourcing(self):
        gates = Path(__file__).resolve().parents[2] / "scripts/gates.sh"
        text = gates.read_text()
        start = text.index("check_integration_isolation() {")
        definition = text[start:text.index("\n}", start) + 2]
        source = 'source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh"'
        for action, expected in ((source + " || exit 1", 0), (source, 1), ("true", 1)):
            with self.subTest(action=action), tempfile.TemporaryDirectory(prefix="m-gate-contract-") as temporary:
                root = Path(temporary)
                (root / "integration").mkdir()
                (root / "integration/probe.sh").write_text("#!/bin/bash\n" + action + "\n")
                environment = {"PATH": os.defpath, "HOME": str(root / "home"),
                               "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config")}
                result = subprocess.run(["bash", "-c", definition + "\ncheck_integration_isolation"],
                                        cwd=root, env=environment, capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                if expected:
                    self.assertIn("must source the prelude", result.stderr)


class IsolationBoundaryTest(unittest.TestCase):
    def test_defaults_and_aliases_are_refused(self):
        with tempfile.TemporaryDirectory(prefix="m-boundary-") as temporary:
            root = Path(temporary)
            home = root / "home"
            home.mkdir()
            environment = {"HOME": str(home), "XDG_STATE_HOME": str(root / "xdg-state"),
                           "XDG_CONFIG_HOME": str(root / "xdg-config")}
            defaults = (home / ".config/mesh", home / ".local/state/mesh",
                        root / "xdg-state/mesh", root / "xdg-config/mesh")
            for index, default in enumerate(defaults):
                default.mkdir(parents=True)
                alias = root / f"alias-{index}"
                alias.symlink_to(default, target_is_directory=True)
                for candidate in (default, default / "child", alias, alias / "child"):
                    with self.subTest(candidate=candidate):
                        with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                            refuse_live_paths({"fixture": str(candidate)}, environment)
            refuse_live_paths({"state": str(root / "safe-state"), "config": str(root / "safe-config")}, environment)

    def test_symlinked_default_parents_are_refused(self):
        with tempfile.TemporaryDirectory(prefix="m-default-alias-") as temporary:
            root = Path(temporary)
            home = root / "home"
            home.mkdir()
            target = root / "config-parent"
            target.mkdir()
            (home / ".config").symlink_to(target, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                refuse_live_paths({"config": str(target / "mesh")}, {"HOME": str(home)})

    def test_symlinked_default_directory_is_refused(self):
        with tempfile.TemporaryDirectory(prefix="m-default-target-") as temporary:
            root = Path(temporary)
            home = root / "home"
            (home / ".config").mkdir(parents=True)
            target = root / "config-target"
            target.mkdir()
            (home / ".config/mesh").symlink_to(target, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                refuse_live_paths({"config": str(target)}, {"HOME": str(home)})

    def test_fixture_refuses_before_creating_any_files(self):
        with tempfile.TemporaryDirectory(prefix="m-fixture-guard-") as temporary:
            home = Path(temporary)
            default = home / ".config/mesh"
            with patch.dict(os.environ, {"HOME": str(home)}, clear=True):
                with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                    Fixture("/bin/true", default)
            self.assertFalse(default.exists())

    def test_scratch_root_cannot_be_in_a_mesh_default(self):
        helper = Path(__file__).with_name("isolation.py")
        with tempfile.TemporaryDirectory(prefix="m-scratch-guard-") as temporary:
            root = Path(temporary)
            home = root / "home"
            default = home / ".config/mesh"
            default.mkdir(parents=True)
            probe = root / "probe.sh"
            probe.write_text('printf "%s\\n" "$HOME"\n')
            environment = {"PATH": os.defpath, "HOME": str(home), "TMPDIR": str(default)}
            result = subprocess.run([sys.executable, str(helper), "/bin/bash", str(probe)],
                                    env=environment, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("integration isolation refuses TMPDIR", result.stderr)
            self.assertEqual(list(default.iterdir()), [])

    def test_caller_isolated_directories_are_preserved(self):
        helper = Path(__file__).with_name("isolation.py")
        with tempfile.TemporaryDirectory(prefix="m-scratch-owned-") as temporary:
            root = Path(temporary)
            home = root / "caller-home"
            home.mkdir()
            probe = root / "probe.sh"
            probe.write_text('env > "$1"\n')
            capture = root / "environment"
            environment = {"PATH": os.defpath, "HOME": str(home), "TMPDIR": str(root),
                           "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config")}
            result = subprocess.run([sys.executable, str(helper), "/bin/bash", str(probe), str(capture)],
                                    env=environment, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            child = dict(line.split("=", 1) for line in capture.read_text().splitlines())
            for name in ("MESH_STATE_DIR", "MESH_CONFIG_DIR"):
                self.assertEqual(child[name], environment[name])
            self.assertNotEqual(child["HOME"], environment["HOME"])
            self.assertTrue(Path(child["HOME"]).is_relative_to(root))
            self.assertFalse(Path(child["HOME"]).parent.exists(), "wrapper scratch must be cleaned after the child exits")
            for name in ("XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"):
                self.assertTrue(Path(child[name]).is_relative_to(Path(child["HOME"]).parent))

    def test_reentry_does_not_bypass_the_guard(self):
        repo = Path(__file__).resolve().parents[2]
        script = repo / "integration/kill_waits.sh"
        with tempfile.TemporaryDirectory(prefix="m-reentry-") as temporary:
            home = Path(temporary)
            environment = {"PATH": os.defpath, "HOME": str(home), "MESH_INTEGRATION_ENTRY": str(script),
                           "MESH_STATE_DIR": str(home / "state"), "MESH_CONFIG_DIR": str(home / ".config/mesh")}
            result = subprocess.run(["bash", str(script)], env=environment, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("integration isolation refuses", result.stderr)
            self.assertFalse((home / ".config/mesh").exists())


class ReviewRegressionsTest(unittest.TestCase):
    def test_source_failure_never_invokes_mesh(self):
        entry = Path(__file__).resolve().parents[1] / "kill_waits.sh"
        source = entry.read_text().splitlines()[1]
        for broken in ("missing", "error"):
            with self.subTest(helper=broken), tempfile.TemporaryDirectory(prefix="m-source-") as temporary:
                root = Path(temporary)
                marker = root / "mesh-invoked"
                binary = root / "mesh"
                binary.write_text(f'#!/bin/sh\nprintf invoked > "{marker}"\n')
                binary.chmod(0o700)
                probe = root / "probe.sh"
                probe.write_text('#!/bin/bash\n' + source + '\n"$MESH"\n')
                if broken == "error":
                    (root / "helpers").mkdir()
                    (root / "helpers/isolate.sh").write_text("false\n")
                environment = {"PATH": os.defpath, "HOME": str(root / "home"), "TMPDIR": str(root),
                               "MESH": str(binary), "MESH_STATE_DIR": str(root / "state"),
                               "MESH_CONFIG_DIR": str(root / "config")}
                result = subprocess.run(["bash", str(probe)], env=environment, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertFalse(marker.exists(), "failed sourcing must stop before Mesh")

    def test_symlink_launch_never_invokes_mesh(self):
        entry = Path(__file__).resolve().parents[1] / "kill_waits.sh"
        with tempfile.TemporaryDirectory(prefix="m-entry-link-") as temporary:
            root = Path(temporary)
            alias = root / "kill_waits.sh"
            alias.symlink_to(entry)
            marker = root / "mesh-invoked"
            binary = root / "mesh"
            binary.write_text(f'#!/bin/sh\nprintf invoked > "{marker}"\nexit 99\n')
            binary.chmod(0o700)
            environment = {"PATH": os.defpath, "HOME": str(root / "home"), "TMPDIR": str(root),
                           "MESH": str(binary), "MESH_STATE_DIR": str(root / "state"),
                           "MESH_CONFIG_DIR": str(root / "config")}
            result = subprocess.run(["bash", str(alias)], env=environment, capture_output=True, text=True, timeout=15)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertFalse(marker.exists(), "an entry-point alias without helpers must fail closed")

    def test_account_home_default_symlink_targets_are_refused(self):
        with tempfile.TemporaryDirectory(prefix="m-passwd-home-") as temporary:
            root = Path(temporary)
            home = root / "caller-home"
            home.mkdir()
            account = root / "account-home"
            for suffix in (".config/mesh", ".local/state/mesh"):
                with self.subTest(default=suffix):
                    default = account / suffix
                    default.parent.mkdir(parents=True, exist_ok=True)
                    target = root / suffix.replace("/", "-")
                    target.mkdir()
                    default.symlink_to(target, target_is_directory=True)
                    with patch("isolation.pwd.getpwuid", return_value=SimpleNamespace(pw_dir=str(account))):
                        with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                            refuse_live_paths({"config": str(target)}, {"HOME": str(home)})

    def test_fixture_refuses_all_destinations_before_writes(self):
        for name in ("remote-config", "tailscale.json", "daemon.log", "config/hosts.json"):
            with self.subTest(destination=name), tempfile.TemporaryDirectory(prefix="m-fixture-alias-") as temporary:
                root = Path(temporary)
                home = root / "home"
                default = home / ".config/mesh"
                default.mkdir(parents=True)
                sentinel = default / "hosts.json"
                sentinel.write_text("decoy untouched")
                fixture_root = root / "fixture"
                fixture_root.mkdir()
                destination = fixture_root / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.symlink_to(default if name == "remote-config" else sentinel)
                before = set(fixture_root.iterdir())
                with patch.dict(os.environ, {"HOME": str(home)}, clear=True):
                    with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                        Fixture("/bin/true", fixture_root)
                self.assertEqual(set(fixture_root.iterdir()), before)
                self.assertEqual(sentinel.read_text(), "decoy untouched")

    def test_fixture_rechecks_destinations_before_starting_daemons(self):
        for method in ("start_remote", "start_local_daemon"):
            names = ("remote-config", "tailscale.json", "daemon.log", "config/hosts.json") if method == "start_remote" else ("daemon.log",)
            for name in names:
                with self.subTest(method=method, destination=name), tempfile.TemporaryDirectory(prefix="m-fixture-start-") as temporary:
                    root = Path(temporary)
                    home = root / "home"
                    default = home / ".config/mesh"
                    default.mkdir(parents=True)
                    sentinel = default / "hosts.json"
                    sentinel.write_text("decoy untouched")
                    fixture_root = root / "fixture"
                    fixture_root.mkdir()
                    with patch.dict(os.environ, {"HOME": str(home)}, clear=True):
                        fixture = Fixture("/bin/true", fixture_root)
                        destination = fixture_root / name
                        destination.symlink_to(default if name == "remote-config" else sentinel)
                        with patch("terminal_window.subprocess.Popen", side_effect=RuntimeError("process launched")) as spawn:
                            with self.assertRaisesRegex(RuntimeError, "integration isolation refuses"):
                                getattr(fixture, method)()
                        spawn.assert_not_called()
                    self.assertEqual(sentinel.read_text(), "decoy untouched")

    def test_fixture_preserves_symlinked_tmpdir_spelling(self):
        with tempfile.TemporaryDirectory(prefix="m-fixture-tmp-") as temporary:
            root = Path(temporary)
            real = root / "real"
            real.mkdir()
            alias = root / "short"
            alias.symlink_to(real, target_is_directory=True)
            with tempfile.TemporaryDirectory(dir=alias, prefix="fixture-") as fixture_root:
                environment = {"HOME": str(root / "home"), "TMPDIR": str(alias)}
                with patch.dict(os.environ, environment, clear=True):
                    fixture = Fixture("/bin/true", fixture_root)
                self.assertEqual(fixture.environment["TMPDIR"], str(alias))
                self.assertTrue(Path(fixture.environment["HOME"]).is_relative_to(fixture_root))
                self.assertEqual(fixture.root, Path(fixture_root))

    def test_cancellation_during_process_start_is_not_lost(self):
        process = Mock(pid=123456)

        def launch(*_, **__):
            os.kill(os.getpid(), signal.SIGTERM)
            return process

        with patch("isolation.subprocess.Popen", side_effect=launch), patch("isolation.os.killpg") as kill_group:
            with self.assertRaises(SystemExit) as result:
                run_process(["unused"], {})
        self.assertEqual(result.exception.code, 128 + signal.SIGTERM)
        self.assertEqual(kill_group.call_args_list, [call(process.pid, signal.SIGTERM), call(process.pid, signal.SIGKILL)])
        self.assertEqual(process.wait.call_count, 2)

    def test_cancelling_entry_pid_or_group_stops_children_and_cleans_scratch(self):
        helpers = Path(__file__).resolve().parent
        source = (helpers.parent / "kill_waits.sh").read_text().splitlines()[1]
        for target in ("pid", "group"):
            with self.subTest(target=target), tempfile.TemporaryDirectory(prefix="m-cancel-") as temporary:
                root = Path(temporary)
                (root / "helpers").mkdir()
                for name in ("isolate.sh", "isolation.py"):
                    shutil.copyfile(helpers / name, root / "helpers" / name)
                capture = root / "children"
                probe = root / "probe.sh"
                probe.write_text('#!/bin/bash\n' + source + '\nsleep 300 &\nchild=$!\n'
                                 'printf "%s\\n" "$$" "$child" "$HOME" "$(ps -o pgid= -p $$)" > "$1"\nwait "$child"\n')
                environment = {"PATH": os.defpath, "HOME": str(root / "caller-home"), "TMPDIR": str(root),
                               "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config")}
                entry = subprocess.Popen(["bash", str(probe), str(capture)], env=environment,
                                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
                child_group = None
                try:
                    deadline = time.monotonic() + 5
                    while not capture.exists() and time.monotonic() < deadline:
                        time.sleep(0.01)
                    self.assertTrue(capture.exists(), "guarded probe did not start")
                    inner, child, home, group = capture.read_text().splitlines()
                    child_group = int(group)
                    if target == "pid":
                        entry.terminate()
                    else:
                        os.killpg(entry.pid, signal.SIGTERM)
                    entry.wait(timeout=12)
                    self.assertNotEqual(entry.returncode, 0)
                    for pid in (int(inner), int(child)):
                        status = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True)
                        self.assertFalse(status.stdout.strip() and not status.stdout.strip().startswith("Z"),
                                         f"entry cancellation left process {pid} running")
                    self.assertFalse(Path(home).parent.exists(), "entry cancellation leaked wrapper scratch")
                finally:
                    for group in {entry.pid, child_group} - {None}:
                        try:
                            os.killpg(group, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    entry.wait(timeout=3)


class StandaloneIsolationTest(unittest.TestCase):
    def test_scripts_do_not_read_decoy_hosts(self):
        binary = os.environ["MESH"]
        repo = Path(__file__).resolve().parents[2]
        go_root = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
        failures = []
        for name in ("kill_waits.sh", "logs_does_not_attach.sh", "survives_client_death.sh"):
            with tempfile.TemporaryDirectory(prefix="m-decoy-") as temporary:
                root = Path(temporary)
                home = root / "home"
                config = home / ".config" / "mesh"
                config.mkdir(parents=True)
                hosts = config / "hosts.json"
                hosts.write_text(json.dumps({"version": 1, "hosts": []}))
                os.utime(hosts, ns=(1, hosts.stat().st_mtime_ns))
                watcher = None
                if sys.platform == "linux":
                    libc = ctypes.CDLL(None, use_errno=True)
                    watcher = libc.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
                    self.assertGreaterEqual(watcher, 0)
                    self.assertGreaterEqual(libc.inotify_add_watch(watcher, os.fsencode(hosts), 0x1), 0)
                    self.addCleanup(os.close, watcher)
                    hosts.read_bytes()
                    self.assertTrue(os.read(watcher, 4096), "inotify must detect a control read")
                else:
                    hosts.read_bytes()
                    self.assertNotEqual(hosts.stat().st_atime_ns, 1, "this filesystem does not report control reads")
                    os.utime(hosts, ns=(1, hosts.stat().st_mtime_ns))
                environment = {key: os.environ[key] for key in ("PATH", "TERM", "LANG") if key in os.environ}
                environment.update(HOME=str(home), TMPDIR=str(root), MESH=binary)
                environment["PATH"] = str(Path(go_root) / "bin") + os.pathsep + environment.get("PATH", os.defpath)
                result = subprocess.run(["bash", str(repo / "integration" / name)],
                                        env=environment, cwd=repo, capture_output=True, text=True, timeout=30)
                if watcher is None:
                    read = hosts.stat().st_atime_ns != 1
                else:
                    try:
                        read = bool(os.read(watcher, 4096))
                    except BlockingIOError:
                        read = False
                print(f"DECOY {name}: exit={result.returncode}, hosts_read={read}", flush=True)
                if result.returncode or read:
                    failures.append(f"{name}: exit={result.returncode}, hosts_read={read}\n{result.stdout}{result.stderr}")
        self.assertFalse(failures, "\n".join(failures))


if __name__ == "__main__":
    unittest.main()
