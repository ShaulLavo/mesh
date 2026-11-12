"""Three native installed hosts using real authenticated predecessor evidence."""
import json
from datetime import datetime, timezone
import os
from pathlib import Path
import select
import signal
import shutil
import subprocess
import sys
import time

from fixtures import installed_accounts, installed_native, installed_origin, installed_paths, installed_releases


class Driver:
    def __init__(self, accounts, account, descriptor):
        self.account = account
        self.path = Path(account["home"]) / "driver.json"
        self.path.write_text(json.dumps(descriptor))
        os.chown(self.path, account["uid"], -1)
        self.log = (accounts.output / (account["role"] + "-driver.log")).open("wb")
        self.process = None
        script = accounts.source / "scripts/fixtures/installed_order_host.py"
        try:
            self.process = subprocess.Popen(accounts.arguments(account, [sys.executable, script, self.path]),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log, env=os.environ)
            os.set_blocking(self.process.stdout.fileno(), False)
        except BaseException:
            self.log.close()
            raise

    def call(self, operation, timeout=30, **fields):
        self.process.stdin.write(json.dumps({"operation": operation} | fields).encode() + b"\n")
        self.process.stdin.flush()
        deadline = time.monotonic() + timeout
        data = b""
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError("ordered host driver exited: " + self.account["role"])
            if select.select([self.process.stdout], [], [], min(1, max(0, deadline - time.monotonic())))[0]:
                data += os.read(self.process.stdout.fileno(), 65536)
            if b"\n" in data:
                return json.loads(data)
            if len(data) > 2 << 20:
                raise RuntimeError("ordered host response exceeds its bound")
        raise RuntimeError("ordered native host operation exceeded its bound: " + operation)

    def close(self):
        try:
            if self.process and self.process.poll() is None:
                self.call("close")
                self.process.wait(timeout=10)
        finally:
            self.stop_driver()
            self.log.close()

    def stop_driver(self):
        if self.process is None or self.process.poll() is not None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=10)


def prepare(releases, origin, source):
    first, failed, latest = installed_releases.VERSIONS
    registry = releases.registry
    releases.registry = None
    releases.build(first)
    releases.publish(first, 0, [releases.prove(source, first)])
    releases.registry = registry
    for ordinal, version in enumerate((failed, latest), 1):
        releases.build(version)
        releases.publish(version, ordinal, [releases.prove(releases.builds[first]["path"], version)])
    origin.serve_tree(releases.directory)
    origin.head(releases.directory, first)


def run(root, output, descriptor):
    installed_accounts.require_hosted_root()
    installed_paths.validate(root)
    accounts, drivers = installed_accounts.Accounts(root, output), []
    record = {"passed": False, "supplementalSourceBuilds": True, "observations": []}
    try:
        accounts.create()
        share_inputs(accounts, descriptor)
        for account in accounts.accounts:
            value = descriptor | {"root": account["home"], "output": str(output), "previous": None,
                "host": descriptor["host"] | {"name": account["role"]}}
            drivers.append(Driver(accounts, account, value))
        bootstrap(drivers, record)
        scenarios(root, drivers, descriptor, record)
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        teardown(accounts, drivers, record, output)


def share_inputs(accounts, descriptor):
    command = installed_accounts.command
    source = accounts.prepare_sources()
    command(["/usr/bin/chgrp", accounts.group, str(accounts.output)])
    accounts.output.chmod(0o2770)
    for name in ("proxy.json", "fixture-ca.pem"):
        path = accounts.root / name
        command(["/usr/bin/chgrp", accounts.group, str(path)])
        path.chmod(0o640)
    if not Path(descriptor["preparer"]).is_file():
        raise RuntimeError("ordered hosts lack the production helper preparation tool")
    preparer = source / "native-helper"
    shutil.copy2(descriptor["preparer"], preparer)
    descriptor["preparer"] = str(preparer)


def bootstrap(drivers, record):
    peers = [driver.call("prepare") for driver in drivers]
    members = [{key: peer[key] for key in ("id", "endpoint", "platform")} for peer in peers]
    for index, driver in enumerate(drivers):
        after = [] if index == 0 else [peers[index - 1]["id"]]
        driver.call("configure", peers=members, after=after, soak=5_000_000_000)
    for driver in drivers:
        result = driver.call("adopt", timeout=120, version=installed_releases.VERSIONS[0])
        record.setdefault("adopted", {})[driver.account["role"]] = result
    record["peers"] = members


