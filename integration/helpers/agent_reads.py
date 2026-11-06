"""Finite passive host reads against a pinned fake host. No daemon or worker runs."""

from contextlib import contextmanager
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time


def environment(root):
    return dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                MESH_STATE_DIR=str(root / "state"), MESH_PRIVACY="0")


def invoke(binary, root, args, exit_code=0):
    result = subprocess.run([binary, *args], env=environment(root), capture_output=True,
                            text=True, timeout=10, check=False)
    assert result.returncode == exit_code, (args, result.returncode, result.stdout, result.stderr)
    assert not result.stderr, result.stderr
    value = json.loads(result.stdout)
    assert value["schemaVersion"] == 1 and len(value["requestId"]) == 26, value
    assert value["operation"] in ("hosts.list", "hosts.inspect"), value
    if exit_code == 0:
        assert value["status"] == "ok" and value["errors"] == [], value
    if exit_code == 3:
        assert value["status"] == "partial" and value["data"] is not None, value
    if exit_code in (1, 2):
        assert value["status"] == "error" and value["data"] is None, value
    return value


def no_state(root):
    assert not (root / "state").exists(), "read initialized state"


def empty_cases(binary, root):
    value = invoke(binary, root, ["hosts", "--json"], 3)
    local = value["data"]["hosts"]
    assert len(local) == 1 and local[0]["hostId"] is None, value
    assert local[0]["reachability"] == "uninitialized", value
    no_state(root)
    assert not (root / "config").exists()
    for args in (["hosts", "--json", "--bogus"], ["inspect", "--json"],
                 ["hosts", "--json", "--timeout=99ms"],
                 ["inspect", "fixture", "--json", "--timeout=61s"],
                 ["hosts", "--json", "--privacy"]):
        value = invoke(binary, root, args, 2)
        assert value["errors"][0]["code"] == "usage.invalid", value
        no_state(root)
    value = invoke(binary, root, ["inspect", "missing", "--json", "--cached"], 1)
    assert value["errors"][0]["code"] == "host.unknown", value
    no_state(root)


@contextmanager
def fake_host(fixture, root):
    child = None
    try:
        child = subprocess.Popen([fixture, str(root)], stdin=subprocess.PIPE,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 text=True, env=environment(root))
        ready = read_reply(child)
        yield child, ready
    finally:
        stop_host(child)


def stop_host(child):
    if child is None:
        return
    child.terminate()
    try:
        child.wait(timeout=5)
    except subprocess.TimeoutExpired:
        child.kill()
        child.wait(timeout=5)
    child.stdin.close()
    child.stdout.close()
    child.stderr.close()


def read_reply(child):
    readable, _, _ = select.select([child.stdout], [], [], 5)
    assert readable, "fixture did not answer within five seconds"
    return json.loads(child.stdout.readline())


def control(child, text):
    child.stdin.write(text + "\n")
    child.stdin.flush()
    return read_reply(child)


def inspect(binary, root, host_id, *flags, exit_code=0):
    return invoke(binary, root, ["inspect", host_id, "--json", *flags], exit_code)


def cache_cases(binary, root, child, host_id):
    first = inspect(binary, root, host_id)["data"]
    assert first["host"]["declared"]["host"]["profile"]["instructions"] == "host notes", first
    assert first["host"]["declared"]["fleet"]["profile"]["instructions"] == "fleet notes", first
    assert first["access"]["state"] == "live" and first["access"]["grant"] == "full-device", first
    value = invoke(binary, root, ["hosts", "--json"])
    remote = next(h for h in value["data"]["hosts"] if h["hostId"] == host_id)
    assert remote["declared"]["host"]["profile"]["instructions"] is None, remote
    assert "profile" not in remote["declared"]["fleet"], remote
    before = control(child, "operations")
    named = inspect(binary, root, "fixture", "--cached")["data"]
    assert named["host"]["hostId"] == host_id, named
    cached = inspect(binary, root, host_id, "--cached")["data"]
    assert cached["host"]["declared"]["host"]["profile"]["instructions"] == "host notes", cached
    assert cached["access"]["grant"] == "unknown" and cached["access"]["observedAt"] is None, cached
    invoke(binary, root, ["hosts", "--json", "--cached"])
    assert control(child, "operations") == before, "cached reads contacted host"
    control(child, "change")
    invoke(binary, root, ["hosts", "--json"])
    incomplete = inspect(binary, root, host_id, "--cached", exit_code=3)
    assert incomplete["data"]["host"]["declared"]["host"]["state"] == "unavailable", incomplete
    assert "profile" not in incomplete["data"]["host"]["declared"]["host"], incomplete
    inspect(binary, root, host_id)
    control(child, "regress")
    regressed = inspect(binary, root, host_id, exit_code=3)
    assert any(e["code"] == "profile.regressed" for e in regressed["errors"]), regressed
    inspect(binary, root, host_id)


def error_cases(binary, root, child, host_id):
    control(child, "unsupported")
    value = inspect(binary, root, host_id, exit_code=3)
    assert any(e["code"] == "protocol.unsupported" for e in value["errors"]), value
    assert value["data"]["access"]["grant"] == "unknown", value
    control(child, "corrupt")
    value = inspect(binary, root, host_id, exit_code=3)
    assert value["data"]["host"]["declared"]["host"]["freshness"] == "cached", value
    control(child, "blocked")
    started = time.monotonic()
    invoke(binary, root, ["hosts", "--json", "--timeout=100ms"], 3)
    assert time.monotonic() - started < 3, "host read exceeded finite control budget"
    assert not (root / "state" / "s").exists(), "read created session directory"
    assert not (root / "state" / "mesh.db").exists(), "read created catalog database"
    assert set(control(child, "operations")) == {"host.info", "host.context"}


def keyless_case(binary, root, host_id):
    value = inspect(binary, root, host_id, exit_code=1)
    assert value["errors"][0]["code"] == "host.uninitialized", value
    no_state(root)


def main():
    binary, fixture, path = sys.argv[1:]
    root = Path(path)
    empty_cases(binary, root / "empty")
    with fake_host(fixture, root / "live") as (child, ready):
        live = root / "live"
        keyless = root / "keyless"
        (keyless / "config").mkdir(parents=True, mode=0o700)
        (keyless / "config" / "hosts.json").write_bytes((live / "config" / "hosts.json").read_bytes())
        keyless_case(binary, keyless, ready["id"])
        cache_cases(binary, live, child, ready["id"])
        error_cases(binary, live, child, ready["id"])
    stopped = inspect(binary, live, ready["id"], exit_code=3)
    assert stopped["data"]["access"]["grant"] == "unknown", stopped
    assert not (live / "state" / "s").exists()
    print("PASS: passive host context, cache completeness, typed errors and no lifecycle controls")


if __name__ == "__main__":
    main()
