#!/usr/bin/env python3
"""Exercise explicit binary policy and local consent on isolated real daemons."""

from contextlib import ExitStack
import json
from pathlib import Path
import sys
import tempfile

from fleet_sharing import Device
from isolation import check_environment, termination_cleanup
from terminal_window import require


def automatic(device, *arguments, success=True):
    result = device.command("update", "automatic", *arguments, "--json", success=success)
    return result, json.loads(result.stdout) if result.stdout else None


def saved_fleet(devices):
    members = [{"id": device.id, "endpoint": device.record()["endpoint"],
                "platform": {"os": "linux", "arch": "amd64"}} for device in devices]
    value = {"version": 1, "name": "isolated automatic fleet", "revision": 1,
             "members": members}
    for device in devices:
        path = device.config / "fleet.json"
        path.write_text(json.dumps(value))
        path.chmod(0o600)


def main(binary):
    check_environment()
    with termination_cleanup(), ExitStack() as cleanup:
        root = Path(cleanup.enter_context(tempfile.TemporaryDirectory(prefix="au-")))
        devices = []
        for directory, name in (("a", "automatic-one"), ("b", "automatic-two")):
            (root / directory).mkdir()
            device = Device(binary, root / directory, name)
            cleanup.callback(device.close)
            devices.append(device)
        coordinator, recipient = devices
        saved_fleet(devices)
        coordinator.save_connections(recipient)
        recipient.save_connections(coordinator)
        recipient.command("update", "trust", coordinator.id)
        for device in devices:
            device.start()
        _, absent = automatic(coordinator, "status")
        require(absent["record"] is None and absent["revision"] == 0, str(absent))
        require(absent["savedMembership"] and absent["pendingEpoch"] == 1, str(absent))
        _, enabled = automatic(coordinator, "enable", "--expected-revision", "0", "--yes")
        record = enabled["record"]
        require(record["policy"]["ownerId"] == coordinator.id, str(enabled))
        require(enabled["state"] == "waiting for enrollment confirmation", str(enabled))
        for device in devices:
            _, consent = automatic(device, "authorize", coordinator.id,
                                   "--fleet", str(device.config / "fleet.json"), "--epoch", "1",
                                   "--expected-revision", "0")
            require(consent["enabled"] and consent["membership"] == record["policy"]["membership"], str(consent))
        denied, _ = automatic(recipient, "revoke", "--expected-revision", "1",
                              "--coordinator", coordinator.id, success=False)
        require(denied.returncode != 0, "recipient consent accepted remote redirection")
        _, revoked = automatic(recipient, "revoke", "--expected-revision", "1")
        require(not revoked["enabled"] and revoked["revision"] == 2, str(revoked))
        stale, _ = automatic(coordinator, "disable", "--expected-revision", "0", success=False)
        require(stale.returncode != 0, "stale administrative decision succeeded")
        _, disabled = automatic(coordinator, "disable", "--expected-revision", "1")
        require(not disabled["record"]["policy"]["enabled"], str(disabled))
        human = coordinator.command("update", "automatic", "status").stdout.decode("utf-8")
        require("Automatic updates:" in human, human)
        require(record["policy"]["id"] not in human and coordinator.id not in human,
                "default automatic status exposed authority identifiers")
        structured = coordinator.command("update", "status", "--json")
        joined = json.loads(structured.stdout)
        require(joined["updates"]["sources"][0]["automatic"]["value"]["record"]["policy"]
                == disabled["record"]["policy"], str(joined))
        coordinator.stop_daemon()
        coordinator.start()
        _, reopened = automatic(coordinator, "status")
        require(reopened["record"]["policy"] == disabled["record"]["policy"], str(reopened))
        require(reopened["record"].get("current") is None, "development build admitted a release")
        for device in devices:
            require(not (device.local / "update" / "helper").exists(), "policy command installed a helper")
        print("PASS: automatic policy, explicit local consent, CAS and restart preserve fixture authority")


if __name__ == "__main__":
    main(sys.argv[1])
