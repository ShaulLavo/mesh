#!/usr/bin/env python3
"""Standalone integration entry points must not read the caller's address book."""

import ctypes
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from isolation import refuse_live_paths
from terminal_window import Fixture


class EntryPointContractTest(unittest.TestCase):
    def test_every_script_sources_the_prelude_first(self):
        scripts = Path(__file__).resolve().parents[1].glob("*.sh")
        expected = 'source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh"'
        for script in scripts:
            with self.subTest(script=script.name):
                self.assertEqual(script.read_text().splitlines()[1], expected)


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
