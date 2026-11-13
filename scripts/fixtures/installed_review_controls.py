"""Pure review countermodels and real owned-process supervision controls."""
import copy
import io
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import signal
import sys
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import installed_account_run, installed_accounts, installed_order, installed_order_host, installed_native, installed_publication, installed_releases, native_services, installed_fresh, installed_adoption


class Clock:
    def __init__(self):
        self.seconds = 20
        self.base = datetime(2026, 11, 12, tzinfo=timezone.utc)

    def advance(self, seconds):
        self.seconds += seconds

    def now(self):
        return self.base + timedelta(seconds=self.seconds)

    def stamp(self):
        return self.now().isoformat()


class OrderModel:
    def __init__(self, ignore_soak=False, ignore_failed=False):
        self.clock, self.ignore_soak, self.ignore_failed = Clock(), ignore_soak, ignore_failed
        self.latest, self.drivers = installed_releases.VERSIONS[0], {}

    def head(self, _directory, version):
        self.latest = version


class OrderDriver:
    def __init__(self, model, role):
        self.model, self.account = model, {"role": role, "id": role}
        model.drivers[role] = self
        self.online, self.fault = True, []
        self.value = {"build": {"version": model.latest}, "state": {"phase": "waiting"}, "journal": {},
                      "entries": [], "failed": [], "peers": [], "dependents": [{}], "peerFailures": {}, "stateWrittenAt": 0,
                      "helper": {}, "route": {}, "ready": True}

    def call(self, operation, **fields):
        self.model.clock.advance(0.05)
        if operation == "stop":
            self.online = False
        if operation == "fault":
            self.fault = fields["versions"]
        if operation == "start":
            self.online = True
            self.tick()
        if operation == "inspect" and self.online:
            self.tick()
        return copy.deepcopy(self.value)

    def tick(self):
        if self.value["build"]["version"] == self.model.latest:
            return
        if self.model.latest in self.fault:
            self.value["failed"] = [{"version": self.model.latest}]
            self.value["journal"] = {"request": {"manifest": {"version": self.model.latest}}, "phase": "rolled_back"}
            return
        if self.permitted():
            self.commit()

    def permitted(self):
        predecessor = {"canary": None, "burst": "canary", "sleep": "burst"}[self.account["role"]]
        if predecessor is None:
            return True
        peer = self.model.drivers[predecessor]
        self.value["peers"] = [{"id": predecessor, "state": "verified", "readAt": self.model.clock.stamp()}]
        self.value["peerFailures"][predecessor] = copy.deepcopy(peer.value["failed"])
        self.value["stateWrittenAt"] = self.model.clock.now().timestamp()
        if self.model.ignore_failed and peer.value["failed"]:
            return True
        if not peer.online or peer.value["build"]["version"] != self.model.latest:
            return False
        age = (self.model.clock.now() - installed_order.commit_time(peer.value, self.model.latest)).total_seconds()
        return self.model.ignore_soak or age >= 5

    def commit(self):
        target = self.model.latest
        self.value["build"] = {"version": target}
        self.value["journal"] = {"request": {"manifest": {"version": target}}, "phase": "committed"}
        self.value["entries"].append({"version": target, "metadata": "same-pin", "workers": 1,
                                      "workersPreserved": 1, "committedAt": self.model.clock.stamp()})


def exercise_model(model, driver_type=OrderDriver):
    drivers = [driver_type(model, role) for role in ("canary", "sleep", "burst")]
    record = {"observations": []}
    clock = SimpleNamespace(monotonic=lambda: model.clock.seconds, sleep=model.clock.advance)
    dates = SimpleNamespace(now=lambda _: model.clock.now(), fromisoformat=datetime.fromisoformat, fromtimestamp=datetime.fromtimestamp)
    with patch.object(installed_order.installed_origin, "Origin", return_value=model), patch.object(installed_order, "time", clock), patch.object(installed_order, "datetime", dates):
        installed_order.scenarios(Path("/unused"), drivers, {"releases": {"directory": "/unused"}}, record)
    return record


