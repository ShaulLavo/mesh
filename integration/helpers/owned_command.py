#!/usr/bin/env python3
"""Run one admitted command under a Linux subreaper owner that joins every descendant.

The controller writes an admission, then starts this launcher as its direct
child with an inherited pidfd for itself. The launcher double-forks an owner
that leaves the caller's ancestry and becomes a child subreaper. The owner
starts work only after the launcher accepts it. On its first trigger it sends
one cooperative signal to descendants that existed before the trigger, kills
everything at the grace deadline, and deletes its scratch root only after
waitpid reports that no descendant is left.

The state record advances monotonically: admitted, started, running, then
joined, retained or declined. Joined proves the descendant join only. The
launcher writes owner-exit after its owner's pidfd reports exit, and the
controller observes the launcher's own exit through its Popen object.

Standalone use outside a controller is not supported: the launcher declines
unless its parent is the admitted runner and the runner's handle was inherited.
"""
import errno
import fcntl
import json
import os
from pathlib import Path
import select
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import traceback
from typing import NamedTuple

sys.dont_write_bytecode = True

from process_tree import ECHILD, IDLE, MORE, Kernel, OwnershipFailure, Reaper, Tree, Unsupported  # noqa: E402 - copied helpers stay bytecode-free

TERMINATION_SIGNALS = (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)
REPORT_LIMIT = 128
LEAK_LINES = 32
FINAL = frozenset({"joined", "retained", "declined"})
NEXT = {"admitted": {"started", "declined"}, "started": {"running", "declined", "retained"},
        "running": {"joined", "retained"}}
# Seconds. Budget runs from launcher start and includes owner setup.
DEFAULTS = {"budget": 30.0, "grace": 30.0, "kill_wait": 5.0, "settle": 5.0, "leak_grace": 5.0,
            "handshake": 5.0, "allowance": 5.0, "tick": 0.1}


class Trigger(NamedTuple):
    sig: int
    status: int
    reason: str
    time: float


class Declined(Exception):
    def __init__(self, reason, status):
        super().__init__(reason)
        self.status = status


class Interrupted(Exception):
    pass


class SetupFailed(Exception):
    def __init__(self, status):
        super().__init__(f"setup exited with status {status}")
        self.status = status


def readable(fd):
    poller = select.poll()
    poller.register(fd, select.POLLIN)
    return bool(poller.poll(0))


def read_available(fd):
    """Bytes ready on a nonblocking fd, b"" at EOF, or None when nothing is ready."""
    try:
        return os.read(fd, REPORT_LIMIT + 1)
    except BlockingIOError:
        return None


def write_all(fd, data):
    view = memoryview(data)
    while view:
        view = view[os.write(fd, view):]


def try_lock(lock):
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return False
    return True


def lock_until(lock, deadline):
    while not try_lock(lock):
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.005)
    return True


class Record:
    """Atomic, monotonic job records. A partial write is never visible."""

    def __init__(self, directory):
        self.directory = Path(directory)

    def admission(self):
        return json.loads((self.directory / "admission.json").read_text())

    def read(self):
        try:
            value = json.loads((self.directory / "state.json").read_text())
        except (OSError, ValueError):
            return {"state": "unknown"}
        return value if isinstance(value, dict) else {"state": "unknown"}

    def write(self, name, value):
        temporary = self.directory / f".{name}.{os.getpid()}"
        temporary.write_text(json.dumps(value, sort_keys=True) + "\n")
        os.replace(temporary, self.directory / name)

    def advance(self, state, deadline, **fields):
        """Move to state only along NEXT. False when the record is elsewhere or locked past deadline."""
        with open(self.directory / "lock", "a") as lock:
            if not lock_until(lock, deadline):
                return False
            current = self.read()
            if state not in NEXT.get(current.get("state"), ()):
                return False
            self.write("state.json", {**current, **fields, "state": state, "previous": current.get("state")})
            return True


def request_cancel(path, sig):
    """Publish the first cancellation signal. Later requests leave it unchanged."""
    path = Path(path)
    temporary = path.with_name(f".{path.name}.{os.getpid()}")
    temporary.write_text(f"{int(sig)}\n")
    try:
        os.link(temporary, path)
    except FileExistsError:
        pass
    finally:
        temporary.unlink()


def read_cancel(path):
    try:
        text = Path(path).read_text().strip()
    except FileNotFoundError:
        return None
    except OSError:
        return signal.SIGTERM
    return int(text) if text.isdigit() and int(text) in TERMINATION_SIGNALS else signal.SIGTERM