def scenarios(root, drivers, descriptor, record):
    first, failed, latest = installed_releases.VERSIONS
    canary, sleeping, burst = drivers
    origin = installed_origin.Origin(root)
    directory = Path(descriptor["releases"]["directory"])
    sleeping.call("stop")
    canary.call("fault", versions=[failed])
    origin.head(directory, failed)
    restart(canary)
    failed_state = await_version(canary, first, "rolled_back", 90)
    if not any(edge["version"] == failed for edge in failed_state["failed"]):
        raise RuntimeError("failed native canary did not retain its failed edge")
    restart(burst)
    held = await_held(burst, first, 45)
    record["observations"].append({"canaryFailure": bounded(failed_state), "heldDependent": bounded(held)})
    origin.head(directory, latest)
    canary.call("fault", versions=[])
    restart(canary)
    canary_latest = await_version(canary, latest, "committed", 90)
    # The intermediate host remains asleep while the real canary advances.
    restart(burst)
    still_held = await_held(burst, first, 45)
    wait_soak(canary, canary_latest, latest)
    sleeping.call("start")
    sleeper_latest = await_version(sleeping, latest, "committed", 90)
    wait_soak(sleeping, sleeper_latest, latest)
    restart(burst)
    burst_latest = await_version(burst, latest, "committed", 90)
    verify_order(canary_latest, sleeper_latest, burst_latest, latest)
    record["observations"].append({"canaryLatest": bounded(canary_latest), "sleepHeld": bounded(still_held),
        "sleepLatest": bounded(sleeper_latest), "burstLatest": bounded(burst_latest)})


def restart(driver):
    driver.call("stop")
    driver.call("start")


def await_version(driver, version, phase, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        journal = value.get("journal") or {}
        current = journal.get("request", {}).get("manifest", {}).get("version")
        ready = current == version if phase == "committed" else current == installed_releases.VERSIONS[1]
        settled = ready and journal.get("phase") == phase and not value["state"].get("owned")
        observed = driver.call("inspect") if settled else {}
        if observed.get("ready") and observed["build"]["version"] == version:
            return observed
        time.sleep(0.1)
    raise RuntimeError("ordered host did not reach its native image/journal bound: " + driver.account["role"])


def await_held(driver, version, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        held = value["state"]["phase"] == "waiting" and not value["state"].get("owned")
        observed = driver.call("inspect") if held else {}
        if observed.get("ready") and observed["build"]["version"] == version:
            return observed
        time.sleep(0.1)
    raise RuntimeError("ordered dependent did not hold its original native image")


def bounded(value):
    return {key: value[key] for key in ("build", "state", "entries", "failed", "helper", "peers", "dependents")}


def commit_time(value, version):
    entry = next(entry for entry in value["entries"] if entry["version"] == version)
    return datetime.fromisoformat(entry["committedAt"].replace("Z", "+00:00"))


def wait_soak(driver, value, version):
    committed = commit_time(value, version)
    deadline = time.monotonic() + 6
    while (datetime.now(timezone.utc) - committed).total_seconds() < 5:
        if time.monotonic() > deadline:
            raise RuntimeError("ordered native soak clock did not advance")
        driver.call("inspect")
        time.sleep(0.1)


def verify_order(canary, sleeper, burst, version):
    entries = [next(entry for entry in value["entries"] if entry["version"] == version) for value in (canary, sleeper, burst)]
    if len({entry["metadata"] for entry in entries}) != 1 or any(entry["workersPreserved"] != entry["workers"] for entry in entries):
        raise RuntimeError("ordered native hosts disagree on their exact pinned release or preserved workers")
    times = [commit_time(value, version) for value in (canary, sleeper, burst)]
    if times != sorted(times) or len(set(times)) != 3:
        raise RuntimeError("native predecessor commits did not occur in policy order")
    if any((times[index + 1] - times[index]).total_seconds() < 5 for index in range(2)):
        raise RuntimeError("native dependent committed before its actual predecessor soak")
    if any(entry["version"] == installed_releases.VERSIONS[1] for value in (sleeper, burst) for entry in value["entries"]):
        raise RuntimeError("dependent adopted the failed canary release")
    if not canary["dependents"] or not sleeper["peers"] or not sleeper["dependents"] or not burst["peers"]:
        raise RuntimeError("ordered hosts lack actual authenticated predecessor/consumer observations")


def teardown(accounts, drivers, record, output):
    actions = [driver.close for driver in reversed(drivers)]
    actions.append(accounts.close)
    try:
        installed_native.cleanup_all(actions)
        record["cleanup"] = {"accountsAbsent": True, "servicesAbsent": True, "processesAbsent": True}
    finally:
        (output / "ordered-native.json").write_text(json.dumps(record, indent=2) + "\n")


def interrupted(number, _frame):
    raise InterruptedError("ordered native proof interrupted by signal " + str(number))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    descriptor_path, destination = map(Path, sys.argv[1:])
    run(descriptor_path.parent, destination, json.loads(descriptor_path.read_text()))
