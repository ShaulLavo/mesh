#!/usr/bin/env python3
"""Lifecycle and fault controls for the owned-command subreaper owner.

Real-process controls run the actual launcher and owner under a minimal
controller: this test process, or a small runner script when a control kills
the runner. Holds and faults come from the admission's control directory.
This process is a child subreaper, so anything a failing control leaves
behind is adopted here. Teardown kills and reaps every descendant except the
unrelated sentinel, then removes the scratch root.

These are synthetic workloads. They prove the owner protocol, not cooperation
with the real terminal and SSH fixtures.
"""
import contextlib
import ctypes
import errno
import io
import json
import math
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True

from owned_command import (Control, Declined, Launcher, Owner, Record, Trigger, admit, default_prepare,  # noqa: E402
                           establish_runner, launch, observe, request_cancel)
import process_tree  # noqa: E402
from process_tree import Kernel, OwnershipFailure, Stat, Tree, Unsupported  # noqa: E402

HELPERS = Path(__file__).resolve().parent
LINUX = sys.platform == "linux"
FAST = {"budget": 10, "grace": 0.3, "kill_wait": 1.0, "settle": 0.1, "leak_grace": 0.2,
        "handshake": 3, "allowance": 0.5, "tick": 0.01}

WORK = r'''
import ctypes
import os
from pathlib import Path
import signal
import sys
import time

out, mode = Path(sys.argv[1]), sys.argv[2]


def note(name, value=""):
    temporary = out / ("." + name + str(os.getpid()))
    temporary.write_text(str(value))
    os.replace(temporary, out / name)


def wait_for(name):
    while not (out / name).exists():
        time.sleep(0.002)


def detach(name):
    """Orphan a TERM-ignoring grandchild in a new session; its parent exits at once."""
    middle = os.fork()
    if middle == 0:
        os.setsid()
        if os.fork() == 0:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            note(name, os.getpid())
            time.sleep(60)
        os._exit(0)
    os.waitpid(middle, 0)
    wait_for(name)


def helper(signum, frame):
    if os.fork() == 0:
        os.setsid()
        signal.signal(signal.SIGTERM, lambda *_: note("helper-term"))
        note("helper", os.getpid())
        while True:
            time.sleep(1)
    wait_for("helper")
    os._exit(0)


def chain():
    pids, pid = [], os.getpid()
    while pid > 1:
        pids.append(pid)
        with open(f"/proc/{pid}/stat") as source:
            pid = int(source.read().rsplit(")", 1)[1].split()[1])
    return " ".join(map(str, pids))


note("ran-" + mode, os.getpid())
if mode == "exit":
    sys.exit(int(sys.argv[3]))
elif mode == "orphan-exit":
    detach("orphan")
elif mode == "orphan-stay":
    detach("orphan")
    time.sleep(60)
elif mode == "helper-after-term":
    signal.signal(signal.SIGTERM, helper)
    time.sleep(60)
elif mode == "nested":
    if os.fork() == 0:
        ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        detach("nested-orphan")
        note("nested", os.getpid())
        time.sleep(60)
        os._exit(0)
    wait_for("nested")
elif mode == "forker":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    while True:
        child = os.fork()
        if child == 0:
            time.sleep(60)
            os._exit(0)
        with open(out / "forked", "a") as forked:
            forked.write(f"{child}\n")
        time.sleep(0.005)
elif mode == "chain":
    note("chain", chain())
else:
    time.sleep(60)
'''

RUNNER = r'''
import ctypes
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

helpers, role, record, job = sys.argv[1], sys.argv[2], Path(sys.argv[3]), sys.argv[4]
sys.path.insert(0, helpers)
import owned_command

if role != "runner":
    # A stand-in for a containing session worker that started the controller.
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(signum, lambda *_: (record.parent / "ancestor-signalled").touch())
    if role == "subreaper-session-worker":
        ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0)
    child = subprocess.Popen([sys.executable, "-B", __file__, helpers, "runner", str(record), job])
    (record.parent / "ancestor").write_text(f"{os.getpid()}\n")
    while os.wait()[0] != child.pid:
        pass
    child.returncode = 0
    time.sleep(60)
    sys.exit(0)
runner = owned_command.establish_runner()
owned_command.admit(record, runner, **json.loads(job))
with open(str(record) + ".log", "w") as log:
    process = owned_command.launch(record, runner, stdout=log, stderr=subprocess.STDOUT)
(record.parent / "runner").write_text(f"{os.getpid()}\n")
sys.exit(process.wait())
'''


def wait_until(predicate, description, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.005)
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
    """Test teardown: kill and reap everything this process adopted or started except keep."""
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


def complete_line(path):
    try:
        text = Path(path).read_text()
    except FileNotFoundError:
        return None
    return text.strip() if text.endswith("\n") else None


class FakeProcess:
    def __init__(self, pid, ppid, start):
        self.pid, self.ppid, self.start = pid, ppid, start
        self.alive = True
        self.killable = True