class Triggers:
    """Cancellation sources. The first observed trigger wins and never changes."""

    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.received = None
        self.first = None
        self.runner_fd = None
        self.peer_fd = None
        self.cancel_files = ()
        self.budget_deadline = None
        self.wakeup_r, self.wakeup_w = os.pipe()
        os.set_blocking(self.wakeup_r, False)
        os.set_blocking(self.wakeup_w, False)
        # A handler that does not raise leaves poll restarted; the wakeup fd ends it.
        signal.set_wakeup_fd(self.wakeup_w, warn_on_full_buffer=False)
        for signum in TERMINATION_SIGNALS:
            signal.signal(signum, self.note)

    def note(self, signum, _):
        if self.received is None:
            self.received = signum

    def release(self):
        os.close(self.wakeup_r)
        os.close(self.wakeup_w)

    def check(self):
        if self.first is None:
            self.first = self.observe()
        return self.first

    def observe(self):
        now = self.clock()
        if self.received is not None:
            return Trigger(self.received, 128 + self.received, f"signal {self.received}", now)
        for path in self.cancel_files:
            requested = read_cancel(path)
            if requested is not None:
                return Trigger(requested, 128 + requested, f"cancel file {path}", now)
        if self.runner_fd is not None and readable(self.runner_fd):
            return Trigger(signal.SIGTERM, 143, "runner exited", now)
        if self.peer_fd is not None and readable(self.peer_fd):
            return Trigger(signal.SIGTERM, 143, "launcher exited", now)
        if self.budget_deadline is not None and now >= self.budget_deadline:
            return Trigger(signal.SIGTERM, 124, "budget expired", now)
        return None

    def wait(self, fds, timeout):
        watched = {*fds, self.wakeup_r}
        if self.first is None:
            watched |= {self.runner_fd, self.peer_fd} - {None}
        poller = select.poll()
        for fd in watched:
            poller.register(fd, select.POLLIN)
        poller.poll(max(0.0, timeout) * 1000)
        while read_available(self.wakeup_r):
            pass


class Control:
    """Test-only holds and faults, inactive unless the admission names a control directory."""

    def __init__(self, directory):
        self.directory = Path(directory) if directory else None

    def fault(self, name):
        return self.directory is not None and (self.directory / f"fault-{name}").exists()

    def hold(self, name):
        if self.directory is None or not (self.directory / f"hold-{name}").exists():
            return
        (self.directory / f"at-{name}").write_text(f"{os.getpid()}\n")
        while (self.directory / f"hold-{name}").exists():
            time.sleep(0.005)


def handle_pid(fd):
    lines = Path(f"/proc/self/fdinfo/{fd}").read_text().splitlines()
    for line in lines:
        if line.startswith("Pid:"):
            return int(line.split()[1])
    raise OSError(errno.EBADF, f"descriptor {fd} is not a process handle")


def command_line(pid):
    try:
        with open(f"/proc/{pid}/cmdline", "rb") as source:
            return source.read(512).replace(b"\0", b" ").decode(errors="replace").strip()
    except OSError:
        return "?"


def work_allowance(s):
    """Seconds from launcher start until the launcher gives up on its owner without a trigger."""
    return s["budget"] + max(s["settle"] + s["leak_grace"], s["grace"]) + s["kill_wait"] + s["allowance"]


def cancel_allowance(s):
    """Seconds from a trigger until the launcher gives up on its owner."""
    return s["grace"] + s["kill_wait"] + s["allowance"]


def exit_status(state):
    status = state.get("status")
    if state.get("state") == "joined" and isinstance(status, int):
        return status
    return status if isinstance(status, int) and status else 1


