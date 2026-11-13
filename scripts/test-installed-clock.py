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
from unittest.mock import MagicMock, Mock, patch

from fixtures import installed_clock as clocks, installed_account_run, installed_releases, installed_order, installed_order_trace, installed_order_host, installed_native, installed_accounts, installed_paths, native_services


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

    def test_approved_measurement_envelope_is_explicit_and_never_accepts(self):
        plan = diagnostic(command_ns=1500 * clocks.SECOND, admission_ns=1800 * clocks.SECOND)
        clock = self.clock(plan)
        self.assertEqual(clock.job_deadline_ns - clock.job_origin_ns, 1800 * clocks.SECOND)
        with clock.activate(), patch.object(clocks.time, "monotonic_ns", self.now), patch.object(installed_releases.subprocess, "Popen") as launch, patch.object(installed_releases.native_services, "wait_for"):
            releases = installed_releases.Releases(self.root, "linux/amd64", self.root, clock_plan=plan)
            releases.launch_signer()
        self.assertIn("-test.timeout=1500000000000ns", launch.call_args.args[0])
        self.assertFalse(self.export(clock)["accepted"])

    def test_default_measurement_and_absolute_job_ceiling_cannot_grow_implicitly(self):
        with self.assertRaises(ValueError):
            diagnostic(command_ns=700 * clocks.SECOND, external_ns=100 * clocks.SECOND).validate()
        with self.assertRaises(ValueError):
            diagnostic(command_ns=1500 * clocks.SECOND, admission_ns=1801 * clocks.SECOND).validate()

    def test_extended_measurement_must_fit_the_original_job_declaration(self):
        origin = self.root / "job-origin.json"
        job = {"origin_ns": clocks.time.monotonic_ns(), "run": "owned-run", "job": "owned-job", "admission_ns": 900 * clocks.SECOND}
        origin.write_text(json.dumps(job))
        args = SimpleNamespace(source_host="pc", scenario=None, worker_registry=None, clock_mode="diagnostic", clock_plan=None,
            clock_command_seconds=1500, clock_job_seconds=1800, clock_external_seconds=240, clock_emergency_seconds=60,
            clock_job_start=origin, fresh_managed=False, ordered_hosts=False, format_waypoint=False, signed_chain=False)
        with patch.dict(os.environ, GITHUB_RUN_ID="owned-run", GITHUB_JOB="owned-job"):
            with self.assertRaisesRegex(ValueError, "original job"):
                clocks.from_arguments(args, "linux/amd64")
            job["admission_ns"] = 1800 * clocks.SECOND
            origin.write_text(json.dumps(job))
            self.assertEqual(clocks.from_arguments(args, "linux/amd64").plan.admission_ns, 1800 * clocks.SECOND)

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
        timer.assert_any_call(signal.ITIMER_REAL, 0)
        timer.assert_called_with(signal.ITIMER_REAL, 60)
        value = self.export(clock, False)
        self.assertEqual(value["failures"][0]["type"], "ClockExpired")
        self.assertFalse(value["measurement_completed"])

    def test_operation_sample_uses_one_end_observation(self):
        def ticks():
            self.now.advance(101)
            return self.now()
        clock = clocks.Clock(diagnostic(), now=ticks)
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
        self.assertIn("worker_artifact_run: ${{ fromJSON(inputs.producer_run) }}", workflow)
        self.assertIn("required: true\n        type: string", workflow.split("producer_run:", 1)[1].split("producer_source:", 1)[0])
        self.assertIn("platform: [linux/amd64, linux/arm64, darwin/arm64]", reusable)
        self.assertIn("mode: [oldest, chain, waypoint, order]", reusable)
        self.assertIn("timeout-minutes: 15", reusable)
        self.assertIn("--clock-job-seconds 1800 --clock-command-seconds 1500 --clock-external-seconds 240 --clock-emergency-seconds 60", reusable)
        self.assertIn("timeout-minutes: ${{ inputs.calibration_only && 30 || 15 }}", reusable)
        self.assertIn("inputs.catalog_evidence && startsWith(matrix.platform, 'linux/') && matrix.mode == 'oldest'", reusable)

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
        self.assertEqual(process.wait.call_args_list[0].kwargs["timeout"], 28)
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
