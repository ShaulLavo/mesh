"""Pure diagnostic controls. No native host or product process is executed."""
import copy
from concurrent.futures import Future
from datetime import datetime, timedelta, timezone
import hashlib
import json
import io
from pathlib import Path
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_clock, installed_order, installed_order_host, installed_order_trace as trace, installed_releases, native_services


def observation(version, stamp="2000-01-01T00:00:00.000000999Z"):
    return {"journal": {"phase": "committed", "updatedAt": stamp,
        "request": {"id": "request", "generation": 2, "manifest": {"version": version}}},
        "state": {"phase": "idle"}, "entries": [{"version": version, "request": "request", "generation": 2,
            "metadata": "pin", "committedAt": stamp}], "failed": [], "peers": [], "peerFailures": {}, "stateWrittenAt": 0,
        "ready": True, "build": {"version": version}}


class CompetingDriver:
    def __init__(self, role, produced, parent_error):
        self.account = {"role": role, "id": role}
        self.produced, self.parent_error = produced, parent_error

    def call(self, operation, **fields):
        if operation == "inspect" and fields.get("active", True) and self.account["role"] == "canary":
            if not self.produced.wait(2):
                raise AssertionError("direct producer did not finish")
            raise self.parent_error
        version = installed_releases.VERSIONS[-1 if self.account["role"] == "canary" else 0]
        return copy.deepcopy(observation(version))


class OrderFailureEvidenceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.deadline = time.monotonic_ns() + 10_000_000_000
        self.native_command = self.enterContext(patch.object(native_services, "command",
            side_effect=AssertionError("pure control invoked a native command")))

    def mapping_error(self):
        expected = SimpleNamespace(st_dev=11, st_ino=22)
        mapped = SimpleNamespace(st_dev=11, st_ino=33)
        with patch.object(native_services.platform, "system", return_value="Linux"), \
                patch.object(Path, "resolve", return_value=Path("/expected/helper")), \
                patch.object(Path, "stat", side_effect=[expected, mapped]) as reads:
            with self.assertRaisesRegex(RuntimeError, "^native service mapped another executable inode$") as caught:
                native_services.mapped_image(1234, "/receipt/helper")
        self.assertEqual(reads.call_count, 2)
        return caught.exception

    def host(self):
        state = self.root / "state"
        (state / "update/helper").mkdir(parents=True)
        receipt = {"executable": "/receipt/helper", "servicePath": "/units/helper.service", "digest": "d" * 64}
        (state / "update/helper/installed.json").write_text(json.dumps(receipt))
        journal = observation(installed_releases.VERSIONS[-1])["journal"]
        (state / "update/installation.json").write_text(json.dumps(journal))
        (self.root / "ordered-ledger.json").write_text(json.dumps({"state": {"phase": "idle"}, "entries": [], "peers": []}))
        manager = SimpleNamespace(label="mesh-update-helper", system="Linux", manager_pid=Mock(return_value=1234))
        host = object.__new__(installed_order_host.Host)
        host.closed, host.diagnostics = False, trace.Trace("canary")
        host.case = SimpleNamespace(root=self.root, state=state, origin=SimpleNamespace(ledger=Mock()), ready=lambda: True,
            control=lambda _: {"host": {"build": {"version": installed_releases.VERSIONS[-1]}}}, inspect_retained=Mock(),
            route_observer=SimpleNamespace(snapshot=lambda: {}), units=SimpleNamespace(managers={"helper": manager}), close=Mock())
        return host, receipt

    def inspect_failure(self, host, error):
        with patch.object(native_services, "mapped_image", side_effect=error) as mapped:
            with self.assertRaises(RuntimeError) as caught:
                host.dispatch("inspect", {"fixtureDeadlineNs": self.deadline})
        self.assertIs(caught.exception, error)
        mapped.assert_called_once_with(1234, "/receipt/helper")
        host.case.units.managers["helper"].manager_pid.assert_called_once_with()

    def test_sampled_helper_identity_survives_teardown_and_account_export(self):
        host, receipt = self.host()
        error = self.mapping_error()
        self.inspect_failure(host, error)
        saved = json.loads((self.root / trace.TIMELINE).read_bytes())
        evidence = saved["reserved"]["helperInspectionFailure"]["details"]
        self.assertEqual(evidence["receipt"], receipt)
        self.assertEqual(evidence["service"], {"label": "mesh-update-helper", "manager": "Linux", "pid": 1234})
        self.assertEqual(evidence["mapping"]["expected"], {"executable": "/expected/helper", "device": 11, "inode": 22})
        self.assertEqual(evidence["mapping"]["mapped"], {"executable": "/proc/1234/exe", "device": 11, "inode": 33})
        self.assertEqual(evidence["installationJournal"]["phase"], "committed")
        (host.case.state / "update/helper/installed.json").write_text('{"executable":"/later/helper"}')
        host.close()
        output = self.root / "output"
        output.mkdir()
        original_stderr = b"original driver traceback\nRuntimeError: native service mapped another executable inode\n"
        (output / "canary-driver.log").write_bytes(original_stderr)
        with patch("sys.stdout", new_callable=io.StringIO) as stream:
            installed_order_host.export_logs(self.root)
        installed_order.publish_logs(stream.getvalue(), output, "canary-ordered", trace.Trace("parent"))
        exported = json.loads((output / ("canary-ordered-" + trace.TIMELINE)).read_bytes())
        self.assertEqual(exported["reserved"]["helperInspectionFailure"]["details"], evidence)
        self.assertEqual((output / "canary-driver.log").read_bytes(), original_stderr)

    def test_capture_failure_or_expired_deadline_preserves_original_exception(self):
        host, _receipt = self.host()
        original = self.mapping_error()
        with patch.object(host.diagnostics, "retain_failure", side_effect=OSError("capture unavailable")), patch.object(trace, "notify_failure") as notify:
            self.inspect_failure(host, original)
        notify.assert_not_called()
        self.assertIn("capture unavailable", " ".join(original.__notes__))
        with installed_clock.handoff(self.deadline):
            self.check_expired_capture(host, original)
        self.assertIn("existing deadline expired", " ".join(original.__notes__))

    def check_expired_capture(self, host, original):
        with patch.object(installed_clock, "remaining_until", side_effect=installed_clock.ClockExpired("existing deadline expired")):
            with patch.object(host.diagnostics, "retain_failure") as reserve, patch.object(installed_clock, "parent_deadline", side_effect=AssertionError("deadline reset")):
                trace.helper_inspection_failure(host, original, host.case.units.managers["helper"], 1234, {}, {})
        reserve.assert_not_called()

    def test_long_failure_identities_are_bounded_and_mark_truncation(self):
        host, receipt = self.host()
        original = self.mapping_error()
        receipt["servicePath"] = "\U0001f680" * 16384
        with installed_clock.handoff(self.deadline):
            trace.helper_inspection_failure(host, original, host.case.units.managers["helper"], 1234, receipt, {})
        payload = trace.encoded(host.diagnostics.reserved)
        self.assertLessEqual(len(payload), 8192)
        saved = host.diagnostics.reserved["helperInspectionFailure"]["details"]
        self.assertEqual(saved["receipt"]["truncatedFields"], ["servicePath"])
        self.assertTrue(host.diagnostics.complete)

    def test_success_keeps_mapped_result_and_performs_no_failure_capture(self):
        expected = SimpleNamespace(st_dev=11, st_ino=22)
        with patch.object(native_services.platform, "system", return_value="Linux"), \
                patch.object(Path, "resolve", return_value=Path("/expected/helper")), patch.object(Path, "stat", return_value=expected):
            with patch.object(Path, "open", return_value=io.BytesIO(b"helper")):
                image = native_services.mapped_image(1234, "/receipt/helper")
        self.assertEqual(image, {"pid": 1234, "inode": 22, "device": 11, "sha256": hashlib.sha256(b"helper").hexdigest()})
        host, receipt = self.host()
        image["sha256"] = receipt["digest"]
        with patch.object(native_services, "mapped_image", return_value=image), patch.object(trace, "helper_inspection_failure") as capture:
            self.assertEqual(host.inspect(True)["helper"], image)
        capture.assert_not_called()
        self.assertEqual(host.diagnostics.reserved, {})

    def test_mapped_image_selects_platform_path_without_native_commands(self):
        expected = SimpleNamespace(st_dev=11, st_ino=22)
        for system, image_path in (("Linux", Path("/proc/1234/exe")), ("Darwin", Path("/darwin/helper"))):
            with self.subTest(system=system), patch.object(native_services.platform, "system", return_value=system), \
                    patch.object(native_services, "darwin_image", return_value=Path("/darwin/helper")) as darwin, \
                    patch.object(Path, "resolve", return_value=Path("/expected/helper")), \
                    patch.object(Path, "stat", autospec=True, return_value=expected) as reads, \
                    patch.object(Path, "open", autospec=True, return_value=io.BytesIO(b"helper")) as opened:
                image = native_services.mapped_image(1234, "/receipt/helper")
                self.assertEqual([call.args[0] for call in reads.call_args_list], [Path("/expected/helper"), image_path])
                opened.assert_called_once_with(image_path, "rb")
                self.assertEqual(image, {"pid": 1234, "inode": 22, "device": 11, "sha256": hashlib.sha256(b"helper").hexdigest()})
            if system == "Darwin":
                darwin.assert_called_once_with(1234, expected)
            else:
                darwin.assert_not_called()
        self.native_command.assert_not_called()

    def test_observed_exit_records_status_or_signal_without_polling_again(self):
        for returncode in (1, -15, 143):
            driver = self.exited_driver(returncode)
            with self.assertRaisesRegex(RuntimeError, "^ordered host driver exited: canary$"):
                driver.call("inspect")
            saved = driver.diagnostics.reserved["driverExit"]["details"]
            self.assertEqual(saved["returncode"], returncode)
            self.assertEqual(saved["signal"], 15 if returncode < 0 else None)
            self.assertEqual(saved["pid"], 5678)
            self.assertEqual(saved["stderrFile"], "canary-driver.log")
            driver.process.poll.assert_called_once_with()

    def exited_driver(self, returncode):
        driver = object.__new__(installed_order.Driver)
        driver.account = {"role": "canary"}
        driver.diagnostics = trace.Trace("canary")
        driver.fixture_deadline_ns = driver.cleanup_deadline_ns = self.deadline
        driver.process = SimpleNamespace(pid=5678, poll=Mock(return_value=returncode))
        driver.write_request = Mock()
        return driver

    def test_exit_status_survives_unavailable_optional_capture(self):
        driver = self.exited_driver(-15)
        with patch.object(driver.diagnostics, "retain_failure", side_effect=ValueError("reservation bound")):
            with self.assertRaisesRegex(RuntimeError, "^ordered host driver exited: canary$") as caught:
                driver.call("inspect")
        self.assertIn("pid=5678 returncode=-15 signal=15", " ".join(caught.exception.__notes__))
        driver.process.poll.assert_called_once_with()