class Launcher:
    """The controller's direct child. It accepts one owner and outlives it."""

    def __init__(self, directory, prepare):
        self.triggers = Triggers()
        self.start = time.monotonic()
        self.prepare = prepare
        self.record = Record(directory)
        self.admission = self.record.admission()
        self.settings = {name: float(self.admission.get(name, value)) for name, value in DEFAULTS.items()}
        self.control = Control(self.admission.get("control"))
        self.kernel = Kernel()
        self.triggers.cancel_files = (*self.admission.get("cancel", ()), self.record.directory / "cancel")

    def run(self):
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        signal.signal(signal.SIGPIPE, signal.SIG_IGN)
        try:
            self.kernel.require()
            self.control.hold("launcher-parent")
            self.check_runner()
            trigger = self.triggers.check()
            if trigger:
                raise Declined(f"cancelled before start: {trigger.reason}", trigger.status)
            owner = self.handshake()
        except Declined as declined:
            return self.decline(str(declined), declined.status)
        except (Unsupported, OwnershipFailure) as error:
            return self.decline(str(error), 1)
        return self.supervise(*owner)

    def decline(self, reason, status):
        print(f"FAIL: owned command declined: {reason}", file=sys.stderr, flush=True)
        self.record.advance("declined", time.monotonic() + self.settings["allowance"], reason=reason, status=status)
        self.control.hold("launcher-declined")
        return status

    def check_runner(self):
        runner = self.admission["runner"]
        if os.getppid() != runner["pid"]:
            raise Declined("runner exited before the launcher read its parent", 143)
        fd = int(runner["fd"])
        try:
            os.set_inheritable(fd, False)
            handle = handle_pid(fd)
            stat = self.kernel.stat(runner["pid"])
        except (OSError, OwnershipFailure) as error:
            raise Declined(f"runner handle is unavailable: {error}", 143) from error
        # The live handle after the stat read binds that read to the admitted runner.
        if handle != runner["pid"] or stat is None or stat.start != runner["start"]:
            raise Declined("runner identity does not match its admission", 1)
        if readable(fd) or os.getppid() != runner["pid"]:
            raise Declined("runner exited before the launcher checked its handle", 143)
        self.triggers.runner_fd = fd

    def handshake(self):
        deadline = self.start + self.settings["handshake"]
        report_r, report_w = os.pipe()
        accept_r, accept_w = os.pipe()
        try:
            if self.control.fault("launcher-fork"):
                raise OSError(errno.EAGAIN, "injected fork failure")
            intermediate = os.fork()
        except OSError as error:
            for fd in (report_r, report_w, accept_r, accept_w):
                os.close(fd)
            raise Declined(f"fork owner: {error}", 1) from error
        if intermediate == 0:
            become_intermediate(self, report_r, report_w, accept_r, accept_w)
        os.close(report_w)
        os.close(accept_r)
        try:
            return self.accept(report_r, accept_w, intermediate, deadline)
        finally:
            os.close(report_r)

    def accept(self, report_r, accept_w, intermediate, deadline):
        fd = None
        try:
            line = self.read_report(report_r, deadline)
            self.reap_intermediate(intermediate, deadline)
            pid, start, adopter = parse_report(line)
            fd = self.open_owner(pid, start, intermediate)
            self.offer(accept_w, deadline, owner={"pid": pid, "start": start, "adopter": adopter})
        except Declined:
            # Closing acceptance is the owner's EOF: it exits without starting anything.
            os.close(accept_w)
            if fd is not None:
                self.await_exit(fd, deadline + self.settings["allowance"])
                os.close(fd)
            raise
        return pid, fd, accept_w

    def offer(self, accept_w, deadline, **fields):
        if not self.record.advance("started", deadline + self.settings["allowance"], **fields):
            raise Declined("record was not admitted", 1)
        self.control.hold("launcher-accept")
        trigger = self.triggers.check()
        if trigger:
            raise Declined(f"cancelled before start: {trigger.reason}", trigger.status)
        if time.monotonic() >= deadline:
            raise Declined("handshake deadline passed before acceptance", 1)
        try:
            os.write(accept_w, b"A")
        except BrokenPipeError as error:
            # Only the owner holds the read end, so EPIPE proves it never read acceptance.
            raise Declined("owner closed acceptance before it was delivered", 1) from error
        except OSError:
            # Delivery is unknown; supervision treats a missing final record as retained.
            pass

    def read_report(self, fd, deadline):
        os.set_blocking(fd, False)
        data = b""
        while b"\n" not in data:
            trigger = self.triggers.check()
            if trigger:
                raise Declined(f"cancelled before start: {trigger.reason}", trigger.status)
            if len(data) > REPORT_LIMIT:
                raise Declined("owner report exceeded its size limit", 1)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Declined("owner did not report before the handshake deadline", 1)
            self.triggers.wait([fd], min(remaining, self.settings["tick"]))
            chunk = read_available(fd)
            if chunk == b"":
                raise Declined("owner report ended before a complete line", 1)
            data += chunk or b""
        line = data.split(b"\n", 1)[0]
        if len(line) > REPORT_LIMIT:
            raise Declined("owner report exceeded its size limit", 1)
        return line.decode(errors="replace")

    def reap_intermediate(self, pid, deadline):
        while time.monotonic() < deadline:
            try:
                reaped, _ = os.waitpid(pid, os.WNOHANG)
            except ChildProcessError as error:
                raise Declined(f"intermediate status was lost: {error}", 1) from error
            if reaped:
                return
            time.sleep(0.001)
        raise Declined("intermediate did not exit before the handshake deadline", 1)

    def open_owner(self, pid, start, intermediate):
        if self.control.fault("launcher-pidfd"):
            raise Declined("open owner handle: injected failure", 1)
        try:
            fd = self.kernel.pidfd_open(pid)
        except OwnershipFailure as error:
            raise Declined(str(error), 1) from error
        if fd is None:
            raise Declined("owner exited before acceptance", 1)
        try:
            self.check_owner(fd, pid, start, intermediate)
        except BaseException:
            # The handle is the launcher's only after the owner passes every check.
            self.kernel.close(fd)
            raise
        return fd

    def check_owner(self, fd, pid, start, intermediate):
        try:
            stat = self.kernel.stat(pid)
        except OwnershipFailure as error:
            raise Declined(str(error), 1) from error
        if stat is None or stat.start != start or stat.ppid in (intermediate, os.getpid()) or self.kernel.exited(fd):
            raise Declined("owner identity or reparenting does not match its report", 1)

    def await_exit(self, fd, deadline):
        while not readable(fd) and time.monotonic() < deadline:
            self.triggers.wait([fd], self.settings["tick"])

    def supervise(self, owner_pid, owner_fd, accept_w):
        s = self.settings
        cutoff = self.start + work_allowance(s)
        forwarded = False
        while not readable(owner_fd):
            trigger = self.triggers.check()
            if trigger and not forwarded:
                # The launcher never signals its owner; the owner reads this file.
                request_cancel(self.record.directory / "cancel", trigger.sig)
                forwarded = True
                cutoff = min(cutoff, trigger.time + cancel_allowance(s))
            if time.monotonic() >= cutoff:
                return self.unproved(owner_pid)
            self.triggers.wait([owner_fd], s["tick"])
        self.record.write("owner-exit", {"owner": owner_pid, "observed": "pidfd"})
        os.close(owner_fd)
        os.close(accept_w)
        state = self.record.read()
        if state.get("state") not in FINAL:
            self.record.advance("retained", time.monotonic() + s["allowance"],
                                reasons=["owner exited without a final record"])
            state = self.record.read()
        self.control.hold("launcher-exit")
        return exit_status(state)

    def unproved(self, owner_pid):
        reason = f"owner {owner_pid} exit was not observed before the deadline"
        print(f"FAIL: owned command: {reason}", file=sys.stderr, flush=True)
        self.record.advance("retained", time.monotonic() + self.settings["allowance"], reasons=[reason])
        return 1