class SlowInspectionDriver(OrderDriver):
    def call(self, operation, **fields):
        value = super().call(operation, **fields)
        latest = installed_releases.VERSIONS[-1]
        if operation != "inspect" or not fields.get("active", True) or value["build"]["version"] != latest:
            return value
        if self.account["role"] in ("canary", "burst"):
            time.sleep(6)
            self.model.inspections.append(self.account["role"])
        return value


class LiveOrderClock:
    def __init__(self):
        self.base = datetime.now(timezone.utc)
        self.started = time.monotonic()

    @property
    def seconds(self):
        return time.monotonic() - self.started

    def advance(self, seconds):
        time.sleep(seconds)

    def now(self):
        return self.base + timedelta(seconds=self.seconds)

    def stamp(self):
        return self.now().isoformat()


class TrackedOrderDriver(OrderDriver):
    def call(self, operation, **fields):
        role, owner = self.account["role"], threading.get_ident()
        latest = self.model.latest == installed_releases.VERSIONS[-1]
        if latest and self.model.owners.setdefault(role, owner) != owner:
            raise RuntimeError("host protocol stream changed controller")
        arming = not latest and owner != self.model.main
        stage = "validate" if operation == "inspect" and fields.get("active", True) else operation
        if (latest or arming) and self.model.failure == (role, "arming" if arming else stage):
            raise RuntimeError("injected controller failure")
        result = super().call(operation, **fields)
        if arming:
            self.model.armed.add(role)
        if latest and operation == "inspect" and fields.get("active", True):
            self.model.validated.add(role)
        return result


def tracked_order_model(failure=None):
    model = OrderModel()
    model.main, model.failure = threading.get_ident(), failure
    model.owners, model.armed, model.validated = {}, set(), set()
    return model


class OrderReviewTest(unittest.TestCase):
    def test_slow_full_inspections_do_not_consume_either_preexpiry_opportunity(self):
        model = OrderModel()
        model.clock = LiveOrderClock()
        model.inspections = []
        record = exercise_model(model, SlowInspectionDriver)
        self.assertEqual(model.inspections, ["canary", "burst"])
        self.assertIn("directBeforeSoak", record["observations"][1])
        self.assertIn("sleepBeforeSoak", record["observations"][1])

    def test_release_waits_for_both_context_reads_and_streams_have_single_owners(self):
        model = tracked_order_model()
        original = model.head
        def release(directory, version):
            if version == installed_releases.VERSIONS[-1]:
                self.assertEqual(model.armed, {"burst", "sleep"})
                self.assertFalse(model.drivers["sleep"].online)
            original(directory, version)
        model.head = release
        record = exercise_model(model, TrackedOrderDriver)
        self.assertEqual(len(set(model.owners.values())), 3)
        self.assertEqual(model.validated, {"canary", "burst", "sleep"})
        self.assertEqual({row["host"] for row in record["opportunityControllers"]}, {"burst", "sleep"})
        self.assertTrue(all(row["armedAt"] <= record["latestReleaseStartedAt"] for row in record["opportunityControllers"]))

    def test_controller_failures_drain_waiters_before_returning(self):
        failures = (("burst", "arming"), ("sleep", "arming"), ("canary", "start"), ("burst", "start"),
                    ("sleep", "start"), ("canary", "validate"), ("burst", "validate"), ("sleep", "validate"))
        for failure in failures:
            self.check_controller_failure(failure)

    def check_controller_failure(self, failure):
        model = tracked_order_model(failure)
        with self.assertRaisesRegex(RuntimeError, "injected controller failure"):
            exercise_model(model, TrackedOrderDriver)
        self.assertFalse(any(thread.name.startswith("ordered-opportunity") for thread in threading.enumerate()))

    def test_after_expiry_state_cannot_become_a_preexpiry_proof(self):
        model = OrderModel()
        latest = installed_releases.VERSIONS[-1]
        predecessor = {"entries": [{"version": latest, "committedAt": model.clock.stamp()}]}
        model.clock.advance(5)
        dates = SimpleNamespace(now=lambda _: model.clock.now(), fromisoformat=datetime.fromisoformat)
        with patch.object(installed_order, "datetime", dates):
            with self.assertRaisesRegex(RuntimeError, "fixture missed the pre-expiry opportunity"):
                installed_order.exercise_soak(None, predecessor, installed_releases.VERSIONS[0], latest)

    def test_product_that_ignores_soak_is_rejected_before_expiry(self):
        with self.assertRaisesRegex(RuntimeError, "before its predecessor"):
            exercise_model(OrderModel(ignore_soak=True))

    def test_awake_direct_dependent_cannot_ignore_failed_canary(self):
        with self.assertRaisesRegex(RuntimeError, "before its predecessor"):
            exercise_model(OrderModel(ignore_failed=True))

    def test_product_enforces_soak_and_releases_with_existing_opportunities(self):
        record = exercise_model(OrderModel())
        self.assertIn("awakeDirectHold", record["observations"][0])
        self.assertIn("directBeforeSoak", record["observations"][1])
        self.assertIn("sleepBeforeSoak", record["observations"][1])


class OrderedBootstrapTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="u8b-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_installation_creates_actual_config_before_ordered_configure(self):
        binary = self.root / "baseline"
        binary.write_bytes(b"local fixture input, never executed")
        manager = SimpleNamespace(environment={"XDG_RUNTIME_DIR": "/unused", "DBUS_SESSION_BUS_ADDRESS": "unused"})
        units = SimpleNamespace(managers={"daemon": manager})
        with patch.object(installed_native, "Units", return_value=units):
            case = installed_native.Installation(self.root, {"name": "canary-ordered"},
                {"daemon": {"executable": str(binary)}}, Path("/unused"))
        case.initialize_preserved_baseline = lambda: {}
        host = object.__new__(installed_order_host.Host)
        host.case = case
        host.configure({"peers": [], "after": [], "soak": 5000000000})
        fleet = Path(case.environment["MESH_CONFIG_DIR"]) / "fleet.json"
        self.assertEqual(json.loads(fleet.read_text())["name"], "installed-order")
        self.assertEqual(case.adoption_policy["soak"], 5000000000)

    def test_configure_uses_configured_directory(self):
        configured = self.root / "configured"
        configured.mkdir()
        host = object.__new__(installed_order_host.Host)
        host.case = SimpleNamespace(root=self.root, environment={"MESH_CONFIG_DIR": str(configured)}, initialize_preserved_baseline=lambda: {})
        host.configure({"peers": [], "after": [], "soak": 5000000000})
        self.assertTrue((configured / "fleet.json").exists())
        self.assertFalse((self.root / "config").exists())

    def driver(self):
        home, output = self.root / "canary", self.root / "runner-private/evidence"
        root = home / "canary-ordered"
        root.mkdir(parents=True)
        output.mkdir(parents=True)
        (root / "route-observation.json").write_text('{"observerStopped":true,"connected":false}')
        (root / "listener-failure.json").write_text('{"listener":{"status":"stopped"}}')
        driver = object.__new__(installed_order.Driver)
        driver.account = {"role": "canary", "home": str(home)}
        driver.accounts = SimpleNamespace(source=Path(__file__).resolve().parents[2], output=output,
            validate_resources=lambda: None, owned=lambda account: None, run=self.account_run)
        driver.log, driver.process = io.BytesIO(), None
        return driver

    def account_run(self, account, arguments, **options):
        self.assertEqual(Path(arguments[-1]), Path(account["home"]) / "canary-ordered")
        # Simulate runuser's I/O boundary without changing OS accounts or privileges.
        command = [sys.executable, "-c", "from pathlib import Path; from fixtures.installed_order_host import export_logs; import sys; export_logs(Path(sys.argv[1]))", str(arguments[-1])]
        return subprocess.run(command, **options)

    def test_account_never_writes_runner_private_output(self):
        driver = self.driver()
        host = object.__new__(installed_order_host.Host)
        host.closed, host.output = False, driver.accounts.output
        host.case = SimpleNamespace(root=Path(driver.account["home"]) / "canary-ordered",
            host={"name": "canary-ordered"}, close=lambda: None)
        with patch.object(installed_native.shutil, "copy2", side_effect=PermissionError("runner ancestor denies account")):
            host.close()
        driver.close()
        route = driver.accounts.output / "canary-ordered-route-observation.json"
        self.assertEqual(json.loads(route.read_text()), {"observerStopped": True, "connected": False})
        failure = driver.accounts.output / "canary-ordered-listener-failure.json"
        self.assertEqual(json.loads(failure.read_text()), {"listener": {"status": "stopped"}})
        self.assertTrue(driver.log.closed)

    def test_changed_account_identity_refuses_export(self):
        driver = self.driver()
        with patch.object(driver.accounts, "run") as run, patch.object(driver.accounts, "owned", side_effect=RuntimeError("identity changed")):
            with self.assertRaisesRegex(RuntimeError, "identity changed"):
                driver.close()
        run.assert_not_called()
        self.assertTrue(driver.log.closed)

    def test_export_bound_failure_keeps_primary_and_runs_account_cleanup(self):
        driver = self.driver()
        (Path(driver.account["home"]) / "canary-ordered/preserved-before.json").write_bytes(b"x" * 262145)
        stopped = []
        driver.accounts.close = lambda: stopped.append(True)
        record = {"passed": False, "error": "original configure failure", "observations": []}
        with self.assertRaisesRegex(RuntimeError, "(?s)original configure failure; cleanup failed:.*file bound"):
            installed_order.teardown(driver.accounts, [driver], record, driver.accounts.output)
        self.assertEqual(stopped, [True])
        self.assertTrue(driver.log.closed)
        saved = json.loads((driver.accounts.output / "ordered-native.json").read_text())
        self.assertIn("file bound", saved["cleanupError"])
        self.assertNotIn("cleanup", saved)

    def test_close_failure_still_stops_real_driver_then_retains_evidence(self):
        driver = self.driver()
        driver.process = subprocess.Popen(["/bin/sleep", "30"])
        self.addCleanup(AccountRunReviewTest.stop_process, self, driver.process)
        with patch.object(driver, "close_host", side_effect=RuntimeError("injected close failure")):
            with self.assertRaisesRegex(RuntimeError, "injected close failure"):
                driver.close()
        self.assertIsNotNone(driver.process.poll())
        self.assertTrue(driver.log.closed)
        self.assertTrue((driver.accounts.output / "canary-ordered-route-observation.json").exists())

    def test_export_rejects_unexpected_filename_before_publication(self):
        output = self.root / "output"
        output.mkdir()
        with self.assertRaisesRegex(RuntimeError, "unexpected file"):
            installed_order.publish_logs(json.dumps({"../private": "78"}), output, "canary-ordered")
        self.assertEqual(list(output.iterdir()), [])

    def test_log_tail_is_bounded_and_exact(self):
        data = b"discarded" + b"x" * 65536
        with (self.root / "daemon.log").open("w+b") as stream:
            stream.write(data)
            stream.seek(0)
            self.assertEqual(installed_order_host.read_log(stream, "daemon.log"), data[-65536:])