class FakeKernel:
    """A scripted process table for races that real hosts cannot schedule."""

    def __init__(self, owner=100, ticks=1000):
        self.owner = owner
        self.ticks = ticks
        self.table = {}
        self.handles = {}
        self.sent = []
        self.on_stat = {}
        self.omit = []
        self.statuses = []
        self.hidden = False
        self.clock = None
        self.closed = []
        self.later = []
        self.flood = 0

    def add(self, pid, ppid, start=10):
        self.table[pid] = FakeProcess(pid, ppid, start)
        return self.table[pid]

    def exit(self, pid, adopter=None):
        process = self.table.pop(pid)
        process.alive = False
        for child in self.table.values():
            if child.ppid == pid:
                child.ppid = self.owner if adopter is None else adopter

    def owned(self, process):
        seen = set()
        while process is not None and process.pid not in seen:
            if process.ppid == self.owner:
                return True
            seen.add(process.pid)
            process = self.table.get(process.ppid)
        return False

    def now_ticks(self):
        return self.ticks

    def pidfd_open(self, pid):
        process = self.table.get(pid)
        if process is None:
            return None
        fd = 1000 + len(self.handles)
        self.handles[fd] = process
        return fd

    def exited(self, fd):
        return not self.handles[fd].alive

    def stat(self, pid):
        process = self.table.get(pid)
        if process is None:
            return None
        result = Stat("S", process.ppid, process.start)
        hook = self.on_stat.pop(pid, None)
        if hook:
            hook()
        return result

    def children(self, pid):
        found = [child.pid for child in self.table.values() if child.ppid == pid]
        if pid == self.owner and self.omit:
            skipped = self.omit.pop(0)
            found = [child for child in found if child not in skipped]
        return found

    def send(self, fd, sig):
        process = self.handles[fd]
        if not process.alive:
            return False
        self.sent.append((process.pid, process.start, sig, self.clock.now() if self.clock else None))
        if sig == signal.SIGKILL and process.killable:
            self.exit(process.pid)
        return True

    def now(self):
        return self.clock.now() if self.clock else 0.0

    def waitpid(self):
        if self.statuses:
            return self.statuses.pop(0)
        if self.flood:
            # An endless supply of ready statuses, each costing a little time.
            self.flood -= 1
            self.clock.value += 0.001
            return 5000 + self.flood, 0
        due = [entry for entry in self.later if entry[0] <= self.now()]
        if due:
            self.later.remove(due[0])
            return due[0][1], due[0][2]
        if self.later or self.hidden or any(self.owned(process) for process in self.table.values()):
            return 0, 0
        raise ChildProcessError

    def close(self, fd):
        self.closed.append(fd)


class FakeClock:
    def __init__(self):
        self.value = 0.0

    def now(self):
        return self.value


class FakeTriggers:
    def __init__(self, clock, at=None, sig=signal.SIGTERM):
        self.clock = clock
        self.at = at
        self.sig = sig
        self.first = None
        self.budget_deadline = None

    def check(self):
        if self.first is None and self.at is not None and self.clock.now() >= self.at:
            self.first = Trigger(self.sig, 128 + self.sig, "test trigger", self.clock.now())
        return self.first

    def wait(self, fds, timeout):
        self.clock.value = round(self.clock.value + timeout, 6)


def silent_signals(test):
    original = process_tree.record_signal
    process_tree.record_signal = lambda *_, **__: None
    test.addCleanup(setattr, process_tree, "record_signal", original)


