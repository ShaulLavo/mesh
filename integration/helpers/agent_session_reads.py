"""Exercise only finite, armed fake hosts and owned on-disk session records."""

import json
import os
import selectors
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path


def environment(root):
    return dict(os.environ, MESH_CONFIG_DIR=str(root / "config"),
                MESH_STATE_DIR=str(root / "state"), MESH_PRIVACY="0")


def invoke(binary, root, args, expected=0, code=None):
    result = subprocess.run([binary, "session", *args], env=environment(root),
                            capture_output=True, text=True, timeout=5, check=False)
    assert result.returncode == expected, (args, result.returncode, result.stdout, result.stderr)
    value = json.loads(result.stdout)
    assert not result.stderr and value["schemaVersion"] == 1, (value, result.stderr)
    operation = "sessions.inspect" if args[0] == "inspect" else "sessions.list"
    assert value["operation"] == operation and len(value["requestId"]) == 26, value
    if code:
        assert value["errors"][0]["code"] == code, value
    if expected in (1, 2):
        assert value["status"] == "error" and value["data"] is None, value
    if expected == 0:
        assert value["status"] == "ok" and value["errors"] == [], value
    if expected == 3:
        assert value["status"] == "partial" and value["data"] is not None, value
    return value["data"]


def usage_cases(binary, root):
    for flags in (["--limit", "0"], ["--limit", "501"], ["--limit0"],
                  ["--limit501"], ["--timeout", "0"], ["--timeout", "61s"],
                  ["--bogus"], ["--privacy"], ["--page", "bad"],
                  ["--json=invalid"], ["--limit", "--json=false"]):
        invoke(binary, root, ["list", "--host", "unused", "--json", *flags], 2, "usage.invalid")
    for args in (["inspect", "--host", "unused", "--json"],
                 ["inspect", "bad/path", "--host", "unused", "--json"],
                 ["nope", "--json"], ["--json"], ["list", "--json"],
                 ["list", "--host", "unused", "--json", "--", "extra"]):
        invoke(binary, root, args, 2, "usage.invalid")
    assert not (root / "state").exists() and not (root / "config").exists()
    for args in (["list", "--host", "unused", "--", "--json"],
                 ["list", "--host", "unused", "--json", "--json=false"]):
        result = subprocess.run([binary, "session", *args], env=environment(root),
                                capture_output=True, text=True, timeout=5, check=False)
        assert result.returncode == 2 and not result.stdout, result
    (root / "config").mkdir(parents=True)
    (root / "config" / "domains.json").write_text("not json")
    invoke(binary, root, ["list", "--host", "unused", "--json"], 1, "state.unavailable")
    assert not (root / "state").exists()


def live_cases(binary, root, ids):
    base = ["list", "--host", ids["a"], "--json"]
    data = invoke(binary, root, base)
    assert data["hostId"] == ids["a"] and data["freshness"] == "live", data
    assert [s["id"] for s in data["sessions"]] == ["7K3D", "9K3D"], data
    assert all("command" not in s and "cwd" not in s for s in data["sessions"]), data
    detailed = invoke(binary, root, base + ["--all", "--detail"])
    assert detailed["sessions"][0]["command"] == ["sh", "secret argument"], detailed
    saved = detailed["sessions"][1]
    assert saved["exitCode"] == 37 and saved["hibernated"] and saved["recoveryPending"], saved
    page = invoke(binary, root, base + ["--all", "--limit", "1"])
    assert page["nextPage"] and len(page["sessions"]) == 1, page
    next_page = invoke(binary, root, base + ["--all", "--limit", "1", "--page", page["nextPage"]])
    assert next_page["sessions"][0]["id"] == "8K3D", next_page
    invoke(binary, root, ["list", "--host", ids["b"], "--json", "--page", page["nextPage"]], 2, "usage.invalid")
    inspect = ["inspect", "7K3D", "--host", ids["a"], "--json"]
    detail = invoke(binary, root, inspect)
    assert detail["liveState"] == "live" and detail["live"]["attached"], detail
    assert "preview" not in detail["live"] and detail["saved"] is None, detail
    assert detail["live"]["terminalTitle"] == "title", detail
    detail = invoke(binary, root, inspect + ["--preview"])
    assert detail["live"]["preview"] == ["private preview"], detail
    for preview in ([], ["--preview"]):
        detail = invoke(binary, root, ["inspect", "8K3D", "--host", ids["a"], "--json", *preview])
        assert detail["live"] is None and detail["liveState"] == "saved", detail
        assert detail["saved"]["checkpointAt"] is None, detail
        assert detail["saved"]["preview"] == (["saved preview"] if preview else []), detail
    detail = invoke(binary, root, ["inspect", "9K3D", "--host", ids["a"], "--json"], 3)
    assert detail["liveState"] == "unavailable" and detail["live"] is None, detail
    invoke(binary, root, ["inspect", "AK3D", "--host", ids["a"], "--json"], 1, "session.not_found")
    invoke(binary, root, ["list", "--host", ids["wrong"], "--json"], 1, "host.identity_mismatch")
    assert not (root / "state" / "mesh.db").exists(), "optional cache created"


