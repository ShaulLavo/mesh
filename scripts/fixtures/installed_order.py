"""Three native installed hosts using real authenticated predecessor evidence."""
import json
from datetime import datetime, timedelta, timezone
import os
from pathlib import Path
import select
import signal
import shutil
import subprocess
import sys
import time

from fixtures import installed_accounts, installed_native, installed_origin, installed_paths, installed_releases, installed_account_run


class Driver:
    def __init__(self, accounts, account, descriptor):
        self.accounts, self.account = accounts, account
        self.path = Path(account["home"]) / "driver.json"
        self.path.write_text(json.dumps(descriptor))
        os.chown(self.path, account["uid"], -1)
        self.log = (accounts.output / (account["role"] + "-driver.log")).open("wb")
        self.process = None
        script = accounts.source / "scripts/fixtures/installed_order_host.py"
        try:
            self.process = subprocess.Popen(accounts.arguments(account, [sys.executable, script, self.path]),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log,
                env=os.environ | {"PYTHONPATH": str(accounts.source / "scripts")})
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
        installed_native.cleanup_all([self.close_host, self.stop_driver, self.retain_logs, self.log.close])

    def close_host(self):
        if self.process and self.process.poll() is None:
            self.call("close")
            self.process.wait(timeout=10)

    def retain_logs(self):
        if self.process and self.process.poll() is None:
            raise RuntimeError("ordered host driver still consumes its evidence")
        accounts, account = self.accounts, self.account
        accounts.validate_resources()
        accounts.owned(account)
        root = Path(account["home"]) / (account["role"] + "-ordered")
        script = accounts.source / "scripts/fixtures/installed_order_host.py"
        result = accounts.run(account, [sys.executable, script, "--logs", root],
            capture_output=True, text=True, timeout=10, env=os.environ | {"PYTHONPATH": str(accounts.source / "scripts")})
        if result.returncode:
            raise RuntimeError("ordered account evidence export failed: " + account["role"] + ": " + result.stderr[-4096:])
        publish_logs(result.stdout, accounts.output, account["role"] + "-ordered")

    def stop_driver(self):
        if self.process is None or self.process.poll() is not None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=10)


def publish_logs(payload, output, name):
    if len(payload) > 2 << 20:
        raise RuntimeError("ordered account evidence exceeds its transfer bound")
    files = json.loads(payload)
    allowed = {"preserved-before.json", "preserved-after.json", "route-observation.json", "daemon.log", "daemon.err"}
    if not isinstance(files, dict) or not files.keys() <= allowed:
        raise RuntimeError("ordered account evidence contains an unexpected file")
    decoded = {filename: bytes.fromhex(data) for filename, data in files.items()}
    for filename, data in decoded.items():
        (output / (name + "-" + filename)).write_bytes(data)


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