class OwnershipEdgeTest(unittest.TestCase):
    def setUp(self):
        silent_signals(self)
        self.kernel = FakeKernel()
        self.tree = Tree(self.kernel, 100)

    def kill_all(self):
        self.tree.discover(math.inf, time.monotonic)
        return self.tree.deliver(signal.SIGKILL, "test", lambda pid, start: True, math.inf, time.monotonic)

    def test_replaced_parent_does_not_authorize_its_old_child(self):
        self.kernel.add(200, 100)
        self.kernel.add(300, 200)

        def replace_parent():
            # The parent exits, its child is adopted outside the job, and the parent's PID is reused inside it.
            self.kernel.exit(200, adopter=1)
            self.kernel.add(200, 100, start=20)

        self.kernel.on_stat[300] = replace_parent
        self.tree.discover(math.inf, time.monotonic)
        self.assertNotIn(300, self.tree.proved)
        self.kill_all()
        self.assertEqual([(pid, start) for pid, start, _, _ in self.kernel.sent], [(200, 20)])
        self.assertTrue(self.kernel.table[300].alive)

    def test_replaced_target_is_not_signalled(self):
        self.kernel.add(300, 100)

        def replace_target():
            self.kernel.exit(300)
            self.kernel.add(300, 1, start=30)

        self.kernel.on_stat[300] = replace_target
        self.assertEqual(self.kill_all(), [])
        self.assertEqual(self.kernel.sent, [])
        self.assertNotIn(300, self.tree.proved, "a stat read its live handle does not bind proved nothing")

    def test_exited_handle_never_reaches_a_replacement(self):
        self.kernel.add(300, 100)
        self.tree.discover(math.inf, time.monotonic)
        self.kernel.exit(300)
        self.kernel.add(300, 1, start=30)
        self.assertEqual(self.tree.deliver(signal.SIGKILL, "test", lambda pid, start: True, math.inf, time.monotonic),
                         [])
        self.assertEqual(self.kernel.sent, [])

    def test_omitted_child_is_found_by_a_later_round(self):
        self.kernel.add(201, 100)
        self.kernel.add(202, 100)
        self.kernel.omit = [{202}]
        self.tree.discover(math.inf, time.monotonic)
        self.assertEqual(set(self.tree.proved), {201})
        self.kernel.exit(201)
        self.tree.discover(math.inf, time.monotonic)
        self.assertEqual(set(self.tree.proved), {202})

    def test_reparented_orphan_keeps_its_proved_handle(self):
        self.kernel.add(200, 100)
        self.kernel.add(300, 200)
        self.tree.discover(math.inf, time.monotonic)
        self.kernel.exit(200)
        self.tree.discover(math.inf, time.monotonic)
        self.assertEqual(self.tree.live(), [300])

    def test_broad_parent_traversal_and_delivery_stop_at_their_deadlines_and_resume(self):
        clock = FakeClock()
        self.kernel.clock = clock
        for pid in range(201, 211):
            self.kernel.add(pid, 100)
        reads = []
        read = self.kernel.stat

        def slow_read(pid):
            clock.value += 0.1
            reads.append(pid)
            return read(pid)

        self.kernel.stat = slow_read
        self.tree.discover(0.15, clock.now)
        self.assertLessEqual(clock.now(), 0.25 + 1e-9, "one parent's children ran past the cutoff")
        rounds = 0
        while not self.tree.discover(clock.now() + 0.15, clock.now) and rounds < 50:
            rounds += 1
        self.assertEqual(len(self.tree.proved), 10)
        self.assertEqual(sorted(reads), list(range(201, 211)), "a cutoff lost traversal progress")
        send = self.kernel.send

        def slow_send(fd, sig):
            clock.value += 0.1
            return send(fd, sig)

        self.kernel.send = slow_send
        started = clock.now()
        sent = self.tree.deliver(signal.SIGTERM, "test", lambda pid, start: True, started + 0.15, clock.now)
        self.assertLessEqual(clock.now(), started + 0.25 + 1e-9)
        self.assertLess(len(sent), 10)

    def test_unreadable_target_metadata_closes_its_handle_each_round(self):
        self.kernel.add(201, 100)

        def unreadable(pid):
            raise OwnershipFailure("read stat for pid 201: Permission denied")

        self.kernel.stat = unreadable
        for _ in range(3):
            with self.assertRaises(OwnershipFailure):
                self.tree.edge(201, 100)
        self.assertEqual(len(self.kernel.handles), 3)
        self.assertEqual(sorted(self.kernel.closed), sorted(self.kernel.handles))
        self.assertEqual(self.tree.proved, {})

    def test_missing_metadata_for_a_live_target_is_a_failure(self):
        self.kernel.add(201, 100)
        self.kernel.stat = lambda pid: None
        with self.assertRaises(OwnershipFailure):
            self.tree.edge(201, 100)
        self.assertEqual(sorted(self.kernel.closed), sorted(self.kernel.handles))

    def test_missing_metadata_for_an_exited_target_is_ordinary_exit(self):
        self.kernel.add(201, 100)

        def gone(pid):
            self.kernel.exit(201)
            return None

        self.kernel.stat = gone
        self.assertFalse(self.tree.edge(201, 100))
        self.assertEqual(sorted(self.kernel.closed), sorted(self.kernel.handles))

    def test_owner_validation_failure_closes_the_owner_handle(self):
        launcher = object.__new__(Launcher)
        launcher.kernel = self.kernel
        launcher.control = Control(None)
        self.kernel.add(300, 1, start=5)

        def unreadable(pid):
            raise OwnershipFailure("read stat for pid 300: Permission denied")

        for stat in (unreadable, lambda pid: Stat("S", 1, 6), lambda pid: None):
            self.kernel.stat = stat
            with self.subTest(stat=stat):
                self.assertRaises(Declined, launcher.open_owner, 300, 5, 299)
        self.assertEqual(len(self.kernel.handles), 3)
        self.assertEqual(sorted(self.kernel.closed), sorted(self.kernel.handles))


def recording_refusal(code, calls):
    def refuse(fd, sig):
        calls.append(sig)
        raise OSError(code, os.strerror(code))
    return refuse


def unreadable_metadata(pid):
    raise OwnershipFailure(f"read pid {pid}: Permission denied")


@unittest.skipUnless(LINUX, "owned commands need Linux child subreaping")
class CapabilityTest(unittest.TestCase):
    def assert_refused(self, kernel):
        before = os.listdir("/proc/self/fd")
        with self.assertRaises(Unsupported):
            kernel.require()
        self.assertEqual(len(os.listdir("/proc/self/fd")), len(before), "preflight leaked a descriptor")

    def test_unusable_pidfd_signaling_is_refused_before_work(self):
        for code in (errno.ENOSYS, errno.EPERM):
            calls = []
            kernel = Kernel()
            kernel.pidfd_send_signal = recording_refusal(code, calls)
            with self.subTest(errno=errno.errorcode[code]):
                self.assert_refused(kernel)
                self.assertEqual(calls, [0])

    def test_unreadable_self_metadata_is_refused_before_work(self):
        for name, replacement in (("stat", unreadable_metadata), ("stat", lambda pid: None),
                                  ("children", unreadable_metadata)):
            kernel = Kernel()
            setattr(kernel, name, replacement)
            with self.subTest(interface=name):
                self.assert_refused(kernel)