class OrderTraceTest(unittest.TestCase):
    def test_competing_failure_retains_producer_before_parent_selected_exception(self):
        produced = threading.Event()
        parent_error = ValueError("canary full inspection failed")
        drivers = [CompetingDriver(role, produced, parent_error) for role in ("canary", "sleep", "burst")]
        original = installed_order.fail_pending
        def propagate(future, error):
            if "fixture missed" in str(error):
                produced.set()
            return original(future, error)
        record = {}
        with patch.object(installed_order, "fail_pending", side_effect=propagate):
            with self.assertRaises(ValueError) as caught:
                installed_order.release_latest(type("Origin", (), {"head": lambda *_: None})(), Path("/unused"), drivers, record)
        self.assertIs(caught.exception, parent_error)
        self.assertIn("orderDiagnostics", record)
        direct = record["orderDiagnostics"]["burst"]["failures"]
        self.assertEqual(direct[0]["kind"], "producer")
        self.assertIn("fixture missed", direct[0]["message"])
        self.assertNotIn("predecessor.result", direct[0]["traceback"])
        self.assertEqual(record["orderDiagnostics"]["sleep"]["failures"][0]["kind"], "propagated")
        selected = record["orderDiagnostics"]["canary"]["failures"][-1]
        self.assertEqual((selected["kind"], selected["type"]), ("parent-selected", "ValueError"))
        self.assertFalse(any(thread.name.startswith("ordered-opportunity") for thread in threading.enumerate()))

    def test_torn_multi_read_waits_for_matching_commit_before_handoff(self):
        old_read, published = threading.Event(), threading.Event()
        latest = installed_releases.VERSIONS[-1]
        live = observation(installed_releases.VERSIONS[0])
        live["state"]["owned"] = {"request": "request", "generation": 2}
        driver = SimpleNamespace(account={"role": "canary"}, diagnostics=trace.Trace("canary"))
        def read(operation, **fields):
            entries = copy.deepcopy(live["entries"])
            if fields.get("active") is False:
                old_read.set()
                self.assertTrue(published.wait(2))
            value = copy.deepcopy(live)
            value["entries"] = entries
            return value
        driver.call = read
        def publish_after_old_ledger():
            self.assertTrue(old_read.wait(2))
            live.update(observation(latest))
            published.set()
        writer = threading.Thread(target=publish_after_old_ledger)
        writer.start()
        handoff = Future()
        try:
            installed_order.await_version(driver, latest, "committed", 90, lambda reply: trace.publish(driver, handoff, reply))
        finally:
            published.set()
            writer.join(2)
        self.assertFalse(writer.is_alive())
        self.assertEqual(live["entries"][0]["version"], latest)
        captured = json.loads(driver.diagnostics.handoff_bytes)
        self.assertTrue(captured["correlation"]["matchingLedgerObserved"])
        self.assertFalse(driver.diagnostics.reserved["lastObservedIneligible"]["details"]["correlation"]["matchingLedgerObserved"])
        missing = copy.deepcopy(captured["observation"])
        missing["state"]["owned"] = {"request": "request", "generation": 2, "metadata": "pin"}
        missing["state"]["owned"]["metadata"] = "another-pin"
        self.assertFalse(trace.correlation(missing)["ledgerMetadataMatchesOwned"])
        self.assertEqual(captured["observation"], handoff.result())
        self.assertEqual(installed_order.commit_time_ns(handoff.result(), latest), installed_order.timestamp_ns(live["entries"][0]["committedAt"]))

    def test_subfive_second_nanosecond_gap_is_rejected_before_handoff(self):
        predecessor = observation(installed_releases.VERSIONS[-1], "2026-01-01T00:00:00.000000999Z")
        dependent = observation(installed_releases.VERSIONS[-1], "2026-01-01T00:00:05.000000000Z")
        target = installed_releases.VERSIONS[-1]
        driver = SimpleNamespace(diagnostics=trace.Trace("burst"))
        cutoff = installed_order.commit_time_ns(predecessor, target) + installed_order.SOAK_NS
        trace.cutoff(driver, predecessor, target, cutoff)
        published = []
        with self.assertRaisesRegex(RuntimeError, "ignored the configured predecessor soak"):
            installed_order.publish_soaked_commit(dependent, target, cutoff, published.append)
        self.assertEqual(published, [])
        self.assertEqual(5_000_000_000 - 999, 4_999_999_001)
        raw = driver.diagnostics.reserved["cutoff"]["details"]["rawPredecessor"]["committedAt"]
        self.assertEqual(raw, "2026-01-01T00:00:00.000000999Z")
        self.assertEqual(driver.diagnostics.reserved["cutoff"]["details"]["parserResolutionNs"], 1)
        dependent["entries"][0]["committedAt"] = "2026-01-01T00:00:05.000000999Z"
        installed_order.publish_soaked_commit(dependent, target, cutoff, published.append)
        self.assertEqual(published, [dependent])

    def test_commit_handoff_rejects_stale_request_generation_and_duplicate_entries(self):
        target = installed_releases.VERSIONS[-1]
        for key, stale in (("request", "previous-request"), ("generation", 1), ("version", installed_releases.VERSIONS[0])):
            value = observation(target)
            value["entries"][0][key] = stale
            self.assertFalse(installed_order.target_committed(value, target))
        value = observation(target)
        value["entries"].append(copy.deepcopy(value["entries"][0]))
        self.assertFalse(installed_order.target_committed(value, target))

    def test_native_timestamp_parser_preserves_offsets_and_nine_digit_fractions(self):
        exact = installed_order.timestamp_ns("2026-01-01T00:00:00.000000999Z")
        self.assertEqual(exact - installed_order.timestamp_ns("2026-01-01T00:00:00Z"), 999)
        self.assertEqual(exact, installed_order.timestamp_ns("2026-01-01T01:00:00.000000999+01:00"))
        with self.assertRaises(ValueError):
            installed_order.timestamp_ns("2026-01-01T00:00:00.0000000001Z")

    def test_completed_hold_at_exact_nanosecond_cutoff_is_rejected(self):
        cutoff = installed_order.timestamp_ns("2026-01-01T00:00:00.000000999Z")
        with patch.object(installed_order.time, "time_ns", return_value=cutoff), self.assertRaisesRegex(RuntimeError, "only observed after expiry"):
            installed_order.require_preexpiry_read({}, cutoff)
        with patch.object(installed_order.time, "time_ns", return_value=cutoff - 1):
            self.assertEqual(installed_order.require_preexpiry_read({}, cutoff), {})

    def test_delayed_source_eligibility_is_distinct_from_delayed_read_completion(self):
        for cause in ("eligibility", "read-completion"):
            self.check_delayed_source(cause)

    def check_delayed_source(self, cause):
        clock = SyntheticClock()
        target = installed_releases.VERSIONS[-1]
        reply = observation(target, clock.now().isoformat())
        driver = SimpleNamespace(account={"role": "canary"}, diagnostics=trace.Trace("canary"))
        calls, seen = [], []
        def call(operation, **fields):
            calls.append((operation, fields))
            value = copy.deepcopy(reply)
            if fields.get("active") is False:
                return delayed_reply(clock, value, cause, len(calls))
            return value
        driver.call = call
        with patch.object(installed_order, "time", clock.api()), patch.object(installed_order, "datetime", clock.dates()), patch.object(trace, "time", clock.api()):
            installed_order.await_version(driver, target, "committed", 90, lambda value: seen.append((clock.seconds, value)))
            with self.assertRaisesRegex(RuntimeError, "fixture missed"):
                installed_order.exercise_soak(None, seen[0][1], installed_releases.VERSIONS[0], target)
        self.assertGreaterEqual(seen[0][0], 6)
        samples = [row["last"]["details"]["settled"] for key, row in driver.diagnostics.events.items() if key.startswith("eligibility.sample:")]
        self.assertEqual(False in samples, cause == "eligibility")
        self.assertFalse(any(operation == "stop" for operation, _fields in calls))

    def test_delayed_consumer_wake_preserves_publication_boundary(self):
        clock, entered, wake = SyntheticClock(), threading.Event(), threading.Event()
        target = installed_releases.VERSIONS[-1]
        producer = SimpleNamespace(diagnostics=trace.Trace("canary"))
        driver = SimpleNamespace(account={"role": "burst", "id": "burst"}, diagnostics=trace.Trace("burst"),
            call=lambda *_args, **_kwargs: observation(installed_releases.VERSIONS[0]))
        handoff, errors = Future(), []
        class DelayedFuture:
            def result(self):
                value = handoff.result()
                entered.set()
                if not wake.wait(2):
                    raise AssertionError("consumer wake barrier expired")
                return value
        def consume():
            try:
                installed_order.soak_controller(driver, DelayedFuture(), None, Future(), installed_releases.VERSIONS[0], target)
            except BaseException as error:
                errors.append(error)
        with patch.object(installed_order, "datetime", clock.dates()), patch.object(trace, "time", clock.api()):
            trace.publish(producer, handoff, observation(target, clock.now().isoformat()))
            consumer = threading.Thread(target=consume)
            consumer.start()
            try:
                self.assertTrue(entered.wait(2))
                clock.advance(6)
            finally:
                wake.set()
                consumer.join(2)
        self.assertFalse(consumer.is_alive())
        self.assertIn("fixture missed", str(errors[0]))
        published = producer.diagnostics.events["future.publish:::"]["last"]["end"]["monotonicNs"]
        resumed = driver.diagnostics.events["future.wake:::"]["last"]["end"]["monotonicNs"]
        self.assertEqual(resumed - published, 6_000_000_000)

    def test_diagnostic_write_failure_preserves_original_exception_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            self.check_diagnostic_write_failure(Path(directory))

    def check_diagnostic_write_failure(self, root):
        driver = SimpleNamespace(account={"role": "canary"}, close=lambda: None, diagnostics=trace.Trace("canary"))
        record = {"passed": False, "error": "original product failure"}
        primary = ValueError(record["error"])
        original = Path.write_bytes
        def write(path, data):
            if path.name.endswith("-timeline.json"):
                raise OSError("diagnostic write rejected")
            return original(path, data)
        with patch.object(Path, "write_bytes", write), self.assertRaises(ValueError) as caught:
            try:
                raise primary
            finally:
                installed_order.teardown(SimpleNamespace(close=lambda: None), [driver], record, root)
        self.assertIs(caught.exception, primary)
        summary = record["orderDiagnostics"]["canary"]
        self.assertFalse(summary["complete"])
        self.assertEqual(summary["failures"][0]["kind"], "export")

    def test_cleanup_failure_keeps_existing_precedence_and_separate_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            cleanup_error = OSError("native cleanup failed")
            def close():
                raise cleanup_error
            driver = SimpleNamespace(account={"role": "canary"}, close=close, diagnostics=trace.Trace("canary"))
            record = {"passed": False, "error": "original product failure"}
            with self.assertRaisesRegex(RuntimeError, "original product failure; cleanup failed") as caught:
                installed_order.teardown(SimpleNamespace(close=lambda: None), [driver], record, Path(directory))
            self.assertIn("native cleanup failed", str(caught.exception.__cause__))
            failures = record["orderDiagnostics"]["canary"]["failures"]
            self.assertEqual(failures[-1]["kind"], "cleanup")
            self.assertFalse(record["passed"])

    def test_incomplete_diagnostics_cannot_create_a_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            driver = SimpleNamespace(account={"role": "canary"}, close=lambda: None, diagnostics=trace.Trace("canary"))
            driver.diagnostics.complete = False
            record = {"passed": True}
            with self.assertRaisesRegex(RuntimeError, "coverage incomplete"):
                installed_order.teardown(SimpleNamespace(close=lambda: None), [driver], record, Path(directory))
            self.assertFalse(json.loads((Path(directory) / "ordered-native.json").read_text())["passed"])

    def test_export_failure_is_separate_and_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            root, files, owner = Path(directory), {}, trace.Trace("canary")
            (root / trace.TIMELINE).write_bytes(b"x" * (trace.HISTORY_LIMIT + 1))
            trace.account_export(root, files)
            decoded = {key: bytes.fromhex(value) for key, value in files.items()}
            trace.export_status(decoded, owner)
            self.assertFalse(owner.complete)
            self.assertEqual(owner.failures[-1]["kind"], "export")
            self.assertLess(len(trace.encoded(files)), 4096)
            driver = SimpleNamespace(account={"role": "canary"}, close=lambda: None, diagnostics=owner)
            self.assert_primary_survives(root, driver)

    def assert_primary_survives(self, root, driver):
        primary = ValueError("original product failure")
        record = {"passed": False, "error": str(primary)}
        with self.assertRaises(ValueError) as caught:
            try:
                raise primary
            finally:
                installed_order.teardown(SimpleNamespace(close=lambda: None), [driver], record, root)
        self.assertIs(caught.exception, primary)

    def test_failed_native_read_keeps_available_boundary_data_and_original_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            self.check_native_read(Path(directory), "ledger", 1)

    def test_nonledger_inspection_does_not_require_a_ledger_sidecar(self):
        with tempfile.TemporaryDirectory() as directory:
            self.check_native_read(Path(directory), "inspect", 0)

    def check_native_read(self, root, kind, returncode):
        owner = trace.Trace("account")
        origin = object.__new__(installed_order_host.Origin)
        origin.root, origin.diagnostics = root, owner
        def process(*_args, **fields):
            if fields["env"]["MESH_INSTALLED_READ_KIND"] == "ledger":
                (root / trace.READER).write_text(json.dumps({"complete": True, "spans": [{"phase": "ledger.read", "success": False}]}))
            return SimpleNamespace(returncode=returncode, stdout="original native failure", stderr="")
        with patch.object(installed_order_host.subprocess, "run", side_effect=process):
            if returncode:
                self.assert_native_failure(origin, root, kind)
            else:
                origin.read_host(kind, root / "state", root / "out.json")
        self.assertTrue(owner.complete)
        if kind == "ledger":
            self.assertEqual(owner.reader["spans"][0]["success"], False)
        self.assertIn("native.reader:" + kind + "::", owner.events)

    def assert_native_failure(self, origin, root, kind):
        with self.assertRaisesRegex(RuntimeError, "original native failure"):
            origin.read_host(kind, root / "state", root / "out.json")

    def test_incomplete_available_sidecar_is_exported_with_failed_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            root, files = Path(directory), {}
            owner = trace.Trace("account")
            owner.complete = False
            owner.failure(ValueError("producer original"), "dispatch", "producer")
            owner.write(root / trace.TIMELINE)
            trace.account_export(root, files)
            retained = json.loads(bytes.fromhex(files[trace.TIMELINE]))
            self.assertEqual(retained["failures"][0]["message"], "producer original")
            self.assertFalse(json.loads(bytes.fromhex(files[trace.STATUS]))["complete"])

    def test_handoff_has_separate_budget_and_failure_records_survive_history_bound(self):
        owner = trace.Trace("canary")
        value = observation(installed_releases.VERSIONS[-1])
        value["extra"] = "x" * 100_000
        owner.handoff(value)
        self.assertGreater(len(owner.handoff_bytes), trace.HISTORY_LIMIT)
        self.assertTrue(owner.complete)
        owner.event("oversized", blob="x" * trace.HISTORY_LIMIT)
        owner.failure(ValueError("original"), "controller", "producer")
        summary = owner.summary()
        self.assertLessEqual(len(trace.encoded(summary)), trace.HISTORY_LIMIT)
        self.assertEqual(summary["failures"][0]["message"], "original")
        self.assertIn("handoff", summary["reserved"])
        self.assertFalse(summary["complete"])

    def test_unicode_failures_fit_encoded_budget_and_survive_account_export(self):
        owner = trace.Trace("canary")
        failures = [RuntimeError("\U0001f680" * 4096) for _ in range(8)]
        for failure in failures:
            owner.failure(failure, "dispatch", "operation")
        owner.events["forced-overflow"] = {"payload": "x" * trace.HISTORY_LIMIT}
        summary = owner.summary()
        self.assertLessEqual(len(trace.encoded(summary)), trace.HISTORY_LIMIT)
        self.assertFalse(summary["complete"])
        self.assertTrue(summary["historyOmitted"])
        self.check_exported_unicode(owner, failures)

    def check_exported_unicode(self, owner, failures):
        with tempfile.TemporaryDirectory() as directory:
            root, files = Path(directory), {}
            owner.write(root / trace.TIMELINE)
            trace.account_export(root, files)
        payload = bytes.fromhex(files[trace.TIMELINE])
        self.assertLessEqual(len(payload), trace.HISTORY_LIMIT)
        retained = json.loads(payload)["failures"]
        self.assertEqual([row["exceptionId"] for row in retained], [id(error) for error in failures])
        self.assertEqual(len(retained), 8)
        self.assertTrue(all(row["textTruncated"] for row in retained))
        self.assertTrue(all(len(trace.encoded(row["message"])) <= 512 for row in retained))
        self.assertTrue(all(len(trace.encoded(row["traceback"])) <= 2048 for row in retained))
        self.assertFalse(json.loads(bytes.fromhex(files[trace.STATUS]))["complete"])

    def test_reserved_carrier_is_observed_without_rebasing_or_semantics(self):
        with tempfile.TemporaryDirectory() as directory:
            class Endpoint:
                def __init__(self):
                    self.diagnostics = trace.Trace("mock")
                    self.case = SimpleNamespace(root=Path(directory))
                @trace.calls
                def call(self, operation, timeout=30, **fields):
                    return fields["fixtureDeadlineNs"], timeout
                @trace.dispatched
                def dispatch(self, operation, value):
                    return value["fixtureDeadlineNs"]
            endpoint = Endpoint()
            self.assertEqual(endpoint.call("inspect", fixtureDeadlineNs=123), (123, 30))
            self.assertEqual(endpoint.dispatch("inspect", {"fixtureDeadlineNs": 123}), 123)
            for row in endpoint.diagnostics.events.values():
                self.assertEqual(row["last"]["details"]["fixtureDeadlineNs"], 123)


class SyntheticClock:
    def __init__(self):
        self.seconds = 0
        self.base = datetime(2026, 1, 1, tzinfo=timezone.utc)

    def advance(self, seconds):
        self.seconds += seconds

    def now(self):
        return self.base + timedelta(seconds=self.seconds)

    def api(self):
        return SimpleNamespace(monotonic=lambda: self.seconds, sleep=self.advance,
            monotonic_ns=lambda: int(self.seconds * 1_000_000_000), time_ns=lambda: installed_order.timestamp_ns(self.now().isoformat()))

    def dates(self):
        return SimpleNamespace(now=lambda _: self.now(), fromisoformat=datetime.fromisoformat)


def delayed_reply(clock, value, cause, call_number):
    if cause == "eligibility" and call_number == 1:
        value["state"]["owned"] = {"request": "request", "generation": 2}
        return value
    clock.advance(6)
    return value
