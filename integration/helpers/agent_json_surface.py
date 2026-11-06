"""Exercise the finite agent entry points with private, disposable client state."""

import json
import os
from contextlib import contextmanager
from pathlib import Path
import subprocess
import sys

from isolation import termination_cleanup


def invoke(binary, root, arguments, expected, code=None):
    environment = dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                       MESH_STATE_DIR=str(root / "state"), MESH_PRIVACY="0")
    result = subprocess.run([binary, *arguments], env=environment, capture_output=True,
                            text=True, timeout=10, check=False)
    assert result.returncode == expected, (arguments, result.returncode, result.stderr)
    if expected == 0 or code:
        envelope = json.loads(result.stdout)
        assert envelope["operation"] == "services.list", envelope
        assert envelope["schemaVersion"] == 1, envelope
        assert len(envelope["requestId"]) == 26, envelope
        assert result.stderr == "", result.stderr
        if code:
            assert envelope["status"] == "error" and envelope["data"] is None, envelope
            assert envelope["errors"][0]["code"] == code, envelope
        else:
            assert envelope["status"] == "ok" and envelope["errors"] == [], envelope
            assert envelope["data"] == {"services": [], "hostErrors": []}, envelope
    return result


def absent_state(root):
    assert not (root / "state").exists(), "passive read created state"


def fresh_cases(binary, root):
    for alias in ("ls", "list"):
        invoke(binary, root, ["serve", alias, "--json"], 0)
        absent_state(root)
    for flags in (["--bogus"], ["extra"], ["--privacy"], ["--timeout=0"]):
        invoke(binary, root, ["serve", "ls", "--json", *flags], 2, "usage.invalid")
        absent_state(root)
    invoke(binary, root, ["serve", "ls", "--json=false", "--json=true",
                          "--privacy", "--privacy=false"], 0)
    absent_state(root)
    config = root / "config"
    config.mkdir(mode=0o700)
    (config / "domains.json").write_text("{")
    invoke(binary, root, ["serve", "list", "--json"], 1, "state.unavailable")
    absent_state(root)
    (config / "domains.json").unlink()
    (config / "hosts.json").write_text("{")
    invoke(binary, root, ["serve", "ls", "--json"], 1, "state.unavailable")
    separated = invoke(binary, root, ["--host", "session", "--", "tool", "--json"], 1)
    assert not separated.stdout, separated.stdout
    absent_state(root)
    (config / "hosts.json").unlink()


def saved_remote_case(binary, root):
    host = {"id": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            "meshIdentity": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
            "endpoint": "ws://127.0.0.1:1/control/ws"}
    (root / "config" / "hosts.json").write_text(json.dumps({"version": 1, "hosts": [host]}))
    invoke(binary, root, ["serve", "ls", "--json"], 1, "host.uninitialized")
    absent_state(root)


def daemon_identity_case(binary, root):
    environment = dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                       MESH_STATE_DIR=str(root / "state"))
    with (root / "daemon.log").open("w") as log, isolated_daemon(binary, environment, log) as daemon:
        wait_for_socket(daemon, root / "state" / "daemon.sock")
    database = root / "state" / "mesh.db"
    database.unlink()
    (root / "state" / "s").rmdir()
    before = (root / "state" / "identity.key").read_bytes()
    invoke(binary, root, ["serve", "list", "--json", "--timeout=100ms"], 1, "host.unreachable")
    assert not database.exists(), "service read created an absent cache"
    assert not (root / "state" / "s").exists(), "service read created sessions"
    assert before == (root / "state" / "identity.key").read_bytes(), "identity changed"


@contextmanager
def isolated_daemon(binary, environment, log):
    daemon = None
    try:
        daemon = subprocess.Popen([binary, "daemon"], env=environment, stdout=log,
                                  stderr=subprocess.STDOUT)
        yield daemon
    finally:
        if daemon is not None:
            stop_daemon(daemon)


def wait_for_socket(daemon, path):
    import time
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        assert daemon.poll() is None, "isolated daemon exited"
        if path.is_socket():
            return
        time.sleep(0.01)
    raise AssertionError("isolated daemon did not listen")


def stop_daemon(daemon):
    daemon.terminate()
    try:
        daemon.wait(timeout=5)
    except subprocess.TimeoutExpired:
        daemon.kill()
        daemon.wait(timeout=5)
        raise


def main():
    binary, directory = sys.argv[1:]
    root = Path(directory)
    with termination_cleanup():
        fresh_cases(binary, root)
        saved_remote_case(binary, root)
        daemon_identity_case(binary, root)
    print("PASS: agent JSON errors, aliases, separation and passive missing-state/cache reads")


if __name__ == "__main__":
    main()