class OwnerStagesTest(unittest.TestCase):
    def setUp(self):
        silent_signals(self)
        self.enterContext(contextlib.redirect_stderr(io.StringIO()))
        self.directory = Path(tempfile.mkdtemp(prefix="m-owned-stage-"))
        self.addCleanup(shutil.rmtree, self.directory)
        self.clock = FakeClock()
        self.kernel = FakeKernel()
        self.kernel.clock = self.clock

    def owner(self, at=None, sig=signal.SIGTERM):
        record = Record(self.directory / "record")
        record.directory.mkdir()
        record.write("state.json", {"state": "running"})
        owner = Owner({"name": "fake"}, dict(FAST), record, FakeTriggers(self.clock, at, sig), self.kernel,
                      Control(None), clock=self.clock.now, pid=100)
        owner.root = self.directory / "m-isolate-fake"
        owner.root.mkdir()
        return owner

    def accepted_owner(self, **admission):
        """An owner that has just been accepted and will run admission's setup and command."""
        record = Record(self.directory / "record")
        record.directory.mkdir()
        record.write("state.json", {"state": "started"})
        admission = {"name": "fake", "env": {}, "scratch_parent": str(self.directory), **admission}
        return Owner(admission, dict(FAST), record, FakeTriggers(self.clock), self.kernel, Control(None),
                     clock=self.clock.now, pid=100)

    def test_reused_pid_status_cannot_finish_a_new_setup_or_leader(self):
        owner = self.accepted_owner(setup=[["/fake/setup"]], command=["/fake/leader"])
        # An earlier child with this PID was already reaped; the kernel may hand the PID out again.
        owner.reaper.statuses[777] = 0
        spawned = []

        def spawn(path, argv, environment, **_):
            spawned.append((path, self.clock.now()))
            status = 0 if path == "/fake/setup" else 7
            self.kernel.later.append((self.clock.now() + 0.05, 777, status << 8))
            return 777

        with patch("owned_command.os.posix_spawn", side_effect=spawn):
            owner.run(default_prepare)
        state = owner.record.read()
        self.assertEqual([path for path, _ in spawned], ["/fake/setup", "/fake/leader"])
        self.assertGreaterEqual(spawned[1][1], 0.05, "the leader started before setup's own status arrived")
        self.assertEqual((state["state"], state["status"]), ("joined", 7))

    def test_pending_cancellation_stops_the_next_setup_command(self):
        owner = self.accepted_owner(setup=[["/fake/first"], ["/fake/second"]], command=["/fake/leader"])
        spawned = []

        def spawn(path, argv, environment, **_):
            spawned.append(path)
            # The command publishes cancellation, then exits successfully.
            owner.triggers.at = self.clock.now()
            self.kernel.later.append((self.clock.now(), 900 + len(spawned), 0))
            return 900 + len(spawned)

        with patch("owned_command.os.posix_spawn", side_effect=spawn):
            owner.run(default_prepare)
        state = owner.record.read()
        self.assertEqual(spawned, ["/fake/first"])
        self.assertEqual((state["state"], state["status"]), ("joined", 143))

    def test_continuously_ready_reaper_stops_at_the_kill_deadline(self):
        self.kernel.add(201, 100).killable = False
        self.kernel.flood = 100000
        state = self.cancel(self.owner(at=0))
        self.assertEqual(state["state"], "retained")
        self.assertLessEqual(self.clock.now(), FAST["grace"] + FAST["kill_wait"] + FAST["tick"] + 0.01)

    def cancel(self, owner):
        owner.begin(owner.triggers.check())
        owner.cleanup()
        owner.finish()
        return owner.record.read()

    def signals(self, sig):
        return [pid for pid, _, sent, _ in self.kernel.sent if sent == sig]

    def test_omitted_pre_trigger_descendant_gets_one_cooperative_signal(self):
        self.kernel.add(201, 100)
        self.kernel.add(202, 100)
        self.kernel.omit = [{202}]
        state = self.cancel(self.owner(at=0))
        self.assertEqual(sorted(self.signals(signal.SIGTERM)), [201, 202])
        self.assertEqual(sorted(self.signals(signal.SIGKILL)), [201, 202])
        self.assertEqual((state["state"], state["status"]), ("joined", 143))
        self.assertFalse((self.directory / "m-isolate-fake").exists())

    def test_process_started_after_the_trigger_is_killed_not_terminated(self):
        self.kernel.add(201, 100, start=2000)
        state = self.cancel(self.owner(at=0))
        self.assertEqual(self.signals(signal.SIGTERM), [])
        kills = [when for pid, _, sent, when in self.kernel.sent if sent == signal.SIGKILL]
        self.assertGreaterEqual(kills[0], FAST["grace"])
        self.assertEqual(state["state"], "joined")

    def test_empty_listing_is_not_a_join(self):
        self.kernel.hidden = True
        state = self.cancel(self.owner(at=0))
        self.assertEqual(state["state"], "retained")
        self.assertTrue((self.directory / "m-isolate-fake").exists())
        self.assertAlmostEqual(self.clock.now(), FAST["grace"] + FAST["kill_wait"], delta=0.05)

    def test_signal_permission_failure_retains_state(self):
        self.kernel.add(201, 100)

        def refuse(fd, sig):
            raise OwnershipFailure("signal through handle: Operation not permitted")

        self.kernel.send = refuse
        state = self.cancel(self.owner(at=0))
        self.assertEqual(state["state"], "retained")
        self.assertIn(201, state["pids"])
        self.assertTrue(any("not permitted" in reason for reason in state["reasons"]))
        self.assertTrue((self.directory / "m-isolate-fake").exists())

    def test_unreadable_ownership_retains_state_even_after_echild(self):
        self.kernel.add(201, 100)

        def unreadable(pid):
            self.kernel.exit(201)
            raise OwnershipFailure(f"read children of pid {pid}: Permission denied")

        self.kernel.children = unreadable
        state = self.cancel(self.owner(at=0))
        self.assertEqual(state["state"], "retained")
        self.assertTrue((self.directory / "m-isolate-fake").exists())

    def test_missing_leader_status_retains_state(self):
        owner = self.owner(at=0)
        owner.leader = 500
        state = self.cancel(owner)
        self.assertEqual(state["state"], "retained")
        self.assertIn("leader status is missing", state["reasons"])

    def test_survivor_after_the_kill_deadline_retains_state(self):
        self.kernel.add(201, 100).killable = False
        state = self.cancel(self.owner(at=0))
        self.assertEqual(state["state"], "retained")
        self.assertEqual(state["pids"], [201])

    def test_cancellation_during_leak_cleanup_keeps_its_deadlines(self):
        for at, cooperative in ((0.05, signal.SIGINT), (0.2, signal.SIGTERM)):
            with self.subTest(trigger_at=at):
                self.setUp()
                self.kernel.add(201, 100)
                owner = self.owner(at=at, sig=signal.SIGINT)
                owner.status = 0
                owner.schedule_settle(0.0)
                deadlines = (owner.coop_end, owner.kill_end)
                owner.cleanup()
                owner.finish()
                self.assertEqual((owner.coop_end, owner.kill_end), deadlines)
                self.assertEqual(self.signals(cooperative), [201])
                kill = next(when for _, _, sent, when in self.kernel.sent if sent == signal.SIGKILL)
                self.assertAlmostEqual(kill, deadlines[0], delta=0.02)
                self.assertEqual(owner.record.read()["status"], 0)


