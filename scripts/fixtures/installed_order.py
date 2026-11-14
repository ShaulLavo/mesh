"""Three native installed hosts using real authenticated predecessor evidence."""
import json
from concurrent.futures import Future, ThreadPoolExecutor
from datetime import datetime, timezone
from functools import partial
import os
from pathlib import Path
import re
import select
import signal
import shutil
import subprocess
import sys
import time

from fixtures import installed_accounts, installed_native, installed_origin, installed_paths, installed_releases, installed_account_run, installed_listener_evidence, installed_clock
from fixtures import installed_order_trace as order_trace

SOAK_NS = 5_000_000_000
EPOCH = datetime(1970, 1, 1, tzinfo=timezone.utc)
RFC3339 = re.compile(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})\Z")


class Driver:
    def __init__(self, accounts, account, descriptor):
        self.accounts, self.account = accounts, account
        order_trace.attach(self)
        self.path = Path(account["home"]) / "driver.json"
        self.path.write_text(json.dumps(descriptor))
        os.chown(self.path, account["uid"], -1)
        self.log = (accounts.output / (account["role"] + "-driver.log")).open("wb")
        self.process = None
        self.fixture_deadline_ns = descriptor["fixtureDeadlineNs"]
        self.cleanup_deadline_ns = descriptor["fixtureCleanupDeadlineNs"]
        script = accounts.source / "scripts/fixtures/installed_order_host.py"
        try:
            self.process = subprocess.Popen(accounts.arguments(account, [sys.executable, script, self.path]),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log,
                env=os.environ | {"PYTHONPATH": str(accounts.source / "scripts")})
            os.set_blocking(self.process.stdout.fileno(), False)
            os.set_blocking(self.process.stdin.fileno(), False)
        except BaseException:
            self.log.close()
            raise

    @order_trace.calls
    def call(self, operation, timeout=30, **fields):
        parent = self.cleanup_deadline_ns if operation == "close" else self.fixture_deadline_ns
        deadline_ns = min(parent, installed_clock.parent_deadline(timeout))
        installed_clock.remaining_until(deadline_ns, timeout)
        payload = json.dumps(fields | {"operation": operation, "fixtureDeadlineNs": deadline_ns}).encode() + b"\n"
        self.write_request(payload, deadline_ns, operation)
        data = b""
        while time.monotonic_ns() < deadline_ns:
            returncode = self.process.poll()
            if returncode is not None:
                error = RuntimeError("ordered host driver exited: " + self.account["role"])
                order_trace.driver_exit(self, error, operation, returncode, deadline_ns)
                raise error
            if select.select([self.process.stdout], [], [], installed_clock.remaining_until(deadline_ns, 1))[0]:
                data += os.read(self.process.stdout.fileno(), 65536)
            if len(data) > 2 << 20:
                raise RuntimeError("ordered host response exceeds its bound")
            if b"\n" in data:
                return json.loads(data)
        raise RuntimeError("ordered native host operation exceeded its bound: " + operation)

    def write_request(self, payload, deadline_ns, operation):
        pending = memoryview(payload)
        first = True
        while pending:
            if not select.select([], [self.process.stdin], [], installed_clock.remaining_until(deadline_ns, 1))[1]:
                continue
            if first:
                order_trace.event(self, "request.send", operation=operation, fixtureDeadlineNs=deadline_ns)
                first = False
            pending = pending[os.write(self.process.stdin.fileno(), pending):]

    def close(self):
        installed_native.cleanup_all([self.close_host, self.stop_driver, self.retain_logs, self.log.close])

    def close_host(self):
        if self.process and self.process.poll() is None:
            self.call("close")
            self.process.wait(timeout=installed_clock.timeout(10))

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
        publish_logs(result.stdout, accounts.output, account["role"] + "-ordered", order_trace.trace(self))

    def stop_driver(self):
        if self.process is None or self.process.poll() is not None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=installed_clock.timeout(10))
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=installed_clock.timeout(10))


