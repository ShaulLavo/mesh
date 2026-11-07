"""Own the enrollment fixture's fixed, non-detaching CLI/SSH/CLI chain."""

import json
import os
from pathlib import Path
import resource
import select
import signal
import subprocess
import sys
import time


OUTPUT_LIMIT = 1 << 20


def signal_group(group, number):
    try:
        os.killpg(group, number)
        return "present"
    except ProcessLookupError:
        return "absent"
    except PermissionError:
        # Darwin excludes zombies from killpg, including signal 0. EPERM also
        # covers live members we cannot signal; neither case proves absence.
        return "denied"


def group_exists(group):
    return signal_group(group, 0) != "absent"


def stop_group(child, group, grace):
    deadline = time.monotonic() + grace
    last_signal = signal_group(group, signal.SIGTERM)
    killed = False
    while time.monotonic() < deadline:
        status = child.poll()
        if status is not None and not group_exists(group):
            return True, None
        if not killed and time.monotonic() >= deadline - 2:
            last_signal = signal_group(group, signal.SIGKILL)
            killed = True
        time.sleep(0.01)
    probe = signal_group(group, 0) if child.poll() is not None else "child-running"
    if probe == "absent":
        return True, None
    return False, f"group {group} join unproved; probe {probe}; last cleanup signal {last_signal}"


def wait_command(child, timeout):
    deadline = time.monotonic() + timeout if timeout else None
    while True:
        status = child.poll()
        if status is not None:
            return "exit"
        if deadline is not None and time.monotonic() >= deadline:
            return "timeout"
        readable, _, _ = select.select([sys.stdin], [], [], 0.01)
        if readable:
            os.read(sys.stdin.fileno(), 1)
            return "stop"


def run_anchor(parent_group, timeout, stdout, stderr, args):
    # Our PID creates the command group, then stays reserved while we move out.
    # Neither reaping the CLI nor its early exit can allow this group ID to be
    # reused. No signal is sent after this anchor exits. The fixed chain must
    # inherit this group; this is not ownership of arbitrary detaching programs.
    if os.getpgrp() != os.getpid() or parent_group == os.getpid():
        raise RuntimeError("enrollment anchor requires its own non-session process group")
    for number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(number, lambda *_: None)
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    resource.setrlimit(resource.RLIMIT_FSIZE, (OUTPUT_LIMIT, OUTPUT_LIMIT))
    with open(stdout, "wb") as output, open(stderr, "wb") as errors:
        child = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=output, stderr=errors)
    os.setpgid(0, parent_group)
    failure = None
    try:
        reason = wait_command(child, timeout)
    except BaseException as error:
        reason = "error"
        failure = str(error)
    joined = child.poll() is not None and not group_exists(os.getpid())
    leaked = reason == "exit" and not joined
    if not joined:
        joined, cleanup_error = stop_group(child, os.getpid(), 8 if not timeout else 4)
        failure = failure or cleanup_error
    print(json.dumps({"joined": joined, "reason": reason, "status": child.returncode,
                      "leaked": leaked, "group": os.getpid(), "error": failure}), flush=True)


class OwnedCommand:
    def __init__(self, root, args, environment, timeout=0):
        self.args = args
        self.timeout = timeout
        self.joined = False
        self.receipt = None
        self.directory = Path(root) / f"command-{time.monotonic_ns()}"
        self.directory.mkdir(mode=0o700)
        self.stdout = self.directory / "stdout"
        self.stderr = self.directory / "stderr"
        self.anchor = subprocess.Popen(
            [sys.executable, str(Path(__file__).resolve()), "--anchor", str(os.getpgrp()),
             str(timeout), str(self.stdout), str(self.stderr), *args],
            env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, preexec_fn=os.setpgrp)

    def collect(self, timeout):
        if self.receipt is not None:
            return self.receipt
        try:
            output, errors = self.anchor.communicate(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            raise RuntimeError(f"anchor join unproved; retained {self.directory}; anchor {self.anchor.pid}") from error
        try:
            self.receipt = json.loads(output)
        except (ValueError, UnicodeError) as error:
            raise RuntimeError(f"anchor failed; retained {self.directory}: {errors[:4096]!r}") from error
        self.joined = self.receipt["joined"] and self.anchor.returncode == 0
        if not self.joined:
            raise RuntimeError(f"command group join unproved; retained {self.directory}: {self.receipt}")
        return self.receipt

    def result(self, check=False, cancelled=lambda: False):
        # Keep stdin open during the command. communicate() would request stop
        # through EOF before the original ten-second command deadline.
        deadline = time.monotonic() + self.timeout + 6
        while self.anchor.poll() is None and time.monotonic() < deadline:
            if cancelled():
                self.close()
                raise InterruptedError("enrollment fixture cancelled")
            time.sleep(0.01)
        receipt = self.collect(max(0, deadline - time.monotonic()))
        output = self.stdout.read_bytes()[:OUTPUT_LIMIT].decode(errors="replace")
        errors = self.stderr.read_bytes()[:OUTPUT_LIMIT].decode(errors="replace")
        if receipt["reason"] == "timeout":
            raise subprocess.TimeoutExpired(self.args, self.timeout, output, errors)
        if receipt["leaked"]:
            raise RuntimeError(f"command left a process in its enrollment group: {self.args}")
        if receipt["reason"] == "error":
            raise RuntimeError(f"enrollment command failed: {receipt['error']}")
        result = subprocess.CompletedProcess(self.args, receipt["status"], output, errors)
        if check:
            result.check_returncode()
        return result

    def close(self):
        if self.receipt is not None:
            if not self.joined:
                raise RuntimeError(f"command join unproved; retained {self.directory}")
            return
        self.collect(10)


if __name__ == "__main__":
    run_anchor(int(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5], sys.argv[6:])