def parse_report(line):
    fields = line.split()
    if fields[:1] == ["fail"]:
        raise Declined("owner setup failed: " + " ".join(fields[1:]), 1)
    try:
        pid, start, adopter = (int(value) for value in fields[1:])
    except ValueError as error:
        raise Declined(f"owner report is malformed: {line!r}", 1) from error
    if fields[0] != "ready" or min(pid, start, adopter) <= 0:
        raise Declined(f"owner report is malformed: {line!r}", 1)
    return pid, start, adopter


def become_intermediate(launcher, report_r, report_w, accept_r, accept_w):
    """Fork the owner and exit at once, so the owner is orphaned to the nearest subreaper."""
    code = 1
    try:
        os.close(report_r)
        os.close(accept_w)
        intermediate = os.getpid()
        code = 0 if os.fork() else become_owner(launcher, intermediate, report_w, accept_r)
    except BaseException:
        traceback.print_exc()
    finally:
        flush_streams()
        os._exit(code)


def flush_streams():
    # os._exit skips Python's own flush.
    for stream in (sys.stdout, sys.stderr):
        flush_stream(stream)


def flush_stream(stream):
    try:
        stream.flush()
    except (OSError, ValueError):
        pass


def report_payload(control, text):
    if control.fault("owner-silent"):
        return None
    if control.fault("owner-partial"):
        return text[:5].encode()
    if control.fault("owner-oversized"):
        return b"9" * (REPORT_LIMIT * 2)
    return (text + "\n").encode()


def become_owner(launcher, intermediate, report_w, accept_r):
    os.setsid()
    triggers = Triggers()
    launcher.triggers.release()
    control = launcher.control
    # Wait slightly past the launcher's own deadline so it always decides first.
    deadline = launcher.start + launcher.settings["handshake"] + launcher.settings["allowance"]
    while os.getppid() == intermediate:
        if time.monotonic() >= deadline:
            return 1
        time.sleep(0.001)
    kernel = launcher.kernel
    inject_signal_faults(control, kernel)
    try:
        if control.fault("owner-prctl"):
            raise Unsupported("enable child subreaper: injected failure")
        kernel.require()
        kernel.enable_subreaper()
        kernel.default_sigchld()
        stat = kernel.stat(os.getpid())
    except (Unsupported, OwnershipFailure) as error:
        write_all(report_w, f"fail {error}\n".encode()[:REPORT_LIMIT].rstrip(b"\n") + b"\n")
        return 1
    control.hold("owner-report")
    if control.fault("owner-close-accept"):
        os.close(accept_r)
        write_all(report_w, report_payload(control, f"ready {os.getpid()} {stat.start} {os.getppid()}"))
        return linger(launcher.record, deadline)
    payload = report_payload(control, f"ready {os.getpid()} {stat.start} {os.getppid()}")
    if payload is not None:
        write_all(report_w, payload)
    if control.fault("owner-partial"):
        return 0
    if not await_acceptance(triggers, accept_r, deadline, launcher.settings["tick"]):
        return 0
    os.close(report_w)
    control.hold("owner-accepted")
    triggers.runner_fd = launcher.triggers.runner_fd
    triggers.peer_fd = accept_r
    triggers.cancel_files = launcher.triggers.cancel_files
    triggers.budget_deadline = launcher.start + launcher.settings["budget"]
    owner = Owner(launcher.admission, launcher.settings, launcher.record, triggers, kernel, control)
    return owner.run(launcher.prepare)


