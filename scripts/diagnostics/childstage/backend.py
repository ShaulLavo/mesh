"""Kernel observation is enabled only on a disposable GitHub-hosted runner."""
import json
import ctypes
import collections
import itertools
import os
from pathlib import Path
import selectors
import select
import shutil
import signal
import subprocess
import sys
import time


HERE = Path(__file__).resolve().parent
MAX_BYTES = 16 << 20


def require_disposable():
    if os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or os.geteuid() == 0:
        raise RuntimeError("requires an explicitly authorized disposable GitHub-hosted normal-user job")


def capabilities(output):
    require_disposable()
    binary = shutil.which("bpftrace")
    result = {"kernel": os.uname().release, "uid": os.geteuid(), "gid": os.getegid(), "groups": os.getgroups(),
              "bpftracePresent": bool(binary), "bpftracePath": binary, "btfPresent": Path("/sys/kernel/btf/vmlinux").is_file(),
              "sourceSha256": __import__("hashlib").sha256((HERE / "returns.bt").read_bytes()).hexdigest(),
              "coverageProved": False, "compilePassed": False}
    if binary and result["btfPresent"]:
        result["version"] = subprocess.check_output([binary, "--version"], stderr=subprocess.STDOUT, timeout=5).decode().strip()
        with (output / "bpf-compile.log").open("xb") as stream:
            result["compileStatus"] = subprocess.run(["sudo", "-n", binary, "-d", str(HERE / "returns.bt"), str(os.getpid())],
                                                      stdout=stream, stderr=subprocess.STDOUT, timeout=30).returncode
        result["compilePassed"] = result["compileStatus"] == 0
    (output / "capabilities.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def start_ticks(pid):
    try:
        return int(Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[19])
    except (OSError, ValueError, IndexError):
        return None


class KernelRun:
    def __init__(self, output, name):
        self.output, self.name = output, name
        self.tasks, self.seen, self.rows = {}, set(), []
        self.hook = self.target = None
        self.bytes = 0

    def own(self, pid, boot_ns=None):
        ticks = start_ticks(pid)
        if ticks is None:
            return
        if boot_ns is not None and ticks != boot_ns * os.sysconf("SC_CLK_TCK") // 1_000_000_000:
            raise RuntimeError("kernel event and live task start identity disagree")
        if pid in self.tasks:
            if pidfd_alive(self.tasks[pid]):
                return
            os.close(self.tasks.pop(pid))
        descriptor = os.pidfd_open(pid)
        if start_ticks(pid) != ticks:
            os.close(descriptor)
            raise RuntimeError("task identity changed while opening pidfd")
        self.tasks[pid] = descriptor

    def record(self, line, stream):
        self.bytes += len(line)
        if self.bytes > MAX_BYTES or len(self.rows) >= 32768:
            raise RuntimeError("kernel observer output bound reached")
        row = json.loads(line)
        if row["event"] == "coverage-error":
            raise RuntimeError("kernel observer reports incomplete coverage")
        if row["event"] == "return" and row["number"] != row["entryNumber"]:
            raise RuntimeError("kernel observer lost syscall entry identity")
        self.remember(row)
        self.rows.append(row)
        stream.write(json.dumps(row) + "\n")
        stream.flush()
        return row

    def remember(self, row):
        if row["event"] == "created":
            self.seen.add(row["tid"])
            if row["tid"] == row["tgid"]:
                self.own(row["tid"], row["startBootNs"])
        if "device" in row and "bytes" in row:
            raw = row["device"]
            row["image"] = f"{os.makedev(raw >> 20, raw & 0xFFFFF)}:{row['inode']}:{row['bytes']}"
        if row["event"] == "return":
            row["call"] = {"number": row["number"], "args": row["args"], "enteredNs": row["enteredNs"],
                           "clone3Flags": row["clone3Flags"]}
            row["context"] = {"kernel": {key: row[key] for key in ("uid", "euid", "gid", "egid", "ppid", "pgid", "sid",
                                                                      "seccomp", "cgroupId", "startBootNs")}, "image": row["image"]}

    def stop_hook(self):
        if self.hook is None or self.hook.poll() is not None:
            return
        if not self.hook.stdin.closed:
            self.hook.stdin.close()

    def cleanup(self):
        self.discover()
        for descriptor in self.tasks.values():
            kill_owned(descriptor)
        if self.target is not None:
            self.target.wait(timeout=5)
        self.stop_hook()
        if self.hook is not None:
            wait_hook(self)
        self.settle()
        remaining = [pid for pid, fd in self.tasks.items() if pidfd_alive(fd)]
        for descriptor in self.tasks.values():
            os.close(descriptor)
        return remaining

    def discover(self):
        alive = [pid for pid, fd in self.tasks.items() if pidfd_alive(fd)]
        queue, seen = collections.deque([os.getpid()] + alive), set()
        while queue:
            parent = queue.popleft()
            if parent in seen:
                continue
            seen.add(parent)
            if len(seen) > 8192:
                raise RuntimeError("owned descendant cleanup bound exceeded")
            queue.extend(self.descendants(parent))

    def descendants(self, parent):
        result = []
        for child in child_pids(parent):
            if self.hook is not None and child == self.hook.pid:
                continue
            if task_parent(child) != parent:
                continue
            self.own(child)
            result.append(child)
        return result

    def settle(self):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            self.discover()
            for descriptor in self.tasks.values():
                kill_owned(descriptor)
            reap_adopted()
            if not any(pidfd_alive(fd) for fd in self.tasks.values()):
                return
            time.sleep(0.01)

    def run(self, argv, seconds, after_exit=None):
        require_disposable()
        previous = subreaper()
        read, write = os.pipe()
        ready_read, ready_write = os.pipe()
        try:
            return self.observe(argv, seconds, (read, write, ready_read, ready_write), after_exit)
        finally:
            os.close(read)
            os.close(write)
            os.close(ready_read)
            os.close(ready_write)
            subreaper(previous)

    def observe(self, argv, seconds, pipes, after_exit):
        read, write, ready_read, ready_write = pipes
        path = self.output / f"{self.name}.jsonl"
        failure = ""
        try:
            with (self.output / f"{self.name}.log").open("xb") as child_log, (self.output / f"{self.name}.kernel.log").open("xb") as hook_log, path.open("x") as trace:
                self.target = subprocess.Popen([sys.executable, str(HERE / "barrier.py"), str(read), str(ready_write)] + argv,
                                               pass_fds=(read, ready_write), stdout=child_log, stderr=subprocess.STDOUT)
                self.own(self.target.pid)
                wait_barrier(ready_read)
                self.hook = subprocess.Popen(["sudo", "-n", sys.executable, str(HERE / "hook-supervisor.py"), str(self.target.pid), shutil.which("bpftrace")],
                                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=hook_log)
                return self.consume(trace, write, seconds, after_exit)
        except BaseException as error:
            failure = type(error).__name__
            raise
        finally:
            remaining = self.cleanup()
            with path.open("a") as trace:
                trace.write(json.dumps({"event": "teardown", "remainingTasks": len(remaining), "observerError": failure}) + "\n")
            if remaining:
                raise RuntimeError("kernel observer cleanup incomplete")

    def consume(self, stream, write, seconds, after_exit):
        self.armed = self.stopped = False
        with selectors.DefaultSelector() as selector:
            selector.register(self.hook.stdout, selectors.EVENT_READ)
            return self.poll(selector, stream, write, seconds, after_exit)

    def poll(self, selector, stream, write, seconds, after_exit):
        started = time.monotonic()
        while time.monotonic() - started < seconds:
            self.armed = self.step(selector, stream, write, self.armed)
            self.maybe_stop(after_exit)
            if not self.armed and time.monotonic() - started > 10:
                raise RuntimeError("kernel observer did not arm within 10 seconds")
            if self.hook.poll() is not None:
                self.drain(stream)
                return self.finished(self.armed, self.stopped)
        raise RuntimeError("kernel observation reached unchanged wall bound")

    def maybe_stop(self, after_exit):
        if self.target.poll() is None or self.stopped:
            return
        self.stopped = True
        if after_exit:
            after_exit()
        self.stop_hook()

    def step(self, selector, stream, write, armed):
        if not selector.select(timeout=0.05):
            return armed
        line = self.hook.stdout.readline(16384)
        if not line:
            return armed
        row = self.record(line, stream)
        if row["event"] == "armed":
            if armed or row["root"] != self.target.pid:
                raise RuntimeError("ambiguous kernel observer arming")
            os.write(write, b"x")
            return True
        return armed

    def drain(self, stream):
        while line := self.hook.stdout.readline(16384):
            self.record(line, stream)

    def finished(self, armed, stopped):
        log = (self.output / f"{self.name}.kernel.log").read_text(errors="replace")
        if not armed or not stopped or self.hook.returncode != 0 or "lost" in log.lower():
            raise RuntimeError("kernel observer did not complete with loss-free arming")
        return self.target.returncode


def pidfd_alive(descriptor):
    return not select.select([descriptor], [], [], 0)[0]


def kill_owned(descriptor):
    if not pidfd_alive(descriptor):
        return
    try:
        signal.pidfd_send_signal(descriptor, signal.SIGKILL)
    except ProcessLookupError:
        pass


def wait_barrier(ready_read):
    if not select.select([ready_read], [], [], 10)[0] or os.read(ready_read, 1) != b"r":
        raise RuntimeError("owned launcher did not reach its prelaunch barrier")


def wait_hook(run):
    run.hook.wait(timeout=10)


def subreaper(value=None):
    libc = ctypes.CDLL(None, use_errno=True)
    previous = ctypes.c_int()
    if libc.prctl(37, ctypes.byref(previous), 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "read owned controller subreaper state")
    if libc.prctl(36, 1 if value is None else value, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "set owned controller subreaper state")
    return previous.value


def child_pids(parent):
    try:
        tasks = Path(f"/proc/{parent}/task").iterdir()
        return children_from_tasks(itertools.islice(tasks, 8193))
    except FileNotFoundError:
        return set()


def children_from_tasks(tasks):
    result = set()
    for index, task in enumerate(tasks):
        if index >= 8192:
            raise RuntimeError("owned thread list exceeds cleanup bound")
        result.update(task_children(task))
    if len(result) > 8192:
        raise RuntimeError("owned child list exceeds cleanup bound")
    return result


def task_children(task):
    try:
        with (task / "children").open() as stream:
            data = stream.read(131073)
        if len(data) > 131072:
            raise RuntimeError("owned child list exceeds byte bound")
        return set(map(int, data.split()))
    except FileNotFoundError:
        return set()


def task_parent(pid):
    try:
        return int(Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[1])
    except (OSError, ValueError, IndexError):
        return None


def reap_adopted():
    try:
        while os.waitpid(-1, os.WNOHANG)[0]:
            pass
    except ChildProcessError:
        pass
