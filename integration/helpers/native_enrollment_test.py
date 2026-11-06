import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch


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
    unittest.main()
