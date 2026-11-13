"""Pure/mock controls; actual daemon coverage is a separate mandatory runtime gate."""
import ast
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_chain, installed_clock, installed_native
from fixtures import installed_transport_lifecycle as lifecycle


class LifecycleControls(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="mesh-lifecycle-control-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = patch.dict(os.environ, {lifecycle.FLAG: "1"})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.running = {"version": "v0.1.9001", "digest": "b" * 64, "commit": "a" * 40}
        self.identity = {"pid": 88} | self.running
        self.rows = [{"schema": 1, "sequence": 1, "stage": "process.begin", "dropped": 0, "identity": self.identity}]
        for stage in lifecycle.REQUIRED:
            self.rows.append({"schema": 1, "sequence": len(self.rows) + 1, "stage": stage, "dropped": 0,
                "connection": 1, "session": "AB12", "remote": "127.0.0.1:35428", "local": "127.0.0.1:56537", "error": "ok"})
        self.directory = self.root / "transport-lifecycle"
        self.directory.mkdir()
        self.path = self.directory / "lifecycle-88-owned.jsonl"
        self.write_rows()
        self.case = SimpleNamespace(root=self.root, state=self.root / "state", binary=self.root / "mesh",
            session="AB12", worker=43, shell=44, process_stamps={43: "worker-birth", 44: "shell-birth"},
            terminal=SimpleNamespace(pid=42, poll=Mock(return_value=None)),
            transport_original={"client": {"pid": 42, "sha256": "c" * 64}, "session": "AB12"},
            pair={"daemon": {"artifact": {"binarySha256": "c" * 64}}})

    def write_rows(self):
        self.path.write_text("".join(json.dumps(row) + "\n" for row in self.rows))

    def test_positive_gate_binds_original_client_daemon_and_tcp_tuple(self):
        image = {"pid": 88, "sha256": self.running["digest"], "inode": 7, "device": 8}
        with patch.object(installed_native, "peer_pid", return_value=88), \
                patch.object(installed_native.native_services, "mapped_image", return_value=image) as mapped, \
                patch.object(lifecycle, "original_sockets", return_value={("127.0.0.1:35428", "127.0.0.1:56537")}), \
                patch.object(installed_clock, "timeout", side_effect=lambda value: value):
            observed = lifecycle.prove(self.case, {"running": self.running})
            lifecycle.prove(self.case, {"running": self.running}, "later successful original attachment")
        mapped.assert_called_once_with(88, self.case.binary)
        self.assertEqual(observed["status"], "covered")
        self.assertEqual(observed["original"]["client"]["pid"], 42)
        self.assertTrue(observed["clientTcpTupleMatched"])
        self.assertEqual(observed["observed"]["connection"], 1)

    def test_gate_rejects_incomplete_wrong_process_wrong_image_and_missing_events(self):
        for change in (lambda rows: rows[0]["identity"].update(pid=99),
                       lambda rows: rows[0]["identity"].update(digest="c" * 64),
                       lambda rows: rows.pop()):
            rows = json.loads(json.dumps(self.rows))
            change(rows)
            with self.assertRaises(lifecycle.CoverageIncomplete):
                lifecycle.matching_original({"owned": rows}, 88, self.running, "AB12")

    def test_gate_rejects_lost_events_and_partial_records(self):
        self.rows[-1]["dropped"] = 1
        self.write_rows()
        with self.assertRaises(lifecycle.CoverageIncomplete):
            lifecycle.records(self.case)
        self.path.write_bytes(b'{"schema":1')
        with self.assertRaises(lifecycle.CoverageIncomplete):
            lifecycle.records(self.case)

    def test_gate_rejects_out_of_order_authentication_and_another_session(self):
        self.rows[3]["sequence"], self.rows[4]["sequence"] = self.rows[4]["sequence"], self.rows[3]["sequence"]
        with self.assertRaises(lifecycle.CoverageIncomplete):
            lifecycle.matching_original({"owned": self.rows}, 88, self.running, "AB12")
        self.rows[3]["sequence"], self.rows[4]["sequence"] = self.rows[4]["sequence"], self.rows[3]["sequence"]
        self.rows[-2]["session"] = "XXXX"
        with self.assertRaises(lifecycle.CoverageIncomplete):
            lifecycle.matching_original({"owned": self.rows}, 88, self.running, "AB12")

    def test_original_capture_samples_image_once_without_replacement(self):
        image = {"pid": 42, "sha256": "c" * 64, "inode": 7, "device": 8}
        terminal = self.case.terminal
        with patch.object(installed_native.native_services, "mapped_image", return_value=image) as mapped:
            lifecycle.capture_original(self.case)
        mapped.assert_called_once_with(42, self.case.binary)
        self.assertIs(self.case.terminal, terminal)
        self.assertEqual(self.case.transport_original["worker"], 43)

    def test_gate_rejects_update_only_or_revoked_grants(self):
        for before, after in (("grant.full", "grant.update"), ("grant.current", "grant.revoked")):
            rows = json.loads(json.dumps(self.rows))
            next(row for row in rows if row["stage"] == before)["stage"] = after
            with self.assertRaises(lifecycle.CoverageIncomplete):
                lifecycle.matching_original({"owned": rows}, 88, self.running, "AB12")

    def test_disabled_observation_is_noop(self):
        with patch.dict(os.environ, {lifecycle.FLAG: "0"}):
            lifecycle.configure(SimpleNamespace())
            lifecycle.capture_original(SimpleNamespace())
            self.assertIsNone(lifecycle.prove(SimpleNamespace(), {}))
            self.assertIsNone(lifecycle.retain(SimpleNamespace(), self.root, "off"))

    def test_clock_cannot_freeze_instrumented_samples_or_plan(self):
        workload = {"transportLifecycle": lifecycle.MARKER}
        sample = installed_clock.SampleRef({"source": "mock"}, workload, 1, True, True)
        self.assertFalse(sample.eligible(workload))
        plan = installed_clock.ClockPlan(mode="frozen", workload=workload)
        with self.assertRaisesRegex(ValueError, "cannot freeze"):
            plan.validate()
        clock = SimpleNamespace(plan=SimpleNamespace(mode="frozen", workload=workload))
        with patch.object(installed_clock, "current", return_value=clock):
            with self.assertRaisesRegex(lifecycle.CoverageIncomplete, "diagnostic"):
                lifecycle.configure(SimpleNamespace(root=self.root, environment={}))

    def test_retention_keeps_incomplete_logs_and_cannot_mask_primary_failure(self):
        self.rows[-1]["dropped"] = 1
        self.write_rows()
        output = self.root / "output"
        output.mkdir()
        result = lifecycle.retain(self.case, output, "case", RuntimeError("original failure"))
        self.assertEqual(result["status"], "incomplete")
        self.assertTrue((output / "case-transport-lifecycle" / self.path.name).exists())
        self.assertFalse(result["accepted"])

    def test_completed_instrumented_exports_are_ineligible_for_budgets(self):
        now = [100 * installed_clock.SECOND]
        plan = installed_clock.ClockPlan(mode="diagnostic", workload={"transportLifecycle": lifecycle.MARKER},
            command_ns=600 * installed_clock.SECOND, external_ns=240 * installed_clock.SECOND,
            emergency_ns=60 * installed_clock.SECOND)
        clock = installed_clock.Clock(plan, now=lambda: now[0])
        with clock.operation("observed"):
            now[0] += installed_clock.SECOND
        clock.switch("completion")
        now[0] += installed_clock.MILLISECOND
        result = clock.export(self.root / "clock.json", True, {"transportLifecycle": lifecycle.MARKER})
        self.assertTrue(result["measurement_completed"])
        self.assertFalse(result["accepted"])
        self.assertTrue(all(not row["eligible"] and row["diagnostic_complete"] for row in result["samples"] + result["leaves"]))

    def test_failed_coverage_prevents_selected_workload_and_preserves_exception(self):
        case = Mock()
        case.start_adoption.return_value = {"pin": "exact-pin"}
        primary = lifecycle.CoverageIncomplete("actual daemon events absent")
        with patch.object(installed_chain.installed_adoption, "AdoptionCase", return_value=case), \
                patch.object(lifecycle, "prove", side_effect=primary), patch.object(installed_chain, "finish"):
            with self.assertRaises(lifecycle.CoverageIncomplete) as raised:
                installed_chain.run_case(self.root, self.root, {}, {}, None, Mock(), Mock(), None, "final", {"index": 3})
        self.assertIs(raised.exception, primary)
        case.start_cohort.assert_not_called()
        case.write_fault.assert_not_called()
        case.units.stop.assert_not_called()

    def test_helper_failure_exports_only_sampled_numeric_metadata(self):
        primary = RuntimeError("exact original failure")
        primary.mapped_image = {"expected": {"executable": "/private", "device": 1, "inode": 2},
                                "mapped": {"executable": "/private-actual", "device": 3, "inode": 4}}
        self.assertEqual(lifecycle.sampled_failure(primary), {"expected": {"device": 1, "inode": 2}, "mapped": {"device": 3, "inode": 4}})
        self.assertEqual(str(primary), "exact original failure")

    def test_tcp_endpoint_decodes_linux_numeric_tuple(self):
        self.assertEqual(lifecycle.tcp_endpoint("0100007F:8A64"), "127.0.0.1:35428")
        self.assertEqual(lifecycle.tcp_endpoint("00000000000000000000000001000000:8A64"), "[::1]:35428")

    def test_new_python_blocks_remain_at_most_three_deep(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location("lifecycle_nesting", str(Path(lifecycle.__file__).parents[1] / "test-installed-catchup.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        from fixtures import transport_lifecycle_vehicle
        for path in (Path(lifecycle.__file__), Path(__file__), Path(transport_lifecycle_vehicle.__file__)):
            self.assertLessEqual(module.maximum_depth(ast.parse(path.read_text()), 0), 3, str(path))

    def test_vehicle_keeps_one_complete_original_v249_profile_and_source_pin(self):
        from fixtures import transport_lifecycle_vehicle
        import re
        vehicle = transport_lifecycle_vehicle.render()
        self.assertEqual(re.findall(r"^  ([a-z]+):$", vehicle.split("\njobs:\n", 1)[1], re.M), ["plans", "prepare", "consume"])
        self.assertIn("profile: [final]", vehicle)
        self.assertNotIn("profile: [none, first, final]", vehicle)
        self.assertIn("MESH_TRANSPORT_LIFECYCLE: '1'", vehicle)
        self.assertIn('"version"]=="v0.1.249"', vehicle)
        self.assertIn("--export-prepared", vehicle)
        self.assertIn("--installed-slice", vehicle)
        self.assertEqual(set(re.findall(r"inputs\.([a-z_]+)", vehicle)), {"source_sha", "producer_run", "producer_source", "producer_prefix"})
        self.assertIn('test "$(git rev-parse HEAD)" = "$MESH_HARNESS_SOURCE"', vehicle)