def inject_signal_faults(control, kernel):
    """Fault hook: make pidfd signaling unavailable or denied for this owner's preflight."""
    for fault, code in (("owner-pidfd-enosys", errno.ENOSYS), ("owner-pidfd-eperm", errno.EPERM)):
        if control.fault(fault):
            kernel.pidfd_send_signal = refuse_with(code)


def refuse_with(code):
    def refuse(fd, sig):
        raise OSError(code, os.strerror(code))
    return refuse


def linger(record, deadline):
    """Fault hook: stay alive until the launcher has checked this owner, so EPIPE is what it sees next."""
    while record.read().get("state") == "admitted" and time.monotonic() < deadline:
        time.sleep(0.005)
    return 0


def await_acceptance(triggers, accept_r, deadline, tick):
    """True after the acceptance byte. EOF or the deadline means start nothing.

    Before acceptance this owner reacts only to acceptance EOF and its fixed
    handshake-plus-allowance cutoff. Runner death, cancellation and the budget
    are not watched here; the launcher declines on them and closes acceptance,
    or, if acceptance wins, the accepted owner sees them before any setup.
    Exiting here on those triggers could race a delivered acceptance.
    """
    os.set_blocking(accept_r, False)
    while time.monotonic() < deadline:
        triggers.wait([accept_r], min(tick, deadline - time.monotonic()))
        data = read_available(accept_r)
        if data is not None:
            return data != b""
    return False


