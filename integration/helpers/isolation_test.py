#!/usr/bin/env python3
"""Standalone integration entry points must not read the caller's address book."""

from contextlib import ExitStack
import ctypes
import errno
import fcntl
import json
import os
from pathlib import Path
import shutil
import select
import signal
import socket
import struct
import subprocess
import termios
import time
from types import SimpleNamespace
import sys
import tempfile
import unittest
from unittest.mock import patch

from isolation import refuse_live_paths
from fleet_sharing import Device
from terminal_window import Fixture

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "scripts"))
import verify_runner  # noqa: E402 - the runner lives beside verify.sh

# The standalone entry is a controller of one owned job, so it needs the owner beside it.
ENTRY_HELPERS = ("isolate.sh", "isolation.py", "process_diagnostics.py", "owned_command.py", "process_tree.py")


def copy_entry_helpers(root):
    (root / "helpers").mkdir()
    for name in ENTRY_HELPERS:
        shutil.copyfile(Path(__file__).with_name(name), root / "helpers" / name)


def signal_group(group, signum):
    try:
        os.killpg(group, signum)
    except ProcessLookupError:
        pass


def stop_entry(entry, groups):
    """Teardown for an entry and every process group it recorded, whatever the test observed."""
    if entry.poll() is None:
        signal_group(entry.pid, signal.SIGTERM)
        try:
            entry.wait(timeout=12)
        except subprocess.TimeoutExpired:
            pass
    for group in groups:
        signal_group(group, signal.SIGKILL)
    entry.wait(timeout=3)


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

    def test_name_observation_uses_existing_slow_budget(self):
        budget = verify_runner.budgets({})
        selected = {name: verify_runner.budget_for(name, budget)
                    for name in ("ordinary.sh", "packaging_contract.sh", "name_observation.sh")}
        self.assertEqual(selected, {"ordinary.sh": 30, "packaging_contract.sh": 600, "name_observation.sh": 600})

    def test_verify_execs_the_owned_controller_before_creating_anything(self):
        wrapper = (REPO / "scripts/verify.sh").read_text()
        self.assertIn('exec python3 -B "$repo_root/scripts/verify_runner.py" "$repo_root" "$BASH"', wrapper)
        for created in ("mktemp", "mkdir", "trap", "timeout", "kill"):
            self.assertNotIn(created, wrapper)


