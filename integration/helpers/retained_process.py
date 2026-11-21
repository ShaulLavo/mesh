"""Process references for the workers and shells started by update proofs."""

import errno
import os
import select
import subprocess
import sys
from typing import NamedTuple

from process_tree import Kernel


class ProcessInfo(NamedTuple):
    uid: int
    parent: int
    birth: tuple[str, ...]
    command: str


def process_info(pid):
    result = subprocess.run(["/bin/ps", "-ww", "-p", str(pid), "-o", "uid=,ppid=,lstart=,stat=,command="],
                            env=os.environ | {"LC_ALL": "C"}, capture_output=True, text=True,
                            timeout=5, check=False)
    fields = result.stdout.split(maxsplit=8)
    if result.returncode or len(fields) != 9 or fields[7].startswith("Z"):
        return None
    return ProcessInfo(int(fields[0]), int(fields[1]), tuple(fields[2:7]), fields[8].strip())


class LinuxReference:
    def __init__(self, pid):
        self.kernel = Kernel()
        self.fd = self.kernel.pidfd_open(pid)
        if self.fd is None:
            raise ProcessLookupError(errno.ESRCH, f"process {pid} exited before capture")

    def exited(self, timeout=0):
        poller = select.poll()
        poller.register(self.fd, select.POLLIN)
        ready = poller.poll(int(timeout * 1000 + 0.5))
        if ready and not ready[0][1] & (select.POLLIN | select.POLLHUP):
            raise RuntimeError("could not observe exit through retained pidfd")
        return bool(ready)

    def close(self):
        self.kernel.close(self.fd)


class DarwinReference:
    def __init__(self, pid):
        self.pid = pid
        self.queue = select.kqueue()
        self.saw_exit = False
        try:
            event = select.kevent(pid, filter=select.KQ_FILTER_PROC, flags=select.KQ_EV_ADD,
                                  fflags=select.KQ_NOTE_EXIT)
            self.queue.control([event], 0)
        except BaseException:
            self.queue.close()
            raise

    def exited(self, timeout=0):
        if self.saw_exit:
            return True
        events = self.queue.control(None, 1, timeout)
        self.saw_exit = any(event.fflags & select.KQ_NOTE_EXIT for event in events)
        if events and not self.saw_exit:
            raise RuntimeError(f"could not observe exit of retained process {self.pid}")
        return self.saw_exit

    def close(self):
        self.queue.close()


class RetainedProcess:
    def __init__(self, pid, *, parent=None, matches=None):
        if type(pid) is not int or pid <= 1 or pid == os.getpid():
            raise ValueError(f"invalid retained process PID {pid!r}")
        self.pid = pid
        self.reference = DarwinReference(pid) if sys.platform == "darwin" else LinuxReference(pid)
        try:
            info = process_info(pid)
            if info is None or info.uid != os.getuid():
                raise RuntimeError(f"could not verify ownership of retained process {pid}")
            if matches is not None and not matches(info.command):
                raise RuntimeError(f"process {pid} does not match the fixture worker command")
            if parent is not None and (info.parent != parent.pid or not parent.is_current()):
                raise RuntimeError(f"retained shell {pid} is not a child of the fixture worker")
            self.birth = info.birth
            if not self.is_current():
                raise RuntimeError(f"retained process {pid} exited during capture")
        except BaseException:
            self.reference.close()
            raise

    def is_current(self):
        if self.reference is None or self.reference.exited():
            return False
        info = process_info(self.pid)
        # Check the reference after ps so its metadata belongs to this lifetime.
        if self.reference.exited():
            return False
        if info is None or (info.uid, info.birth) != (os.getuid(), self.birth):
            raise RuntimeError(f"could not verify ownership of retained process {self.pid}")
        return True

    def close(self):
        if self.reference is not None:
            self.reference.close()
            self.reference = None