class RecordTest(unittest.TestCase):
    def test_records_only_move_forward(self):
        with tempfile.TemporaryDirectory(prefix="m-owned-record-") as temporary:
            record = Record(temporary)
            deadline = time.monotonic() + 1
            record.write("state.json", {"state": "admitted"})
            self.assertTrue(record.advance("started", deadline))
            self.assertTrue(record.advance("running", deadline, root="/r"))
            self.assertFalse(record.advance("declined", deadline))
            self.assertTrue(record.advance("joined", deadline, status=0))
            self.assertFalse(record.advance("retained", deadline))
            self.assertEqual(record.read()["root"], "/r")
            (Path(temporary) / "state.json").write_text('{"state": "runn')
            self.assertEqual(record.read(), {"state": "unknown"})
            self.assertFalse(record.advance("joined", deadline))


@unittest.skipUnless(LINUX, "owned commands need Linux child subreaping")
class OwnedCommandTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runner = establish_runner()
        cls.sentinel = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(600)"],
                                        start_new_session=True)

    @classmethod
    def tearDownClass(cls):
        cls.sentinel.kill()
        cls.sentinel.wait()
        os.close(cls.runner["fd"])

    def setUp(self):
        self.addCleanup(self.teardown_scene)
        self.make_scene()

    def make_scene(self):
        self.root = Path(tempfile.mkdtemp(prefix="m-owned-test-"))
        self.processes = []
        self.out, self.scratch, self.control = (self.root / name for name in ("out", "scratch", "control"))
        for directory in (self.out, self.scratch, self.control):
            directory.mkdir()
        self.work_script = self.root / "work.py"
        self.work_script.write_text(WORK)
        self.runner_script = self.root / "runner.py"
        self.runner_script.write_text(RUNNER)

    def teardown_scene(self):
        for hold in self.control.glob("hold-*"):
            hold.unlink()
        for process in self.processes:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
        cleared = remove_descendants({self.sentinel.pid})
        shutil.rmtree(self.root)
        self.assertTrue(cleared, "teardown could not remove test processes")
        self.assertIsNone(self.sentinel.poll(), "the unrelated sentinel was signalled")

    def next_scene(self):
        self.teardown_scene()
        self.make_scene()

    def work(self, mode, *arguments):
        return [sys.executable, "-B", str(self.work_script), str(self.out), mode, *arguments]

    def job(self, command, name, **settings):
        return {**FAST, "name": name, "command": command, "env": {"PATH": os.environ.get("PATH", os.defpath)},
                "scratch_parent": str(self.scratch), "control": str(self.control),
                "cancel": [str(self.root / "cancel")], **settings}

    def start(self, command, name="job", popen=None, **settings):
        record = self.root / name
        admit(record, self.runner, **self.job(command, name, **settings))
        with open(self.root / f"{name}.log", "w") as log:
            process = launch(record, self.runner, stdout=log, stderr=subprocess.STDOUT, **(popen or {}))
        self.processes.append(process)
        return process, record

    def start_runner(self, command, role="runner", **settings):
        record = self.root / "job"
        job = json.dumps(self.job(command, "job", **settings))
        process = subprocess.Popen([sys.executable, "-B", str(self.runner_script), str(HELPERS), role,
                                    str(record), job], cwd=self.root)
        self.processes.append(process)
        runner = int(wait_until(lambda: complete_line(self.root / "runner"), "runner start"))
        return process, record, runner

    def outcome(self, process, record, timeout=5.0):
        return wait_until(lambda: observe(process, record), "a proved outcome", timeout)

    def final(self, record, timeout=5.0):
        return wait_until(lambda: Record(record).read() if Record(record).read()["state"] in
                          ("joined", "retained", "declined") else None, "a final record", timeout)

    def hold(self, name):
        (self.control / f"hold-{name}").touch()

    def release(self, name):
        (self.control / f"hold-{name}").unlink()

    def reached(self, name):
        return int(wait_until(lambda: complete_line(self.control / f"at-{name}"), f"hold {name}"))

    def fault(self, name):
        (self.control / f"fault-{name}").touch()

    def marker(self, name):
        return int(wait_until(lambda: (self.out / name).exists() and (self.out / name).read_text(), name))

    def log(self, name="job"):
        return (self.root / f"{name}.log").read_text()

    def assert_joined(self, state, status, process=None):
        self.assertEqual((state["state"], state.get("status")), ("joined", status), self.log())
        self.assertTrue((self.root / "job" / "owner-exit").exists())
        self.assertEqual(list(self.scratch.iterdir()), [], "inner root survived a join")
        if process is not None:
            self.assertEqual(process.returncode, status)

    def assert_declined(self, state, reason, status=1):
        self.assertEqual(state["state"], "declined", self.log())
        self.assertIn(reason, state["reason"])
        self.assertEqual(state["status"], status)
        self.assert_nothing_started()

    def assert_nothing_started(self):
        self.assertEqual(list(self.out.glob("ran-*")), [], "workload started without acceptance")
        self.assertEqual(list(self.scratch.iterdir()), [], "owner created state without acceptance")

    def test_success_keeps_actual_status_with_inherited_ignored_sigchld(self):
        process, record = self.start(self.work("exit", "7"),
                                     popen={"preexec_fn": lambda: signal.signal(signal.SIGCHLD, signal.SIG_IGN)})
        self.assert_joined(self.outcome(process, record), 7, process)

    def test_fast_orphan_on_the_normal_path_is_reported_killed_and_joined(self):
        process, record = self.start(self.work("orphan-exit"))
        state = self.outcome(process, record)
        orphan = self.marker("orphan")
        self.assert_joined(state, 0, process)
        self.assertIn(f"LEAK: job: pid {orphan}", self.log())
        self.assertFalse(running(orphan))

    def test_fast_orphan_on_cancellation_is_killed_and_joined(self):
        process, record = self.start(self.work("orphan-stay"))
        orphan = self.marker("orphan")
        process.send_signal(signal.SIGTERM)
        self.assert_joined(self.outcome(process, record), 143, process)
        self.assertFalse(running(orphan))

    def test_first_signal_decides_the_status(self):
        process, record = self.start(self.work("sleep"))
        self.marker("ran-sleep")
        process.send_signal(signal.SIGTERM)
        wait_until(lambda: (record / "cancel").exists(), "forwarded cancellation")
        process.send_signal(signal.SIGINT)
        self.assert_joined(self.outcome(process, record), 143, process)

    def test_budget_expiry_reports_124(self):
        process, record = self.start(self.work("sleep"), budget=0.5)
        self.assert_joined(self.outcome(process, record), 124, process)

    def test_helper_started_after_cancellation_is_killed_without_term(self):
        process, record = self.start(self.work("helper-after-term"))
        self.marker("ran-helper-after-term")
        process.send_signal(signal.SIGTERM)
        helper = self.marker("helper")
        self.assert_joined(self.outcome(process, record), 143, process)
        self.assertFalse((self.out / "helper-term").exists(), "a post-trigger cleanup helper received TERM")
        self.assertFalse(running(helper))

    def test_fork_churn_through_the_kill_stage_is_joined(self):
        process, record = self.start(self.work("forker"))
        wait_until(lambda: (self.out / "forked").exists(), "forks")
        process.send_signal(signal.SIGTERM)
        self.assert_joined(self.outcome(process, record), 143, process)
        forked = [int(pid) for pid in (self.out / "forked").read_text().split()]
        self.assertEqual([pid for pid in forked if running(pid)], [])

    def test_nested_subreaper_and_its_orphan_are_joined(self):
        process, record = self.start(self.work("nested"))
        state = self.outcome(process, record)
        nested, orphan = self.marker("nested"), self.marker("nested-orphan")
        self.assert_joined(state, 0, process)
        self.assertFalse(running(nested) or running(orphan))

    def test_hanging_setup_services_cancellation_promptly(self):
        process, record = self.start(self.work("exit", "0"), setup=[self.work("setup-hang")], budget=600)
        setup = self.marker("ran-setup-hang")
        started = time.monotonic()
        request_cancel(self.root / "cancel", signal.SIGTERM)
        self.assert_joined(self.outcome(process, record), 143, process)
        self.assertLess(time.monotonic() - started, FAST["grace"] + FAST["kill_wait"] + 1)
        self.assertFalse(running(setup))
        self.assertFalse((self.out / "ran-exit").exists(), "work started after cancellation")

    def test_setup_counts_against_the_budget(self):
        process, record = self.start(self.work("exit", "0"), setup=[self.work("setup-hang")], budget=0.5)
        self.assert_joined(self.outcome(process, record), 124, process)
        self.assertFalse((self.out / "ran-exit").exists())

    def test_runner_death_during_setup_cancels_promptly(self):
        process, record, _ = self.start_runner(self.work("exit", "0"), setup=[self.work("setup-hang")], budget=600)
        setup = self.marker("ran-setup-hang")
        process.kill()
        process.wait()
        state = self.final(record, timeout=FAST["grace"] + FAST["kill_wait"] + 1)
        self.assertEqual((state["state"], state["status"]), ("joined", 143), self.log())
        wait_until(lambda: (record / "owner-exit").exists(), "owner exit observation")
        self.assertFalse(running(setup))

    def test_runner_death_before_the_first_parent_read_declines(self):
        self.hold("launcher-parent")
        process, record, _ = self.start_runner(self.work("exit", "0"))
        launcher = self.reached("launcher-parent")
        process.kill()
        process.wait()
        wait_until(lambda: stat_fields(launcher) and int(stat_fields(launcher)[1]) == os.getpid(), "adoption")
        self.release("launcher-parent")
        self.assert_declined(self.final(record), "runner exited before the launcher read its parent", 143)
        self.assertNotIn("owner", Record(record).read())

    def test_runner_identity_must_match_the_admission(self):
        record = self.root / "job"
        admit(record, {**self.runner, "start": self.runner["start"] + 1}, **self.job(self.work("exit", "0"), "job"))
        with open(self.root / "job.log", "w") as log:
            process = launch(record, self.runner, stdout=log, stderr=subprocess.STDOUT)
        self.processes.append(process)
        self.assert_declined(self.outcome(process, record), "runner identity does not match")

    def test_cancellation_before_start_declines(self):
        request_cancel(self.root / "cancel", signal.SIGINT)
        process, record = self.start(self.work("exit", "0"))
        self.assert_declined(self.outcome(process, record), "cancelled before start", 130)
        self.assertEqual(process.returncode, 130)

    def test_cancellation_before_acceptance_declines(self):
        self.hold("launcher-accept")
        process, record = self.start(self.work("exit", "0"))
        self.reached("launcher-accept")
        owner = Record(record).read()["owner"]["pid"]
        process.send_signal(signal.SIGTERM)
        self.release("launcher-accept")
        self.assert_declined(self.outcome(process, record), "cancelled before start", 143)
        wait_until(lambda: not running(owner), "unaccepted owner exit")

    def test_launcher_death_before_acceptance_starts_nothing(self):
        self.hold("launcher-accept")
        process, record = self.start(self.work("exit", "0"))
        self.reached("launcher-accept")
        owner = Record(record).read()["owner"]["pid"]
        process.kill()
        process.wait()
        wait_until(lambda: not running(owner), "unaccepted owner exit")
        self.assertEqual(Record(record).read()["state"], "started")
        self.assertEqual(observe(process, record)["state"], "unknown")
        self.assert_nothing_started()

    def test_cancellation_after_acceptance_is_joined_never_declined(self):
        self.hold("owner-accepted")
        process, record = self.start(self.work("exit", "0"))
        self.reached("owner-accepted")
        process.send_signal(signal.SIGTERM)
        wait_until(lambda: (record / "cancel").exists(), "forwarded cancellation")
        self.release("owner-accepted")
        self.assert_joined(self.outcome(process, record), 143, process)
        self.assertFalse((self.out / "ran-exit").exists())

    def test_accepted_owner_death_is_retained_not_declined(self):
        self.hold("owner-accepted")
        process, record = self.start(self.work("exit", "0"))
        owner = self.reached("owner-accepted")
        kill_exact(owner)
        state = self.outcome(process, record)
        self.assertEqual(state["state"], "retained", self.log())
        self.assertIn("owner exited without a final record", state["reasons"])
        self.assertEqual(process.returncode, 1)

    def test_handshake_failures_decline_before_any_work(self):
        cases = {"owner-silent": "did not report", "owner-partial": "before a complete line",
                 "owner-oversized": "size limit", "owner-close-accept": "closed acceptance",
                 "owner-prctl": "enable child subreaper", "launcher-fork": "fork owner",
                 "launcher-pidfd": "open owner handle"}
        for fault, reason in cases.items():
            with self.subTest(fault=fault):
                self.next_scene()
                self.fault(fault)
                process, record = self.start(self.work("exit", "0"), handshake=0.5)
                self.assert_declined(self.outcome(process, record), reason)

    def test_unusable_pidfd_signaling_declines_before_any_work(self):
        for fault in ("owner-pidfd-enosys", "owner-pidfd-eperm"):
            with self.subTest(fault=fault):
                self.next_scene()
                self.fault(fault)
                process, record = self.start(self.work("exit", "0"))
                self.assert_declined(self.outcome(process, record), "interfaces are unavailable")

    def test_joined_record_before_owner_exit_is_not_completion(self):
        self.hold("owner-joined")
        process, record = self.start(self.work("exit", "0"))
        owner = self.reached("owner-joined")
        self.assertEqual(Record(record).read()["state"], "joined")
        self.assertIsNone(observe(process, record))
        process.send_signal(signal.SIGSTOP)
        self.release("owner-joined")
        wait_until(lambda: not running(owner), "owner exit")
        time.sleep(0.05)
        self.assertIsNone(observe(process, record), "a stopped launcher was treated as exited")
        process.send_signal(signal.SIGCONT)
        self.assert_joined(self.outcome(process, record), 0, process)

    def test_owner_exit_before_launcher_exit_is_not_completion(self):
        self.hold("launcher-exit")
        process, record = self.start(self.work("exit", "0"))
        self.reached("launcher-exit")
        self.assertTrue((record / "owner-exit").exists())
        self.assertIsNone(observe(process, record))
        self.release("launcher-exit")
        self.assert_joined(self.outcome(process, record), 0, process)

    def test_declined_record_with_a_stuck_launcher_is_not_completion(self):
        self.hold("launcher-declined")
        request_cancel(self.root / "cancel", signal.SIGTERM)
        process, record = self.start(self.work("exit", "0"))
        self.reached("launcher-declined")
        self.assertEqual(Record(record).read()["state"], "declined")
        self.assertIsNone(observe(process, record))
        self.release("launcher-declined")
        self.assert_declined(self.outcome(process, record), "cancelled before start", 143)

    def test_stopped_owner_is_retained_at_the_observation_cutoff(self):
        process, record = self.start(self.work("sleep"), budget=0.3)
        self.marker("ran-sleep")
        owner = Record(record).read()["owner"]["pid"]
        os.kill(owner, signal.SIGSTOP)
        cutoff = 0.3 + max(FAST["settle"] + FAST["leak_grace"], FAST["grace"]) + FAST["kill_wait"] + FAST["allowance"]
        state = self.outcome(process, record, timeout=cutoff + 1)
        self.assertEqual(state["state"], "retained")
        self.assertTrue(running(owner), "the stopped owner must still be live")
        self.assertFalse((record / "owner-exit").exists())
        self.assertEqual(process.returncode, 1)

    def test_signal_failure_retains_both_roots(self):
        self.fault("owner-eperm")
        process, record = self.start(self.work("orphan-exit"))
        state = self.outcome(process, record)
        orphan = self.marker("orphan")
        self.assertEqual(state["state"], "retained", self.log())
        self.assertIn(orphan, state["pids"])
        self.assertTrue(Path(state["root"]).is_dir(), "inner root deleted after a failed signal")
        self.assertTrue(running(orphan))

    def test_survivor_after_the_kill_wait_is_retained(self):
        self.fault("owner-no-kill")
        process, record = self.start(self.work("orphan-exit"))
        state = self.outcome(process, record)
        self.assertEqual(state["state"], "retained", self.log())
        self.assertIn(self.marker("orphan"), state["pids"])
        self.assertTrue(Path(state["root"]).is_dir())

    def test_owner_leaves_a_containing_worker_unless_a_subreaper_intervenes(self):
        for role, adopted in (("session-worker", False), ("subreaper-session-worker", True)):
            with self.subTest(role=role):
                self.next_scene()
                process, record, runner = self.start_runner(self.work("chain"), role=role)
                state = self.final(record)
                self.assertEqual((state["state"], state["status"]), ("joined", 0), self.log())
                ancestor = int((self.root / "ancestor").read_text())
                chain = [int(pid) for pid in (self.out / "chain").read_text().split()]
                owner = state["owner"]
                self.assertEqual(chain[1], owner["pid"])
                self.assertEqual(owner["adopter"], ancestor if adopted else os.getpid())
                self.assertEqual(ancestor in chain, adopted)
                self.assertNotIn(runner, chain)
                self.assertTrue(running(ancestor))
                self.assertFalse((self.root / "ancestor-signalled").exists())


def setUpModule():
    if LINUX and ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "enable the test's own subreaper")


if __name__ == "__main__":
    unittest.main()