def publish_logs(payload, output, name, diagnostics=None):
    if len(payload) > 2 << 20:
        raise RuntimeError("ordered account evidence exceeds its transfer bound")
    files = json.loads(payload)
    allowed = {"preserved-before.json", "preserved-after.json", "route-observation.json", "daemon.log", "daemon.err", installed_listener_evidence.FAILURE} | order_trace.ACCOUNT_FILES
    if not isinstance(files, dict) or not files.keys() <= allowed:
        raise RuntimeError("ordered account evidence contains an unexpected file")
    decoded = {filename: bytes.fromhex(data) for filename, data in files.items()}
    for filename, data in decoded.items():
        if filename in order_trace.ACCOUNT_FILES:
            order_trace.write_diagnostic(data, output / (name + "-" + filename), diagnostics)
            continue
        (output / (name + "-" + filename)).write_bytes(data)
    order_trace.export_status(decoded, diagnostics)


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
    accounts, drivers = None, []
    record = {"passed": False, "supplementalSourceBuilds": True, "observations": []}
    try:
        accounts = installed_accounts.Accounts(root, output)
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
        for driver in drivers:
            order_trace.failure(driver, error, "run", "parent-selected")
        record["error"] = str(error)
        latch_failure(root, descriptor, error)
        raise
    finally:
        if accounts is not None:
            teardown(accounts, drivers, record, output, lambda error: latch_failure(root, descriptor, error))


def latch_failure(root, descriptor, error):
    deadline_ns = installed_account_run.owner_failure(root, error, descriptor["fixtureEmergencyNs"])
    installed_clock.failure_cleanup_handoff(deadline_ns)


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
    canary_latest, direct_latest, direct_hold, sleeper_latest, sleeper_hold = release_latest(origin, directory, drivers, record)
    # A successor can authenticate after its predecessor's commit snapshot.
    # Both controller streams have drained before these final ledger reads.
    canary_latest = canary.call("inspect", active=False)
    direct_latest = direct.call("inspect", active=False)
    verify_order(canary_latest, direct_latest, sleeper_latest, latest)
    record["observations"].append({"canaryLatest": bounded(canary_latest), "directBeforeSoak": bounded(direct_hold),
        "directLatest": bounded(direct_latest), "sleepBeforeSoak": bounded(sleeper_hold), "sleepLatest": bounded(sleeper_latest)})


def release_latest(origin, directory, drivers, record):
    for driver in drivers:
        order_trace.attach(driver)
    try:
        return release_latest_owned(origin, directory, drivers, record)
    finally:
        order_trace.summaries(drivers, record)


def release_latest_owned(origin, directory, drivers, record):
    first, _, latest = installed_releases.VERSIONS
    canary, sleeping, direct = drivers
    canary_commit, direct_commit = Future(), Future()
    direct_armed, sleeper_armed = Future(), Future()
    # Each controller owns one driver stream until every inspection has finished.
    with ThreadPoolExecutor(max_workers=2, thread_name_prefix="ordered-opportunity") as controllers:
        try:
            direct_result = controllers.submit(soak_controller, direct, canary_commit, direct_commit, direct_armed, first, latest)
            sleeper_result = controllers.submit(soak_controller, sleeping, direct_commit, None, sleeper_armed, first, latest)
            record["opportunityControllers"] = [direct_armed.result(), sleeper_armed.result()]
            record["latestReleaseStartedAt"] = datetime.now(timezone.utc).isoformat()
            origin.head(directory, latest)
            order_trace.event(canary, "exposure.reply")
            canary.call("fault", versions=[])
            restart(canary)
            canary_latest = await_version(canary, latest, "committed", 90, partial(order_trace.publish, canary, canary_commit))
            order_trace.event(canary, "direct.collect.begin")
            direct_latest, direct_hold = direct_result.result()
            order_trace.event(canary, "direct.collect.end")
            order_trace.event(canary, "sleeper.collect.begin")
            sleeper_latest, sleeper_hold = sleeper_result.result()
            order_trace.event(canary, "sleeper.collect.end")
            return canary_latest, direct_latest, direct_hold, sleeper_latest, sleeper_hold
        except BaseException as error:
            order_trace.failure(canary, error, "release_latest", "parent-selected")
            fail_pending(canary_commit, error)
            raise


