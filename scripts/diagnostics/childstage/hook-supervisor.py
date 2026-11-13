"""Disposable runner only: remove this invocation's BPF hooks on controller EOF."""
import ctypes
import json
import os
from pathlib import Path
import selectors
import signal
import socket
import struct
import subprocess
import sys


def parent_death():
    parent = os.getppid()
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(1, signal.SIGKILL, 0, 0, 0) != 0 or os.getppid() != parent:
        os._exit(125)


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


def main():
    if os.geteuid() != 0 or len(sys.argv) != 3 or not sys.argv[1].isdecimal():
        raise SystemExit("hook supervisor requires its owned root PID")
    signal.signal(signal.SIGTERM, terminate)
    with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as notification:
        address = "mesh-childstage-" + os.urandom(16).hex()
        notification.setsockopt(socket.SOL_SOCKET, socket.SO_PASSCRED, 1)
        notification.bind("\0" + address)
        return run_hook(notification, address, int(sys.argv[1]))


def run_hook(notification, address, root):
    environment = dict(os.environ, NOTIFY_SOCKET="@" + address)
    hook = subprocess.Popen([sys.argv[2], "-q", "-B", "line",
                             str(Path(__file__).with_name("returns.bt")), str(root)], env=environment, preexec_fn=parent_death)
    try:
        watch(notification, hook, root)
    finally:
        stop_hook(hook)
    return hook.returncode


def watch(notification, hook, root):
    with selectors.DefaultSelector() as selector:
        selector.register(notification, selectors.EVENT_READ)
        selector.register(sys.stdin, selectors.EVENT_READ)
        watch_loop(selector, hook, root)


def watch_loop(selector, hook, root):
    while hook.poll() is None:
        if ready_step(selector, hook, root):
            return


def ready_step(selector, hook, root):
    for key, _ in selector.select(timeout=0.05):
        if key.fileobj is sys.stdin:
            return controller_closed()
        notification(key.fileobj, hook, root)
    return False


def controller_closed():
    if os.read(sys.stdin.fileno(), 1):
        raise RuntimeError("hook supervisor accepts EOF only")
    return True


def notification(stream, hook, root):
    message, ancillary, _, _ = stream.recvmsg(4096, socket.CMSG_SPACE(12))
    credentials = [data for level, kind, data in ancillary if level == socket.SOL_SOCKET and kind == socket.SCM_CREDENTIALS]
    if len(credentials) != 1 or struct.unpack("3i", credentials[0])[0] != hook.pid:
        raise RuntimeError("readiness notification belongs to another process")
    if b"READY=1" in message.splitlines():
        print(json.dumps({"event": "armed", "root": root, "backend": "bpftrace", "diagnosticOnly": True}), flush=True)


def stop_hook(hook):
    if hook.poll() is not None:
        return
    hook.send_signal(signal.SIGINT)
    try:
        hook.wait(timeout=5)
    except subprocess.TimeoutExpired:
        hook.kill()
        hook.wait(timeout=3)


if __name__ == "__main__":
    raise SystemExit(main())