OWNER_CONTROL = '''import json,os,signal,subprocess,sys,time
from pathlib import Path
from fixtures import installed_account_run as run
root,output=map(Path,sys.argv[1:3]); operation=sys.argv[3]
run.claim(root,output,operation)
child=subprocess.Popen(['/bin/sleep','30'])
run.save(output/'child.json',run.identity(child.pid))
def interrupted(*args):
    raise InterruptedError('injected stop')
signal.signal(signal.SIGTERM,interrupted)
try:
    while True: time.sleep(.01)
except InterruptedError:
    pass
finally:
    child.terminate(); child.wait(timeout=5)
    run.save(output/'accounts-cleanup.json',{'operation':operation,'accounts':[], 'groupAbsent':True,'resourcesStopped':True})
'''


class AccountRunReviewTest(unittest.TestCase):
    def test_existing_source_is_never_claimed_for_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="u8r-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp")) as directory:
            accounts = object.__new__(installed_accounts.Accounts)
            accounts.root, accounts.source = Path(directory), None
            existing = accounts.root / "account-source"
            existing.mkdir()
            (existing / "preserve").write_text("pre-existing")
            with self.assertRaisesRegex(RuntimeError, "existing"):
                accounts.prepare_sources()
            self.assertIsNone(accounts.source)
            self.assertTrue((existing / "preserve").exists())

    def test_missing_report_retains_order_inputs(self):
        with tempfile.TemporaryDirectory(prefix="u8r-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp")) as directory:
            root, output = Path(directory) / "root", Path(directory) / "output"
            root.mkdir()
            output.mkdir()
            (root / "ordered-input.json").write_text("{}")
            with self.assertRaisesRegex(RuntimeError, "missing"):
                installed_account_run.verify_cleanup(root, output)
            self.assertTrue(root.exists())

    @unittest.skipUnless(sys.platform == "linux", "ordered-account supervision uses Linux process identities")
    def test_real_owned_timeout_runs_finally_and_preserves_failure(self):
        with tempfile.TemporaryDirectory(prefix="u8r-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp")) as directory:
            self.timeout_control(Path(directory))

    @unittest.skipUnless(sys.platform == "linux", "ordered-account supervision uses Linux process identities")
    def test_real_owned_injected_failure_still_runs_finally(self):
        with tempfile.TemporaryDirectory(prefix="u8r-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp")) as directory:
            self.timeout_control(Path(directory), injected=True)

    def test_changed_pid_lifetime_cannot_be_signalled(self):
        owned = {"pid": os.getpid(), "uid": os.geteuid(), "birth": "selected-process", "boot": "selected-boot"}
        with patch.object(installed_account_run, "identity", return_value=owned | {"birth": "another-process"}), patch.object(os, "kill") as kill:
            with self.assertRaisesRegex(RuntimeError, "lifetime changed"):
                installed_account_run.signal_bound(owned, signal.SIGTERM)
        kill.assert_not_called()

    def timeout_control(self, directory, injected=False):
        root, output = directory.resolve() / "root", directory.resolve() / "output"
        root.mkdir()
        output.mkdir()
        descriptor = root / "ordered-input.json"
        descriptor.write_text("{}")
        import hashlib
        ticket = {"schema": 1, "operation": "control", "root": str(root), "output": str(output), "owner": None,
                  "cleanup_deadline_ns": time.monotonic_ns() + 20_000_000_000,
                  "supervisor": installed_account_run.identity(os.getpid()), "descriptor": hashlib.sha256(descriptor.read_bytes()).hexdigest()}
        path = root / "account-run.json"
        installed_account_run.save(path, ticket)
        environment = os.environ | {"PYTHONPATH": str(Path(__file__).parents[1]), "PYTHONDONTWRITEBYTECODE": "1"}
        process = subprocess.Popen([sys.executable, "-c", OWNER_CONTROL, str(root), str(output), "control"], env=environment)
        self.addCleanup(self.stop_process, process)
        native_services.wait_for(lambda: (output / "child.json").exists(), "owned timeout control did not claim/start")
        supervised = InjectedWait(process) if injected else process
        with self.assertRaises(RuntimeError if injected else subprocess.TimeoutExpired):
            installed_account_run.supervise(supervised, path, output, 0.01, self.signal_owner, lambda _path: None)
        installed_account_run.verify_cleanup(root, output)
        child = json.loads((output / "child.json").read_text())
        self.assertFalse(installed_account_run.alive(child))

    def signal_owner(self, path, number):
        owned = json.loads(path.read_text())["owner"]
        installed_account_run.signal_bound(owned, number)

    def stop_process(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)


