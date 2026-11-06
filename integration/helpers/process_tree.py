"""Linux process ownership for the owned-command supervisor.

The owner is a child subreaper, so every descendant either keeps a live parent
chain to it or is reparented to it. Only waitpid reporting ECHILD, after the
owner stops starting work, proves that the tree has ended. Discovery exists to
deliver signals safely and never proves completion.

Traversal, delivery and reaping check absolute deadlines between bounded units
and retain their progress across ticks. An operation already in progress can
finish after a cutoff; these loops do not promise interruptible kernel calls.
"""
from collections import deque
import ctypes
import glob
import os
from itertools import islice
from pathlib import Path
import select
import signal
import sys
import time
from typing import NamedTuple

from process_diagnostics import record_signal

PR_SET_CHILD_SUBREAPER = 36
PR_GET_CHILD_SUBREAPER = 37

# Reaper outcomes. Only ECHILD proves that no child is left.
ECHILD = "echild"
IDLE = "idle"
MORE = "more"


class Unsupported(RuntimeError):
    """A required kernel or Python interface is missing or denied; nothing may start."""


class OwnershipFailure(RuntimeError):
    """Ownership could not be read or enforced; state must be retained."""


class Stat(NamedTuple):
    state: str
    ppid: int
    start: int


class Proved(NamedTuple):
    fd: int
    start: int


class Kernel:
    """The real interfaces. Tests replace this object with a scripted kernel."""

    def __init__(self):
        self.libc = None
        self.clock_ticks = 0
        self.pidfd_send_signal = getattr(signal, "pidfd_send_signal", None)

    def require(self):
        if sys.platform != "linux":
            raise Unsupported("process ownership needs Linux child subreaping")
        missing = [name for name in ("pidfd_open", "posix_spawn", "waitstatus_to_exitcode") if not hasattr(os, name)]
        missing += [name for name in ("pidfd_send_signal", "set_wakeup_fd") if not hasattr(signal, name)]
        if missing:
            raise Unsupported("process ownership needs " + ", ".join(missing))
        if not glob.glob(f"/proc/{os.getpid()}/task/*/children"):
            raise Unsupported("process ownership needs /proc task children lists")
        try:
            self.libc = ctypes.CDLL(None, use_errno=True)
            self.clock_ticks = os.sysconf("SC_CLK_TCK")
        except (OSError, AttributeError, ValueError) as error:
            raise Unsupported(f"process ownership interfaces are unavailable: {error}") from error
        self.probe_self()

    def probe_self(self):
        """Use each interface the owner depends on once, against this process.

        Signal 0 proves pidfd signaling works here; it does not promise permission
        for every later descendant, so runtime sends still fail closed.
        """
        pid = os.getpid()
        try:
            fd = os.pidfd_open(pid)
        except OSError as error:
            raise Unsupported(f"process ownership interfaces are unavailable: {error}") from error
        try:
            self.pidfd_send_signal(fd, 0)
            stat = self.stat(pid)
            self.children(pid)
            Path(f"/proc/{pid}/status").read_text()
            exited = self.exited(fd)
        except (OSError, OwnershipFailure) as error:
            raise Unsupported(f"process ownership interfaces are unavailable: {error}") from error
        finally:
            os.close(fd)
        if stat is None or exited:
            raise Unsupported("process ownership needs readable /proc stat for this process")

    def enable_subreaper(self):
        if self.libc.prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
            raise Unsupported(f"enable child subreaper: {os.strerror(ctypes.get_errno())}")
        value = ctypes.c_int(0)
        if self.libc.prctl(PR_GET_CHILD_SUBREAPER, ctypes.byref(value), 0, 0, 0) != 0 or value.value != 1:
            raise Unsupported("child subreaper is not in effect")

    def default_sigchld(self):
        # Ignored SIGCHLD makes the kernel discard statuses the owner must report.
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        ignored = next(line.split()[1] for line in Path(f"/proc/{os.getpid()}/status").read_text().splitlines()
                       if line.startswith("SigIgn:"))
        if int(ignored, 16) & 1 << (signal.SIGCHLD - 1):
            raise Unsupported("SIGCHLD is still ignored")

    def now_ticks(self):
        # /proc start times count clock ticks of CLOCK_BOOTTIME.
        return int(time.clock_gettime(time.CLOCK_BOOTTIME) * self.clock_ticks)

    def pidfd_open(self, pid):
        try:
            return os.pidfd_open(pid)
        except ProcessLookupError:
            return None
        except OSError as error:
            raise OwnershipFailure(f"open handle for pid {pid}: {error}") from error

    def exited(self, fd):
        poller = select.poll()
        poller.register(fd, select.POLLIN)
        return bool(poller.poll(0))

    def stat(self, pid):
        try:
            with open(f"/proc/{pid}/stat") as source:
                fields = source.read(4096).rsplit(")", 1)[1].split()
        except (FileNotFoundError, ProcessLookupError):
            return None
        except (OSError, IndexError) as error:
            raise OwnershipFailure(f"read stat for pid {pid}: {error}") from error
        try:
            return Stat(fields[0], int(fields[1]), int(fields[19]))
        except (IndexError, ValueError) as error:
            raise OwnershipFailure(f"parse stat for pid {pid}: {error}") from error

    def children(self, pid):
        return [child for child in self.child_steps(pid) if child is not None]

    def child_steps(self, pid):
        """Yield between task reads and candidate tokens, keeping task and file descriptors owned."""
        try:
            tasks = os.scandir(f"/proc/{pid}/task")
        except (FileNotFoundError, ProcessLookupError):
            return
        except OSError as error:
            raise OwnershipFailure(f"list tasks of pid {pid}: {error}") from error
        with tasks as entries:
            for task in entries:
                yield None
                yield from self.task_child_steps(pid, f"{task.path}/children")

    def task_child_steps(self, pid, path):
        try:
            with open(path) as source:
                yield from child_tokens(source)
        except (FileNotFoundError, ProcessLookupError):
            return
        except (OSError, ValueError) as error:
            raise OwnershipFailure(f"read children of pid {pid}: {error}") from error

    def send(self, fd, sig):
        try:
            self.pidfd_send_signal(fd, sig)
        except ProcessLookupError:
            return False
        except OSError as error:
            raise OwnershipFailure(f"signal {sig} through handle: {error}") from error
        return True

    def waitpid(self):
        return os.waitpid(-1, os.WNOHANG)

    def close(self, fd):
        os.close(fd)