class Owner:
    """The accepted owner: runs work, services triggers, then proves the join."""

    def __init__(self, admission, settings, record, triggers, kernel, control, clock=time.monotonic, pid=None):
        self.admission = admission
        self.settings = settings
        self.record = record
        self.triggers = triggers
        self.kernel = kernel
        self.control = control
        self.clock = clock
        self.tree = Tree(kernel, os.getpid() if pid is None else pid)
        self.reaper = Reaper(kernel, clock)
        self.name = admission.get("name", "owned command")
        self.root = None
        self.leader = None
        self.status = None
        self.trigger = None
        self.coop_signal = None
        self.coop_ticks = None
        self.coop_at = self.coop_end = self.kill_end = None
        self.signalled = set()
        self.failures = []
        self.joined = False
        if control.fault("owner-eperm"):
            kernel.send = refuse_signal
        if control.fault("owner-no-kill"):
            kernel.send = lambda fd, sig: True

    def run(self, prepare):
        try:
            self.start()
            argv, environment = prepare(self)
            self.interrupt_if_triggered()
            self.leader = self.spawn(argv, environment, session=True)
            self.status = self.wait_for(self.leader)
            self.schedule_settle(self.clock())
        except Interrupted:
            pass
        except SetupFailed as failed:
            self.status = failed.status
            self.schedule_settle(self.clock())
        except (OSError, OwnershipFailure, KeyError, ValueError) as error:
            self.fail(f"start {self.name}: {error}")
            self.status = 127 if isinstance(error, FileNotFoundError) else 1
            self.schedule_settle(self.clock())
        self.cleanup()
        return self.finish()

    def start(self):
        if self.admission.get("cwd"):
            os.chdir(self.admission["cwd"])
        self.root = Path(tempfile.mkdtemp(prefix="m-isolate-", dir=self.admission.get("scratch_parent")))
        if not self.record.advance("running", self.clock() + self.settings["allowance"], root=str(self.root)):
            raise OwnershipFailure("record did not accept the running state")
        self.interrupt_if_triggered()

    def spawn(self, argv, environment, stdout=None, session=False):
        pid = self.start_process(argv, environment, stdout, session)
        # The kernel reuses a PID only after its earlier holder was reaped, so any saved status is stale.
        self.reaper.claim(pid)
        return pid

    def start_process(self, argv, environment, stdout, session):
        path = argv[0] if "/" in argv[0] else shutil.which(argv[0], path=environment.get("PATH", os.defpath))
        if path is None:
            raise FileNotFoundError(errno.ENOENT, f"command not found: {argv[0]}")
        actions = [] if stdout is None else [(os.POSIX_SPAWN_DUP2, stdout, 1)]
        return os.posix_spawn(path, list(argv), environment, file_actions=actions, setsid=session,
                              setsigdef=(signal.SIGPIPE, signal.SIGXFSZ))

    def run_setup(self, argv, environment, output):
        """Run one setup command while cancellation, runner death and the budget stay live."""
        # A trigger that arrived while the previous setup command ran must stop this one starting.
        self.interrupt_if_triggered()
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_CLOEXEC, 0o600)
        try:
            pid = self.spawn(argv, environment, stdout=fd)
        finally:
            os.close(fd)
        return self.wait_for(pid)

    def wait_for(self, pid):
        # Only a wakeup on this child's exit: it is unreaped, so the handle is its own.
        try:
            handle = self.kernel.pidfd_open(pid)
        except OwnershipFailure:
            handle = None
        try:
            return self.await_status(pid, [] if handle is None else [handle])
        finally:
            if handle is not None:
                self.kernel.close(handle)

    def await_status(self, pid, wakeups):
        while True:
            drained = self.reaper.reap(self.clock() + self.settings["tick"])
            if pid in self.reaper.statuses:
                return self.reaper.statuses[pid]
            if drained == ECHILD:
                raise OwnershipFailure(f"status of child {pid} was lost")
            self.interrupt_if_triggered()
            if drained != MORE:
                self.triggers.wait(wakeups, self.settings["tick"])

    def interrupt_if_triggered(self):
        trigger = self.triggers.check()
        if trigger is not None:
            self.begin(trigger)
            raise Interrupted(trigger.reason)

    def schedule(self, coop_at, coop_end):
        # An existing schedule is never restarted or extended.
        if self.coop_end is None:
            self.coop_at, self.coop_end = coop_at, coop_end
            self.kill_end = coop_end + self.settings["kill_wait"]

    def schedule_settle(self, now):
        # The work has finished, so its budget no longer cancels anything.
        self.triggers.budget_deadline = None
        settled = now + self.settings["settle"]
        self.schedule(settled, settled + self.settings["leak_grace"])

    def begin(self, trigger):
        self.trigger = trigger
        if self.status is None:
            self.status = trigger.status
        self.schedule(trigger.time, trigger.time + self.settings["grace"])
        if self.coop_signal is None:
            self.coop_signal, self.coop_ticks = trigger.sig, self.kernel.now_ticks()
            self.coop_at = min(self.coop_at, trigger.time)

    def cleanup(self):
        """Work in tick-sized slices so triggers and the absolute deadlines are checked between them."""
        while True:
            drained = self.reap(self.slice(self.kill_end))
            if drained == ECHILD:
                self.joined = True
                return
            now = self.clock()
            self.observe_trigger()
            if now >= self.kill_end:
                self.fail("descendant join was not proved before the kill deadline")
                return
            self.step(now)
            if drained != MORE:
                self.triggers.wait([], self.settings["tick"])

    def slice(self, end):
        return min(self.clock() + self.settings["tick"], end)

    def reap(self, deadline):
        try:
            return self.reaper.reap(deadline)
        except OwnershipFailure as error:
            self.fail(str(error))
            return IDLE

    def observe_trigger(self):
        trigger = self.triggers.check()
        if self.trigger is None and trigger is not None:
            self.begin(trigger)

    def step(self, now):
        try:
            self.advance_stage(now)
        except OwnershipFailure as error:
            self.fail(str(error))

    def advance_stage(self, now):
        if now >= self.coop_end:
            self.tree.discover(self.slice(self.kill_end), self.clock)
            self.tree.deliver(signal.SIGKILL, "owned-command-kill", lambda pid, start: True,
                              self.slice(self.kill_end), self.clock)
        elif now >= self.coop_at:
            self.cooperate()

    def cooperate(self):
        leaked = self.coop_signal is None
        if leaked:
            self.coop_signal, self.coop_ticks = signal.SIGTERM, self.kernel.now_ticks()
        # Each tick continues or restarts the traversal: one traversal can omit a pre-trigger descendant.
        self.tree.discover(self.slice(self.coop_end), self.clock)
        if leaked:
            self.report_leaks()
        # Processes started after the trigger are cleanup helpers; only the kill stage reaches them.
        # Start ticks have 10 ms resolution, and a process in the trigger's own tick counts as later:
        # it gets no cooperative signal even if it existed just before the trigger.
        self.signalled.update(self.tree.deliver(
            self.coop_signal, "owned-command-cooperative",
            lambda pid, start: start < self.coop_ticks and (pid, start) not in self.signalled,
            self.slice(self.coop_end), self.clock))

    def report_leaks(self):
        deadline = self.slice(self.coop_end)
        for pid, known in self.tree.sample(LEAK_LINES):
            if self.clock() >= deadline:
                return
            if self.kernel.exited(known.fd) or self.clock() >= deadline:
                continue
            command = command_line(pid)
            if self.clock() >= deadline:
                return
            if not self.kernel.exited(known.fd) and self.clock() < deadline:
                print(f"LEAK: {self.name}: pid {pid} {command}", file=sys.stderr, flush=True)

    def fail(self, message):
        if message not in self.failures:
            self.failures.append(message)
            print(f"FAIL: owner {self.name}: {message}", file=sys.stderr, flush=True)

    def finish(self):
        if self.leader is not None and self.leader not in self.reaper.statuses:
            self.fail("leader status is missing")
        known = [] if self.joined else self.tree.sample(LEAK_LINES)
        try:
            if not self.tree.close(self.clock() + self.settings["allowance"], self.clock):
                self.fail("handle cleanup did not finish before its deadline")
        except (OSError, OwnershipFailure) as error:
            self.fail(f"close owned handles: {error}")
        if self.joined and not self.failures and self.root is not None:
            self.control.hold("owner-delete")
            try:
                shutil.rmtree(self.root)
            except OSError as error:
                self.fail(f"remove {self.root}: {error}")
        status = 1 if self.status is None else self.status
        deadline = self.clock() + self.settings["allowance"]
        joined = self.joined and not self.failures
        if joined:
            joined = self.record.advance("joined", deadline, status=status)
        else:
            self.record.advance("retained", deadline, status=status, pids=[pid for pid, _ in known],
                                pids_partial=True, root=str(self.root), reasons=self.failures)
        self.control.hold("owner-joined")
        return 0 if joined else 1