class ServedPortOwnershipTest(unittest.TestCase):
    def assert_reserved(self, ports):
        self.assertEqual(len(set(ports)), 5)
        for port in ports:
            with self.subTest(port=port):
                self.assert_port_reserved(port)

    def assert_port_reserved(self, port):
        addresses = ((socket.AF_INET, "127.0.0.1"), (socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::1"))
        for family, address in addresses:
            with socket.socket(family) as competitor:
                self.assert_bind_refused(competitor, address, port)

    def assert_bind_refused(self, competitor, address, port):
        with self.assertRaises(OSError, msg="fixture released its port before startup") as raised:
            competitor.bind((address, port))
        self.assertEqual(raised.exception.errno, errno.EADDRINUSE)

    def assert_start_restart(self, port):
        with socket.socket() as client:
            client.settimeout(0.2)
            self.assertEqual(client.connect_ex(("127.0.0.1", port)), errno.ECONNREFUSED)
        for _ in range(2):
            with socket.socket() as listener:
                listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                listener.bind(("127.0.0.1", port))
                listener.listen()
            self.assert_port_reserved(port)

    def check_entry(self, entry, signum):
        process = subprocess.Popen(["bash", str(entry)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
        try:
            self.assertTrue(select.select([process.stdout], [], [], 10)[0], "fixture did not allocate ports")
            ports = list(map(int, process.stdout.readline().split()))
            self.assert_reserved(ports)
            self.assert_start_restart(ports[-1])
            if signum is not None:
                os.kill(process.pid, signum)
        finally:
            output, errors = self.finish_entry(process, signum)
        expected = 0 if signum is None else 128 + signum
        self.assertEqual(process.returncode, expected, output.decode() + errors.decode())
        for port in ports:
            with socket.socket() as competitor:
                competitor.bind(("127.0.0.1", port))

    def finish_entry(self, process, signum):
        try:
            return process.communicate(input=b"\n" if signum is None else None, timeout=15)
        finally:
            stop_entry(process, [])

    def new_devices(self, root, cleanup):
        devices = []
        for name in ("pc", "mac", "server-a", "server-b"):
            directory = root / name
            directory.mkdir(mode=0o700)
            device = Device.__new__(Device)
            cleanup.callback(device.close)
            device.__init__(os.environ["MESH"], directory, name)
            devices.append(device)
        return devices

    def test_fleet_device_ports_stay_owned_before_start_and_across_restart(self):
        with tempfile.TemporaryDirectory(prefix="m-fleet-ports-") as temporary, ExitStack() as cleanup:
            devices = self.new_devices(Path(temporary), cleanup)
            ports = [device.port for device in devices]
            self.assertEqual(len(set(ports)), len(devices))
            for port in ports:
                self.assert_port_reserved(port)
                self.assert_start_restart(port)
        for port in ports:
            with socket.socket() as competitor:
                competitor.bind(("127.0.0.1", port))

    def test_kept_ports_stay_reserved_until_the_fixture_owner_exits(self):
        source = (REPO / "integration/serve_kept.sh").read_text()
        start = source.index('\n', source.index('|| fail "approve fixture client"')) + 1
        allocation = source[start:source.index("CONTROL_PORT=", start)]
        with tempfile.TemporaryDirectory(prefix="m-served-ports-") as temporary:
            root = Path(temporary)
            copy_entry_helpers(root)
            entry = root / "serve_kept.sh"
            entry.write_text('#!/bin/bash\nsource "$(dirname -- "$0")/helpers/isolate.sh" || exit 1\n'
                             + allocation + '\nprintf "%s " "${PORTS[@]}"; printf "\\n"\nread -r _ || :\n')
            for signum in (None, signal.SIGTERM):
                self.check_entry(entry, signum)


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

    def test_prelude_python_does_not_write_bytecode(self):
        for reentry in (False, True):
            with self.subTest(reentry=reentry), tempfile.TemporaryDirectory(prefix="m-bytecode-") as temporary:
                self.check_prelude_bytecode(Path(temporary), reentry)

    def check_prelude_bytecode(self, root, reentry):
        copy_entry_helpers(root)
        (root / "bytecode_probe.py").write_text("VALUE = 1\n")
        probe = root / "probe.sh"
        probe.write_text('#!/bin/bash\nsource "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1\n'
                         "python3 -c 'import bytecode_probe'\n")
        environment = {"PATH": os.defpath, "HOME": str(root / "home"), "TMPDIR": str(root),
                       "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config"),
                       "PYTHONDONTWRITEBYTECODE": "0"}
        if reentry:
            environment["MESH_INTEGRATION_ENTRY"] = str(probe)
        result = subprocess.run(["bash", str(probe)], cwd=root, env=environment,
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(list(root.rglob("__pycache__")))

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

    def test_termination_cleanup_is_not_interrupted_by_later_signals(self):
        from isolation import protected_cleanup, termination_cleanup
        stages = []
        with self.assertRaises(SystemExit) as stopped, termination_cleanup():
            try:
                os.kill(os.getpid(), signal.SIGTERM)
                stages.append("unreachable")
            finally:
                os.kill(os.getpid(), signal.SIGINT)
                stages.append("cleanup finished")
        self.assertEqual(stopped.exception.code, 128 + signal.SIGTERM)
        self.assertEqual(stages, ["cleanup finished"])
        with termination_cleanup(), protected_cleanup():
            os.kill(os.getpid(), signal.SIGTERM)
            stages.append("protected cleanup finished")
        self.assertEqual(stages[-1], "protected cleanup finished")

    def test_standalone_entry_refuses_before_creating_anything_without_ownership(self):
        with tempfile.TemporaryDirectory(prefix="m-entry-refuse-") as temporary:
            root = Path(temporary)
            probe = root / "probe.sh"
            probe.write_text(f'touch "{root}/ran"\n')
            program = ("import os, runpy, sys; sys.platform = 'darwin'; sys.argv = sys.argv[1:]; "
                       "sys.path.insert(0, os.path.dirname(sys.argv[0])); runpy.run_path(sys.argv[0], run_name='__main__')")
            environment = {"PATH": os.defpath, "HOME": str(root / "home"), "TMPDIR": str(root),
                           "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config")}
            result = subprocess.run([sys.executable, "-B", "-c", program, str(Path(__file__).with_name("isolation.py")),
                                     "/bin/bash", str(probe)], env=environment, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn("process ownership needs Linux child subreaping", result.stderr)
            self.assertEqual(sorted(path.name for path in root.iterdir()), ["probe.sh"])

    def test_cancellation_fixture_cleans_up_before_process_capture_is_read(self):
        with tempfile.TemporaryDirectory(prefix="m-cancel-cleanup-") as temporary:
            root = Path(temporary)
            captured = []

            def fail_readiness(*_):
                capture, = root.glob("m-cancel-*/children")
                captured.append(capture.read_text().splitlines())
                raise AssertionError("forced readiness failure before reading process capture")

            case = type(self)("test_cancelling_entry_pid_or_group_stops_children_and_cleans_scratch")
            result = unittest.TestResult()
            try:
                with patch("tempfile.tempdir", str(root)), patch.object(case, "assertTrue", side_effect=fail_readiness):
                    case.run(result)
                self.assertEqual(len(result.failures), 2, result.failures)
                self.assertFalse(result.errors, result.errors)
                self.assertEqual(len(captured), 2)
                for inner, child, home, _ in captured:
                    for pid in (int(inner), int(child)):
                        status = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True)
                        self.assertFalse(status.stdout.strip() and not status.stdout.strip().startswith("Z"),
                                         f"failed readiness left process {pid} running")
                    self.assertFalse(Path(home).parent.exists(), "failed readiness leaked wrapper scratch")
            finally:
                for inner, child, _, group in captured:
                    for pid in (int(inner), int(child)):
                        try:
                            if os.getpgid(pid) == int(group):
                                os.killpg(int(group), signal.SIGKILL)
                                break
                        except ProcessLookupError:
                            pass

    def test_cancellation_uses_runner_toolchain_before_system_fallback(self):
        with tempfile.TemporaryDirectory(prefix="m-cancel-go-") as temporary:
            root = Path(temporary)
            fallback = root / "go"
            fallback.write_text('#!/bin/sh\nprintf invoked > "${0%/*}/invoked"\n'
                                'echo "fallback Go would bootstrap a cold toolchain" >&2\nexit 97\n')
            fallback.chmod(0o700)
            with patch("os.defpath", str(root) + os.pathsep + os.defpath):
                self.test_cancelling_entry_pid_or_group_stops_children_and_cleans_scratch()
            self.assertFalse((root / "invoked").exists(), "cancellation fixture used fallback Go")

    def test_cancelling_entry_pid_or_group_stops_children_and_cleans_scratch(self):
        source = (Path(__file__).resolve().parents[1] / "kill_waits.sh").read_text().splitlines()[1]
        go_root = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
        for target in ("pid", "group"):
            with self.subTest(target=target), tempfile.TemporaryDirectory(prefix="m-cancel-") as temporary:
                self.cancel_entry(Path(temporary), target, source, go_root)

    def cancel_entry(self, root, target, source, go_root):
        copy_entry_helpers(root)
        capture = root / "children"
        probe = root / "probe.sh"
        probe.write_text('#!/bin/bash\n' + source + '\nsleep 300 &\nchild=$!\n'
                         'printf "%s\\n" "$$" "$child" "$HOME" "$(ps -o pgid= -p $$)" > "$1"\nwait "$child"\n')
        environment = {"PATH": str(Path(go_root) / "bin") + os.pathsep + os.defpath,
                       "HOME": str(root / "caller-home"), "TMPDIR": str(root),
                       "MESH_STATE_DIR": str(root / "state"), "MESH_CONFIG_DIR": str(root / "config")}
        stderr = root / "probe.stderr"
        with stderr.open("w") as errors:
            entry = subprocess.Popen(["bash", str(probe), str(capture)], env=environment,
                                     stdout=subprocess.DEVNULL, stderr=errors, start_new_session=True)
        groups = {entry.pid}
        try:
            self.assert_entry_cancelled(entry, target, capture, stderr, groups)
        finally:
            stop_entry(entry, groups)

    def assert_entry_cancelled(self, entry, target, capture, stderr, groups):
        deadline = time.monotonic() + 5
        while not capture.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(capture.exists(), f"guarded probe did not start (exit={entry.poll()}):\n{stderr.read_text()}")
        inner, child, home, group = capture.read_text().splitlines()
        groups.add(int(group))
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


def pipe_full(fd):
    """True when the pipe behind fd has no free page left, so a writer would block."""
    unread = struct.unpack("i", fcntl.ioctl(fd, termios.FIONREAD, b"\0\0\0\0"))[0]
    return unread > fcntl.fcntl(fd, fcntl.F_GETPIPE_SZ) - 4096


def alive(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except OSError:
        return False


def wait_until(predicate, description, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.01)
    raise AssertionError("timed out waiting for " + description)


@unittest.skipUnless(sys.platform == "linux", "the standalone entry owns processes only on Linux")
class StandaloneOutputTest(unittest.TestCase):
    """A caller that never reads stderr must not stop the standalone entry's cancellation."""

    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="m-entry-output-"))
        self.process = None
        self.addCleanup(self.teardown)

    def teardown(self):
        (self.root / "release").touch()
        if self.process is not None:
            stop_entry(self.process, {self.process.pid})
            self.process.communicate(timeout=5)
        shutil.rmtree(self.root)

    def test_output_control_uses_the_selected_go_before_system_fallback(self):
        fallback = self.root / "go"
        fallback.write_text('#!/bin/sh\nprintf invoked > "${0%/*}/invoked"\nexit 97\n')
        fallback.chmod(0o700)
        case = type(self)("test_blocked_stderr_cannot_hold_cancellation")
        result = unittest.TestResult()
        with patch("os.defpath", str(self.root) + os.pathsep + os.defpath):
            case.run(result)
        self.assertFalse((self.root / "invoked").exists(), "output control used fallback Go")
        self.assertFalse(result.failures, result.failures)
        self.assertFalse(result.errors, result.errors)

    def test_blocked_stderr_cannot_hold_cancellation(self):
        work = self.root / "fixture.py"
        work.write_text("import os, sys, time\nfrom pathlib import Path\nout = Path(sys.argv[1])\n"
                        "(out / 'workload').write_text(str(os.getpid()))\n"
                        "sys.stderr.write('x' * (1024 * 1024)); sys.stderr.flush()\n"
                        "while not (out / 'release').exists():\n    time.sleep(0.01)\n")
        script = self.root / "entry.sh"
        script.write_text(f"exec {sys.executable} -B {work} {self.root}\n")
        go_root = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
        environment = {"PATH": str(Path(go_root) / "bin") + os.pathsep + os.defpath,
                       "TMPDIR": str(self.root), "HOME": str(self.root), "PYTHONDONTWRITEBYTECODE": "1"}
        self.process = subprocess.Popen([sys.executable, "-B", str(Path(__file__).with_name("isolation.py")),
                                         "/bin/bash", str(script)], env=environment, stdout=subprocess.DEVNULL,
                                        stderr=subprocess.PIPE, start_new_session=True)
        workload = int(wait_until(lambda: (self.root / "workload").exists() and (self.root / "workload").read_text(),
                                  "the workload"))
        wait_until(lambda: pipe_full(self.process.stderr.fileno()), "the entry's unread stderr to fill")
        entry = next(self.root.glob("m-entry-*"))
        self.process.send_signal(signal.SIGTERM)
        wait_until(lambda: (entry / "cancel").exists(), "cancellation published while stderr is blocked", 3)
        # Nobody reads stderr: the entry must still exit and keep the diagnostics it could not deliver.
        wait_until(lambda: self.process.poll() is not None, "entry exit with no reader", 15)
        self.assertEqual(self.process.returncode, 128 + signal.SIGTERM)
        self.assertFalse(alive(workload), "the workload survived cancellation")
        kept = list(self.root.glob("m-entry-diagnostics-*"))
        self.assertEqual(len(kept), 1, "undelivered diagnostics were not kept")
        self.assertGreaterEqual(kept[0].read_text().count("x"), 1024 * 1024)
        self.assertEqual(list(self.root.glob("m-isolate-*")), [])


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
