"""Exact keyed PTY pages, finite waits and instance-fenced controls."""
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

from durable_create import exchange, receipt
from session_lifecycle import LifecycleFixture
from terminal_window import eventually


def bound(kind, r, **extra):
    return dict(type="execution." + kind, requestId="output-" + kind,
                sessionId=r["sessionId"], instance=r["instance"], **extra)


def create(fixture, key, command):
    return receipt(exchange(fixture, dict(type="execution.create", requestId=key, requestKey=key,
                                         kind="pty", cwd=str(fixture.root), command=command)))


def wait(fixture, r):
    response = exchange(fixture, bound("wait", r, timeoutMillis=3000))
    assert response.get("executionWait") == "completed", response
    return response["executionReceipt"]


def logs(fixture, r, cursor="", size=65536):
    response = exchange(fixture, bound("logs", r, stream="pty", cursor=cursor, maxBytes=size))
    assert response.get("type") == "execution.receipt", response
    return response["executionPage"]


def exact_and_controls(fixture):
    r = create(fixture, "binary", ["/bin/sh", "-c", "printf 'A\\000\\377Z'; exit 7"])
    final = wait(fixture, r)
    assert final["result"]["termination"]["code"] == 7 and final["result"]["capture"] == "complete", final
    first = logs(fixture, r, size=2)
    second = logs(fixture, r, first["nextCursor"], 2)
    assert base64.b64decode(first["bytesBase64"]) + base64.b64decode(second["bytesBase64"]) == b"A\x00\xffZ"
    assert logs(fixture, r, size=2) == first and second["eof"]
    fixture.stop_daemon()
    fixture.start_local_daemon()
    assert logs(fixture, r, size=2) == first
    wait(fixture, r)
    live = create(fixture, "live", ["/bin/sh", "-c", "while :; do sleep 1; done"])
    empty = logs(fixture, live)
    assert not empty["eof"] and not empty["streamClosed"]
    timeout = exchange(fixture, bound("wait", live, timeoutMillis=100))
    assert timeout["executionWait"] == "timed_out", timeout
    wrong = exchange(fixture, bound("signal", dict(live, instance="0" * 32), signal="kill"))
    assert wrong["type"] == "error" and fixture.metadata(live["sessionId"])["state"] in ("running", "detached"), wrong
    stopped = exchange(fixture, bound("kill", live))
    assert stopped["executionDelivery"] == "delivered", stopped
    killed = wait(fixture, live)
    assert killed["killRequested"] and killed["result"]["termination"]["kind"] == "killed", killed
    removed = exchange(fixture, bound("remove", r))
    assert removed["executionReceipt"]["removal"] == "removed", removed
    expired = exchange(fixture, bound("logs", r, stream="pty", maxBytes=64))
    assert expired["errorCode"] == "logs.cursor_expired" and not expired["executionPage"]["available"], expired
    expired_result = exchange(fixture, bound("wait", r, timeoutMillis=100))
    assert expired_result["errorCode"] == "result.expired", expired_result
    host = exchange(fixture, dict(type="host.info", requestId="host"))["host"]["id"]
    result = subprocess.run([fixture.binary, "session", "wait", "--host", host, live["sessionId"],
                             "--instance", live["instance"], "--json"], env=fixture.environment,
                            capture_output=True, text=True, timeout=5, check=False)
    assert result.returncode == 0 and not result.stderr, (result.stdout, result.stderr)
    assert json.loads(result.stdout)["data"]["wait"] == "completed", result.stdout


def crashed_worker(fixture):
    fixture.capture_tree()
    r = create(fixture, "crash", ["/bin/sh", "-c", "printf durable-prefix; while :; do sleep 1; done"])
    worker, child = fixture.session_processes(r["sessionId"])
    try:
        eventually(lambda: base64.b64decode(logs(fixture, r)["bytesBase64"]) == b"durable-prefix", "prefix not committed")
        assert fixture.kernel.send(fixture.tree.proved[worker].fd, signal.SIGKILL)
        eventually(lambda: not fixture.alive(worker), "owned worker did not die")
        fixture.stop_daemon()
        fixture.start_local_daemon()
        page = logs(fixture, r)
        assert base64.b64decode(page["bytesBase64"]) == b"durable-prefix" and not page["eof"]
        result = exchange(fixture, bound("wait", r, timeoutMillis=100))
        assert result.get("executionWait") == "timed_out" and result["executionReceipt"]["result"] is None and result["executionReceipt"]["phase"] == "uncertain", result
    finally:
        if fixture.alive(child):
            fixture.kernel.send(fixture.tree.proved[child].fd, signal.SIGKILL)


def main():
    root = Path(os.environ["HOME"]).parent / "durable-output"
    root.mkdir()
    fixture = LifecycleFixture(sys.argv[1], root)
    try:
        fixture.start_local_daemon()
        exact_and_controls(fixture)
        crashed_worker(fixture)
    finally:
        fixture.close()
    print("PASS: exact binary independent pages, daemon reopen, finite wait, worker crash, exact kill, removal and expiry")


if __name__ == "__main__":
    main()
