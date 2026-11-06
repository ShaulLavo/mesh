#!/usr/bin/env python3
"""Exercise the real runner with controlled integrations and a noncompiling Go shim.

Each test copies verify.sh, the controller and the owner into a private
repository. Controlled fixtures record their PIDs, the run root and their
owner's scratch root, and file barriers hold them until the test releases
them. An unrelated sentinel must survive every run. The test process is a
child subreaper, so its teardown can kill and reap anything a failing runner
leaves behind before it removes its own root.
"""
import ctypes
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time
import unittest
from unittest.mock import patch

REPO = Path(__file__).resolve().parents[1]
LINUX = sys.platform == "linux"
COPIED = ("scripts/verify.sh", "scripts/verify_runner.py", "integration/helpers/isolate.sh",
          "integration/helpers/isolation.py", "integration/helpers/owned_command.py",
          "integration/helpers/process_tree.py", "integration/helpers/process_diagnostics.py")
PRELUDE = 'source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1'
# Short stages keep the controls quick; the cancellation bound below is derived from them.
TIMINGS = {"grace": 1.0, "kill_wait": 1.0, "settle": 0.2, "leak_grace": 0.3, "allowance": 1.0,
           "handshake": 3.0, "tick": 0.05}
CANCEL_BOUND = TIMINGS["grace"] + TIMINGS["kill_wait"] + 2 * TIMINGS["allowance"] + 2.0
GO_SHIM = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import sys
import time
root = Path(__file__).resolve().parents[1]
args = sys.argv[1:]


def hang(stage):
    if not (root / f"hang-{stage}").exists():
        return
    child = subprocess.Popen([sys.executable, str(root / "ignore.py")], start_new_session=True)
    (root / f"{stage}.hung").write_text(f"{os.getpid()} {child.pid}")
    while True:
        time.sleep(1)


if args[0] == "env":
    # Only an owner's per-entry setup sees the integration binary.
    hang("entry-env" if "MESH_INTEGRATION_BINARY" in os.environ else "env")
    print(root if args[1] in ("GOROOT", "GOCACHE", "GOMODCACHE") else "")
else:
    hang("build")
    with (root / "builds").open("a") as log:
        log.write(json.dumps(args) + "\n")
    Path(args[args.index("-o") + 1]).touch()
'''
BUSCTL_SHIM = r'''#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import sys
import time
root = Path(__file__).resolve().parents[1]
if (root / "hang-bus").exists():
    child = subprocess.Popen([sys.executable, str(root / "ignore.py")], start_new_session=True)
    (root / "bus.hung").write_text(f"{os.getpid()} {child.pid}")
    while True:
        time.sleep(1)
sys.exit(1)
'''
IGNORE = '''import signal
import time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
while True:
    time.sleep(1)
'''
WORKER = r'''
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
root = Path(__file__).resolve().parent
name, mode, role = sys.argv[1:]
state = Path(os.environ["MESH_CONFIG_DIR"]).parents[1]
isolated = Path(os.environ["HOME"]).parent
prefix = root / (name + "." + role)


def roots():
    return f"{state.exists()} {isolated.exists()}"


def stop(signum, frame):
    prefix.with_suffix(prefix.suffix + ".signal").write_text(f"{roots()} {signum}")


if role == "orphan":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    prefix.with_suffix(prefix.suffix + ".ready").write_text(str(os.getpid()))
    while True:
        time.sleep(1)
signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
(root / (name + ".state")).write_text(f"{state}\n{isolated}")
if mode == "noisy":
    sys.stderr.write("x" * (1024 * 1024) + "\n")
    sys.exit(7)
if mode == "leak":
    subprocess.Popen([sys.executable, __file__, name, mode, "orphan"], start_new_session=True)
    sys.exit(0)
child = None
if role == "fixture":
    child = subprocess.Popen([sys.executable, __file__, name, mode, "child"],
                             start_new_session=mode == "detached")
prefix.with_suffix(prefix.suffix + ".ready").write_text(str(os.getpid()))
while not (root / "release").exists() or (mode == "stubborn" and role == "child"):
    time.sleep(0.01)