def local_cases(binary, root, ids):
    inspect = ["inspect", "7K3D", "--host", ids["local"], "--json"]
    detail = invoke(binary, root, inspect)
    assert detail["liveState"] == "live" and "preview" not in detail["live"], detail
    detail = invoke(binary, root, ["inspect", "8K3D", "--host", ids["local"], "--json"])
    assert detail["session"]["exitCode"] == 37 and detail["live"] is None, detail
    assert detail["saved"]["checkpointAt"] is None and detail["saved"]["preview"] == [], detail
    invoke(binary, root, ["inspect", "AK3D", "--host", ids["local"], "--json"], 1, "session.not_found")
    missing = root / "missing-sessions"
    (missing / "state").mkdir(parents=True)
    shutil.copy2(root / "state" / "identity.key", missing / "state" / "identity.key")
    data = invoke(binary, missing, ["list", "--host", ids["local"], "--json"])
    assert data["sessions"] == [] and data["nextPage"] is None, data
    invoke(binary, missing, ["inspect", "AK3D", "--host", ids["local"], "--json"], 1, "session.not_found")
    assert {p.name for p in (missing / "state").iterdir()} == {"identity.key"}
    assert not (missing / "config").exists()


def stopped_cases(binary, root, ids):
    config = json.loads((root / "config" / "hosts.json").read_text())
    config["hosts"][0]["endpoint"] = "ws://127.0.0.1:1/control/ws"
    (root / "config" / "hosts.json").write_text(json.dumps(config))
    started = time.monotonic()
    invoke(binary, root, ["list", "--host", ids["a"], "--json", "--timeout", "100ms"], 1, "host.unreachable")
    assert time.monotonic() - started < 2
    missing = root / "keyless"
    (missing / "config").mkdir(parents=True)
    shutil.copy2(root / "config" / "hosts.json", missing / "config" / "hosts.json")
    invoke(binary, missing, ["list", "--host", ids["a"], "--json"], 1, "host.uninitialized")
    assert not (missing / "state").exists()
    invoke(binary, missing, ["list", "--host", "unknown", "--json"], 1, "host.unknown")
    assert not (missing / "state").exists()


def stop_fixture(process):
    try:
        stdout, stderr = process.communicate("stop\n", timeout=15)
    except subprocess.TimeoutExpired as error:
        process.send_signal(signal.SIGQUIT)
        try:
            stdout, stderr = process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            stdout, stderr = process.communicate(timeout=5)
        raise AssertionError(f"Fixture did not join: {stdout} {stderr}") from error
    assert process.returncode == 0, (process.returncode, stderr)
    return json.loads(stdout)


def interrupted(_signal, _frame):
    raise KeyboardInterrupt


def run(binary, fixture, root):
    signal.signal(signal.SIGTERM, interrupted)
    usage_cases(binary, root / "usage")
    (root / "hosts").mkdir(parents=True, mode=0o700)
    process = subprocess.Popen([fixture, "--armed", str(root / "hosts")],
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True)
    try:
        with selectors.DefaultSelector() as ready:
            ready.register(process.stdout, selectors.EVENT_READ)
            assert ready.select(5), "fixture did not become ready"
        ids = json.loads(process.stdout.readline())
        live_cases(binary, root / "hosts", ids)
        local_cases(binary, root / "hosts", ids)
        stopped_cases(binary, root / "hosts", ids)
    finally:
        events = stop_fixture(process)
        assert not (root / "hosts" / "state" / "s" / "7K3D" / "sock").exists()
        assert all(e["type"] in ("host.info", "session.list", "session.inspect") for e in events), events
        inspections = [e for e in events if e["type"] == "session.inspect"]
        assert all((e["previewCols"], e["previewRows"]) in ((1, 1), (80, 24)) for e in inspections), inspections
        print(f"Joined finite fixture {process.pid}; {len(events)} passive requests.")
    print("PASS: passive session reads, paging, privacy, saved state and usage envelopes")


if __name__ == "__main__":
    run(sys.argv[1], sys.argv[2], Path(sys.argv[3]))