def run(root, output, descriptor, operation):
    installed_accounts.require_hosted_root()
    installed_paths.validate(root)
    installed_account_run.claim(root, output, operation)
    accounts, drivers = installed_accounts.Accounts(root, output), []
    record = {"passed": False, "supplementalSourceBuilds": True, "observations": []}
    try:
        accounts.create()
        share_inputs(accounts, descriptor)
        for account in accounts.accounts:
            value = descriptor | {"root": account["home"], "previous": None,
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
    for driver, peer in zip(drivers, peers, strict=True):
        driver.account["id"] = peer["id"]
    members = [{key: peer[key] for key in ("id", "endpoint", "platform")} for peer in peers]
    # Keep the canary's direct dependent awake; sleep is the final host.
    predecessors = {"canary": None, "burst": 0, "sleep": 2}
    for driver in drivers:
        predecessor = predecessors[driver.account["role"]]
        after = [] if predecessor is None else [peers[predecessor]["id"]]
        configured = driver.call("configure", peers=members, after=after, soak=5_000_000_000)
        record.setdefault("baselineInitialization", {})[driver.account["role"]] = configured["baselineInitialization"]
    for driver in drivers:
        result = driver.call("adopt", timeout=120, version=installed_releases.VERSIONS[0])
        record.setdefault("adopted", {})[driver.account["role"]] = result
    record["peers"] = members


def scenarios(root, drivers, descriptor, record):
    first, failed, latest = installed_releases.VERSIONS
    canary, sleeping, direct = drivers
    origin = installed_origin.Origin(root)
    directory = Path(descriptor["releases"]["directory"])
    sleeping.call("stop", expectedOffline=True)
    canary.call("fault", versions=[failed])
    origin.head(directory, failed)
    restart(canary)
    failed_state = await_version(canary, first, "rolled_back", 90)
    if not any(edge["version"] == failed for edge in failed_state["failed"]):
        raise RuntimeError("failed native canary did not retain its failed edge")
    started = opportunity(direct)
    held = await_opportunity(direct, first, failed, started, 45)
    require_failed_canary_read(held, failed_state, canary.account["id"])
    record["observations"].append({"canaryFailure": bounded(failed_state), "awakeDirectHold": bounded(held)})
    origin.head(directory, latest)
    canary.call("fault", versions=[])
    restart(canary)
    canary_latest = await_version(canary, latest, "committed", 90)
    direct_latest, direct_hold = exercise_soak(direct, canary_latest, first, latest)
    sleeper_latest, sleeper_hold = exercise_soak(sleeping, direct_latest, first, latest)
    verify_order(canary_latest, direct_latest, sleeper_latest, latest)
    record["observations"].append({"canaryLatest": bounded(canary_latest), "directBeforeSoak": bounded(direct_hold),
        "directLatest": bounded(direct_latest), "sleepBeforeSoak": bounded(sleeper_hold), "sleepLatest": bounded(sleeper_latest)})


def restart(driver):
    driver.call("stop")
    driver.call("start")


def opportunity(driver):
    started = datetime.now(timezone.utc)
    driver.call("stop")
    driver.call("start", expectedOffline=False)
    return started


def assert_held(value, source, target):
    journal = value.get("journal") or {}
    attempted = journal.get("request", {}).get("manifest", {}).get("version") == target
    if attempted or value["state"].get("owned") or value.get("build", {}).get("version", source) != source:
        raise RuntimeError("dependent issued or activated the release before its predecessor gate permitted it")


def fresh_opportunity(value, started):
    reads = [datetime.fromisoformat(peer["readAt"].replace("Z", "+00:00")) for peer in value["peers"] if peer.get("state") == "verified"]
    written = datetime.fromtimestamp(value.get("stateWrittenAt", 0), timezone.utc)
    return bool(reads) and max(reads) >= started and written >= started and value["state"]["phase"] == "waiting"


def await_opportunity(driver, source, target, started, timeout, cutoff=None):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        assert_held(value, source, target)
        if fresh_opportunity(value, started):
            return require_preexpiry_read(value, cutoff) | {"opportunityStartedAt": started.isoformat()}
        if cutoff is not None and datetime.now(timezone.utc) >= cutoff:
            raise RuntimeError("dependent did not demonstrate a completed update opportunity before its configured soak expired")
        time.sleep(0.05)
    raise RuntimeError("awake dependent did not complete a fresh predecessor-gated update opportunity")


def require_preexpiry_read(value, cutoff):
    if cutoff is not None and datetime.now(timezone.utc) >= cutoff:
        raise RuntimeError("soak hold was only observed after expiry")
    return value


def require_failed_canary_read(held, canary, canary_id):
    failures = held.get("peerFailures", {}).get(canary_id, [])
    expected = canary["failed"]
    if not expected or not all(entry in failures for entry in expected):
        raise RuntimeError("awake direct dependent did not retain the authenticated failed-canary evidence")


def exercise_soak(driver, predecessor, source, target):
    cutoff = commit_time(predecessor, target) + timedelta(seconds=5)
    if datetime.now(timezone.utc) >= cutoff:
        raise RuntimeError("fixture missed the pre-expiry opportunity; no soak proof recorded")
    started = opportunity(driver)
    held = await_opportunity(driver, source, target, started, 45, cutoff)
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        if datetime.now(timezone.utc) < cutoff:
            assert_held(value, source, target)
        if target_committed(value, target):
            return validated_soaked_commit(driver, target, cutoff), held
        started = repeat_opportunity(driver, value, started)
        time.sleep(0.1)
    raise RuntimeError("dependent did not release after soak despite real update opportunities on both sides of expiry")


def target_committed(value, target):
    journal = value.get("journal") or {}
    return journal.get("phase") == "committed" and journal.get("request", {}).get("manifest", {}).get("version") == target and not value["state"].get("owned")


def repeat_opportunity(driver, value, started):
    if fresh_opportunity(value, started) and not value["state"].get("owned"):
        return opportunity(driver)
    return started


def validated_soaked_commit(driver, target, cutoff):
    observed = driver.call("inspect")
    if commit_time(observed, target) < cutoff:
        raise RuntimeError("dependent ignored the configured predecessor soak")
    return observed


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
    return {key: value[key] for key in ("state", "entries", "failed", "peers", "peerFailures", "stateWrittenAt")}


def commit_time(value, version):
    entry = next(entry for entry in value["entries"] if entry["version"] == version)
    return datetime.fromisoformat(entry["committedAt"].replace("Z", "+00:00"))


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
    except BaseException as error:
        record["passed"] = False
        record["cleanupError"] = str(error)
        primary = record.get("error", "ordered native teardown failed")
        raise RuntimeError(primary + "; cleanup failed: " + str(error)) from error
    finally:
        (output / "ordered-native.json").write_text(json.dumps(record, indent=2) + "\n")


def interrupted(number, _frame):
    raise InterruptedError("ordered native proof interrupted by signal " + str(number))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    descriptor_path, destination = map(Path, sys.argv[1:3])
    run(descriptor_path.parent, destination, json.loads(descriptor_path.read_text()), sys.argv[3])
