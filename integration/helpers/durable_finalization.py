"""Pause the real finalizer and crash or retire it under the existing outer owner."""
import os
from pathlib import Path
import signal
import sys

from durable_create import exchange
from durable_output import bound, create, wait
from session_lifecycle import LifecycleFixture
from terminal_window import eventually


def scenario(fixture, stage, crash):
    fixture.capture_tree()
    release = fixture.root / (stage + ".release")
    r = create(fixture, stage, ["/bin/sh", "-c",
               'while [ ! -f "$1" ]; do sleep .01; done; printf final', "owned", str(release)])
    worker, child = fixture.session_processes(r["sessionId"])
    directory = fixture.local / "s" / r["sessionId"]
    hold = directory / (".execution-hold-" + stage)
    hold.touch()
    release.touch()
    try:
        eventually(lambda: Path(str(hold) + ".reached").exists(), "actual finalizer did not reach " + stage)
        if crash:
            before = exchange(fixture, bound("wait", r, timeoutMillis=100))
            assert before.get("executionWait") == "timed_out" and before["executionReceipt"]["result"] is None, before
            assert fixture.kernel.send(fixture.tree.proved[worker].fd, signal.SIGKILL)
            eventually(lambda: not fixture.alive(worker), "finalizing worker did not exit")
            fixture.stop_daemon()
            fixture.start_local_daemon()
            after = exchange(fixture, bound("wait", r, timeoutMillis=100))
            assert after.get("executionWait") == "timed_out" and after["executionReceipt"]["result"] is None, after
        else:
            wait(fixture, r)
            denied = exchange(fixture, bound("remove", r))
            assert denied["type"] == "error" and directory.exists(), denied
            hold.unlink()
            removed = exchange(fixture, bound("remove", r))
            assert removed["executionReceipt"]["removal"] == "removed", removed
            assert not directory.exists()
            eventually(lambda: not fixture.alive(worker), "released finalizer did not exit")
    finally:
        if hold.exists():
            hold.unlink()
        for pid in (worker, child):
            if fixture.alive(pid):
                fixture.kernel.send(fixture.tree.proved[pid].fd, signal.SIGKILL)
        fixture.tree.close()
        fixture.tree = None


def main():
    root = Path(os.environ["HOME"]).parent / "durable-finalization"
    root.mkdir()
    fixture = LifecycleFixture(sys.argv[1], root)
    try:
        fixture.start_local_daemon()
        for stage in ("before_capture_finish", "before_result"):
            scenario(fixture, stage, True)
        for stage in ("after_result", "before_lifetime_release"):
            scenario(fixture, stage, False)
    finally:
        fixture.close()
    print("PASS: actual worker death before final manifests/results stays unknown; result visibility never releases finalizer ownership")


if __name__ == "__main__":
    main()
