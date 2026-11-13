"""Pure/mocked fixture clock, handoff, cleanup and calibration controls."""
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_clock as clocks, installed_account_run, installed_releases, installed_order, installed_order_trace, installed_order_host


class Time:
    def __init__(self):
        self.value = 100 * clocks.SECOND

    def __call__(self):
        return self.value

    def advance(self, duration):
        self.value += duration


def diagnostic(**changes):
    return clocks.ClockPlan(**({"mode": "diagnostic", "workload": {"scenario": "chain"},
        "command_ns": 600 * clocks.SECOND, "external_ns": 240 * clocks.SECOND, "emergency_ns": 60 * clocks.SECOND} | changes))


def budget(key, elapsed=clocks.SECOND):
    workload = {"operation": key}
    sample = clocks.SampleRef({"harness": "calibration source", "memberSha256": "a" * 64}, workload, elapsed, True, True)
    return clocks.PhaseBudget(key, clocks.bound(elapsed), workload, (sample,))


class ClockTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="clock-control-")
        self.addCleanup(self.temp.cleanup)
        self.root, self.now = Path(self.temp.name), Time()

    def clock(self, plan=None, **kwargs):
        return clocks.Clock(plan or diagnostic(), now=self.now, **kwargs)

    def export(self, clock, cleaned=True):
        return clock.export(self.root / "clock.json", cleaned, {"source": "exact tested source"})

    def test_exact_integer_rounding(self):
        self.assertEqual(clocks.bound(206_560_223_778), 232_381_000_000)
        self.assertEqual(clocks.bound(84_348_697_836), 94_893_000_000)
        self.assertEqual(clocks.bound(79_585_554_795), 89_534_000_000)
        self.assertEqual(clocks.bound(8_000_000), 9_000_000)
        self.assertEqual(clocks.bound(8_000_001), 10_000_000)
        for invalid in (0, -1, 1.25, True):
            with self.assertRaises(ValueError):
                clocks.bound(invalid)

    def test_unbound_and_missing_executable_plan_fail_closed(self):
        with self.assertRaisesRegex(ValueError, "unbound"):
            clocks.ClockPlan().validate()
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root)
        with patch.object(installed_releases.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "explicit clock"):
                releases.start_signer()
        run.assert_not_called()
        releases.close()

    def test_signer_uses_run_deadline_not_the_short_startup_phase(self):
        clock = self.clock()
        clock.stack.append({"key": "startup", "deadline_ns": self.now() + clocks.SECOND})
        with clock.activate(), patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_releases.subprocess, "Popen") as launch, patch.object(installed_releases.native_services, "wait_for"):
            releases = installed_releases.Releases(self.root, "linux/amd64", self.root, clock_plan=clock.plan)
            releases.launch_signer()
        self.assertIn("-test.timeout=600000000000ns", launch.call_args.args[0])
        launch.assert_called_once()

    def test_diagnostic_never_self_sizes_or_accepts(self):
        clock = self.clock()
        with clock.activate():
            with clocks.operation("build:v9001"):
                self.now.advance(clocks.SECOND)
        value = self.export(clock)
        self.assertFalse(value["accepted"])
        self.assertTrue(value["measurement_completed"])
        self.assertEqual(clock.plan.phases, ())
        self.assertEqual(value["disjoint_ns"], value["command_ns"])
        self.assertEqual(value["leaves"][0]["end_ns"], value["leaves"][1]["start_ns"])

    def test_frozen_exact_sequence_and_derived_total(self):
        phases = tuple(budget(key) for key in ("setup#1", "work#1", "setup#2"))
        plan = clocks.ClockPlan("frozen", {"scenario": "chain"}, phases, (), sum(row.limit_ns for row in phases),
                                240 * clocks.SECOND, 60 * clocks.SECOND, "independent review", budget("external", 213_333_333_333))
        clock = self.clock(plan)
        with clock.operation("work"):
            self.now.advance(100_000_000)
        self.assertTrue(self.export(clock)["accepted"])

    def test_censored_wrong_workload_and_changed_bound_are_rejected(self):
        row = budget("setup#1")
        censored = clocks.SampleRef({"source": "same"}, row.workload, clocks.SECOND, False, True)
        invalid = clocks.PhaseBudget(row.key, row.limit_ns, row.workload, (censored,))
        for value in (invalid, clocks.PhaseBudget(row.key, row.limit_ns + 1, row.workload, row.samples),
                      clocks.PhaseBudget(row.key, row.limit_ns, {"operation": "changed"}, row.samples)):
            with self.assertRaises(ValueError):
                value.validate()

    def test_job_admission_includes_external_and_emergency(self):
        with self.assertRaisesRegex(clocks.ClockExpired, "admission"):
            self.clock(job_origin_ns=self.now() - 241 * clocks.SECOND)
        with self.assertRaises(ValueError):
            diagnostic(emergency_ns=61 * clocks.SECOND).validate()

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
            self.now.advance(600 * clocks.SECOND)
            with self.assertRaises(clocks.ClockExpired):
                clock.check()
            first = clock.cleanup_deadline_ns
            clock.fail(RuntimeError("cleanup also failed"))
        self.assertEqual(clock.cleanup_deadline_ns, first)
        timer.assert_called_with(signal.ITIMER_REAL, 0)
        value = self.export(clock, False)
        self.assertEqual(value["failures"][0]["type"], "ClockExpired")
        self.assertFalse(value["measurement_completed"])

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
        with patch.object(clocks, "send_group") as send, patch.object(clocks, "wait_group") as wait, patch.object(clocks, "native_descendants", return_value={}):
            clocks.stop_group(process, clocks.time.monotonic_ns() + 10 * clocks.SECOND)
        self.assertEqual(send.call_count, 2)
        send.assert_any_call(123, signal.SIGKILL)
        self.assertTrue(wait.call_args.kwargs["require_absent"])

    def test_actual_exited_wrapper_does_not_leave_its_sleep_child(self):
        code = "import subprocess; subprocess.Popen(['/bin/sleep','30'])"
        process = subprocess.Popen([sys.executable, "-c", code], start_new_session=True)
        try:
            process.wait(timeout=5)
            clocks.stop_group(process, clocks.time.monotonic_ns() + 10 * clocks.SECOND)
            self.assertFalse(clocks.group_alive(process.pid))
        finally:
            clocks.send_group(process.pid, signal.SIGKILL)
            process.wait(timeout=5)

    def test_ordered_recovery_has_finite_outer_timeout(self):
        ticket = self.root / "account-run.json"
        ticket.write_text(json.dumps({"cleanup_deadline_ns": clocks.time.monotonic_ns() + clocks.SECOND}))
        with patch.object(installed_account_run.subprocess, "run", side_effect=subprocess.TimeoutExpired("recover", 1)) as run:
            with self.assertRaises(subprocess.TimeoutExpired):
                installed_account_run.recover(ticket)
        self.assertLessEqual(run.call_args.kwargs["timeout"], 1)

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
        self.assertEqual(request["fixtureDeadlineNs"], self.now() + 30 * clocks.SECOND)

    def test_order_trace_sends_once_at_first_ready_write_with_injected_deadline(self):
        driver = installed_order.Driver.__new__(installed_order.Driver)
        driver.fixture_deadline_ns = driver.cleanup_deadline_ns = self.now() + 90 * clocks.SECOND
        driver.account, driver.process = {"role": "control"}, Mock()
        driver.process.poll.return_value = None
        driver.diagnostics = installed_order_trace.Trace("control")
        delivered, writes, expected_deadline = bytearray(), [], self.now() + 30 * clocks.SECOND

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

    def test_calibration_vehicle_has_full_matrix_and_no_dispatch_on_change(self):
        workflow = Path(".github/workflows/installed-clock-calibration.yml").read_text()
        reusable = Path(".github/workflows/installed-catchup.yml").read_text()
        self.assertIn("workflow_dispatch:", workflow)
        self.assertNotIn("pull_request:", workflow)
        self.assertIn("calibration_only: true", workflow)
        self.assertIn("platform: [linux/amd64, linux/arm64, darwin/arm64]", reusable)
        self.assertIn("mode: [oldest, chain, waypoint, order]", reusable)
        self.assertIn("timeout-minutes: 15", reusable)
        self.assertIn("--clock-command-seconds 600 --clock-external-seconds 240 --clock-emergency-seconds 60", reusable)
        self.assertIn("inputs.catalog_evidence && startsWith(matrix.platform, 'linux/') && matrix.mode == 'oldest'", reusable)


if __name__ == "__main__":
    unittest.main()