class InjectedWait:
    def __init__(self, process):
        self.process, self.inject = process, True

    def wait(self, timeout):
        if self.inject:
            self.inject = False
            raise RuntimeError("injected owner supervision failure")
        return self.process.wait(timeout=timeout)


class PublishedRouteReviewTest(unittest.TestCase):
    def expected(self):
        return {"version": "middle", "binary": "middle-image", "metadata": "signed-metadata", "manifest": "signed-manifest"}

    def record(self):
        expected = self.expected()
        hop = {"expected": expected, "build": {"digest": "middle-image"}, "helper": {"sha256": "middle-image"}}
        return {"expectedHops": [expected], "authenticatedTarget": {"binary": "target-image"}, "hops": [hop]}

    def test_terminal_without_required_hop_is_rejected(self):
        record = self.record()
        record["hops"] = []
        with self.assertRaisesRegex(RuntimeError, "skipped"):
            installed_publication.verify_observed_route(record, set())

    def test_receipt_matching_old_helper_is_not_authenticated_final_helper(self):
        record = self.record()
        case = SimpleNamespace(inspect_retained=lambda: None, control=lambda _: {"host": {"build": {"digest": "target-image"}}},
            releases=SimpleNamespace(builds={"target": {"digest": "target-image"}}), mapped_helper=lambda: {"sha256": "old-helper-image"})
        with self.assertRaisesRegex(RuntimeError, "authenticated expected"):
            installed_publication.validate_published_route(case, record, {}, {}, "target", None, False, {"middle"})

    def test_hop_helper_must_match_authenticated_image(self):
        record = self.record()
        record["hops"][0]["helper"]["sha256"] = "receipt-only-image"
        with self.assertRaisesRegex(RuntimeError, "authenticated expected"):
            installed_publication.verify_observed_route(record, {"middle"})

    def test_terminal_iteration_observes_before_validation(self):
        record = self.record()
        record["hops"] = []
        journal = {"phase": "committed", "request": {"manifest": {"version": "target"}}}
        ledger = {"state": {}}
        events = []
        def observe(_case, current, _first, _target, seen):
            events.append("observe")
            current["hops"] = self.record()["hops"]
            seen.add("middle")
        def validate(_case, current, _journal, _ledger, _target, _fault, _rollback, seen):
            installed_publication.verify_observed_route(current, seen)
            events.append("validate")
        with patch.object(installed_publication, "observe_published_hop", side_effect=observe), patch.object(installed_publication, "validate_published_route", side_effect=validate):
            with patch.object(installed_publication.installed_chain, "read_ledger", return_value=ledger), patch.object(installed_publication.installed_chain, "read_journal", return_value=journal):
                installed_publication.await_published_route(SimpleNamespace(), record, "first", "target", None, False)
        self.assertEqual(events, ["observe", "validate"])


