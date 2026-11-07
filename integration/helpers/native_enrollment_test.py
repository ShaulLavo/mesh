import errno
import itertools
import json
import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch


HELPERS = Path(__file__).resolve().parent
REPO = HELPERS.parent.parent


def wait_ready(path):
    deadline = time.monotonic() + 2
    while not path.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    if not path.exists():
        raise AssertionError(f"fixture did not publish {path}")


def ready_anchor(root, ready):
    script = root / "ready_anchor.py"
    script.write_text(f'''import sys, time
from pathlib import Path
sys.path.insert(0, {str(HELPERS)!r})
import enrollment_command as owned
original = owned.wait_command
def after_ready(child, timeout):
    deadline = time.monotonic() + 2
    while not Path({str(ready)!r}).exists():
        if child.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError("nested command did not reach timeout barrier")
        time.sleep(0.01)
    return original(child, timeout)
owned.wait_command = after_ready
owned.run_anchor(int(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5], sys.argv[6:])
''')
    return script


def nested_command(fixture, ready, shim):
    with patch("enrollment_command.__file__", str(ready_anchor(fixture.root, ready))):
        return fixture.start(["/bin/sh", "-c", '"$1"; wait', "outer-cli", str(shim)],
                             os.environ, timeout=0.3)


def wait_zombie(pid):
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        state = subprocess.run(["ps", "-p", str(pid), "-o", "stat="],
                               capture_output=True, text=True, timeout=2).stdout
        if "Z" in state:
            return
        time.sleep(0.01)
    raise AssertionError(f"owned child {pid} did not become a zombie")


def owned_zombie_probe(parent_group):
    from enrollment_command import group_exists

    group = os.getpid()
    child = subprocess.Popen([sys.executable, "-c", "import sys; sys.stdin.buffer.read(1)"],
                             stdin=subprocess.PIPE)
    try:
        os.setpgid(0, parent_group)
        assert group_exists(group), "live group reported absent"
        child.stdin.close()
        wait_zombie(child.pid)
        assert group_exists(group), "unreaped group reported absent"
    finally:
        if child.poll() is None:
            child.kill()
        child.wait(timeout=2)
        absent = not group_exists(group)
        print(json.dumps({"child_reaped": child.pid, "group_absent": absent}), flush=True)
        assert absent, "reaped group still pending"


class CancelDuringCleanup:
    joined = False

    def close(self):
        os.kill(os.getpid(), signal.SIGTERM)
        self.joined = True


