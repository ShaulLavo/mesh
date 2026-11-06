#!/usr/bin/env python3
"""Signed fleet publication through three isolated, authenticated real daemons."""

from contextlib import ExitStack
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import shutil
import sys

from fleet_sharing import Device
from isolation import check_environment, termination_cleanup
from mesh_control import request_id, round_trip
from terminal_window import eventually, require


def save_json(path, value):
    path.write_text(json.dumps(value))
    path.chmod(0o600)


def saved_fleet(device, *members):
    save_json(device.config / "fleet.json", {
        "version": 1, "name": "fixture", "revision": 1,
        "members": [{"id": member.id, "endpoint": member.record()["endpoint"]}
                    for member in members],
    })


def envelope(device, *arguments, allow_partial=False):
    result = device.command("profile", *arguments, success=False)
    require(not result.stderr, f"JSON command wrote diagnostics: {result.stderr!r}")
    value = json.loads(result.stdout)
    if result.returncode == 3 and allow_partial:
        require(value["status"] == "partial" and value["data"] is not None, str(value))
        require(value["errors"] and all(error["code"] == "state.unavailable"
                                       and error["target"] == "fleet/outbox.json"
                                       for error in value["errors"]), str(value))
        return value["data"]
    require(result.returncode == 0 and value["status"] == "ok" and value["errors"] == [], str(value))
    return value["data"]


def write_profile(device, expected, text):
    path = device.root / "input.json"
    save_json(path, {"schemaVersion": 1, "instructions": text})
    return envelope(device, "set", "--fleet", "--file", str(path),
                    "--expected-revision", str(expected), "--json")


def status(device):
    return envelope(device, "status", "--fleet", "--json", allow_partial=True)


def member_state(device, member, state, code=None):
    rows = status(device)["members"]
    row = next((row for row in rows if row["hostId"] == member.id), None)
    if row is None or row["state"] != state:
        return False
    return code is None or row.get("lastError", {}).get("code") == code


def replica_at(device, revision):
    replica = status(device)["replica"]
    return replica["state"] == "valid" and replica["revision"] == revision


def local_sync(device):
    value = round_trip(str(device.local / "daemon.sock"),
                       {"type": "fleet.profile.sync", "requestId": request_id("fleet-sync")})
    require(value["type"] == "ok", str(value))


def freeze_notice(device):
    directory = device.local / "update-notice"
    directory.mkdir(mode=0o700)
    save_json(directory / "notice.json", {
        "nextAttempt": (datetime.now(timezone.utc) + timedelta(days=1)).isoformat(),
    })


def local_cases(a):
    first = write_profile(a, 0, "local notes")
    require(first["committed"] and first["queued"] and not first["fleetSaved"], str(first))
    require(first["members"] == [] and first["revision"] == 1, str(first))
    retry = write_profile(a, 0, "local notes")
    require(not retry["changed"] and retry["revision"] == 1, str(retry))
    saved_fleet(a, a)
    second = write_profile(a, 1, "fleet notes")
    require(second["fleetSaved"] and second["members"] == [], str(second))
    require(status(a)["fleetSaved"], "self-only saved fleet treated as missing")
    require(not (a.local / "daemon.sock").exists(), "local edit started a daemon")


def offline_relay_cases(a, b, c):
    saved_fleet(a, a, b)
    saved_fleet(b, a, b, c)
    saved_fleet(c, c)
    a.command("update", "trust", b.id)
    b.command("update", "trust", a.id)
    c.command("update", "trust", b.id)
    a.start()
    c.start()
    result = write_profile(a, 2, "shared fleet notes")
    require(result["revision"] == 3, str(result))
    eventually(lambda: member_state(a, b, "pending", "host.unreachable"),
               "offline member did not remain pending")
    require(not (b.local / "daemon.sock").exists(), "delivery started offline daemon")
    a.stop_daemon()
    b.start()
    a.start()
    eventually(lambda: replica_at(b, 3), "restart did not deliver the durable outbox")
    eventually(lambda: member_state(a, b, "delivered"), "durable acceptance did not settle sender")
    eventually(lambda: member_state(b, c, "pending", "fleet.signer_untrusted"),
               "trusted relay did not expose untrusted document author")
    c.command("update", "trust", a.id)
    envelope(b, "status", "--fleet", "--retry", "--json")
    eventually(lambda: replica_at(c, 3), "explicit retry did not deliver after author trust")
    eventually(lambda: member_state(b, c, "delivered"), "relay did not settle durable acceptance")


def divergence_cases(a, b, c):
    a.stop_daemon()
    b.stop_daemon()
    left = write_profile(a, 3, "writer a")
    right = write_profile(b, 3, "writer b")
    require(left["revision"] == right["revision"] == 4, "distributed writers did not each allocate N+1")
    require(left["digest"] != right["digest"], "distinct writes have equal digests")
    b.start()
    a.start()
    local_sync(b)
    eventually(lambda: member_state(a, b, "conflict", "fleet.revision_conflict"),
               "same-revision divergence was not visible")
    require(status(a)["replica"]["digest"] == left["digest"], "member conflict replaced local replica")
    final = write_profile(a, 4, "owner resolves divergence")
    require(final["revision"] == 5, str(final))
    eventually(lambda: replica_at(b, 5) and replica_at(c, 5), "later owner revision did not converge")
    eventually(lambda: member_state(a, b, "delivered") and member_state(b, c, "delivered"),
               "convergence lacked durable member acknowledgements")
    require(status(b)["replica"]["digest"] == final["digest"], "B did not converge")
    require(status(c)["replica"]["digest"] == final["digest"], "C did not converge")


def campaign(binary, root, cleanup_state):
    devices = []
    try:
        with ExitStack() as cleanup:
            for name in ("alpha", "bravo", "charlie"):
                path = root / name
                path.mkdir(mode=0o700)
                device = Device(binary, path, name)
                devices.append(device)
                cleanup.callback(close_device, device, cleanup_state)
                freeze_notice(device)
            a, b, c = devices
            local_cases(a)
            offline_relay_cases(a, b, c)
            divergence_cases(a, b, c)
            for device in devices:
                require(not any((device.local / "s").glob("*/meta.json")), "profile delivery created a session")
                require(not any((device.local / "wake").rglob("*.cooldown")),
                        "profile delivery reserved a wake transmission")
    except BaseException:
        for device in devices:
            log = device.root / "daemon.log"
            if log.exists():
                print(f"{device.name} daemon log:\n{log.read_text()}", file=sys.stderr)
            for name in ("profile.json", "outbox.json"):
                path = device.local / "fleet" / name
                if path.exists():
                    print(f"{device.name} {name}: {path.read_text()}", file=sys.stderr)
        raise
    print("PASS: signed fleet notes, local and saved-self distinction, offline restart catch-up, trusted relay rejection, explicit retry and distributed divergence without wake or sessions")


def close_device(device, cleanup_state):
    try:
        device.close()
    except BaseException:
        cleanup_state["joined"] = False
        raise


def main():
    check_environment()
    root = Path(os.environ["HOME"]) / "fleet-profile"
    root.mkdir(mode=0o700)
    cleanup_state = {"joined": True}
    try:
        with termination_cleanup():
            campaign(str(Path(sys.argv[1]).resolve()), root, cleanup_state)
    finally:
        if cleanup_state["joined"]:
            shutil.rmtree(root)
        else:
            print(f"Fleet profile cleanup did not join all consumers. Retained {root}", file=sys.stderr)


if __name__ == "__main__":
    main()
