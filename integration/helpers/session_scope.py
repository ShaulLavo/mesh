#!/usr/bin/env python3
"""Observe exactly one session unit through the integration's preserved user bus."""

import json
import os
import subprocess
import sys


SIGNATURE = "a(ssssssouso)"


def unit_state(reply, unit):
    if not isinstance(reply, dict) or reply.get("type") != SIGNATURE:
        raise ValueError("unexpected systemd unit reply signature")
    data = reply.get("data")
    if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], list) or len(data[0]) != 1:
        raise ValueError("systemd did not return exactly the requested unit")
    record = data[0][0]
    if not isinstance(record, list) or len(record) != 10 or record[0] != unit:
        raise ValueError("systemd returned an unexpected unit record")
    load, active = record[2:4]
    if not isinstance(load, str) or not isinstance(active, str):
        raise ValueError("systemd returned non-text unit states")
    if load == "not-found" and active == "inactive":
        return "not-found"
    if load != "loaded" or active not in {"active", "reloading", "refreshing", "activating", "deactivating", "inactive"}:
        raise ValueError(f"unexpected systemd unit state: load={load!r}, active={active!r}")
    return active


def observe(unit):
    address = os.environ.get("DBUS_SESSION_BUS_ADDRESS")
    if not address:
        raise ValueError("the preserved user bus address is missing")
    # systemctl's local user transport uses the private XDG_RUNTIME_DIR, not this bus.
    command = ["busctl", "--address=" + address, "--json=short", "call", "org.freedesktop.systemd1",
               "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", "ListUnitsByNames", "as", "1", unit]
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode:
        raise ValueError(f"systemd query exited {result.returncode}: {result.stderr.strip()}")
    return unit_state(json.loads(result.stdout), unit)


def main():
    unit = sys.argv[1]
    try:
        print(observe(unit))
        return 0
    except (OSError, ValueError) as error:
        print(f"FAIL: observe {unit}: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