class NativeEnrollmentTest(unittest.TestCase):
    def test_cancellation_during_cleanup_fails_after_successful_join(self):
        from native_enrollment import FixtureRoot

        command = CancelDuringCleanup()
        fixture = FixtureRoot()
        with self.assertRaisesRegex(InterruptedError, "cancelled"), fixture:
            fixture.commands.append(command)
        self.assertTrue(fixture.cancelled)
        self.assertTrue(command.joined)
        self.assertFalse(fixture.root.exists())

    def test_native_workflow_does_not_depend_on_generic_guard(self):
        workflow = (REPO / ".github/workflows/ci.yml").read_text()
        auth = workflow.split("  auth-cutover:\n", 1)[1].split("  machine-naming:\n", 1)[0]
        self.assertIn("- platform: darwin/arm64\n            runner: macos-15", auth)
        step = workflow.split("      - name: Verify native pinned-fleet enrollment\n", 1)[1]
        step = step.split("      - name:", 1)[0]
        self.assertNotIn("if:", step)
        self.assertIn('python3 integration/helpers/native_enrollment.py "$MESH"', step)
        self.assertNotIn("bash integration/device_enrollment.sh", step)
        self.assertNotIn("MESH_INTEGRATION_ENTRY", step)
        for name in ("HOME", "MESH_STATE_DIR", "MESH_CONFIG_DIR", "TMPDIR"):
            self.assertIn(f'export {name}=', step)
        self.assertEqual(step.count('go build -o "$MESH" ./cmd/mesh'), 1)
        shell = (REPO / "integration/device_enrollment.sh").read_text()
        self.assertEqual(shell.splitlines()[1],
                         'source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1')
        self.assertIn('python3 "$helpers/native_enrollment.py" "$MESH"', shell)

    def test_owned_zombie_group_is_pending_until_reaped(self):
        result = subprocess.run([sys.executable, __file__, "--zombie-probe", str(os.getpgrp())],
                                preexec_fn=os.setpgrp, capture_output=True, text=True, timeout=8)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(json.loads(result.stdout)["group_absent"])

    def test_permission_denied_probe_never_proves_join(self):
        from enrollment_command import stop_group

        def probe_denied(_group, number):
            if number == 0:
                raise PermissionError(errno.EPERM, "injected probe denial")

        clock = itertools.count()
        with patch("enrollment_command.os.killpg", side_effect=probe_denied), \
                patch("enrollment_command.time.monotonic", side_effect=lambda: next(clock)), \
                patch("enrollment_command.time.sleep"):
            joined, error = stop_group(Mock(poll=lambda: 0), 12345, 4)
        self.assertFalse(joined)
        self.assertIn("probe denied", error)

    def test_permission_denied_group_never_proves_join(self):
        from enrollment_command import stop_group

        clock = itertools.count()
        denied = PermissionError(errno.EPERM, "injected group permission denial")
        with patch("enrollment_command.os.killpg", side_effect=denied) as send, \
                patch("enrollment_command.time.monotonic", side_effect=lambda: next(clock)), \
                patch("enrollment_command.time.sleep"):
            joined, error = stop_group(Mock(poll=lambda: 0), 12345, 4)
        self.assertFalse(joined)
        self.assertIn("denied", error)
        self.assertIn(unittest.mock.call(12345, signal.SIGKILL), send.call_args_list)

    def test_permission_denied_group_can_join_only_after_absence(self):
        from enrollment_command import stop_group

        denied = PermissionError(errno.EPERM, "injected group permission denial")
        absent = ProcessLookupError(errno.ESRCH, "injected empty group")
        with patch("enrollment_command.os.killpg", side_effect=[None, denied, absent]) as send, \
                patch("enrollment_command.time.sleep"):
            joined, error = stop_group(Mock(poll=lambda: 0), 12345, 4)
        self.assertTrue(joined)
        self.assertIsNone(error)
        self.assertEqual(send.call_args_list, [unittest.mock.call(12345, signal.SIGTERM),
                                             unittest.mock.call(12345, 0),
                                             unittest.mock.call(12345, 0)])

    def test_nested_cli_ssh_timeout_joins_with_inherited_output(self):
        from native_enrollment import FixtureRoot

        with FixtureRoot() as fixture:
            root = fixture.root
            sentinel = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
            try:
                ready = root / "ready"
                child = root / "child.sh"
                child.write_text(f"printf 'nested CLI ready\\n'\n: > '{ready}'\nexec sleep 30\n")
                shim = root / "ssh"
                shim.write_text(f'#!/bin/sh\nexec /bin/sh "{child}"\n')
                shim.chmod(0o700)
                started = time.monotonic()
                command = nested_command(fixture, ready, shim)
                self.assertRaises(subprocess.TimeoutExpired, command.result)
                self.assertTrue(command.joined)
                self.assertIn("nested CLI ready", command.stdout.read_text())
                self.assertLess(time.monotonic() - started, 6)
                self.assertIsNone(sentinel.poll())
            finally:
                sentinel.terminate()
                sentinel.wait(timeout=2)

    def test_daemon_needing_escalation_is_joined_within_eight_seconds(self):
        from native_enrollment import FixtureRoot

        with FixtureRoot() as fixture:
            root = fixture.root
            ready = root / "ready"
            code = "import signal,sys,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); open(sys.argv[1],'w').close(); time.sleep(30)"
            command = fixture.start([sys.executable, "-c", code, str(ready)], os.environ)
            try:
                wait_ready(ready)
            finally:
                started = time.monotonic()
                command.close()
            self.assertTrue(command.joined)
            self.assertLess(time.monotonic() - started, 8)

    def test_failed_join_keeps_paths_and_reports_failure(self):
        from native_enrollment import FixtureRoot

        class Unjoined:
            joined = False

            def close(self):
                raise RuntimeError("injected unsuccessful join")

        with tempfile.TemporaryDirectory(prefix="enrollment-control-") as directory:
            with patch("native_enrollment.tempfile.mkdtemp", return_value=directory), \
                    self.assertRaisesRegex(RuntimeError, "retained.*" + directory), FixtureRoot() as fixture:
                fixture.commands.append(Unjoined())
                marker = fixture.root / "state"
                marker.write_text("still owned")
            self.assertEqual(marker.read_text(), "still owned")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--zombie-probe":
        owned_zombie_probe(int(sys.argv[2]))
    else:
        unittest.main()
