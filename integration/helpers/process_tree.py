"""Linux process ownership for the owned-command supervisor.

The owner is a child subreaper, so every descendant either keeps a live parent
chain to it or is reparented to it. Only waitpid reporting ECHILD, after the
owner stops starting work, proves that the tree has ended. Discovery exists to
deliver signals safely and never proves completion.

Traversal, delivery and reaping each take an absolute deadline and stop at it
with their progress kept, so a broad or churning tree cannot hold the owner
past its cooperation, kill or join cutoffs.
"""
import ctypes
import glob
import os
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
        found = []
        for path in glob.glob(f"/proc/{pid}/task/*/children"):
            found += self.task_children(pid, path)
        return found

    def task_children(self, pid, path):
        try:
            return [int(child) for child in Path(path).read_text().split()]
        except (FileNotFoundError, ProcessLookupError):
            return []
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
        self.queue = []
        self.visited = set()
        self.candidates = []

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
        self.proved[pid] = Proved(fd, stat.start)
        return True

    def discover(self, deadline, clock):
        """Advance the current traversal until deadline. True when a full round ended.

        The queue and pending candidates survive a cutoff, so the next call
        continues where this one stopped. Omissions are found by later rounds.
        """
        if not self.queue and not self.candidates:
            self.queue = [self.owner, *self.proved]
            self.visited = set()
        while clock() < deadline:
            if not self.advance():
                return True
        return False

    def advance(self):
        """One unit of traversal work. False when the round has nothing left."""
        if self.candidates:
            pid, parent = self.candidates.pop()
            if self.edge(pid, parent):
                self.queue.append(pid)
            return True
        if not self.queue:
            return False
        parent = self.queue.pop()
        if parent in self.visited:
            return True
        if not self.parent_live(parent):
            # Pruning an exited handle is one unit of the same bounded work.
            if parent in self.proved:
                self.forget(parent)
            return True
        self.visited.add(parent)
        self.candidates = [(pid, parent) for pid in self.kernel.children(parent)]
        return True

    def deliver(self, sig, reason, eligible, deadline, clock):
        """Signal proved, live descendants that pass eligible(pid, start), until deadline."""
        delivered = []
        for pid, known in list(self.proved.items()):
            if clock() >= deadline:
                break
            if self.kernel.exited(known.fd):
                self.forget(pid)
                continue
            if not eligible(pid, known.start):
                continue
            record_signal(pid, sig, reason, {"kind": "pidfd_owned_descendant", "owner_pid": self.owner},
                          {"status": "observed", "start_ticks": known.start})
            if self.kernel.send(known.fd, sig):
                delivered.append((pid, known.start))
        return delivered

    def live(self):
        # Nonblocking polls over a finite set; no metadata reads.
        return sorted(pid for pid, known in self.proved.items() if not self.kernel.exited(known.fd))

    def close(self):
        for pid in list(self.proved):
            self.forget(pid)


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
