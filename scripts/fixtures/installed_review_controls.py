"""Pure review countermodels and real owned-process supervision controls."""
import copy
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import signal
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from fixtures import installed_account_run, installed_accounts, installed_order, installed_publication, installed_releases, native_services, installed_fresh


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


def exercise_model(model):
    drivers = [OrderDriver(model, role) for role in ("canary", "sleep", "burst")]
    record = {"observations": []}
    clock = SimpleNamespace(monotonic=lambda: model.clock.seconds, sleep=model.clock.advance)
    dates = SimpleNamespace(now=lambda _: model.clock.now(), fromisoformat=datetime.fromisoformat, fromtimestamp=datetime.fromtimestamp)
    with patch.object(installed_order.installed_origin, "Origin", return_value=model), patch.object(installed_order, "time", clock), patch.object(installed_order, "datetime", dates):
        installed_order.scenarios(Path("/unused"), drivers, {"releases": {"directory": "/unused"}}, record)
    return record


class OrderReviewTest(unittest.TestCase):
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
        installed_fresh.validate_initialized_baseline(before, before | {"config/domains.json": "initialized"}, "config/domains.json")

    def test_configured_policy_stays_strictly_preserved(self):
        before = {"config/domains.json": "configured"}
        with self.assertRaisesRegex(RuntimeError, "already configured"):
            installed_fresh.validate_initialized_baseline(before, {"config/domains.json": "changed"}, "config/domains.json")