def soak_controller(driver, predecessor, successor, armed, source, target):
    phase = "arming"
    try:
        observed = driver.call("inspect", active=False)
        armed.set_result({"host": driver.account["role"], "id": driver.account["id"], "armedAt": datetime.now(timezone.utc).isoformat(),
                          "observation": bounded(observed)})
        committed = partial(order_trace.publish, driver, successor) if successor is not None else None
        phase = "predecessor.wait"
        observed = predecessor.result()
        order_trace.event(driver, "future.wake")
        phase = "soak"
        return exercise_soak(driver, observed, source, target, committed)
    except BaseException as error:
        kind = "propagated" if phase == "predecessor.wait" else "producer"
        order_trace.failure(driver, error, phase, kind)
        fail_pending(armed, error)
        fail_pending(successor, error)
        raise


def fail_pending(future, error):
    if future is not None and not future.done():
        future.set_exception(error)


def restart(driver):
    driver.call("stop")
    driver.call("start")


def opportunity(driver):
    started = datetime.now(timezone.utc)
    order_trace.event(driver, "opportunity.begin")
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
        if cutoff is not None and time.time_ns() >= cutoff:
            raise RuntimeError("dependent did not demonstrate a completed update opportunity before its configured soak expired")
        time.sleep(0.05)
    raise RuntimeError("awake dependent did not complete a fresh predecessor-gated update opportunity")


def require_preexpiry_read(value, cutoff):
    if cutoff is not None and time.time_ns() >= cutoff:
        raise RuntimeError("soak hold was only observed after expiry")
    return value


def require_failed_canary_read(held, canary, canary_id):
    failures = held.get("peerFailures", {}).get(canary_id, [])
    expected = canary["failed"]
    if not expected or not all(entry in failures for entry in expected):
        raise RuntimeError("awake direct dependent did not retain the authenticated failed-canary evidence")


def exercise_soak(driver, predecessor, source, target, committed=None):
    cutoff = commit_time_ns(predecessor, target) + SOAK_NS
    order_trace.cutoff(driver, predecessor, target, cutoff)
    if time.time_ns() >= cutoff:
        order_trace.event(driver, "guard.expired")
        raise RuntimeError("fixture missed the pre-expiry opportunity; no soak proof recorded")
    order_trace.event(driver, "guard.open")
    started = opportunity(driver)
    held = await_opportunity(driver, source, target, started, 45, cutoff)
    order_trace.event(driver, "hold.completed", peers=held["peers"], stateWrittenAt=held["stateWrittenAt"])
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        if time.time_ns() < cutoff:
            assert_held(value, source, target)
        if target_committed(value, target):
            publish_soaked_commit(value, target, cutoff, committed)
            return validated_soaked_commit(driver, target, cutoff), held
        started = repeat_opportunity(driver, value, started)
        time.sleep(0.1)
    raise RuntimeError("dependent did not release after soak despite real update opportunities on both sides of expiry")


def publish_soaked_commit(value, target, cutoff, committed):
    if commit_time_ns(value, target) < cutoff:
        raise RuntimeError("dependent ignored the configured predecessor soak")
    if committed is not None:
        committed(value)


def target_committed(value, target):
    return committed_entry(value, target) is not None


def committed_entry(value, target):
    journal = value.get("journal") or {}
    request = journal.get("request") or {}
    if journal.get("phase") != "committed" or request.get("manifest", {}).get("version") != target or value["state"].get("owned"):
        return None
    if not request.get("id") or type(request.get("generation")) is not int or request["generation"] <= 0:
        return None
    matches = [entry for entry in value["entries"] if entry.get("version") == target
        and entry.get("request") == request["id"] and entry.get("generation") == request["generation"]]
    return matches[0] if len(matches) == 1 else None


def repeat_opportunity(driver, value, started):
    if fresh_opportunity(value, started) and not value["state"].get("owned"):
        return opportunity(driver)
    return started


def validated_soaked_commit(driver, target, cutoff):
    observed = inspect_full(driver)
    if commit_time_ns(observed, target) < cutoff:
        raise RuntimeError("dependent ignored the configured predecessor soak")
    return observed


def await_version(driver, version, phase, timeout, committed=None):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = driver.call("inspect", active=False)
        journal = value.get("journal") or {}
        current = journal.get("request", {}).get("manifest", {}).get("version")
        ready = current == version if phase == "committed" else current == installed_releases.VERSIONS[1]
        settled = ready and journal.get("phase") == phase and not value["state"].get("owned")
        if settled and phase == "committed":
            settled = target_committed(value, version)
        order_trace.sample(driver, value, settled, version, phase)
        if settled and committed is not None:
            commit_time_ns(value, version)
            committed(value)
            committed = None
        observed = inspect_full(driver) if settled else {}
        if observed.get("ready") and observed["build"]["version"] == version:
            return observed
        time.sleep(0.1)
    raise RuntimeError("ordered host did not reach its native image/journal bound: " + driver.account["role"])


