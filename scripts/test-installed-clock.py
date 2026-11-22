"""Pure/mocked fixture observations, hang caps, handoff and cleanup controls."""
import io
import hashlib
import json
import os
from pathlib import Path
import select
import signal
import shutil
import shlex
import subprocess
import tempfile
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, Mock, patch

from fixtures import installed_prepared
from fixtures import installed_clock as clocks, installed_account_run, installed_releases, installed_order, installed_order_trace, installed_order_host, installed_native, installed_accounts, installed_paths, native_services


class Time:
    def __init__(self):
        self.value = 100 * clocks.SECOND

    def __call__(self):
        return self.value

    def advance(self, duration):
        self.value += duration


class ClockTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="clock-control-")
        self.addCleanup(self.temp.cleanup)
        self.root, self.now = Path(self.temp.name), Time()

    def clock(self, **kwargs):
        clock = clocks.Clock({"scenario": "chain"}, now=self.now)
        if "hang_ns" in kwargs:
            clock.deadline_ns = self.now() + kwargs["hang_ns"]
        return clock

    def test_native_wait_preserves_parent_clock_expiry(self):
        clock = self.clock(hang_ns=109 * clocks.MILLISECOND)
        predicate = Mock(return_value=False)
        with clock.activate(), patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(native_services.time, "sleep", side_effect=self.advance_wait):
            with self.assertRaisesRegex(clocks.ClockExpired, "installed fixture hang cap reached") as raised:
                native_services.wait_for(predicate, "installed signature exporter did not start")
        self.assertEqual(raised.exception.__notes__, ["installed signature exporter did not start"])
        self.assertEqual(clock.failures[0]["type"], "ClockExpired")
        self.assertEqual(predicate.call_count, 3)

    def test_native_wait_preserves_handoff_expiry(self):
        deadline = self.now() + 109 * clocks.MILLISECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(native_services.time, "sleep", side_effect=self.advance_wait), clocks.handoff(deadline):
            with self.assertRaisesRegex(clocks.ClockExpired, "parent fixture deadline expired"):
                native_services.wait_for(lambda: False, "handoff wait did not finish")

    def test_native_mapping_wait_preserves_exact_phase_expiry(self):
        clock = self.clock(hang_ns=109 * clocks.MILLISECOND)
        observed, record = [], {}
        def expired_mapping(*_args):
            self.now.advance(110 * clocks.MILLISECOND)
            try:
                clock.check()
            except clocks.ClockExpired as error:
                observed.append(error)
                raise
        with clock.activate(), patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(native_services, "mapped_image", side_effect=expired_mapping):
            with self.assertRaises(clocks.ClockExpired) as raised:
                native_services.wait_for(lambda: native_services.control_image(123, record), "mapping control")
        self.assertIs(raised.exception, observed[0])
        self.assertEqual(record, {})

    def test_native_mapping_error_remains_a_pending_observation(self):
        record = {}
        with patch.object(native_services, "mapped_image", side_effect=RuntimeError("image not observed")):
            self.assertIsNone(native_services.control_image(123, record))
        self.assertEqual(record, {"lastMappingError": "image not observed"})

    def test_native_wait_keeps_local_timeout_error(self):
        start = self.now()
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(native_services.time, "sleep", side_effect=self.advance_wait):
            with self.assertRaisesRegex(RuntimeError, "^local wait did not finish$") as raised:
                native_services.wait_for(lambda: False, "local wait did not finish")
        self.assertIs(type(raised.exception), RuntimeError)
        self.assertEqual(self.now() - start, clocks.HANG_SECONDS * clocks.SECOND)

    def advance_wait(self, seconds):
        self.now.advance(round(seconds * clocks.SECOND))

    def test_native_control_child_entry_works_without_package_path(self):
        marker = self.root / "native-child.json"
        arguments = [str(Path(sys.executable).resolve()), str(Path(native_services.__file__).resolve()), "child", str(marker), "owned-control"]
        environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "PYTHONDONTWRITEBYTECODE": "1"}
        process = None
        try:
            process = subprocess.Popen(arguments, cwd=self.root, env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            self.wait_native_child(process, marker)
            self.assertEqual(json.loads(marker.read_text()), {"pid": process.pid, "token": "owned-control"})
            native_services.wait_for(lambda: native_services.control_image(process.pid, {}), "control child did not exec sleep")
        finally:
            self.stop_native_child(process)

    def wait_native_child(self, process, marker):
        end = clocks.time.monotonic_ns() + 10 * clocks.SECOND
        while not marker.exists():
            if process.poll() is not None:
                self.fail(process.communicate(timeout=2)[1].decode("utf-8", "replace"))
            clocks.time.sleep(clocks.remaining_until(end, 0.01))

    def stop_native_child(self, process):
        if process is None:
            return
        if process.poll() is None:
            process.terminate()
        process.communicate(timeout=2)

    def export(self, clock, cleaned=True):
        return clock.export(self.root / "clock.json", cleaned, {"source": "exact tested source"})


    def test_missing_executable_clock_fails_closed(self):
        with self.assertRaises(ValueError):
            clocks.positive(0)
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root)
        with patch.object(installed_releases.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "active fixture"):
                releases.start_signer()
        run.assert_not_called()
        releases.close()


    def test_outcomes_accept_without_timing_plan(self):
        clock = self.clock()
        with clock.activate():
            with clocks.operation("build:v9001"):
                self.now.advance(clocks.SECOND)
        value = self.export(clock)
        self.assertTrue(value["accepted"])
        self.assertTrue(value["measurement_completed"])
        self.assertEqual(value["disjoint_ns"], value["command_ns"])
        self.assertEqual(value["leaves"][0]["end_ns"], value["leaves"][1]["start_ns"])

    def test_prepared_roles_are_explicit_and_nonprepared_workload_is_unchanged(self):
        self.assertEqual(clocks.prepared_workload(SimpleNamespace()), {})
        self.assertEqual(clocks.prepared_workload(SimpleNamespace(export_prepared=self.root)), {"preparedRole": "prepare"})
        for values in ({"prepared_releases": self.root}, {"installed_slice": self.root},
                       {"export_prepared": self.root, "prepared_releases": self.root}):
            with self.assertRaises(ValueError):
                clocks.prepared_workload(SimpleNamespace(**values))

    def test_prepared_bundle_identity_binds_execution_not_reusable_timing(self):
        args, selection = self.prepared_arguments()
        with patch.dict(os.environ, GITHUB_RUN_ID="run", GITHUB_JOB="job"):
            clock = clocks.from_arguments(args, "linux/amd64")
        identity = hashlib.sha256((args.prepared_releases / "prepared.json").read_bytes()).hexdigest()
        self.assertEqual(clock.provenance["preparedId"], identity)
        self.assertNotIn("preparedId", clock.workload)
        self.assertEqual(clock.workload["preparedRole"], "consume")
        self.assertEqual(clock.workload["installedSlice"], selection)
        self.assertTrue(clock.workload["grouped"])
        self.assertEqual(clock.workload["publishedRegistrySha256"], "a" * 64)

    def prepared_arguments(self):
        bundle = self.root / "bundle"
        bundle.mkdir()
        (bundle / "prepared.json").write_text(json.dumps({"schema": 1, "native": "linux/amd64", "host": {"name": "pc"},
            "mode": "chain", "files": {"placeholder": {}}, "groups": [{"index": 0}], "publishedRegistrySha256": "a" * 64,
            "harnessSource": "b" * 40, "preparationSource": "c" * 40}))
        selection = {"schema": 1, "select": [{"profile": "chain-none", "group_indices": [0]}], "readers": []}
        selected = self.root / "slice.json"
        selected.write_text(json.dumps(selection))
        args = SimpleNamespace(source_host="pc", scenario=None, worker_registry=None, fresh_managed=False, ordered_hosts=False, format_waypoint=False, signed_chain=True,
            export_prepared=None, prepared_releases=bundle, installed_slice=selected)
        return args, selection

    def test_prepared_consumer_rejects_other_registry_or_context(self):
        args, _selection = self.prepared_arguments()
        args.worker_registry = self.root / "other-registry.json"
        args.worker_registry.write_text("{}")
        with self.assertRaisesRegex(ValueError, "supplied registry"):
            clocks.from_arguments(args, "linux/amd64")
        args.worker_registry = None
        args.source_host = "other-host"
        with self.assertRaisesRegex(ValueError, "host or mode"):
            clocks.from_arguments(args, "linux/amd64")

    def test_job_finish_uses_explicit_support_fixture_clock(self):
        origin = self.root / "job-origin.json"
        origin.write_text(json.dumps({"origin_ns": clocks.SECOND}))
        fixture = self.root / "fresh-clock.json"
        fixture.write_text(json.dumps({"origin_ns": 2 * clocks.SECOND, "end_ns": 3 * clocks.SECOND,
                                       "duration_record_only": True}))
        output = io.StringIO()
        with patch.object(clocks.time, "monotonic_ns", return_value=4 * clocks.SECOND), patch("sys.stdout", output):
            clocks.job_record("job-finish", origin, fixture)
        interval = json.loads(output.getvalue().split("CLOCK_JOB_INTERVAL ", 1)[1])
        self.assertEqual(interval["external_observed_ns"], 2 * clocks.SECOND)
        self.assertFalse(interval["external_complete"])


    def test_operation_sample_starts_at_its_leaf_boundary(self):
        clock = self.clock()
        def ticks():
            self.now.advance(1)
            return self.now()
        clock.now = ticks
        with clock.operation("observed"):
            self.now.advance(100)
        self.assertEqual(clock.samples[0]["start_ns"], clock.rows[1]["start_ns"])
        self.assertEqual(clock.samples[0]["end_ns"], clock.rows[1]["end_ns"])


    def test_wall_clock_changes_do_not_change_elapsed(self):
        clock = self.clock()
        with patch.object(clocks.time, "time", return_value=-10**20):
            self.now.advance(clocks.SECOND)
            clock.check()
        self.assertEqual(self.export(clock)["command_ns"], clocks.SECOND)

    def test_timeout_latches_original_and_disarms_work_watchdog(self):
        clock = self.clock()
        clock.watchdog = True
        with patch.object(clocks.signal, "setitimer") as timer:
            self.now.advance(clocks.HANG_SECONDS * clocks.SECOND)
            with self.assertRaises(clocks.ClockExpired):
                clock.check()
            first = clock.cleanup_deadline_ns
            clock.fail(RuntimeError("cleanup also failed"))
        self.assertEqual(clock.cleanup_deadline_ns, first)
        timer.assert_any_call(signal.ITIMER_REAL, 0)
        timer.assert_called_with(signal.ITIMER_REAL, clocks.CLEANUP_SECONDS)
        value = self.export(clock, False)
        self.assertEqual(value["failures"][0]["type"], "ClockExpired")
        self.assertFalse(value["measurement_completed"])

    def test_operation_sample_uses_one_end_observation(self):
        def ticks():
            self.now.advance(101)
            return self.now()
        clock = clocks.Clock({"scenario": "chain"}, now=ticks)
        with clock.operation("observed"):
            self.now.advance(1000)
        row = clock.samples[-1]
        self.assertEqual(row["elapsed_ns"], row["end_ns"] - row["start_ns"])

    def test_clean_local_sample_survives_later_failure(self):
        clock = self.clock()
        with clock.operation("installed-group:0") as key:
            self.now.advance(clocks.SECOND)
            clock.clean_keys.add(key)
        clock.fail(RuntimeError("later group failed"))
        sample = self.export(clock, False)["samples"][0]
        self.assertTrue(sample["eligible"])
        self.assertTrue(sample["cleanup_verified"])

    def test_missing_matrix_key_cannot_finish_green(self):
        clock = self.clock()
        clock.expected_samples.add("installed-group:missing#1")
        value = self.export(clock)
        self.assertFalse(value["measurement_completed"])
        self.assertEqual(value["missing_sample_keys"], ["installed-group:missing#1"])

    def test_first_failure_blocks_new_work_and_keeps_cleanup_bound(self):
        clock = self.clock()
        clock.fail(RuntimeError("original failure"))
        end = clock.cleanup_deadline_ns
        with self.assertRaisesRegex(clocks.ClockExpired, "new fixture work"):
            with clock.operation("build:another"):
                self.fail("new build started")
        self.now.advance(clocks.SECOND)
        clock.begin_cleanup()
        self.assertEqual(clock.cleanup_deadline_ns, end)

    def test_imported_clean_native_sample_survives_failed_command_cleanup(self):
        clock = self.clock()
        clock.samples.append({"key": "native-group:0", "completed": True, "cleanup_verified": True, "elapsed_ns": clocks.SECOND})
        clock.fail(RuntimeError("later native failure"))
        self.assertTrue(self.export(clock, False)["samples"][0]["eligible"])

    def test_delayed_handoff_cannot_restart_remaining_duration(self):
        end = self.now() + clocks.SECOND
        self.now.advance(2 * clocks.SECOND)
        with patch.object(clocks.time, "monotonic_ns", self.now), self.assertRaisesRegex(clocks.ClockExpired, "handoff"):
            with clocks.handoff(end):
                self.fail("expired work entered")

    def test_socket_timeout_obeys_absolute_parent(self):
        with patch.object(clocks.time, "monotonic_ns", self.now):
            with clocks.handoff(self.now() + clocks.SECOND):
                self.now.advance(750_000_000)
                self.assertEqual(clocks.timeout(15), 0.25)

    def test_nested_handoff_keeps_shorter_parent_and_restores_it(self):
        end = self.now() + clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), clocks.handoff(end):
            with clocks.handoff(end + clocks.SECOND):
                self.assertEqual(clocks.HANDOFF.get(), end)
                self.now.advance(clocks.SECOND // 2)
                self.assertEqual(clocks.timeout(10), 0.5)
            self.assertEqual(clocks.HANDOFF.get(), end)
        self.assertIsNone(clocks.HANDOFF.get())

    def test_handoff_expiring_during_alarm_setup_restores_context_and_signal(self):
        token = clocks.HANDOFF.set(None)
        self.addCleanup(clocks.HANDOFF.reset, token)
        end = self.now() + clocks.SECOND
        observations = (self.now(), self.now(), end)
        with patch.object(clocks.time, "monotonic_ns", side_effect=observations), \
                patch.object(clocks.signal, "getsignal", return_value=signal.SIG_DFL), \
                patch.object(clocks.signal, "getitimer", return_value=(0, 0)), \
                patch.object(clocks.signal, "signal") as handler, patch.object(clocks.signal, "setitimer") as timer:
            with self.assertRaises(clocks.ClockExpired), clocks.handoff(end, watchdog=True):
                self.fail("expired handoff entered its child")
        self.assertIsNone(clocks.HANDOFF.get())
        handler.assert_not_called()
        timer.assert_not_called()

    def test_watchdog_handoff_restores_previous_alarm_with_elapsed_time_removed(self):
        end = self.now() + clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(clocks.signal, "getsignal", return_value=signal.SIG_IGN), \
                patch.object(clocks.signal, "getitimer", return_value=(5, 0)), \
                patch.object(clocks.signal, "signal") as handler, patch.object(clocks.signal, "setitimer") as timer:
            with clocks.handoff(end, watchdog=True):
                self.now.advance(clocks.SECOND // 2)
        self.assertEqual([call.args for call in timer.call_args_list],
                         [(signal.ITIMER_REAL, 1), (signal.ITIMER_REAL, 0), (signal.ITIMER_REAL, 4.5)])
        self.assertEqual([call.args for call in handler.call_args_list],
                         [(signal.SIGALRM, clocks.handoff_expired), (signal.SIGALRM, signal.SIG_IGN)])
        self.assertIsNone(clocks.HANDOFF.get())

    def test_watchdog_expiring_after_timer_install_restores_previous_handler(self):
        error = clocks.ClockExpired("timer fired before installation returned")
        end = self.now() + clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(clocks.signal, "getsignal", return_value=signal.SIG_IGN), \
                patch.object(clocks.signal, "getitimer", return_value=(0, 0)), \
                patch.object(clocks.signal, "signal") as handler, \
                patch.object(clocks.signal, "setitimer", side_effect=[error, None]) as timer:
            with self.assertRaises(clocks.ClockExpired) as raised, clocks.handoff(end, watchdog=True):
                self.fail("expired handoff entered its child")
        self.assertIs(raised.exception, error)
        self.assertIsNone(clocks.HANDOFF.get())
        self.assertEqual(handler.call_args_list[-1].args, (signal.SIGALRM, signal.SIG_IGN))
        self.assertEqual(timer.call_args_list[-1].args, (signal.ITIMER_REAL, 0))

    def test_native_partial_export_and_clean_completion(self):
        handoff = self.root / "handoff.json"
        handoff.write_text(json.dumps({"deadline_ns": self.now() + 10 * clocks.SECOND, "mode": "diagnostic", "prefix": "proof#1"}))
        with patch.dict(os.environ, MESH_INSTALLED_CLOCK_HANDOFF=str(handoff)), patch.object(clocks.time, "monotonic_ns", self.now):
            self.native_groups()
        samples = json.loads((self.root / "fixture-group-clock.json").read_text())["samples"]
        self.assertTrue(samples[0]["completed"])
        self.assertFalse(samples[1]["completed"])
        self.assertEqual(samples[0]["elapsed_ns"], clocks.SECOND)

    def native_groups(self):
        with clocks.native_group(self.root, {"index": 0}):
            self.now.advance(clocks.SECOND)
        with self.assertRaisesRegex(RuntimeError, "cleanup unconfirmed"):
            with clocks.native_group(self.root, {"index": 1}):
                raise RuntimeError("cleanup unconfirmed")

    def test_leader_exit_still_stops_owned_descendants(self):
        process = Mock(pid=123)
        process.poll.return_value = 0
        ownership = Mock()
        ownership.live.return_value = {}
        with patch.object(clocks, "send_group") as send, patch.object(clocks, "wait_group") as wait:
            clocks.stop_group(process, clocks.time.monotonic_ns() + 10 * clocks.SECOND, ownership=ownership)
        self.assertEqual(send.call_count, 2)
        send.assert_any_call(123, signal.SIGKILL)
        self.assertTrue(wait.call_args.kwargs["require_absent"])
        ownership.verify.assert_called_once()

    def test_group_probe_uses_its_absolute_cleanup_deadline(self):
        deadline = self.now() + 15 * clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(clocks.subprocess, "run", return_value=Mock(stdout="")) as probe:
            self.assertFalse(clocks.group_alive(123, deadline))
        self.assertEqual(probe.call_args.kwargs["timeout"], 15)

    def test_slow_term_probe_still_escalates_inside_cleanup_deadline(self):
        process, ownership = Mock(pid=123), Mock()
        ownership.live.return_value = {}
        deadline = self.now() + 20 * clocks.SECOND
        states = iter((True, False))
        def slow_alive(_group, _deadline=None):
            self.now.advance(2010 * clocks.MILLISECOND)
            return next(states)
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(clocks, "group_alive", side_effect=slow_alive), \
                patch.object(clocks, "send_group") as send, patch.object(clocks.time, "sleep") as sleep:
            clocks.stop_group(process, deadline, ownership=ownership)
        self.assertEqual([call.args for call in send.call_args_list], [(123, signal.SIGTERM), (123, signal.SIGKILL)])
        self.assertLess(self.now(), deadline)
        sleep.assert_not_called()
        ownership.verify.assert_called_once_with(deadline)

    def test_expired_grace_does_not_confirm_a_remaining_process_group(self):
        process = Mock(pid=123)
        deadline = self.now() + 20 * clocks.SECOND
        def slow_alive(_group, _deadline=None):
            self.now.advance(2010 * clocks.MILLISECOND)
            return True
        with patch.object(clocks.time, "monotonic_ns", self.now), \
                patch.object(clocks, "group_alive", side_effect=slow_alive), patch.object(clocks.time, "sleep"):
            with self.assertRaisesRegex(RuntimeError, "cleanup unconfirmed"):
                clocks.wait_group(process, 123, deadline, 2, require_absent=True)

    def test_actual_exited_wrapper_does_not_leave_its_sleep_child(self):
        code = "import subprocess; subprocess.Popen(['/bin/sleep','30'])"
        process = subprocess.Popen([sys.executable, "-c", code], start_new_session=True)
        try:
            process.wait(timeout=5)
            with self.assertRaisesRegex(RuntimeError, "launch-time ownership unavailable"):
                clocks.stop_group(process, clocks.time.monotonic_ns() + 10 * clocks.SECOND)
            self.assertFalse(clocks.group_alive(process.pid))
        finally:
            clocks.send_group(process.pid, signal.SIGKILL)
            process.wait(timeout=5)

    def test_ordered_recovery_has_finite_outer_timeout(self):
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"cleanup_deadline_ns": clocks.time.monotonic_ns() + clocks.SECOND}))
        process = Mock()
        process.wait.side_effect = subprocess.TimeoutExpired("recover", 1)
        with patch.object(installed_account_run.subprocess, "Popen", return_value=process), patch.object(installed_account_run, "stop_recovery"):
            with self.assertRaises(subprocess.TimeoutExpired):
                installed_account_run.recover(ticket)
        self.assertLessEqual(process.wait.call_args.kwargs["timeout"], 1)

    def test_ordered_owner_failure_anchor_never_resets(self):
        ticket = self.root / "account-run.json"
        owned = {"pid": 123, "birth": "actual process"}
        ticket.write_text(json.dumps({"owner": owned, "cleanup_deadline_ns": self.now() + 600 * clocks.SECOND}))
        with patch.object(installed_account_run, "identity", return_value=owned), patch.object(clocks.time, "monotonic_ns", self.now):
            first = installed_account_run.owner_failure(self.root, RuntimeError("original"), 60 * clocks.SECOND)
            self.now.advance(20 * clocks.SECOND)
            second = installed_account_run.owner_failure(self.root, RuntimeError("later"), 60 * clocks.SECOND)
        self.assertEqual(first, second)
        self.assertEqual(json.loads(ticket.read_text())["originalError"], "original")

    def test_recovery_exhaustion_preserves_original_failure_and_no_success(self):
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"owner": None, "operation": "owned", "cleanup_deadline_ns": clocks.time.monotonic_ns() + clocks.SECOND}))
        process = Mock()
        process.wait.return_value = 1
        recover = Mock(side_effect=subprocess.TimeoutExpired("recover", 1))
        with self.assertRaisesRegex(RuntimeError, "exited unsuccessfully.*recovery failed"):
            installed_account_run.supervise(process, ticket, self.root, 1, Mock(), recover)
        self.assertFalse((self.root / "account-run-result.json").exists())
        self.assertIn("exited unsuccessfully", json.loads((self.root / "account-recovery-failure.json").read_text())["originalError"])

    def test_ordered_final_response_chunk_cannot_cross_existing_two_mib_cap(self):
        driver = installed_order.Driver.__new__(installed_order.Driver)
        driver.fixture_deadline_ns = driver.cleanup_deadline_ns = self.now() + 90 * clocks.SECOND
        driver.account, driver.process = {"role": "control"}, Mock()
        driver.process.poll.return_value = None
        driver.write_request = Mock()
        chunks = [b"x" * 65536] * 32 + [b"x\n"]
        decode = Mock()
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_order.select, "select", return_value=([driver.process.stdout], [], [])), patch.object(installed_order.os, "read", side_effect=chunks), patch.object(installed_order, "json", SimpleNamespace(dumps=json.dumps, loads=decode)):
            with self.assertRaisesRegex(RuntimeError, "response exceeds"):
                driver.call("inspect")
        decode.assert_not_called()
        request = json.loads(driver.write_request.call_args.args[0])
        self.assertEqual(request["fixtureDeadlineNs"], driver.fixture_deadline_ns)

    def test_order_trace_sends_once_at_first_ready_write_with_injected_deadline(self):
        driver = installed_order.Driver.__new__(installed_order.Driver)
        driver.fixture_deadline_ns = driver.cleanup_deadline_ns = self.now() + 90 * clocks.SECOND
        driver.account, driver.process = {"role": "control"}, Mock()
        driver.process.poll.return_value = None
        driver.diagnostics = installed_order_trace.Trace("control")
        delivered, writes, expected_deadline = bytearray(), [], driver.fixture_deadline_ns

        def ready(reads, writes, errors, timeout):
            self.now.advance(clocks.SECOND // 100)
            return reads, writes, errors

        def write(_descriptor, data):
            sends = driver.diagnostics.events["request.send:inspect::"]
            self.assertEqual(sends["count"], 1)
            self.assertEqual(sends["first"]["details"]["fixtureDeadlineNs"], expected_deadline)
            writes.append(self.now())
            delivered.extend(data[:5])
            return min(5, len(data))

        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_order.select, "select", side_effect=ready), patch.object(installed_order.os, "write", side_effect=write), patch.object(installed_order.os, "read", return_value=b'{"ok":true}\n'):
            self.assertEqual(driver.call("inspect", fixtureDeadlineNs=123), {"ok": True})
        self.assertGreater(len(writes), 1)
        send = driver.diagnostics.events["request.send:inspect::"]["first"]
        self.assertEqual(send["end"]["monotonicNs"], writes[0])
        self.assertEqual(json.loads(delivered)["fixtureDeadlineNs"], expected_deadline)

    def test_order_reader_trace_keeps_handoff_timeout_and_dispatch_wrapper(self):
        owner = installed_order_trace.Trace("control")
        origin = installed_order_host.Origin.__new__(installed_order_host.Origin)
        origin.root, origin.diagnostics = self.root, owner
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with clocks.handoff(deadline), patch.object(installed_order_host.subprocess, "run", return_value=Mock()) as run:
            origin.read_process({"MESH_INSTALLED_READ_KIND": "ledger"})
        self.assertGreater(run.call_args.kwargs["timeout"], 0)
        self.assertLessEqual(run.call_args.kwargs["timeout"], 1)
        self.assertIn("native.reader:ledger::", owner.events)
        host = installed_order_host.Host.__new__(installed_order_host.Host)
        host.diagnostics, host.case = owner, SimpleNamespace(root=self.root)
        host.dispatch_operation = Mock(side_effect=lambda *_args: clocks.HANDOFF.get())
        with patch.object(clocks, "start_handoff_alarm", return_value=None):
            self.assertEqual(host.dispatch("inspect", {"fixtureDeadlineNs": deadline}), deadline)
        self.assertIsNone(clocks.HANDOFF.get())
        self.assertIn("dispatch:inspect:True:", owner.events)


    def test_cleanup_alarm_cannot_launch_another_action_after_absolute_expiry(self):
        deadline = self.now() + clocks.SECOND
        next_action = Mock()
        def expire():
            self.now.advance(2 * clocks.SECOND)
            installed_account_run.recovery_expired(None, None)
        with patch.object(clocks.time, "monotonic_ns", self.now), clocks.handoff(deadline):
            with self.assertRaisesRegex(RuntimeError, "finite reserve"):
                installed_native.cleanup_all([expire, next_action])
        next_action.assert_not_called()

    def test_native_command_clamps_to_remaining_reserve_and_refuses_expired_start(self):
        deadline = self.now() + clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), clocks.handoff(deadline), patch.object(native_services.subprocess, "run", return_value=Mock(returncode=0)) as run:
            native_services.command(["owned-control"])
            self.assertEqual(run.call_args.kwargs["timeout"], 1)
            self.now.advance(2 * clocks.SECOND)
            with self.assertRaises(clocks.ClockExpired):
                native_services.command(["not-started"])
        run.assert_called_once()

    def test_expired_cleanup_start_is_latched_before_any_action(self):
        deadline = self.now() + clocks.SECOND
        action, latch = Mock(), Mock()
        with patch.object(clocks.time, "monotonic_ns", self.now), clocks.handoff(deadline):
            self.now.advance(2 * clocks.SECOND)
            with self.assertRaises(RuntimeError):
                installed_native.cleanup_all([action], on_error=latch)
        action.assert_not_called()
        latch.assert_called_once()

    def test_nested_order_teardown_latches_before_next_cleanup_action(self):
        first, events = RuntimeError("first nested cleanup failure"), []
        accounts = Mock(accounts=[])
        def fail():
            raise first
        def latch(*args):
            events.append(("latched", args[1]))
            return self.now() + 60 * clocks.SECOND
        accounts.close.side_effect = lambda: installed_native.cleanup_all([fail, lambda: events.append("next")])
        descriptor = {"fixtureEmergencyNs": 60 * clocks.SECOND}
        with patch.object(installed_accounts, "require_hosted_root"), patch.object(installed_paths, "validate"), patch.object(installed_account_run, "claim"), patch.object(installed_accounts, "Accounts", return_value=accounts), patch.object(installed_order, "share_inputs"), patch.object(installed_order, "bootstrap"), patch.object(installed_order, "scenarios"), patch.object(installed_account_run, "owner_failure", side_effect=latch), patch.object(clocks, "failure_cleanup_handoff"):
            with self.assertRaisesRegex(RuntimeError, "first nested cleanup failure"):
                installed_order.run(self.root, self.root, descriptor, "control")
        self.assertEqual(events[0][0], "latched")
        self.assertIs(events[0][1], first)
        self.assertEqual(events[1], "next")

    def test_order_setup_failure_after_claim_latches_without_replacing_error(self):
        failure = RuntimeError("account setup failed")
        descriptor = {"fixtureEmergencyNs": 60 * clocks.SECOND}
        with patch.object(installed_accounts, "require_hosted_root"), patch.object(installed_paths, "validate"), patch.object(installed_account_run, "claim"), patch.object(installed_accounts, "Accounts", side_effect=failure), patch.object(installed_account_run, "owner_failure", return_value=1) as latch, patch.object(clocks, "failure_cleanup_handoff"):
            with self.assertRaises(RuntimeError) as caught:
                installed_order.run(self.root, self.root, descriptor, "control")
        self.assertIs(caught.exception, failure)
        self.assertIs(latch.call_args.args[1], failure)

    def test_recovery_timeout_signals_the_durable_owner_within_one_reserve(self):
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"cleanup_deadline_ns": self.now() + 60 * clocks.SECOND, "recovery": {"pid": 73}}))
        failure = subprocess.TimeoutExpired("owned-recovery", 28)
        process = Mock()
        process.wait.side_effect = [failure, 0]
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_account_run.subprocess, "Popen", return_value=process), patch.object(installed_account_run.subprocess, "run", return_value=Mock()) as signal_owner, patch.object(installed_account_run, "alive", return_value=False):
            with self.assertRaises(subprocess.TimeoutExpired) as caught:
                installed_account_run.recover(ticket)
        self.assertIs(caught.exception, failure)
        self.assertEqual(process.wait.call_args_list[0].kwargs["timeout"], 58)
        self.assertIn("signal-recovery", signal_owner.call_args.args[0])
        self.assertLessEqual(signal_owner.call_args.kwargs["timeout"], 2)

    def test_missing_clock_export_never_deletes_negatively_owned_inputs(self):
        for name in ("cohort-proof", "native-worker-input.noindex", "native-tools.noindex"):
            (self.root / name).mkdir()
        (self.root / "cohort-proof/fixture-ownership.json").write_text(json.dumps({"signerAbsent": False, "transitionsAbsent": False}))
        workflow = Path(".github/workflows/installed-catchup.yml").read_text()
        section = workflow.split("      - name: Remove owned downloaded native inputs and helper tool\n", 1)[1].split("      - uses:", 1)[0]
        script = "\n".join(line[10:] for line in section.split("        run: |\n", 1)[1].splitlines())
        result = subprocess.run(["bash", "-e", "-c", script], env=os.environ | {"RUNNER_TEMP": str(self.root)}, capture_output=True, timeout=5)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.root / "native-worker-input.noindex").is_dir())
        self.assertTrue((self.root / "native-tools.noindex").is_dir())
        self.assertFalse(json.loads((self.root / "cohort-control/tools-cleanup-decision.json").read_text())["inputsMayBeRemoved"])

    def test_late_terminal_export_censors_the_final_leaf_only(self):
        clock = self.clock()
        clock.samples.append({"key": "earlier", "completed": True, "cleanup_verified": True})
        self.now.value = clock.deadline_ns - clocks.SECOND // 10
        write = Path.write_text
        def slow(path, data, *args, **kwargs):
            self.now.advance(clocks.SECOND)
            return write(path, data, *args, **kwargs)
        with patch.object(Path, "write_text", slow):
            value = clock.export(self.root / "clock.json", True, {})
        self.assertFalse(value["measurement_completed"])
        self.assertFalse(value["leaves"][-1]["eligible"])
        self.assertFalse(value["leaves"][-1]["completed"])
        self.assertTrue(value["samples"][0]["eligible"])

    def test_native_export_cost_is_measured_and_late_return_is_censored(self):
        handoff = self.root / "handoff.json"
        handoff.write_text(json.dumps({"deadline_ns": self.now() + clocks.SECOND, "mode": "diagnostic", "prefix": "proof#1"}))
        write = Path.write_text
        def slow(path, data, *args, **kwargs):
            self.now.advance(2 * clocks.SECOND)
            return write(path, data, *args, **kwargs)
        with patch.dict(os.environ, MESH_INSTALLED_CLOCK_HANDOFF=str(handoff)), patch.object(clocks.time, "monotonic_ns", self.now), patch.object(Path, "write_text", slow), self.assertRaises(clocks.ClockExpired):
            with clocks.native_group(self.root, {"index": 0}):
                self.now.advance(clocks.SECOND // 10)
        row = json.loads((self.root / "fixture-group-clock.json").read_text())["samples"][0]
        self.assertFalse(row["completed"])
        self.assertTrue(row["censored"])
        self.assertGreaterEqual(row["elapsed_ns"], 21 * clocks.SECOND // 10)

    def fragmented_control(self, step_ns, operation):
        payload = b'{"type":"host.info.result"}'
        pending = bytearray(b"\x01" + len(payload).to_bytes(4, "big") + payload)
        connection = MagicMock()
        connection.__enter__.return_value = connection
        def receive(length):
            self.now.advance(step_ns)
            chunk = bytes(pending[:min(3, length)])
            del pending[:len(chunk)]
            return chunk
        connection.recv.side_effect = receive
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_native.controls.socket, "socket", return_value=connection):
            return operation(connection)

    def test_fragmented_native_control_never_restarts_the_absolute_reserve(self):
        case = installed_native.Installation.__new__(installed_native.Installation)
        case.state = self.root
        deadline = self.now() + clocks.SECOND
        with patch.object(clocks.time, "monotonic_ns", self.now), clocks.handoff(deadline), self.assertRaisesRegex(TimeoutError, "absolute socket exchange"):
            self.fragmented_control(clocks.SECOND // 5, lambda _connection: case.control("host.info"))
        self.assertLessEqual(self.now(), deadline + clocks.SECOND // 5)

    def test_nested_cleanup_keeps_watchdog_and_fragmented_io_inside_reserve(self):
        case = installed_native.Installation.__new__(installed_native.Installation)
        case.state = self.root
        clock = self.clock()
        clock.cleanup_deadline_ns = self.now() + clocks.SECOND
        def fail():
            raise RuntimeError("nested cleanup failure")
        with patch.object(clocks.signal, "signal", return_value=signal.SIG_DFL), patch.object(clocks.signal, "setitimer") as timer, clock.activate(watchdog=True):
            with self.assertRaisesRegex(RuntimeError, "absolute socket exchange"):
                self.fragmented_control(clocks.SECOND // 5, lambda _connection: installed_native.cleanup_all([fail, lambda: case.control("host.info")]))
        timer.assert_any_call(signal.ITIMER_REAL, 1)
        self.assertEqual(timer.call_args_list[2].args, (signal.ITIMER_REAL, 1))
        self.assertLessEqual(self.now(), clock.cleanup_deadline_ns + clocks.SECOND // 5)

    def test_diagnostic_export_latches_before_later_account_cleanup(self):
        owner = {"pid": 73, "birth": "owned"}
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"owner": owner, "cleanup_deadline_ns": self.now() + 600 * clocks.SECOND}))
        driver = SimpleNamespace(account={"role": "canary"}, diagnostics=installed_order_trace.Trace("canary"))
        target = self.root / "failed-handoff.json"
        original = Path.write_bytes
        def write(path, data):
            if path == target:
                raise OSError("diagnostic write failed")
            return original(path, data)
        driver.close = lambda: installed_order_trace.write_diagnostic(b"owned", target, driver.diagnostics)
        accounts = SimpleNamespace(close=lambda: self.now.advance(20 * clocks.SECOND))
        descriptor = {"fixtureEmergencyNs": 60 * clocks.SECOND}
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_account_run, "identity", return_value=owner), patch.object(clocks, "failure_cleanup_handoff"), patch.object(Path, "write_bytes", write), self.assertRaisesRegex(RuntimeError, "coverage incomplete"):
            installed_order.teardown(accounts, [driver], {"passed": True}, self.root, lambda error: installed_order.latch_failure(self.root, descriptor, error))
        result = json.loads(ticket.read_text())
        self.assertEqual(result["failure_ns"], 100 * clocks.SECOND)
        self.assertEqual(result["cleanup_deadline_ns"], 160 * clocks.SECOND)

    def test_final_ordered_result_write_latches_the_original_error(self):
        owner = {"pid": 73, "birth": "owned"}
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"owner": owner, "cleanup_deadline_ns": self.now() + 600 * clocks.SECOND}))
        failure = OSError("final result write failed")
        descriptor = {"fixtureEmergencyNs": 60 * clocks.SECOND}
        with patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_account_run, "identity", return_value=owner), patch.object(clocks, "failure_cleanup_handoff"), patch.object(Path, "write_text", side_effect=failure), self.assertRaises(OSError) as caught:
            installed_order.teardown(SimpleNamespace(close=lambda: None), [], {"passed": True}, self.root, lambda error: installed_order.latch_failure(self.root, descriptor, error))
        self.assertIs(caught.exception, failure)
        self.assertEqual(json.loads(ticket.read_text())["failure_ns"], 100 * clocks.SECOND)