def refuse_signal(fd, sig):
    raise OwnershipFailure(f"signal {sig} through handle: injected permission failure")


def default_prepare(owner):
    if "isolation" in owner.admission:
        # Imported only here, so callers of the isolation entry's pure helpers never load the owner.
        from isolation import prepare_isolation
        return prepare_isolation(owner)
    # A controller that keeps secrets out of its records passes the environment to the launcher instead.
    base = os.environ if owner.admission.get("inherit_env") else owner.admission.get("env", {})
    environment = {**base, "MESH_OWNED_ROOT": str(owner.root)}
    for index, argv in enumerate(owner.admission.get("setup", ())):
        status = owner.run_setup(argv, environment, owner.root / f"setup-{index}.out")
        if status:
            raise SetupFailed(status)
    return owner.admission["command"], environment


def establish_runner():
    """Open the controller's own handle once, before any admission."""
    pid = os.getpid()
    fd = os.pidfd_open(pid)
    return {"pid": pid, "start": Kernel().stat(pid).start, "fd": fd}


def admit(directory, runner, **job):
    """Record the admission and expected runner before launch."""
    record = Record(directory)
    record.directory.mkdir(parents=True)
    record.write("admission.json", {**job, "runner": runner})
    record.write("state.json", {"state": "admitted"})
    return record


def require_diagnostic_log(options):
    """Keep the owner out of pipe backpressure; the controller owns a writable job log."""
    output = options.get("stderr")
    if output == subprocess.STDOUT:
        output = options.get("stdout")
    try:
        fd = output if isinstance(output, int) else output.fileno()
        mode = os.fstat(fd).st_mode
        access = fcntl.fcntl(fd, fcntl.F_GETFL) & os.O_ACCMODE
    except (OSError, AttributeError, ValueError) as error:
        raise ValueError("owned command diagnostics need a writable regular log") from error
    if not stat.S_ISREG(mode) or access == os.O_RDONLY:
        raise ValueError("owned command diagnostics need a writable regular log")


def launch(directory, runner, **options):
    require_diagnostic_log(options)
    return subprocess.Popen([sys.executable, "-B", str(Path(__file__).resolve()), str(directory)],
                            pass_fds=(runner["fd"],), **options)


def observe(process, directory):
    """The admission's outcome once its exits are proved; None while the launcher lives.

    A stopped launcher is live. Joined without the launcher's owner-exit
    observation is unknown, never permission to delete.
    """
    if process.poll() is None:
        return None
    state = Record(directory).read()
    if state.get("state") == "joined" and not (Path(directory) / "owner-exit").exists():
        return {**state, "state": "unknown", "reasons": ["owner exit was not observed"]}
    if state.get("state") not in FINAL:
        return {**state, "state": "unknown"}
    return state


class Job:
    """One admission as its controller sees it."""

    def __init__(self, name, directory, settings, admitted):
        self.name = name
        self.directory = Path(directory)
        self.settings = settings
        self.process = None
        self.exit_handle = None
        self.outcome = None
        self.final_seen = False
        # Startup gets the handshake allowance; observing the launcher's exit gets one more allowance.
        self.deadline = admitted + settings["handshake"] + work_allowance(settings) + settings["allowance"]

    def state(self):
        return None if self.outcome is None else self.outcome["state"]

    def proved(self):
        return self.state() in ("joined", "declined")

    def settle(self, outcome):
        self.outcome = outcome
        if self.exit_handle is not None:
            os.close(self.exit_handle)
            self.exit_handle = None