def inspect_full(driver):
    with order_trace.span(order_trace.trace(driver), "inspection.full"):
        return driver.call("inspect")


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


def commit_time_ns(value, version):
    entry = committed_entry(value, version)
    if entry is None:
        raise RuntimeError("committed journal lacks one matching native ledger entry")
    return timestamp_ns(entry["committedAt"])


def timestamp_ns(value):
    match = RFC3339.fullmatch(value)
    if match is None:
        raise ValueError("native commit time is not RFC3339 with nanosecond precision")
    instant = datetime.fromisoformat(match[1] + match[3].replace("Z", "+00:00"))
    delta = instant - EPOCH
    return (delta.days * 86400 + delta.seconds) * 1_000_000_000 + int((match[2] or "").ljust(9, "0"))


def verify_order(canary, sleeper, burst, version):
    entries = [committed_entry(value, version) for value in (canary, sleeper, burst)]
    if any(entry is None for entry in entries):
        raise RuntimeError("ordered host journal does not match its native commit ledger")
    if len({entry["metadata"] for entry in entries}) != 1 or any(entry["workersPreserved"] != entry["workers"] for entry in entries):
        raise RuntimeError("ordered native hosts disagree on their exact pinned release or preserved workers")
    times = [commit_time_ns(value, version) for value in (canary, sleeper, burst)]
    if times != sorted(times) or len(set(times)) != 3:
        raise RuntimeError("native predecessor commits did not occur in policy order")
    if any(times[index + 1] - times[index] < SOAK_NS for index in range(2)):
        raise RuntimeError("native dependent committed before its actual predecessor soak")
    if any(entry["version"] == installed_releases.VERSIONS[1] for value in (sleeper, burst) for entry in value["entries"]):
        raise RuntimeError("dependent adopted the failed canary release")
    if not canary["dependents"] or not sleeper["peers"] or not sleeper["dependents"] or not burst["peers"]:
        raise RuntimeError("ordered hosts lack actual authenticated predecessor/consumer observations")


def teardown(accounts, drivers, record, output, on_error=None):
    with installed_native.cleanup_errors(on_error):
        teardown_owned(accounts, drivers, record, output, on_error)


def teardown_owned(accounts, drivers, record, output, on_error):
    initial_diagnostic_error = order_trace.retain(drivers, record, output)
    if initial_diagnostic_error is not None and record.get("passed") and on_error is not None:
        on_error(initial_diagnostic_error)
    actions = [driver.close for driver in reversed(drivers)]
    actions.append(accounts.close)
    try:
        installed_native.cleanup_all(actions)
        record["cleanup"] = {"accountsAbsent": True, "servicesAbsent": True, "processesAbsent": True}
    except BaseException as error:
        for driver in drivers:
            order_trace.failure(driver, error, "teardown", "cleanup")
        record["passed"] = False
        record["cleanupError"] = str(error)
        primary = record.get("error", "ordered native teardown failed")
        raise RuntimeError(primary + "; cleanup failed: " + str(error)) from error
    finally:
        diagnostic_error = order_trace.retain(drivers, record, output)
        fail_success = diagnostic_error is not None and record.get("passed", False)
        if fail_success:
            record["passed"], record["error"] = False, str(diagnostic_error)
        write_ordered_result(output, record)
    if fail_success:
        if on_error is not None:
            on_error(diagnostic_error)
        raise diagnostic_error


def write_ordered_result(output, record):
    try:
        (output / "ordered-native.json").write_text(json.dumps(record, indent=2) + "\n")
    except BaseException as error:
        installed_native.cleanup_failure(error, None)
        raise


def interrupted(number, _frame):
    raise InterruptedError("ordered native proof interrupted by signal " + str(number))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    descriptor_path, destination = map(Path, sys.argv[1:3])
    descriptor = json.loads(descriptor_path.read_text())
    with installed_clock.handoff(descriptor["fixtureDeadlineNs"], watchdog=True):
        run(descriptor_path.parent, destination, descriptor, sys.argv[3])