class InitializationBoundaryTest(unittest.TestCase):
    def test_initialization_requires_actual_first_start_assertions(self):
        case = object.__new__(installed_fresh.FreshCase)
        case.first_start_verified = False
        with self.assertRaisesRegex(RuntimeError, "first-start assertions"):
            case.initialize_preserved_baseline()

    def test_only_missing_domain_policy_can_be_initialized(self):
        before = {"identity.key": "identity", "config/hosts.json": "configured"}
        installed_adoption.validate_initialized_baseline(before, before | {"config/domains.json": "initialized"}, "config/domains.json")

    def test_configured_policy_stays_strictly_preserved(self):
        before = {"config/domains.json": "configured"}
        with self.assertRaisesRegex(RuntimeError, "already configured"):
            installed_adoption.validate_initialized_baseline(before, {"config/domains.json": "changed"}, "config/domains.json")

    def test_initialization_cannot_delete_or_add_other_preserved_files(self):
        before = {"identity.key": "identity"}
        with self.assertRaisesRegex(RuntimeError, "already configured"):
            installed_adoption.validate_initialized_baseline(before, {}, "domains.json")
        with self.assertRaisesRegex(RuntimeError, "unexpected preserved"):
            installed_adoption.validate_initialized_baseline(before, before | {"fleet.json": "new"}, "domains.json")


class OrderedLifecycleTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="u8life-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_ordered_configuration_initializes_before_adoption_snapshot(self):
        host = object.__new__(installed_order_host.Host)
        host.case = SimpleNamespace(environment={"MESH_CONFIG_DIR": str(self.root)},
                                    initialize_preserved_baseline=Mock(return_value={"command": "actual initialization"}))
        result = host.configure({"peers": [], "after": [], "soak": 5000000000})
        host.case.initialize_preserved_baseline.assert_called_once_with()
        self.assertEqual(result["baselineInitialization"], {"command": "actual initialization"})

    def installation(self):
        case = object.__new__(installed_native.Installation)
        case.route_observer, case.response, case.stream = None, None, None
        case.listener, case.worker, case.shell, case.terminal = None, None, None, None
        case.cleaned, case.cleanup_notes, case.process_stamps = False, [], {}
        case.units = SimpleNamespace(close=Mock())
        return case

    def test_retry_after_verification_failure_does_not_reclose_terminal_fd(self):
        case = self.installation()
        terminal = object.__new__(installed_native.terminal_module.Terminal)
        terminal.status, terminal.master = 0, os.open(os.devnull, os.O_RDONLY)
        self.addCleanup(self.close_descriptor, terminal.master)
        case.terminal = terminal
        case.verify_session_stopped = Mock(side_effect=[RuntimeError("injected owned process remains"), None])
        with self.assertRaisesRegex(RuntimeError, "injected owned process remains"):
            case.close()
        self.assertFalse(case.cleaned)
        case.close()
        case.close()
        self.assertTrue(case.cleaned)
        self.assertEqual(case.verify_session_stopped.call_count, 2)
        with self.assertRaises(OSError):
            os.fstat(terminal.master)

    def close_descriptor(self, descriptor):
        try:
            os.fstat(descriptor)
        except OSError:
            return
        os.close(descriptor)

    def test_stopped_daemon_cleanup_uses_verified_worker_kill_protocol(self):
        case = self.installation()
        case.state, case.session, case.worker = self.root, "owned", 12345
        path = self.root / "s/owned/sock"
        path.parent.mkdir(parents=True)
        path.touch()
        case.process_stamps[case.worker] = "owned lifetime"
        case.ready = lambda: False
        with patch.object(installed_native, "process_stamp", return_value="owned lifetime"), patch.object(installed_native, "peer_pid", return_value=case.worker):
            with patch.object(installed_native.controls, "round_trip", return_value={"type": "ok"}) as control, patch.object(installed_native.os, "kill") as kill:
                case.stop_worker()
        control.assert_called_once_with(str(path), {"type": "session.kill", "requestId": "installed-cleanup", "sessionId": "owned"})
        kill.assert_not_called()

    def test_worker_lifetime_change_is_never_signalled_or_contacted(self):
        case = self.installation()
        case.state, case.session = self.root, "owned"
        path = self.root / "s/owned/sock"
        path.parent.mkdir(parents=True)
        path.touch()
        case.worker, case.process_stamps = 12345, {12345: "original"}
        case.ready = lambda: False
        with patch.object(installed_native, "process_stamp", return_value="replacement"), patch.object(installed_native.os, "kill") as kill, patch.object(installed_native, "peer_pid", return_value=case.worker):
            with patch.object(installed_native.controls, "round_trip") as control, self.assertRaisesRegex(RuntimeError, "ownership"):
                case.stop_worker()
        kill.assert_not_called()
        control.assert_not_called()

    def account(self):
        accounts = object.__new__(installed_accounts.Accounts)
        accounts.persist = Mock()
        accounts.pids = Mock(return_value=[])
        accounts.owned = Mock()
        accounts.stop_processes = Mock()
        accounts.before = "preexisting user@1000.service loaded failed failed\n"
        home = self.root / "home"
        home.mkdir()
        account = {"name": "owned-test", "uid": 12345, "gid": 12345, "home": str(home), "created": True,
                   "ownershipVerified": True, "managerStarted": True, "managerUnitsAbsentBefore": True}
        return accounts, account

    def manager_command(self, arguments, check=True):
        self.calls.append(arguments)
        if "list-units" in arguments:
            output = "user@12345.service loaded failed failed User Manager\n" if self.failed else ""
            return SimpleNamespace(stdout=output, returncode=0)
        if "reset-failed" in arguments:
            self.assertEqual(arguments[-1], "user@12345.service")
            self.failed = False
        return SimpleNamespace(stdout="", returncode=0)

    def test_removal_clears_only_owned_failed_manager_and_is_repeatable(self):
        accounts, account = self.account()
        self.failed, self.calls = True, []
        with patch.object(installed_accounts, "account_exists", side_effect=[SimpleNamespace(), None, None]), patch.object(installed_accounts.pwd, "getpwuid", side_effect=KeyError):
            with patch.object(installed_accounts, "command", side_effect=self.manager_command):
                accounts.remove_account(account)
                accounts.remove_account(account)
        self.assertFalse(self.failed)
        self.assertTrue(account["homeAbsent"])
        resets = [call for call in self.calls if "reset-failed" in call]
        self.assertEqual(resets, [["/usr/bin/systemctl", "reset-failed", "user@12345.service"]])

    def test_missing_manager_ownership_cannot_clear_failed_state(self):
        accounts, account = self.account()
        del account["managerUnitsAbsentBefore"]
        self.failed, self.calls = True, []
        with patch.object(installed_accounts, "account_exists", return_value=SimpleNamespace()), patch.object(installed_accounts, "command", side_effect=self.manager_command):
            with self.assertRaisesRegex(RuntimeError, "manager.*ownership"):
                accounts.remove_account(account)
        self.assertTrue(self.failed)
        self.assertFalse(any("reset-failed" in call for call in self.calls))

    def test_active_manager_is_not_reset(self):
        accounts, account = self.account()
        result = SimpleNamespace(stdout="user@12345.service loaded active running User Manager\n")
        with patch.object(installed_accounts, "command", return_value=result) as command:
            with self.assertRaisesRegex(RuntimeError, "remains active"):
                accounts.clear_failed_managers(account)
        self.assertEqual(command.call_count, 1)

    def test_remaining_process_blocks_failed_manager_reset(self):
        accounts, account = self.account()
        accounts.pids.return_value = [12346]
        with patch.object(installed_accounts, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "remaining test processes"):
                accounts.clear_failed_managers(account)
        command.assert_not_called()

    def test_reallocated_uid_blocks_absent_account_recovery(self):
        accounts, account = self.account()
        with patch.object(installed_accounts.pwd, "getpwuid", return_value=SimpleNamespace()), patch.object(installed_accounts, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "reallocated"):
                accounts.finish_absent_account(account)
        command.assert_not_called()

    def test_preexisting_manager_cannot_become_cleanup_owned(self):
        accounts, _account = self.account()
        accounts.root, accounts.accounts, accounts.prefix, accounts.group = self.root, [], "m8test", "m8test"
        entry = SimpleNamespace(pw_uid=12345, pw_gid=12345)
        result = SimpleNamespace(stdout="user@12345.service loaded failed failed Existing Manager\n")
        with patch.object(installed_accounts, "account_exists", return_value=None), patch.object(installed_accounts.pwd, "getpwnam", return_value=entry):
            with patch.object(installed_accounts, "command", return_value=result) as command, self.assertRaisesRegex(RuntimeError, "pre-existing manager"):
                accounts.create_account("sleep")
        self.assertFalse(accounts.accounts[0]["ownershipVerified"])
        self.assertFalse(any("start" in call.args[0] or "reset-failed" in call.args[0] for call in command.call_args_list))

    def test_inventory_cannot_select_another_uid_for_reset(self):
        accounts, account = self.account()
        result = SimpleNamespace(stdout="user@1000.service loaded failed failed Owner Manager\n")
        with patch.object(installed_accounts, "command", return_value=result) as command:
            with self.assertRaisesRegex(RuntimeError, "inventory is malformed"):
                accounts.clear_failed_managers(account)
        self.assertEqual(command.call_count, 1)

    def test_reset_failure_remains_a_cleanup_failure(self):
        accounts, account = self.account()
        self.failed, self.calls = True, []
        def reject_reset(arguments, check=True):
            if "reset-failed" in arguments:
                raise RuntimeError("injected reset failure")
            return self.manager_command(arguments, check)
        with patch.object(installed_accounts, "command", side_effect=reject_reset):
            with self.assertRaisesRegex(RuntimeError, "injected reset failure"):
                accounts.clear_failed_managers(account)
        self.assertNotIn("managerUnitsAbsent", account)
        self.assertTrue(self.failed)