class Controller:
    """Admit, launch and observe owned commands. It never signals them.

    The first termination signal is only recorded: a handler that raised could
    unwind Popen after the child exists. Cancellation reaches owners through
    the shared cancel file, and through this process's pidfd if it dies.
    Every wait has an absolute deadline; a job whose exits are not proved by
    then is unknown, and its state must be retained.
    """

    def __init__(self, root, triggers, overrides=None, control=None, clock=time.monotonic):
        self.root = Path(root)
        self.triggers = triggers
        self.overrides = dict(overrides or {})
        self.control = Control(control)
        self.clock = clock
        self.runner = establish_runner()
        self.cancel_file = self.root / "cancel"
        self.cancelled = None
        self.failures = []
        self.jobs = []

    def start(self, name, popen, **job):
        """Admit, then launch. An admission that never launches is explicitly declined."""
        directory = self.root / "jobs" / name
        job = {**job, **self.overrides, "name": name, "cancel": [str(self.cancel_file)]}
        admit(directory, self.runner, **job)
        settings = {key: float(job.get(key, value)) for key, value in DEFAULTS.items()}
        tracked = Job(name, directory, settings, self.clock())
        self.jobs.append(tracked)
        self.control.hold(f"before-launch-{name}")
        if self.check() is not None:
            return self.decline(tracked, "cancelled before launch", self.cancelled.status)
        try:
            tracked.process = launch(directory, self.runner, **popen)
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            return self.decline(tracked, f"launch failed: {error}", 1)
        try:
            # Only a wakeup: the launcher is this process's unreaped child, so the handle is its own.
            tracked.exit_handle = os.pidfd_open(tracked.process.pid)
        except OSError:
            pass
        self.control.hold(f"after-launch-{name}")
        self.check()
        return tracked

    def decline(self, job, reason, status):
        if Record(job.directory).advance("declined", self.clock() + job.settings["allowance"], reason=reason, status=status):
            job.settle({**Record(job.directory).read(), "returncode": status})
        else:
            job.settle({"state": "unknown", "reasons": [f"could not record that the job never launched: {reason}"]})
        return job

    def check(self):
        trigger = self.triggers.check()
        if trigger is not None and self.cancelled is None:
            self.cancel(trigger)
        return self.cancelled

    def cancel(self, trigger):
        self.cancelled = trigger
        try:
            request_cancel(self.cancel_file, trigger.sig)
        except OSError as error:
            self.failures.append(f"publish cancellation: {error}")
        for job in self.jobs:
            # Cancellation only shortens a deadline that is already running.
            job.deadline = min(job.deadline, trigger.time + cancel_allowance(job.settings) + job.settings["allowance"])

    def poll(self, job):
        """True once the job's outcome is known: proved, or unknown at its deadline."""
        if job.outcome is not None:
            return True
        state = observe(job.process, job.directory)
        if state is not None:
            job.settle({**state, "returncode": job.process.returncode})
            return True
        now = self.clock()
        if not job.final_seen and Record(job.directory).read().get("state") in FINAL:
            # A final record comes just before the owner and launcher exit; a pause after it is bounded.
            job.final_seen = True
            job.deadline = min(job.deadline, now + 2 * job.settings["allowance"])
        if now < job.deadline:
            return False
        try:
            request_cancel(job.directory / "cancel", signal.SIGTERM)
        except OSError as error:
            self.failures.append(f"cancel {job.name}: {error}")
        job.settle({"state": "unknown", "returncode": None,
                    "reasons": ["launcher exit was not observed before the controller's deadline"]})
        return True

    def wait(self, jobs, until_cancelled=False, each_tick=None):
        """Poll until every job's outcome is known, or the run is cancelled when until_cancelled."""
        while True:
            pending = [job for job in jobs if not self.poll(job)]
            if each_tick is not None:
                each_tick()
            if not pending:
                return True
            if self.check() is not None and until_cancelled:
                return False
            handles = [job.exit_handle for job in pending if job.exit_handle is not None]
            self.triggers.wait(handles, min(job.settings["tick"] for job in pending))

    def proved(self):
        return not self.failures and all(job.proved() for job in self.jobs)


def main(arguments=None, prepare=default_prepare):
    arguments = sys.argv[1:] if arguments is None else arguments
    if len(arguments) != 1:
        print("usage: owned_command.py RECORD_DIRECTORY", file=sys.stderr)
        return 2
    try:
        launcher = Launcher(arguments[0], prepare)
    except (OSError, ValueError, KeyError) as error:
        print(f"FAIL: owned command: {error}", file=sys.stderr)
        return 1
    return launcher.run()


if __name__ == "__main__":
    # A prepare hook imports this module by name; it must get these classes, not a second copy.
    sys.modules.setdefault("owned_command", sys.modules[__name__])
    sys.exit(main())
