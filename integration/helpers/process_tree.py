"""Linux process ownership for the owned-command supervisor.

The owner is a child subreaper, so every descendant either keeps a live parent
chain to it or is reparented to it. Only waitpid reporting ECHILD, after the
owner stops starting work, proves that the tree has ended. Discovery exists to
deliver signals safely and never proves completion.
"""
import ctypes
import errno
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


class Unsupported(RuntimeError):
    """A required kernel or Python interface is missing; nothing may start."""


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
            os.close(os.pidfd_open(os.getpid()))
        except (OSError, AttributeError) as error:
            raise Unsupported(f"process ownership interfaces are unavailable: {error}") from error
        if self.stat(os.getpid()) is None:
            raise Unsupported("process ownership needs readable /proc stat")

    def enable_subreaper(self):
        if self.libc.prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
            raise Unsupported(f"enable child subreaper: {os.strerror(ctypes.get_errno())}")
        value = ctypes.c_int(0)
        if self.libc.prctl(PR_GET_CHILD_SUBREAPER, ctypes.byref(value), 0, 0, 0) != 0 or value.value != 1:
            raise Unsupported("child subreaper is not in effect")

    def default_sigchld(self):
        # Ignored SIGCHLD makes the kernel discard statuses the owner must report.
        signal.signal(signal.SIGCHLD, signal.SIG_DFL)
        with open(f"/proc/{os.getpid()}/status") as source:
            ignored = next(line.split()[1] for line in source if line.startswith("SigIgn:"))
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
        except OSError as error:
            raise OwnershipFailure(f"read stat for pid {pid}: {error}") from error
        try:
            return Stat(fields[0], int(fields[1]), int(fields[19]))
        except (IndexError, ValueError) as error:
            raise OwnershipFailure(f"parse stat for pid {pid}: {error}") from error

    def children(self, pid):
        found = []
        for path in glob.glob(f"/proc/{pid}/task/*/children"):
            try:
                found += [int(child) for child in Path(path).read_text().split()]
            except (FileNotFoundError, ProcessLookupError):
                continue
            except (OSError, ValueError) as error:
                raise OwnershipFailure(f"read children of pid {pid}: {error}") from error
        return found

    def send(self, fd, sig):
        try:
            signal.pidfd_send_signal(fd, sig)
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

    def parent_live(self, parent):
        if parent == self.owner:
            return True
        known = self.proved.get(parent)
        return known is not None and not self.kernel.exited(known.fd)

    def forget(self, pid):
        self.kernel.close(self.proved.pop(pid).fd)

    def prune(self):
        for pid in [pid for pid, known in self.proved.items() if self.kernel.exited(known.fd)]:
            self.forget(pid)

    def edge(self, pid, parent):
        known = self.proved.get(pid)
        if known is not None and not self.kernel.exited(known.fd):
            return True
        if known is not None:
            self.forget(pid)
        fd = self.kernel.pidfd_open(pid)
        if fd is None:
            return False
        stat = self.kernel.stat(pid)
        if stat is None or stat.ppid != parent or self.kernel.exited(fd) or not self.parent_live(parent):
            self.kernel.close(fd)
            return False
        self.proved[pid] = Proved(fd, stat.start)
        return True

    def discover(self, deadline, clock):
        """One bounded outward traversal. Omissions are found by later rounds."""
        self.prune()
        queue = [self.owner, *self.proved]
        visited = set()
        while queue and clock() < deadline:
            parent = queue.pop()
            if parent in visited or not self.parent_live(parent):
                continue
            visited.add(parent)
            queue += [pid for pid in self.kernel.children(parent) if self.edge(pid, parent)]

    def deliver(self, sig, reason, eligible):
        """Signal proved, live descendants that pass eligible(pid, start)."""
        delivered = []
        for pid, known in list(self.proved.items()):
            if self.kernel.exited(known.fd) or not eligible(pid, known.start):
                continue
            record_signal(pid, sig, reason, {"kind": "pidfd_owned_descendant", "owner_pid": self.owner},
                          {"status": "observed", "start_ticks": known.start})
            if self.kernel.send(known.fd, sig):
                delivered.append((pid, known.start))
        return delivered

    def live(self):
        return sorted(pid for pid, known in self.proved.items() if not self.kernel.exited(known.fd))

    def close(self):
        for pid in list(self.proved):
            self.forget(pid)


class Reaper:
    """The owner's only reaper: actual statuses for its children and adopted orphans."""

    def __init__(self, kernel):
        self.kernel = kernel
        self.statuses = {}

    def reap(self):
        """Collect ready statuses. True only when waitpid reports ECHILD."""
        while True:
            try:
                pid, status = self.kernel.waitpid()
            except ChildProcessError:
                return True
            except OSError as error:
                if error.errno == errno.EINTR:
                    continue
                raise OwnershipFailure(f"reap children: {error}") from error
            if pid == 0:
                return False
            code = os.waitstatus_to_exitcode(status)
            self.statuses[pid] = code if code >= 0 else 128 - code