def child_chunk(source, tail):
    chunk = source.read(4096)
    if not chunk:
        return [tail] if tail else [], "", True
    text = tail + chunk
    parts = text.split()
    if text[-1].isspace():
        return parts, "", False
    tail = parts.pop() if parts else text
    if len(tail) > 64:
        raise ValueError("child PID token exceeds its metadata bound")
    return parts, tail, False


def child_tokens(source):
    tail = ""
    while True:
        parts, tail, done = child_chunk(source, tail)
        yield None
        for token in parts:
            yield int(token)
        if done:
            return


class Tree:
    """Descendants proved through handle-validated parent edges from the owner.

    A candidate is accepted only when its own live handle binds the stat read
    and the already-proved parent's handle is still live after that read. No
    chain of bare PID numbers ever authorizes a signal.
    """

    def __init__(self, kernel, owner):
        self.kernel = kernel
        self.owner = owner
        self.proved = {}
        self.queue = deque()
        self.visited = set()
        self.candidates = None
        self.delivery = deque()
        self.pruning = deque()
        self.seeds = deque()
        self.seed_remaining = 0
        self.prune_turn = True

    def parent_live(self, parent):
        if parent == self.owner:
            return True
        known = self.proved.get(parent)
        return known is not None and not self.kernel.exited(known.fd)

    def forget(self, pid):
        self.kernel.close(self.proved.pop(pid).fd)

    def edge(self, pid, parent):
        known = self.proved.get(pid)
        if known is not None and not self.kernel.exited(known.fd):
            return True
        if known is not None:
            self.forget(pid)
        fd = self.kernel.pidfd_open(pid)
        if fd is None:
            return False
        try:
            accepted = self.validate(pid, parent, fd)
        finally:
            if pid not in self.proved:
                self.kernel.close(fd)
        return accepted

    def validate(self, pid, parent, fd):
        stat = self.kernel.stat(pid)
        # Polling after the read binds it to this handle's process.
        if self.kernel.exited(fd):
            return False
        if stat is None:
            raise OwnershipFailure(f"pid {pid} is live but its stat is unreadable")
        if stat.ppid != parent or not self.parent_live(parent):
            return False
        known = Proved(fd, stat.start)
        self.proved[pid] = known
        self.delivery.append((pid, known))
        self.pruning.append((pid, known))
        self.seeds.append((pid, known))
        return True

    def discover(self, deadline, clock):
        """Resume one outward traversal, including each task read, until its cutoff."""
        if not self.queue and self.candidates is None and not self.seed_remaining:
            self.queue.append(self.owner)
            self.seed_remaining = len(self.seeds)
            self.visited = set()
        while clock() < deadline:
            if self.prune_turn:
                self.prune_turn = False
                self.prune_one()
                continue
            self.prune_turn = True
            if not self.advance():
                return True
        return False

    def prune_one(self):
        if not self.pruning:
            return
        pid, known = self.pruning.popleft()
        if self.proved.get(pid) is not known:
            return
        if self.kernel.exited(known.fd):
            self.forget(pid)
            return
        self.pruning.append((pid, known))

    def advance(self):
        if self.candidates is not None:
            return self.advance_candidate()
        if not self.queue:
            return self.advance_seed()
        parent = self.queue.popleft()
        if parent in self.visited or not self.parent_live(parent):
            return True
        self.visited.add(parent)
        self.candidates = parent, iter(self.kernel.child_steps(parent))
        return True

    def advance_seed(self):
        if not self.seed_remaining:
            return False
        self.seed_remaining -= 1
        pid, known = self.seeds.popleft()
        if self.proved.get(pid) is known:
            self.seeds.append((pid, known))
            self.queue.append(pid)
        return True

    def advance_candidate(self):
        parent, children = self.candidates
        if not self.parent_live(parent):
            self.close_candidates()
            return True
        try:
            pid = next(children)
        except StopIteration:
            self.candidates = None
            return True
        if pid is not None and self.edge(pid, parent):
            self.queue.append(pid)
        return True

    def close_candidates(self):
        if self.candidates is None:
            return
        _, children = self.candidates
        self.candidates = None
        children.close()

    def deliver(self, sig, reason, eligible, deadline, clock):
        """Resume a fair handle sweep; a surviving prefix cannot monopolize later slices."""
        delivered = []
        remaining = len(self.delivery)
        while remaining and clock() < deadline:
            remaining -= 1
            pid, known = self.delivery.popleft()
            if self.proved.get(pid) is not known:
                continue
            if self.kernel.exited(known.fd):
                self.forget(pid)
                continue
            self.delivery.append((pid, known))
            if self.send_if_eligible(pid, known, sig, reason, eligible):
                delivered.append((pid, known.start))
        return delivered

    def send_if_eligible(self, pid, known, sig, reason, eligible):
        if not eligible(pid, known.start):
            return False
        record_signal(pid, sig, reason, {"kind": "pidfd_owned_descendant", "owner_pid": self.owner},
                      {"status": "observed", "start_ticks": known.start})
        return self.kernel.send(known.fd, sig)

    def sample(self, limit):
        """A bounded sample of proved identities, not a complete or live process census."""
        return list(islice(self.proved.items(), limit))

    def live(self):
        # Test/inspection helper. The owner's deadline paths use bounded samples instead.
        return sorted(pid for pid, known in self.proved.items() if not self.kernel.exited(known.fd))

    def close(self, deadline=float("inf"), clock=time.monotonic):
        self.close_candidates()
        while self.proved and clock() < deadline:
            _, known = self.proved.popitem()
            self.kernel.close(known.fd)
        return not self.proved


class Reaper:
    """The owner's only reaper: actual statuses for its children and adopted orphans."""

    def __init__(self, kernel, clock=time.monotonic):
        self.kernel = kernel
        self.clock = clock
        self.statuses = {}

    def claim(self, pid):
        """A new direct child now owns pid; a status saved for an earlier incarnation is void."""
        self.statuses.pop(pid, None)

    def reap(self, deadline):
        """Collect ready statuses until deadline.

        ECHILD means no child is left. IDLE means waitpid reported children
        but none ready. MORE means statuses may still be ready at the cutoff.
        """
        while True:
            outcome = self.collect()
            if outcome != MORE:
                return outcome
            if self.clock() >= deadline:
                return MORE

    def collect(self):
        try:
            pid, status = self.kernel.waitpid()
        except ChildProcessError:
            return ECHILD
        except OSError as error:
            raise OwnershipFailure(f"reap children: {error}") from error
        if pid == 0:
            return IDLE
        code = os.waitstatus_to_exitcode(status)
        self.statuses[pid] = code if code >= 0 else 128 - code
        return MORE