if child is not None and mode != "stubborn":
    child.wait()
prefix.with_suffix(prefix.suffix + ".exit").write_text(roots())
print(name + " fixture log", flush=True)
sys.exit(7 if mode == "fail" and role == "fixture" else 0)
'''


def wait_until(predicate, description, timeout=12):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.02)
    raise AssertionError("timed out waiting for " + description)


def stat_fields(pid):
    try:
        with open(f"/proc/{pid}/stat") as source:
            return source.read().rsplit(")", 1)[1].split()
    except (OSError, IndexError):
        return None


def running(pid):
    fields = stat_fields(pid)
    return fields is not None and fields[0] != "Z"


def descendants(root):
    parents = {}
    for entry in os.listdir("/proc"):
        fields = stat_fields(entry) if entry.isdigit() else None
        if fields is not None:
            parents[int(entry)] = int(fields[1])
    found, frontier = set(), [root]
    while frontier:
        parent = frontier.pop()
        children = {pid for pid, ppid in parents.items() if ppid == parent} - found
        found |= children
        frontier += children
    return found


def kill_exact(pid):
    try:
        fd = os.pidfd_open(pid)
    except ProcessLookupError:
        return
    try:
        signal.pidfd_send_signal(fd, signal.SIGKILL)
    except ProcessLookupError:
        pass
    finally:
        os.close(fd)


def reap_one():
    try:
        return os.waitpid(-1, os.WNOHANG)[0] != 0
    except ChildProcessError:
        return False


def reap_ready():
    while reap_one():
        pass


def remove_descendants(keep):
    """Kill and reap everything this subreaper adopted or started except keep."""
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        remaining = descendants(os.getpid()) - keep
        for pid in remaining:
            kill_exact(pid)
        reap_ready()
        if not remaining:
            return True
        time.sleep(0.01)
    return False


def pipe_full(fd):
    """True when the pipe behind fd has no free page left, so a writer would block."""
    unread = struct.unpack("i", fcntl.ioctl(fd, termios.FIONREAD, b"\0\0\0\0"))[0]
    return unread > fcntl.fcntl(fd, fcntl.F_GETPIPE_SZ) - 4096


def complete_line(path):
    try:
        text = Path(path).read_text()
    except FileNotFoundError:
        return None
    return text.strip() if text.endswith("\n") else None


class Scene:
    """A private repository with the real runner, controlled entries and shims."""

    def __init__(self):
        self.root = Path(tempfile.mkdtemp(prefix="mesh-runner-control-"))
        self.tmp, self.control = self.root / "tmp", self.root / "control"
        for directory in ("scripts", "integration/helpers", "bin", "home", "tmp", "control"):
            (self.root / directory).mkdir(parents=True)
        for name in COPIED:
            shutil.copy2(REPO / name, self.root / name)
        for name, text in (("bin/go", GO_SHIM), ("bin/busctl", BUSCTL_SHIM)):
            (self.root / name).write_text(text)
            (self.root / name).chmod(0o755)
        (self.root / "ignore.py").write_text(IGNORE)
        (self.root / "worker.py").write_text(WORKER)
        (self.control / "settings.json").write_text(json.dumps(TIMINGS))
        self.runner = None

    def entries(self, modes):
        for name, mode in modes:
            (self.root / "integration" / f"{name}.sh").write_text(
                f'#!/usr/bin/env bash\n{PRELUDE}\nexec python3 "$PWD/worker.py" {name} {mode} fixture\n')

    def environment(self, **extra):
        return {"PATH": f"{self.root / 'bin'}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
                "HOME": str(self.root / "home"), "TMPDIR": str(self.tmp), "LANG": "C", "TERM": "dumb",
                "MESH_INTEGRATION_JOBS": "2", "MESH_INTEGRATION_CONTROL": str(self.control), **extra}

    def start(self, modes, **extra):
        self.entries(modes)
        self.runner = subprocess.Popen(["bash", str(self.root / "scripts/verify.sh")], env=self.environment(**extra),
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                       start_new_session=True)
        return self.runner

    def hold(self, job, name):
        (self.control / job).mkdir(exist_ok=True)
        (self.control / job / f"hold-{name}").touch()

    def release_hold(self, job, name):
        (self.control / job / f"hold-{name}").unlink()

    def reached(self, job, name, timeout=12):
        return int(wait_until(lambda: complete_line(self.control / job / f"at-{name}"), f"{job} hold {name}", timeout))

    def ready(self, count):
        wait_until(lambda: len(list(self.root.glob("*.ready"))) == count, f"{count} ready fixtures")

    def hung(self, stage):
        """The hanging command and its TERM-ignoring child, once both are recorded."""
        try:
            values = (self.root / f"{stage}.hung").read_text().split()
        except FileNotFoundError:
            return None
        return tuple(int(value) for value in values) if len(values) == 2 else None

    def pids(self):
        found = {int(path.read_text()) for path in self.root.glob("*.ready")}
        for path in self.root.glob("*.hung"):
            found.update(int(value) for value in path.read_text().split())
        return found

    def run_roots(self):
        return list(self.tmp.glob("mesh-verify.*"))

    def isolated_roots(self):
        return list(self.tmp.glob("m-isolate-*"))

    def records(self):
        found = {}
        for root in self.run_roots():
            for state in root.glob("jobs/*/state.json"):
                found[state.parent.name] = json.loads(state.read_text())
        return found

    def release(self):
        for hold in self.control.rglob("hold-*"):
            hold.unlink()
        (self.root / "release").touch()

    def close(self, keep):
        self.release()
        if self.runner is not None:
            if self.runner.poll() is None:
                self.runner.kill()
            self.runner.communicate(timeout=10)
        return remove_descendants(keep)


@unittest.skipUnless(LINUX, "the integration supervisor joins processes only on Linux")
class RunnerCase(unittest.TestCase):
    """The shared harness. Controls are grouped below so each integration entry fits its budget."""
    sentinel = None

    @classmethod
    def setUpClass(cls):
        cls.sentinel = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(600)"],
                                        start_new_session=True)

    @classmethod
    def tearDownClass(cls):
        cls.sentinel.kill()
        cls.sentinel.wait()

    def setUp(self):
        self.scene = Scene()
        self.addCleanup(self.teardown_scene)

    def teardown_scene(self):
        cleared = self.scene.close({self.sentinel.pid})
        shutil.rmtree(self.scene.root)
        self.assertFalse(self.scene.root.exists())
        self.assertTrue(cleared, "teardown could not remove test processes")

    def finish(self, expected, timeout=20):
        stdout, stderr = self.scene.runner.communicate(timeout=timeout)
        self.assertEqual(self.scene.runner.returncode, expected, stdout + stderr)
        self.assertIsNone(self.sentinel.poll(), "unrelated sentinel was signalled")
        return stdout, stderr

    def assert_clean(self):
        self.assertEqual(list(self.scene.tmp.iterdir()), [], "run or owner roots survived")
        live = [pid for pid in self.scene.pids() if running(pid)]
        self.assertEqual(live, [], "runner left owned processes behind")
        for path in self.scene.root.glob("*.exit"):
            self.assertEqual(path.read_text(), "True True", "state removed before child exit")

    def assert_builds(self):
        builds = [json.loads(line) for line in (self.scene.root / "builds").read_text().splitlines()]
        self.assertEqual(len(builds), 2, "runner must build each variant once")
        self.assertNotIn("-tags", builds[0])
        self.assertIn("mesh_integration", builds[1])

    def unproved_exit(self, hold, cancel):
        self.scene.hold("one.sh", hold)
        self.scene.start([("one", "pass")])
        self.scene.ready(2)
        (self.scene.root / "release").touch()
        self.scene.reached("one.sh", hold)
        started = time.monotonic()
        if cancel:
            self.scene.runner.send_signal(signal.SIGTERM)
        _, stderr = self.finish(143 if cancel else 1, timeout=CANCEL_BOUND + 2)
        self.assertLess(time.monotonic() - started, CANCEL_BOUND + 2)
        self.assertIn("FAIL: one.sh: unknown", stderr)
        self.assertIn("FAIL: kept", stderr)
        self.assertEqual(len(self.scene.run_roots()), 1, "the run root was deleted before the launcher exited")


class ResultTests(RunnerCase):
    def test_success(self):
        self.scene.start([("one", "pass"), ("two", "pass")])
        (self.scene.root / "release").touch()
        stdout, stderr = self.finish(0)
        self.assertIn("PASS: one.sh", stdout)
        self.assertIn("PASS: two.sh", stdout)
        self.assertIn("Running integration tests with 2 concurrent jobs", stdout)
        self.assertEqual(stderr, "")
        self.assert_builds()
        self.assert_clean()

    def test_failure_aggregation(self):
        self.scene.start([("one", "fail"), ("two", "pass")])
        (self.scene.root / "release").touch()
        stdout, stderr = self.finish(1)
        self.assertIn("FAIL: one.sh (exit 7)", stderr)
        self.assertIn("  one fixture log", stderr)
        self.assertIn("PASS: two.sh", stdout)
        self.assert_builds()
        self.assert_clean()

    def test_budget_expiry_reports_124(self):
        self.scene.start([("one", "pass")], MESH_INTEGRATION_TIMEOUT="1s")
        _, stderr = self.finish(1)
        self.assertIn("FAIL: one.sh (exit 124)", stderr)
        self.assert_clean()

    def test_normal_path_leak_is_reported_killed_and_joined(self):
        self.scene.start([("one", "leak")])
        stdout, stderr = self.finish(0)
        self.assertIn("PASS: one.sh", stdout)
        self.assertIn("LEAK: one.sh: pid", stderr)
        self.assert_clean()

    def signal_at_launch(self, window):
        self.scene.hold("runner", f"{window}-one.sh")
        self.scene.start([("one", "pass"), ("two", "pass")])
        self.scene.reached("runner", f"{window}-one.sh")
        self.scene.runner.send_signal(signal.SIGTERM)
        time.sleep(0.1)
        self.scene.release_hold("runner", f"{window}-one.sh")

    def test_signal_before_launch_declines_the_admission(self):
        self.signal_at_launch("before-launch")
        self.finish(143, timeout=CANCEL_BOUND + 2)
        self.assertFalse(list(self.scene.root.glob("*.ready")), "a never-launched job started work")
        self.assert_clean()

    def test_signal_right_after_launch_still_joins_the_job(self):
        self.signal_at_launch("after-launch")
        # A workload that already started gets the cancellation, then exits.
        (self.scene.root / "release").touch()
        self.finish(143, timeout=CANCEL_BOUND + 2)
        self.assertFalse((self.scene.root / "two.fixture.ready").exists(), "admissions continued")
        self.assert_clean()


class CancellationTests(RunnerCase):
    def test_launcher_arriving_after_cancellation_declines(self):
        self.scene.hold("one.sh", "launcher-parent")
        self.scene.hold("one.sh", "launcher-declined")
        self.scene.start([("one", "pass")])
        self.scene.reached("one.sh", "launcher-parent")
        self.scene.runner.send_signal(signal.SIGTERM)
        time.sleep(0.1)
        self.scene.release_hold("one.sh", "launcher-parent")
        self.scene.reached("one.sh", "launcher-declined")
        state = self.scene.records()["one.sh"]
        self.assertEqual(state["state"], "declined")
        self.assertIn("cancelled before start", state["reason"])
        self.scene.release_hold("one.sh", "launcher-declined")
        self.finish(143, timeout=CANCEL_BOUND + 2)
        self.assertFalse(list(self.scene.root.glob("*.ready")))
        self.assert_clean()

    def interrupt(self, signum, mode="pass", then=None):
        self.scene.start([("one", mode), ("two", "pass"), ("unstarted", "pass")])
        self.scene.ready(4)
        self.scene.runner.send_signal(signum)
        if then is not None:
            # Signals pending together run their handlers in signal-number order, so order them by observation.
            wait_until(lambda: any((root / "cancel").exists() for root in self.scene.run_roots()), "published cancellation")
            self.scene.runner.send_signal(then)
        wait_until(lambda: len(list(self.scene.root.glob("*.signal"))) == 4, "descendant cancellation")
        for path in self.scene.root.glob("*.signal"):
            self.assertEqual(path.read_text(), f"True True {signum}", "state removed before cancellation")
        self.assertTrue(self.scene.run_roots() and self.scene.isolated_roots(), "state deleted before join")
        (self.scene.root / "release").touch()
        self.finish(128 + signum)
        self.assertFalse((self.scene.root / "unstarted.fixture.ready").exists(), "admissions continued")
        self.assert_builds()
        self.assert_clean()
        self.assertEqual(len(list(self.scene.root.glob("*.exit"))), 3 if mode == "stubborn" else 4)

    def test_term(self):
        self.interrupt(signal.SIGTERM)

    def test_int(self):
        self.interrupt(signal.SIGINT)

    def test_escalation(self):
        self.interrupt(signal.SIGTERM, "stubborn")

    def test_detached_descendant(self):
        self.interrupt(signal.SIGTERM, "detached")

    def test_first_signal_decides_the_status(self):
        self.interrupt(signal.SIGTERM, then=signal.SIGINT)

    def test_runner_kill_cancels_and_joins_every_job(self):
        self.scene.start([("one", "pass"), ("two", "detached")])
        self.scene.ready(4)
        self.scene.runner.kill()
        self.scene.runner.communicate(timeout=10)
        wait_until(lambda: len(list(self.scene.root.glob("*.signal"))) == 4, "cancellation after runner death")
        (self.scene.root / "release").touch()
        wait_until(lambda: not self.scene.isolated_roots(), "owners joining after runner death", CANCEL_BOUND)
        wait_until(lambda: not any(running(pid) for pid in self.scene.pids()), "owned processes exiting")
        self.assertEqual({state["state"] for state in self.scene.records().values()}, {"joined"})
        self.assertEqual(len(self.scene.run_roots()), 1, "nobody may delete the run root after runner death")

    def hang_then_cancel(self, stage):
        (self.scene.root / f"hang-{stage}").touch()
        self.scene.start([("one", "pass")])
        hung, detached = wait_until(lambda: self.scene.hung(stage), f"hanging {stage}")
        started = time.monotonic()
        self.scene.runner.send_signal(signal.SIGTERM)
        self.assertTrue(self.scene.run_roots() and self.scene.isolated_roots(), "roots deleted before join")
        wait_until(lambda: not running(hung), f"cancelled {stage}")
        self.finish(143, timeout=CANCEL_BOUND + 2)
        self.assertLess(time.monotonic() - started, CANCEL_BOUND + 2)
        self.assertFalse(running(detached))
        self.assertFalse((self.scene.root / "one.fixture.ready").exists(), "a test was admitted")
        self.assert_clean()

    def test_hanging_go_environment_services_cancellation(self):
        self.hang_then_cancel("env")

    def test_hanging_bus_check_services_cancellation(self):
        self.hang_then_cancel("bus")

    def test_hanging_build_services_cancellation(self):
        self.hang_then_cancel("build")

    def test_blocked_output_cannot_hold_cancellation(self):
        self.scene.start([("one", "noisy"), ("two", "pass")])
        self.scene.ready(2)
        wait_until(lambda: pipe_full(self.scene.runner.stderr.fileno()), "the runner's unread stderr to fill")
        self.scene.runner.send_signal(signal.SIGTERM)
        wait_until(lambda: any((root / "cancel").exists() for root in self.scene.run_roots()),
                   "cancellation published while output is blocked", CANCEL_BOUND)
        wait_until(lambda: len(list(self.scene.root.glob("*.signal"))) == 2, "the running job's cancellation")
        (self.scene.root / "release").touch()
        # Nobody reads the runner's output, so it must still finish within its bound and keep what it could not deliver.
        wait_until(lambda: self.scene.runner.poll() is not None, "runner exit with no reader", CANCEL_BOUND)
        kept = list(self.scene.tmp.glob("mesh-verify-output.*"))
        self.assertEqual(len(kept), 1, "undelivered output was not kept")
        self.assertIn("x" * 4096, kept[0].read_text())
        self.finish(143)
        kept[0].unlink()
        self.assert_clean()

    def test_runner_death_during_entry_setup_cancels_promptly(self):
        (self.scene.root / "hang-entry-env").touch()
        self.scene.start([("one", "pass")], MESH_INTEGRATION_TIMEOUT="600s")
        hung, detached = wait_until(lambda: self.scene.hung("entry-env"), "hanging entry setup")
        started = time.monotonic()
        self.scene.runner.kill()
        self.scene.runner.communicate(timeout=10)
        wait_until(lambda: not self.scene.isolated_roots(), "owner join after runner death", CANCEL_BOUND)
        self.assertLess(time.monotonic() - started, CANCEL_BOUND)
        self.assertFalse(running(hung) or running(detached))
        self.assertEqual(self.scene.records()["one.sh"]["state"], "joined")
        self.assertEqual(self.scene.records()["one.sh"]["status"], 143)


class ExitProofTests(RunnerCase):
    def test_joined_record_before_owner_exit_keeps_the_run_root(self):
        self.unproved_exit("owner-joined", cancel=False)

    def test_owner_exit_before_launcher_exit_keeps_the_run_root(self):
        self.unproved_exit("launcher-exit", cancel=False)

    def test_cancellation_never_waits_unboundedly_for_a_paused_owner(self):
        self.unproved_exit("owner-joined", cancel=True)

    def test_cancellation_never_waits_unboundedly_for_a_paused_launcher(self):
        self.unproved_exit("launcher-exit", cancel=True)

    def test_declined_record_with_a_stuck_launcher_keeps_the_run_root(self):
        self.scene.hold("one.sh", "launcher-parent")
        self.scene.hold("one.sh", "launcher-declined")
        self.scene.start([("one", "pass")])
        self.scene.reached("one.sh", "launcher-parent")
        self.scene.runner.send_signal(signal.SIGTERM)
        time.sleep(0.1)
        self.scene.release_hold("one.sh", "launcher-parent")
        _, stderr = self.finish(143, timeout=CANCEL_BOUND + 2)
        self.assertIn("FAIL: one.sh: unknown", stderr)
        self.assertEqual(len(self.scene.run_roots()), 1)

    def test_stopped_owner_after_a_post_fork_decline_keeps_the_run_root(self):
        self.scene.hold("one.sh", "owner-report")
        self.scene.start([("one", "pass")])
        owner = self.scene.reached("one.sh", "owner-report")
        os.kill(owner, signal.SIGSTOP)
        wait_until(lambda: stat_fields(owner)[0] == "T", "the owner to stop")
        self.scene.release_hold("one.sh", "owner-report")
        _, stderr = self.finish(1, timeout=TIMINGS["handshake"] + CANCEL_BOUND)
        self.assertIn("FAIL: one.sh: unknown", stderr)
        self.assertIn("FAIL: kept", stderr)
        self.assertEqual(stat_fields(owner)[0], "T", "the stopped owner must still be live")
        self.assertEqual(len(self.scene.run_roots()), 1, "the run root was deleted while the owner lived")
        self.assertFalse(list(self.scene.root.glob("*.ready")), "a declined owner started work")

    def test_launcher_lost_after_started_is_unknown_not_declined(self):
        self.scene.hold("one.sh", "launcher-accept")
        self.scene.start([("one", "pass")])
        launcher = self.scene.reached("one.sh", "launcher-accept")
        self.assertEqual(self.scene.records()["one.sh"]["state"], "started")
        kill_exact(launcher)
        _, stderr = self.finish(1)
        self.assertIn("FAIL: one.sh: unknown", stderr)
        self.assertIn("FAIL: kept", stderr)
        self.assertFalse(list(self.scene.root.glob("*.ready")), "an unaccepted owner started work")


class SourceContractTest(unittest.TestCase):
    def test_runner_never_signals_anything(self):
        for name in ("scripts/verify.sh", "scripts/verify_runner.py"):
            text = (REPO / name).read_text()
            for word in ("kill", "send_signal", "terminate(", "pidfd_send_signal"):
                self.assertNotIn(word, text, f"{name} must not signal")

    def test_unsupported_platform_refuses_before_creating_anything(self):
        scene = Scene()
        self.addCleanup(shutil.rmtree, scene.root)
        scene.entries([("one", "pass")])
        if LINUX:
            # The controller's own refusal, as a non-Linux host reaches it.
            program = ("import runpy, sys; sys.platform = 'darwin'; sys.argv = sys.argv[1:]; "
                       "runpy.run_path(sys.argv[0], run_name='__main__')")
            command = [sys.executable, "-B", "-c", program, str(scene.root / "scripts/verify_runner.py"),
                       str(scene.root), "/bin/bash"]
        else:
            command = ["bash", str(scene.root / "scripts/verify.sh")]
        result = subprocess.run(command, env=scene.environment(), capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("process ownership needs Linux child subreaping", result.stderr)
        self.assertEqual(list(scene.tmp.iterdir()), [], "refusal created state")
        self.assertFalse((scene.root / "builds").exists(), "refusal built Mesh")


@unittest.skipUnless(LINUX, "the controller needs Linux process handles")
class FinishBoundaryTest(unittest.TestCase):
    """A signal recorded after the last job finished still decides the run's status."""

    def setUp(self):
        sys.path.insert(0, str(REPO / "integration/helpers"))
        spec = importlib.util.spec_from_file_location("verify_runner_under_test", REPO / "scripts/verify_runner.py")
        self.runner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.runner)
        handlers = {signum: signal.getsignal(signum) for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)}
        wakeup = signal.set_wakeup_fd(-1)
        self.addCleanup(self.restore_signals, handlers, wakeup)
        self.root = Path(tempfile.mkdtemp(prefix="m-finish-test-"))
        self.addCleanup(shutil.rmtree, self.root, True)

    @staticmethod
    def restore_signals(handlers, wakeup):
        signal.set_wakeup_fd(wakeup)
        for signum, handler in handlers.items():
            signal.signal(signum, handler)

    def finished_run(self):
        from owned_command import Controller, Job, Triggers
        triggers = Triggers()
        self.addCleanup(triggers.release)
        controller = Controller(self.root / "run", triggers)
        self.addCleanup(os.close, controller.runner["fd"])
        job = Job("finished", controller.root / "jobs/finished", dict(self.runner.DEFAULTS), controller.clock())
        job.settle({"state": "joined", "returncode": 0})
        controller.jobs.append(job)
        controller.root.mkdir()
        return triggers, self.runner.Run(REPO, "/bin/bash", {}, 1, {}, controller, None, str(self.root))

    def test_signal_after_the_last_job_decides_the_status(self):
        triggers, run = self.finished_run()
        os.kill(os.getpid(), signal.SIGTERM)
        self.assertEqual(triggers.received, signal.SIGTERM, "the controller's handler did not record the signal")
        self.assertEqual(run.finish(0), 128 + signal.SIGTERM)

    def test_signal_during_final_deletion_decides_the_status(self):
        triggers, run = self.finished_run()
        delete = shutil.rmtree

        def delete_then_signal(path, *arguments, **options):
            delete(path, *arguments, **options)
            os.kill(os.getpid(), signal.SIGTERM)

        with patch.object(self.runner.shutil, "rmtree", side_effect=delete_then_signal):
            status = run.finish(0)
        self.assertEqual(triggers.received, signal.SIGTERM)
        self.assertEqual(status, 128 + signal.SIGTERM)
        self.assertFalse((self.root / "run").exists())


def setUpModule():
    # Adopt controlled orphans so even a failing control can reap its fixtures.
    if LINUX and ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "enable the test's own subreaper")


if __name__ == "__main__":
    unittest.main()