class OutcomePolicyTest(unittest.TestCase):
    def test_slow_completed_operation_accepts_without_a_phase_limit(self):
        now = Time()
        clock = clocks.Clock({}, now=now)
        with clock.operation("slow-functional-outcome"):
            now.advance(500 * clocks.SECOND)
        with tempfile.TemporaryDirectory() as root:
            value = clock.export(Path(root) / "result.json", True, {})
        self.assertTrue(value["accepted"])
        self.assertEqual(value["samples"][0]["elapsed_ns"], 500 * clocks.SECOND)

    def test_successful_case_cleanup_keeps_work_cap_and_final_cleanup_is_bounded_once(self):
        now = Time()
        clock = clocks.Clock({}, now=now)
        clock.begin_cleanup()
        self.assertIsNone(clock.cleanup_deadline_ns)
        clock.begin_cleanup(final=True)
        deadline = now() + clocks.CLEANUP_SECONDS * clocks.SECOND
        now.advance(clocks.SECOND)
        clock.begin_cleanup(final=True)
        clock.fail(ValueError("original failure"))
        self.assertEqual(clock.cleanup_deadline_ns, deadline)
        now.value = deadline
        with self.assertRaises(clocks.ClockExpired):
            clock.check()
        self.assertEqual(clock.failures[0]["error"], "original failure")

    def test_unresolved_partition_cannot_accept_completed_timestamps(self):
        clock = clocks.Clock({"grouped": True, "mode": "chain"})
        with tempfile.TemporaryDirectory() as root:
            value = clock.export(Path(root) / "result.json", True, {})
        self.assertFalse(value["accepted"])
        self.assertEqual(value["unresolved_key_sets"], ["installed-partition-and-scenarios"])

    def test_exec_inherits_one_absolute_parent_deadline(self):
        deadline = clocks.time.monotonic_ns() + 180 * clocks.SECOND
        script = "from fixtures import installed_clock as c; " + \
            "exec('with c.inherited_handoff(watchdog=True):\\n print(c.parent_deadline())')"
        environment = os.environ | clocks.handoff_environment(deadline) | {
            "PYTHONPATH": str(Path(clocks.__file__).parents[1]), "PYTHONDONTWRITEBYTECODE": "1"}
        result = subprocess.run([sys.executable, "-c", script], env=environment, capture_output=True,
                                text=True, check=True, timeout=180)
        self.assertEqual(int(result.stdout), deadline)

    def test_invalid_inherited_deadline_never_dispatches(self):
        for value in ("0", "-1", "not-an-integer"):
            with patch.dict(os.environ, {clocks.HANDOFF_ENV: value}), self.assertRaises(ValueError), clocks.inherited_handoff():
                self.fail("invalid parent dispatched work")


    def test_direct_script_import_and_negative_cleanup_need_no_pythonpath(self):
        source = Path(clocks.__file__).resolve()
        environment = {key: value for key, value in os.environ.items() if key != "PYTHONPATH"}
        environment["PYTHONDONTWRITEBYTECODE"] = "1"
        result = subprocess.run([sys.executable, "-B", "-c", "import runpy,sys; runpy.run_path(sys.argv[1], run_name='import_probe')", str(source)],
                                env=environment, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tempfile.TemporaryDirectory(prefix="outcome-cleanup-cli-") as directory:
            root = Path(directory)
            (root / "cohort-proof").mkdir()
            (root / "cohort-proof/fixture-ownership.json").write_text('{"signerAbsent":false,"transitionsAbsent":false}')
            result = subprocess.run([sys.executable, "-B", str(source), "cleanup-inputs", str(root)],
                                    env=environment, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(json.loads((root / "cohort-control/tools-cleanup-decision.json").read_text())["inputsMayBeRemoved"])



class CleanupHandoffTest(unittest.TestCase):
    def test_cleanup_retires_work_alarm_that_expires_during_teardown(self):
        now = Time()
        with patch.object(clocks.time, "monotonic_ns", now), \
                patch.object(clocks.signal, "getsignal", return_value=signal.SIG_IGN), \
                patch.object(clocks.signal, "getitimer", return_value=(60, 0)), \
                patch.object(clocks.signal, "signal"), patch.object(clocks.signal, "setitimer") as timer:
            self.check_retired_alarm(now, timer)

    def check_retired_alarm(self, now, timer):
        with clocks.handoff(now() + 60 * clocks.SECOND, watchdog=True):
            with clocks.cleanup_handoff():
                now.advance(61 * clocks.SECOND)
            self.assertEqual(timer.call_args.args, (signal.ITIMER_REAL, 0))

    def test_nested_cleanup_reuses_one_deadline_after_work_expired(self):
        now = Time()
        token = clocks.HANDOFF.set(now() - 1)
        self.addCleanup(clocks.HANDOFF.reset, token)
        with patch.object(clocks.time, "monotonic_ns", now), clocks.cleanup_handoff() as deadline:
            self.assertEqual(deadline, now() + clocks.CLEANUP_SECONDS * clocks.SECOND)
            now.advance(clocks.SECOND)
            with clocks.cleanup_handoff() as nested:
                self.assertEqual(nested, deadline)
                self.assertEqual(clocks.parent_deadline(), deadline)
        self.assertEqual(clocks.HANDOFF.get(), now() - clocks.SECOND - 1)

    def test_direct_cohort_stop_gets_cleanup_context_after_work_expired(self):
        from fixtures import installed_cohort
        cohort = installed_cohort.Cohort.__new__(installed_cohort.Cohort)
        cohort.record = Path("/unused-cohort")
        token = clocks.HANDOFF.set(1)
        observed = []
        try:
            with patch.object(installed_cohort.release_workers, "stop", side_effect=lambda record: observed.append((record, clocks.timeout()))):
                cohort.close()
        finally:
            clocks.HANDOFF.reset(token)
        self.assertEqual(observed[0][0], cohort.record)
        self.assertGreater(observed[0][1], 0)
        self.assertLessEqual(observed[0][1], clocks.CLEANUP_SECONDS)

    def test_shell_trap_passes_one_cleanup_endpoint_and_preserves_exit(self):
        with tempfile.TemporaryDirectory(prefix="cleanup-handoff-") as temporary:
            self.shell_trap(Path(temporary))

    def shell_trap(self, root):
        transition, output = root / "transition", root / "evidence"
        transition.mkdir()
        output.mkdir()
        (transition / "cohort.json").write_text('{}')
        deadline = clocks.time.monotonic_ns() + 60 * clocks.SECOND
        handoff = root / "parent.json"
        handoff.write_text(json.dumps({"cleanup_deadline_ns": deadline}))
        receipt, tool = root / "stop.json", root / "stop.py"
        tool.write_text("from fixtures import installed_clock as c; import json,os; from pathlib import Path; " +
                        "exec('with c.inherited_handoff(watchdog=True):\\n Path(os.environ[\"STOP_RECEIPT\"]).write_text(json.dumps({\"deadline\":c.parent_deadline()}))')")
        source = (Path(clocks.__file__).parents[1] / "prove-release-transition.sh").read_text()
        cleanup = source.split("cleanup() {", 1)[1].split("trap cleanup EXIT", 1)[0]
        script = "set -euo pipefail\ncleanup() {" + cleanup + "trap cleanup EXIT\n"
        values = {"test_root": transition, "output_directory": output, "cohort_tool": tool,
                  "cohort": transition / "cohort.json", "state": transition / "state"}
        setup = "\n".join(key + "=" + shlex.quote(str(value)) for key, value in values.items())
        setup += "\nold_daemon= candidate_daemon= client= session= shell_pid= worker_pid= helper_pid= original_worker= original_shell=\n"
        setup += "discover_owned_session_processes() { :; }\n"
        environment = os.environ | {"PYTHONPATH": str(Path(clocks.__file__).parents[1]), "PYTHONDONTWRITEBYTECODE": "1",
            clocks.HANDOFF_ENV: "1", "MESH_INSTALLED_CLOCK_HANDOFF": str(handoff), "STOP_RECEIPT": str(receipt)}
        result = subprocess.run(["bash", "-c", setup + script + "exit 7\n"], env=environment,
                                capture_output=True, text=True, timeout=180)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(json.loads(receipt.read_text())["deadline"], deadline)
        self.assertFalse(transition.exists())


class CompletionEvidenceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="completion-control-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "cohort-proof"
        self.output.mkdir()
        self.now = Time()

    def test_completion_alarm_preserves_committed_ownership_and_partial_record(self):
        for prefix in (b"", b'{"root":'):
            with self.subTest(partial=prefix):
                self.interrupted_ownership(prefix)

    def interrupted_ownership(self, prefix):
        clock = clocks.Clock({"scenario": "chain"}, now=self.now)
        with clock.activate():
            self.interrupt_close(clock, prefix)
        owned = json.loads((self.output / "fixture-ownership.json").read_bytes())
        self.assertIs(owned["signerAbsent"], False)
        self.assertEqual((self.output / "fixture-ownership.tmp").read_bytes(), prefix)
        (self.output / "fixture-clock.json").write_text('{"provenance":{"retained_root":null}}')
        with self.assertRaisesRegex(RuntimeError, "ownership unconfirmed"):
            clocks.cleanup_inputs(self.root)
        decision = json.loads((self.root / "cohort-control/tools-cleanup-decision.json").read_bytes())
        self.assertIs(decision["inputsMayBeRemoved"], False)
        self.assertEqual(decision["reason"], "fixture ownership unconfirmed")

    def interrupt_close(self, clock, prefix):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.output, prepared=True, )
        releases.record_ownership(signer_absent=False)
        releases.signer = Mock()
        releases.signer.poll.return_value = 0
        temporary = self.output / "fixture-ownership.tmp"
        write = Path.write_bytes
        def interrupted(path, data):
            if path not in (temporary, self.output / "fixture-ownership.json"):
                return write(path, data)
            write(path, prefix)
            self.now.value = clock.effective_deadline()
            clock.check()
        with patch.object(Path, "write_bytes", interrupted), self.assertRaisesRegex(clocks.ClockExpired, "completion#1"):
            releases.close()
        self.assertFalse(releases.closed)
        self.assertEqual(clock.failures[0]["type"], "ClockExpired")

    def test_interrupted_commit_keeps_previous_ownership(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.output)
        releases.record_ownership(signer_absent=False)
        error = clocks.ClockExpired("completion commit expired")
        with patch.object(Path, "replace", side_effect=error), self.assertRaises(clocks.ClockExpired) as raised:
            releases.close()
        self.assertIs(raised.exception, error)
        self.assertIs(json.loads((self.output / "fixture-ownership.json").read_bytes())["signerAbsent"], False)
        self.assertIs(json.loads((self.output / "fixture-ownership.tmp").read_bytes())["signerAbsent"], True)

    def test_successful_completion_commits_observed_absence(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.output)
        releases.record_ownership(signer_absent=False)
        releases.close()
        self.assertTrue(releases.closed)
        self.assertEqual(json.loads((self.output / "fixture-ownership.json").read_bytes()),
                         {"root": str(self.root), "signerAbsent": True, "transitionsAbsent": True})
        self.assertFalse((self.output / "fixture-ownership.tmp").exists())

    def test_export_copy_preserves_deadline_identity_and_message(self):
        for sent in ((), (1,)):
            with self.subTest(sent=sent):
                self.interrupt_export_copy(sent)

    def interrupt_export_copy(self, sent):
        source, target = self.root / "worker", self.root / "copied-worker"
        source.write_bytes(b"retained worker")
        files = {}
        error = clocks.ClockExpired("installed fixture hang cap reached at prepared-export:#1")
        with patch.object(shutil, "_USE_CP_SENDFILE", True), patch.object(shutil, "_USE_CP_COPY_FILE_RANGE", False, create=True), \
                patch.object(shutil.os, "sendfile", side_effect=list(sent) + [error]), self.assertRaises(clocks.ClockExpired) as raised:
            installed_prepared.copy_external(source, target.name, self.root, files)
        self.assertIs(raised.exception, error)
        self.assertEqual(str(error), "installed fixture hang cap reached at prepared-export:#1")
        self.assertEqual(files, {})
        self.assertTrue(target.exists())

    def test_root_removal_preserves_deadline_and_remaining_files(self):
        owned = self.root / "owned"
        owned.mkdir()
        (owned / "remaining").write_bytes(b"partial cleanup evidence")
        error = clocks.ClockExpired("installed fixture hang cap reached at completion#2")
        with patch.object(shutil.os, "unlink", side_effect=error), self.assertRaises(clocks.ClockExpired) as raised:
            shutil.rmtree(owned)
        self.assertIs(raised.exception, error)
        self.assertEqual(str(error), "installed fixture hang cap reached at completion#2")
        self.assertTrue((owned / "remaining").exists())


class NativeOwnershipTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="clock-ownership-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.controls = []
        self.addCleanup(self.stop_controls)

    def launch(self, exit_code=0, held=True):
        marker, release = self.root / "child.pid", self.root / "release"
        code = ("import pathlib,subprocess,sys,time; "
                "p=subprocess.Popen(['/bin/sleep','30'],start_new_session=True); "
                "pathlib.Path(sys.argv[1]).write_text(str(p.pid)); "
                "exec('while not pathlib.Path(sys.argv[2]).exists(): time.sleep(.005)' if sys.argv[3]=='hold' else ''); "
                "sys.exit(int(sys.argv[4]))")
        process = subprocess.Popen([sys.executable, "-c", code, str(marker), str(release), "hold" if held else "exit", str(exit_code)], start_new_session=True)
        control = {"process": process, "marker": marker, "child": None, "birth": None}
        self.controls.append(control)
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        while not marker.exists():
            clocks.time.sleep(clocks.remaining_until(deadline, 0.005))
        control["child"] = int(marker.read_text())
        control["birth"] = clocks.process_birth(control["child"], deadline)
        return process, control["child"], release

    def stop_controls(self):
        for control in self.controls:
            process, marker = control["process"], control["marker"]
            deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
            if control["birth"] is not None and clocks.process_birth(control["child"], deadline) == control["birth"]:
                clocks.signal_process(control["child"], signal.SIGKILL)
            clocks.send_group(process.pid, signal.SIGKILL)
            process.wait(timeout=5)
            self.verify_control_stopped(marker)

    def verify_control_stopped(self, marker):
        if not marker.exists():
            return
        pid = int(marker.read_text())
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        while clocks.process_birth(pid, deadline) is not None:
            clocks.time.sleep(clocks.remaining_until(deadline, 0.005))

    def capture_control(self, process, child):
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        ownership = clocks.NativeOwnership(process, self.root, self.root, deadline)
        ownership.note_pid(child, clocks.process_snapshot(deadline))
        self.assertIn(child, ownership.correlated)
        return ownership

    def test_known_escaped_child_survives_leader_and_is_stopped(self):
        process, child, release = self.launch()
        ownership = self.capture_control(process, child)
        release.touch()
        process.wait(timeout=5)
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        with self.assertRaisesRegex(RuntimeError, "ownership coverage/absence unconfirmed"):
            installed_releases.wait_transition(process, ["generic-control"], deadline, ownership)
        clocks.stop_group(process, deadline, ownership=ownership)
        self.assertIsNone(clocks.process_birth(child, deadline))
        ownership.save()
        evidence = json.loads((self.root / "fixture-native-ownership.json").read_text())
        self.assertGreater(evidence["observation_ns"], 0)

    def test_failed_wrapper_still_checks_escaped_child(self):
        process, child, release = self.launch(exit_code=7)
        ownership = self.capture_control(process, child)
        release.touch()
        process.wait(timeout=5)
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        with self.assertRaises(subprocess.CalledProcessError):
            installed_releases.wait_transition(process, ["generic-control"], deadline, ownership)
        clocks.stop_group(process, deadline, ownership=ownership)
        self.assertIsNone(clocks.process_birth(child, deadline))

    def test_already_exited_unobserved_launcher_cannot_claim_absence(self):
        process, child, _release = self.launch(held=False)
        process.wait(timeout=5)
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        ownership = clocks.NativeOwnership(process, self.root, self.root, deadline)
        with self.assertRaisesRegex(RuntimeError, "ownership coverage/absence unconfirmed"):
            clocks.stop_group(process, deadline, (self.root,), ownership=ownership)
        self.assertIsNotNone(clocks.process_birth(child, deadline))

    def rows(self):
        return {pid: {"pid": pid, "parent": parent, "group": pid, "uid": os.getuid(), "birth": "birth-" + str(pid)}
                for pid, parent in ((10, 1), (20, 10), (30, 20))}

    def failed_attempt(self, pid=40):
        return {"pid": pid, "shellPid": 0, "outcome": "EPERM", "startupFailure": {
            "pid": pid, "workerLogTail": 'worker: start /bin/sh: child-start path="/bin/sh" errno=1 stage=unavailable: fork/exec /bin/sh: operation not permitted\n'},
            "cleanup": {"worker": "reaped", "shell": "no published child PID"}}

    def startup_ownership(self):
        path = self.root / "startup.cohort.json"
        path.write_text(json.dumps({"workers": [{"pid": 40, "shellPid": 0}]}))
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        return ownership, path, deadline

    @unittest.skipUnless(sys.platform == "linux", "Linux EPERM startup records")
    def test_reaped_preexec_attempt_does_not_need_outer_live_correlation(self):
        ownership, path, deadline = self.startup_ownership()
        self.assertEqual(ownership.pending, {40})
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30, "startupAttempts": [self.failed_attempt()]}]}))
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership.observe(deadline)
        path.unlink()
        with patch.object(clocks, "process_snapshot", return_value={}):
            ownership.verify(deadline)
        self.assertNotIn(40, ownership.correlated)
        self.assertFalse(any(pid == 40 for pid, _birth in ownership.owned))
        self.assertEqual(ownership.pending, set())
        ownership.save()
        self.assertEqual(json.loads((self.root / "fixture-native-ownership.json").read_text())["reapedStartupPids"], [40])

    @unittest.skipUnless(sys.platform == "linux", "Linux EPERM startup records")
    def test_failed_startup_reaped_without_retry_can_finish_cleanup(self):
        ownership, path, deadline = self.startup_ownership()
        attempt = self.failed_attempt()
        attempt["cleanup"]["worker"] = "birth-checked and reaped"
        path.write_text(json.dumps({"workers": [{"pid": 40, "shellPid": 0, "startupAttempts": [attempt]}]}))
        with patch.object(clocks, "process_snapshot", return_value={}):
            ownership.verify(deadline)
            ownership.verify(deadline)
        self.assertNotIn(40, ownership.correlated)

    @unittest.skipUnless(sys.platform == "linux", "Linux EPERM startup records")
    def test_first_record_after_retry_retains_failed_pid_absence_check(self):
        path = self.root / "retry.cohort.json"
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30, "startupAttempts": [self.failed_attempt()]}]}))
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        self.assertEqual(ownership.expected, {20, 30, 40})
        self.assertEqual(ownership.correlated, {20, 30})
        path.unlink()
        replacement = {40: self.rows()[20] | {"pid": 40, "parent": 1}}
        with patch.object(clocks, "process_snapshot", return_value=replacement), self.assertRaisesRegex(RuntimeError, "changedLifetimes.*40"):
            ownership.verify(deadline)
        with patch.object(clocks, "process_snapshot", return_value={}):
            ownership.verify(deadline)

    @unittest.skipUnless(sys.platform == "linux", "Linux EPERM startup records")
    def test_actual_short_lived_child_reaped_between_outer_snapshots(self):
        path = self.root / "short-lived.cohort.json"
        code = (
            "import json,pathlib,subprocess,sys\n"
            "from fixtures import release_worker_eperm as eperm\n"
            "path=pathlib.Path(sys.argv[1])\n"
            "sys.stdin.readline()\n"
            "child=subprocess.Popen([sys.executable,'-c','raise SystemExit(1)'])\n"
            "row=json.loads(sys.argv[2]); row['pid']=row['startupFailure']['pid']=child.pid\n"
            "path.write_text(json.dumps({'workers':[{'pid':child.pid,'shellPid':0}]}))\n"
            "child.wait(timeout=5)\n"
            "print(child.pid,flush=True)\n"
            "sys.stdin.readline()\n"
            "row['cleanup']=eperm.reconcile(child,row['startupFailure'])\n"
            "path.write_text(json.dumps({'workers':[dict(row,startupAttempts=[row])]}))\n"
            "print('reaped',flush=True)\n"
            "sys.stdin.readline()\n")
        environment = os.environ | {"PYTHONPATH": str(Path(__file__).resolve().parent), "PYTHONDONTWRITEBYTECODE": "1"}
        process = subprocess.Popen([sys.executable, "-B", "-c", code, str(path), json.dumps(self.failed_attempt())],
                                   env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   text=True, start_new_session=True)
        self.addCleanup(self.stop_reaped_launcher, process)
        deadline = clocks.time.monotonic_ns() + 10 * clocks.SECOND
        ownership = clocks.NativeOwnership(process, self.root, self.root, deadline)
        child = int(self.advance_reaped_launcher(process))
        ownership.observe(deadline)
        self.assertEqual(ownership.pending, {child})
        self.assertIsNone(clocks.process_birth(child, deadline))
        self.assertEqual(self.advance_reaped_launcher(process), "reaped")
        ownership.observe(deadline)
        path.unlink()
        self.stop_reaped_launcher(process)
        ownership.verify(deadline)
        self.assertNotIn(child, ownership.correlated)
        self.assertEqual(ownership.reaped_startups, {child})

    def advance_reaped_launcher(self, process):
        process.stdin.write("next\n")
        process.stdin.flush()
        ready, _, _ = select.select([process.stdout], [], [], 5)
        self.assertTrue(ready, "short-lived child control did not answer")
        return process.stdout.readline().strip()

    def stop_reaped_launcher(self, process):
        if process.stdout.closed:
            return
        try:
            process.communicate("next\n" * 3, timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.communicate(timeout=5)
            self.fail("short-lived child launcher did not finish")
        deadline = clocks.time.monotonic_ns() + 5 * clocks.SECOND
        self.assertIsNone(clocks.process_birth(process.pid, deadline))

    def test_unconfirmed_startup_cannot_retire_missing_worker(self):
        changes = [{"cleanup": {}}, {"cleanup": {"worker": "not reaped", "shell": "no published child PID"}},
                   {"cleanup": {"worker": "reaped", "shell": "unknown"}}, {"shellPid": 50}, {"outcome": "failed"},
                   {"startupFailure": {"pid": 41, "workerLogTail": self.failed_attempt()["startupFailure"]["workerLogTail"]}},
                   {"startupFailure": {"pid": 40, "workerLogTail": "worker: unrelated startup failure"}}]
        for change in changes:
            self.check_unconfirmed_startup(change)

    def check_unconfirmed_startup(self, change):
        ownership, path, deadline = self.startup_ownership()
        attempt = self.failed_attempt() | change
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30, "startupAttempts": [attempt]}]}))
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership.observe(deadline)
        with patch.object(clocks, "process_snapshot", return_value={}), self.assertRaisesRegex(RuntimeError, "missing.*40"):
            ownership.verify(deadline)
        self.assertFalse(ownership.verified)

    def test_reaped_attempt_still_refuses_live_unknown_or_reused_pid(self):
        ownership, path, deadline = self.startup_ownership()
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30, "startupAttempts": [self.failed_attempt()]}]}))
        unknown = {40: self.rows()[20] | {"pid": 40, "parent": 1, "birth": "replacement"}}
        with patch.object(clocks, "process_snapshot", return_value=unknown), self.assertRaisesRegex(RuntimeError, "changedLifetimes.*40"):
            ownership.verify(deadline)
        with patch.object(clocks, "process_birth", return_value="replacement"), patch.object(clocks, "signal_process") as send:
            clocks.signal_descendants({40: "original"}, signal.SIGKILL, deadline)
        send.assert_not_called()

    def test_reaped_attempt_cannot_hide_live_or_unobserved_shell(self):
        ownership, path, deadline = self.startup_ownership()
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30, "startupAttempts": [self.failed_attempt()]}]}))
        with patch.object(clocks, "process_snapshot", return_value={30: self.rows()[30]}), self.assertRaisesRegex(RuntimeError, "live.*30"):
            ownership.verify(deadline)
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 50, "startupAttempts": [self.failed_attempt()]}]}))
        with patch.object(clocks, "process_snapshot", return_value={}), self.assertRaisesRegex(RuntimeError, "missing.*50"):
            ownership.verify(deadline)

    def test_removal_before_reap_observation_still_refuses_cleanup(self):
        ownership, path, deadline = self.startup_ownership()
        path.unlink()
        with patch.object(clocks, "process_snapshot", return_value={}), self.assertRaisesRegex(RuntimeError, "missing.*40"):
            ownership.verify(deadline)

    def test_observation_batches_all_native_records_into_one_process_command(self):
        output = self.root / "proof"
        records = (output / "observations/proof.cohort.json", self.root / "mesh-transition.control/cohort.json",
                   self.root / "mesh-transition.control/state/s/owned/meta.json")
        for path in records:
            path.parent.mkdir(parents=True, exist_ok=True)
            value = {"pid": 30} if path.name == "meta.json" else {"workers": [{"pid": 20, "shellPid": 30}]}
            path.write_text(json.dumps(value))
        uid = os.getuid()
        result = SimpleNamespace(stdout="".join(f"{pid} {parent} {pid} {uid} Sat Nov 14 01:02:03 2026 S\n"
                                               for pid, parent in ((10, 1), (20, 10), (30, 20))))
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks.subprocess, "run", return_value=result) as command, \
                patch.object(clocks.os, "scandir", wraps=os.scandir) as scans:
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, output, deadline)
        command.assert_called_once()
        enumerated = [Path(call.args[0]) for call in scans.call_args_list]
        self.assertEqual(enumerated.count(output), 1)
        self.assertEqual(enumerated.count(output / "observations"), 1)
        self.assertEqual(ownership.observed_records, set(records))
        self.assertEqual(ownership.expected, {20, 30})
        self.assertEqual(ownership.correlated, {20, 30})

    def test_cohort_discovery_keeps_hidden_and_link_records_without_following_link_directories(self):
        output, outside = self.root / "proof", self.root / "outside"
        output.mkdir()
        outside.mkdir()
        hidden = output / ".hidden.cohort.json"
        hidden.write_text("{}")
        external = outside / "external.cohort.json"
        external.write_text("{}")
        linked = output / "linked.cohort.json"
        linked.symlink_to(external)
        broken = output / "broken.cohort.json"
        broken.symlink_to(outside / "absent")
        (output / "linked-directory").symlink_to(outside, target_is_directory=True)
        directory = output / "directory.cohort.json"
        directory.mkdir()
        nested = directory / "nested.cohort.json"
        nested.write_text("{}")
        self.assertEqual(set(clocks.recursive_suffix_paths(output, ".cohort.json")), {hidden, linked, broken, directory, nested})

    def test_single_census_preserves_recursive_record_coverage(self):
        for name in ("root.cohort.json", "one/one.cohort.json", "one/deep/deep.cohort.json",
                     "two/two.cohort.json", "two/deep/deep.cohort.json", "two/other/other.cohort.json"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("{}")
        self.assertCountEqual(clocks.recursive_suffix_paths(self.root, ".cohort.json"), self.root.rglob("*.cohort.json"))

    def test_observations_and_final_verification_each_take_fresh_snapshot(self):
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", side_effect=[self.rows(), {}, self.rows(), {}]) as snapshot:
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
            ownership.note_pid(30, self.rows())
            ownership.observe(deadline)
            self.assertEqual(ownership.live(deadline), {pid: row["birth"] for pid, row in self.rows().items()})
            ownership.verify(deadline)
        self.assertEqual(snapshot.call_count, 4)
        self.assertEqual(ownership.scans, 2)
        self.assertTrue(ownership.verified)

    def test_capture_keeps_uid_group_and_lifetime_boundaries(self):
        rows = self.rows()
        rows[40] = rows[30] | {"pid": 40, "parent": 1, "group": 10, "birth": "birth-40"}
        rows[50] = rows[40] | {"pid": 50, "uid": os.getuid() + 1, "birth": "birth-50"}
        rows[60] = rows[40] | {"pid": 60, "parent": 50, "group": 60, "birth": "birth-60"}
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=rows):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        self.assertEqual({pid for pid, _birth in ownership.owned}, {10, 20, 30, 40})
        changed = {10: rows[10] | {"birth": "replacement"}, 40: rows[40] | {"birth": "replacement"}}
        with patch.object(clocks, "process_snapshot", return_value=changed):
            ownership.observe(deadline)
        self.assertNotIn((40, "replacement"), ownership.owned)
        self.assertNotIn((10, "replacement"), ownership.owned)

    def test_signaling_rechecks_each_birth_for_each_signal(self):
        owned = {20: "birth-20", 30: "birth-30"}
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_birth", side_effect=["birth-20", "replacement", None, "birth-30"]) as birth, \
                patch.object(clocks, "signal_process") as send:
            clocks.signal_descendants(owned, signal.SIGTERM, deadline)
            clocks.signal_descendants(owned, signal.SIGKILL, deadline)
        self.assertEqual([call.args for call in birth.call_args_list], [(20, deadline), (30, deadline)] * 2)
        self.assertEqual([call.args for call in send.call_args_list], [(20, signal.SIGTERM), (30, signal.SIGKILL)])

    def test_process_snapshot_uses_one_command_and_excludes_zombies(self):
        uid = os.getuid()
        result = SimpleNamespace(stdout=f"10 1 10 {uid} Sat Nov 14 01:02:03 2026 S\n20 10 20 {uid} Sat Nov 14 01:02:03 2026 Z\n")
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks.subprocess, "run", return_value=result) as command:
            rows = clocks.process_snapshot(deadline)
        self.assertEqual(set(rows), {10})
        self.assertEqual(rows[10]["birth"], f"{uid} Sat Nov 14 01:02:03 2026")
        command.assert_called_once()
        self.assertEqual(command.call_args.args[0], ["ps", "-axww", "-o", "pid=,ppid=,pgid=,uid=,lstart=,stat="])

    def test_native_worker_and_detached_shell_are_correlated_while_live(self):
        state = self.root / "mesh-transition.control"
        state.mkdir()
        (state / "cohort.json").write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30}]}))
        rows = self.rows()
        with patch.object(clocks, "process_snapshot", return_value=rows):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, clocks.time.monotonic_ns() + clocks.SECOND)
        self.assertEqual(ownership.expected, {20, 30})
        self.assertEqual(ownership.correlated, {20, 30})
        with patch.object(clocks, "process_snapshot", return_value={}):
            ownership.verify(clocks.time.monotonic_ns() + clocks.SECOND)

    def test_vanished_native_records_preserve_retained_identity_checks(self):
        for name in ("cohort.json", "state/s/W000/meta.json"):
            self.check_vanished_record(name)

    def check_vanished_record(self, name):
        state = self.root / ("mesh-transition." + name.replace("/", "-").replace(".", "-"))
        path = state / name
        path.parent.mkdir(parents=True)
        value = {"pid": 30} if path.name == "meta.json" else {"workers": [{"pid": 20, "shellPid": 30}]}
        path.write_text(json.dumps(value))
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        expected = ownership.expected.copy()
        original = Path.read_text
        def remove_before_read(candidate, *args, **kwargs):
            if candidate == path:
                path.unlink()
                raise FileNotFoundError(str(path))
            return original(candidate, *args, **kwargs)
        with patch.object(clocks, "process_snapshot", return_value={}), patch.object(Path, "read_text", remove_before_read):
            ownership.observe(deadline)
            ownership.verify(deadline)
        self.assertEqual(ownership.expected, expected)
        self.assertEqual(ownership.correlated, expected)
        self.assertTrue(ownership.verified)
        self.assertEqual(ownership.vanished_record_reads, 1)

    def test_partially_written_native_records_are_read_again(self):
        cases = [(name, partial) for name in ("cohort.json", "state/s/W000/meta.json") for partial in ("", '{"workers": [{"pid": 2')]
        for name, partial in cases:
            with self.subTest(name=name, partial=partial):
                self.check_partial_record(name, partial)

    def check_partial_record(self, name, partial):
        state = self.root / ("mesh-transition.partial-" + str(len(partial)) + "-" + name.replace("/", "-").replace(".", "-"))
        path = state / name
        path.parent.mkdir(parents=True)
        path.write_text(partial)
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
            self.assertEqual(ownership.expected, set())
            self.assertEqual(ownership.incomplete_record_reads, 1)
            value = {"pid": 30} if path.name == "meta.json" else {"workers": [{"pid": 20, "shellPid": 30}]}
            path.write_text(json.dumps(value))
            ownership.observe(deadline)
        self.assertEqual(ownership.expected, {20, 30})
        with patch.object(clocks, "process_snapshot", return_value={}):
            ownership.verify(deadline)
        self.assertTrue(ownership.verified)
        self.assertEqual(ownership.incomplete_record_reads, 1)
        shutil.rmtree(state)

    def test_native_record_that_stays_malformed_fails_verification(self):
        self.check_malformed_native_record('{"pid": ')

    def test_persisted_null_native_record_fails_despite_other_coverage(self):
        self.check_malformed_native_record("null")

    def check_malformed_native_record(self, contents):
        state = self.root / "mesh-transition.malformed"
        state.mkdir()
        (state / "cohort.json").write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30}]}))
        broken = state / "state/s/W000/meta.json"
        broken.parent.mkdir(parents=True)
        broken.write_text(contents)
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
            ownership.observe(deadline)
        with patch.object(clocks, "process_snapshot", return_value={}), \
                self.assertRaisesRegex(RuntimeError, "native record stayed malformed: .*meta.json"):
            ownership.verify(deadline)
        self.assertFalse(ownership.verified)

    def test_unobserved_vanished_record_cannot_establish_coverage(self):
        state = self.root / "mesh-transition.unobserved"
        state.mkdir()
        path = state / "cohort.json"
        path.write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30}]}))
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        with patch.object(clocks, "process_snapshot", return_value=self.rows()), patch.object(Path, "read_text", side_effect=FileNotFoundError):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        path.unlink()
        with patch.object(clocks, "process_snapshot", return_value={}), self.assertRaisesRegex(RuntimeError, "coverage/absence unconfirmed"):
            ownership.verify(deadline)
        self.assertFalse(ownership.verified)

    def test_new_unread_vanished_record_cannot_hide_behind_prior_coverage(self):
        state = self.root / "mesh-transition.known"
        state.mkdir()
        (state / "cohort.json").write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30}]}))
        deadline = clocks.time.monotonic_ns() + clocks.SECOND
        rows = self.rows()
        with patch.object(clocks, "process_snapshot", return_value=rows):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, deadline)
        other = self.root / "mesh-transition.unread"
        other.mkdir()
        path = other / "cohort.json"
        path.write_text(json.dumps({"workers": [{"pid": 40, "shellPid": 50}]}))
        detached = {pid: {"pid": pid, "parent": parent, "group": pid, "uid": os.getuid(), "birth": "birth-" + str(pid)}
                    for pid, parent in ((40, 1), (50, 40))}
        original = Path.read_text
        def disappear(candidate, *args, **kwargs):
            if candidate == path:
                path.unlink()
                raise FileNotFoundError(str(path))
            return original(candidate, *args, **kwargs)
        with patch.object(clocks, "process_snapshot", return_value=detached), patch.object(Path, "read_text", disappear):
            ownership.observe(deadline)
            with self.assertRaisesRegex(RuntimeError, "vanished before observation"):
                ownership.verify(deadline)
        self.assertEqual(ownership.expected, {20, 30})
        self.assertFalse(ownership.verified)

    def test_native_record_permission_error_is_not_treated_as_removal(self):
        state = self.root / "mesh-transition.denied"
        state.mkdir()
        (state / "cohort.json").write_text("{}")
        with patch.object(clocks, "process_snapshot", return_value=self.rows()), patch.object(Path, "read_text", side_effect=PermissionError), self.assertRaises(PermissionError):
            clocks.NativeOwnership(Mock(pid=10), self.root, self.root, clocks.time.monotonic_ns() + clocks.SECOND)

    def test_missing_detached_identity_never_uses_a_late_root_scan(self):
        rows = self.rows()
        rows[30]["parent"] = 1
        state = self.root / "mesh-transition.control"
        state.mkdir()
        (state / "cohort.json").write_text(json.dumps({"workers": [{"pid": 20, "shellPid": 30}]}))
        with patch.object(clocks, "process_snapshot", return_value=rows):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, clocks.time.monotonic_ns() + clocks.SECOND)
        with patch.object(clocks, "process_snapshot", return_value={}):
            with self.assertRaisesRegex(RuntimeError, "missing.*30"):
                ownership.verify(clocks.time.monotonic_ns() + clocks.SECOND)

    def test_reused_expected_pid_is_not_owned_by_its_new_birth(self):
        rows = self.rows()
        with patch.object(clocks, "process_snapshot", return_value=rows):
            ownership = clocks.NativeOwnership(Mock(pid=10), self.root, self.root, clocks.time.monotonic_ns() + clocks.SECOND)
            ownership.note_pid(30, rows)
        changed = {30: rows[30] | {"birth": "another-lifetime", "parent": 1}}
        with patch.object(clocks, "process_snapshot", return_value=changed):
            self.assertEqual(ownership.live(clocks.time.monotonic_ns() + clocks.SECOND), {})
            with self.assertRaisesRegex(RuntimeError, "changedLifetimes.*30"):
                ownership.verify(clocks.time.monotonic_ns() + clocks.SECOND)

    def test_unresolved_transition_keeps_the_consumed_root_receipt_negative(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root)
        directory = self.root / "proof"
        directory.mkdir()
        releases.builds["target"] = {"path": "not-executed"}
        with patch.object(installed_releases.subprocess, "Popen"), patch.object(clocks, "NativeOwnership"), patch.object(installed_releases, "wait_transition", side_effect=RuntimeError("unresolved ownership")), patch.object(installed_releases, "stop_transition", side_effect=RuntimeError("retain root")):
            with self.assertRaisesRegex(RuntimeError, "retain root"):
                releases.run_transition("not-executed", "target", directory, {}, None)
        value = json.loads((self.root / "fixture-ownership.json").read_text())
        self.assertFalse(value["transitionsAbsent"])
        self.assertFalse(releases.transition_clean)
        releases.close()


if __name__ == "__main__":
    unittest.main()
