"""Pure/mocked span grammar, source reduction, cursor and export regressions."""
from copy import deepcopy
from dataclasses import replace
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_clock as clocks, installed_span_controls as controls


class GrammarTest(unittest.TestCase):
    def setUp(self):
        self.plan = controls.reviewed_for_test()
        self.keys = controls.leaf_keys(self.plan)

    def reject(self, keys, plan=None):
        plan = plan or self.plan
        with self.assertRaises(ValueError):
            clocks.partition_leaf_keys(keys, plan.workload)
        parts = [list(row.workload["clockSpan"]["leaves"]) for row in plan.phases]
        lengths = [len(part) for part in parts]
        offset, changed = 0, []
        for length in lengths[:-1]:
            changed.append(keys[offset:offset + length])
            offset += length
        changed.append(keys[offset:])
        malformed = controls.with_parts(plan, changed)
        with tempfile.TemporaryDirectory(prefix="span-invalid-") as directory:
            path = Path(directory) / "plan.json"
            controls.write_plan(path, malformed)
            with self.assertRaises(ValueError):
                clocks.load_plan(path)

    def test_direct_script_import_and_negative_cleanup_need_no_pythonpath(self):
        source = Path(clocks.__file__).resolve()
        environment = {key: value for key, value in os.environ.items() if key != "PYTHONPATH"}
        environment["PYTHONDONTWRITEBYTECODE"] = "1"
        result = subprocess.run([sys.executable, "-B", "-c", "import runpy,sys; runpy.run_path(sys.argv[1], run_name='import_probe')", str(source)],
                                env=environment, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        with tempfile.TemporaryDirectory(prefix="span-cleanup-cli-") as directory:
            root = Path(directory)
            (root / "cohort-proof").mkdir()
            (root / "cohort-proof/fixture-ownership.json").write_text('{"signerAbsent":false,"transitionsAbsent":false}')
            result = subprocess.run([sys.executable, "-B", str(source), "cleanup-inputs", str(root)],
                                    env=environment, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(json.loads((root / "cohort-control/tools-cleanup-decision.json").read_text())["inputsMayBeRemoved"])

    def test_terminal_exception_is_exact_and_unique(self):
        for keys in (self.keys[:-1], self.keys[:-2], self.keys + ["completion#3"],
                     self.keys[:-2] + ["setup#14"] + self.keys[-2:], self.keys[-2:] + self.keys[:-2]):
            self.reject(keys)

    def test_unknown_roots_heads_resumed_owners_and_duplicates_fail(self):
        for old, new in (("signer-build:#1", "unknown:#1"), ("head:v0.1.9001#1", "head:v0.1.9999#1"),
                         ("proof:v0.1.249:v0.1.9001#2", "proof:other-owner#2"), ("signer-start#1", "setup#1")):
            self.reject([new if key == old else key for key in self.keys])
        self.reject(self.keys[:1] + ["head:v0.1.9001#99"] + self.keys[1:])
        self.reject(self.keys[:2] + ["setup#99"] + self.keys[2:])

    def test_groups_readers_and_serving_cannot_move_or_merge(self):
        plan = controls.reviewed_for_test("linux-amd64-waypoint-consume-none")
        keys = controls.leaf_keys(plan)
        self.reject([key.replace(":chain-none:0#", ":chain-none:9#") for key in keys], plan)
        self.reject([key.replace("readers:0#", "readers:9#") for key in keys], plan)
        changed = keys.copy()
        a, b = changed.index("serve:public#1"), changed.index("readers:0#1")
        changed[a], changed[b] = changed[b], changed[a]
        self.reject(changed, plan)
        parts = [row.workload["clockSpan"]["leaves"] for row in plan.phases]
        combined = parts[:3] + [parts[3] + parts[4]] + parts[5:]
        with self.assertRaises(ValueError):
            controls.with_parts(plan, combined).validate()

    def test_membership_projection_source_endpoints_and_old_contract_fail_closed(self):
        phase = self.plan.phases[0]
        variants = [replace(phase, key="wrong"), replace(phase, workload={"clockSpan": phase.workload["clockSpan"]}),
                    replace(phase, samples=(replace(phase.samples[0], workload=self.plan.workload),))]
        for changed in variants:
            with self.assertRaises(ValueError):
                replace(self.plan, phases=(changed,) + self.plan.phases[1:]).validate()
        old = replace(phase, key="setup#1", workload=self.plan.workload,
                      samples=tuple(replace(row, workload=self.plan.workload) for row in phase.samples))
        with self.assertRaisesRegex(ValueError, "explicit span membership"):
            replace(self.plan, phases=(old,) + self.plan.phases[1:]).validate()
        with self.assertRaises(ValueError):
            replace(self.plan, guards=(phase,)).validate()

    def test_reviewed_activated_files_load_and_unreviewed_copy_stays_blocked(self):
        index = json.loads((controls.ROOT / "index.json").read_text())["plans"]
        self.assertEqual(len(index), 37)
        bootstrap = 0
        for name, entry in index.items():
            path = controls.ROOT / entry["path"]
            value = self.check_plan_file(path)
            bootstrap += bool(value["phases"][0].get("telemetry"))
            controls.reviewed_for_test(name).validate()
        self.assertEqual(bootstrap, 19)
        value = json.loads((controls.ROOT / index["linux-amd64-fresh"]["path"]).read_text())
        value["eligibility_review"] = ""
        with tempfile.TemporaryDirectory(prefix="unreviewed-span-") as directory:
            path = Path(directory) / "candidate.json"
            path.write_text(json.dumps(value))
            with self.assertRaisesRegex(ValueError, "reviewed eligibility"):
                clocks.load_plan(path)

    def check_plan_file(self, path):
        value = json.loads(path.read_text())
        if not value["eligibility_review"]:
            self.assertTrue(value["phases"][0].get("telemetry"))
            with self.assertRaisesRegex(ValueError, "reviewed eligibility"):
                clocks.load_plan(path)
            return value
        self.assertNotIn("TEST ONLY", clocks.load_plan(path).eligibility_review)
        return value

    def test_bootstrap_ends_at_last_original_acquisition_with_exact_telemetry_order(self):
        for name in ("linux-amd64-fresh", "linux-amd64-oldest-catalog", "linux-amd64-adoption-all",
                     "linux-arm64-adoption-interrupt", "linux-amd64-chain-prepare", "linux-amd64-order"):
            plan = controls.reviewed_for_test(name)
            bootstrap = plan.phases[0]
            self.assertTrue(bootstrap.workload["clockSpan"]["leaves"][-1].startswith("acquire:"))
            self.assertEqual(bootstrap.workload["clockSpan"]["leaves"],
                             [key for row in bootstrap.telemetry for key in row.workload["clockSpan"]["leaves"]])
            self.assertFalse(any(key.startswith("acquire:") for row in plan.phases[1:] for key in row.workload["clockSpan"]["leaves"]))
            self.assertFalse(any(key.startswith("publish:") for key in bootstrap.workload["clockSpan"]["leaves"]))

    def test_bootstrap_missing_reordered_extended_or_nested_telemetry_is_rejected(self):
        phase = self.plan.phases[0]
        members = phase.telemetry
        variants = ((), members[:-1], members[1:] + members[:1], members + self.plan.phases[1:2],
                    (replace(members[0], telemetry=(members[1],)),) + members[1:])
        for telemetry in variants:
            with self.assertRaisesRegex(ValueError, "phase"):
                replace(self.plan, phases=(replace(phase, telemetry=telemetry),) + self.plan.phases[1:]).validate()
        sample = phase.samples[0]
        provenance = deepcopy(sample.provenance)
        provenance["clock_span"]["end_ns"] -= 1
        changed = replace(phase, samples=(replace(sample, provenance=provenance, elapsed_ns=sample.elapsed_ns - 1),))
        with self.assertRaisesRegex(ValueError, "phase endpoints"):
            replace(self.plan, phases=(changed,) + self.plan.phases[1:]).validate()

    def test_preparation_has_exact_disjoint_ready_endpoint_and_rejects_overlap(self):
        plan = controls.reviewed_for_test("linux-amd64-adoption-interrupt")
        bootstrap, preparation = plan.phases[:2]
        first = preparation.samples[0].provenance["clock_span"]
        self.assertEqual(first["start_ns"], 202_236_162_263)
        self.assertEqual(first["end_ns"], 233_265_468_440)
        self.assertEqual(preparation.limit_ns, 37_030_000_000)
        self.assertEqual(first["start_ns"], bootstrap.samples[0].provenance["clock_span"]["end_ns"])
        self.assertTrue(preparation.workload["clockSpan"]["leaves"][-1].startswith("head:"))
        self.assertFalse(set(bootstrap.workload["clockSpan"]["leaves"]).intersection(preparation.workload["clockSpan"]["leaves"]))
        invalid = replace(preparation, telemetry=preparation.telemetry[:-1])
        with self.assertRaisesRegex(ValueError, "phase telemetry"):
            replace(plan, phases=(bootstrap, invalid) + plan.phases[2:]).validate()
        parts = [row.workload["clockSpan"]["leaves"] for row in plan.phases]
        with self.assertRaisesRegex(ValueError, "sequence|partition"):
            controls.with_parts(plan, [parts[0], parts[0][-1:] + parts[1]] + parts[2:]).validate()
        sample = preparation.samples[0]
        provenance = deepcopy(sample.provenance)
        provenance["clock_span"]["end_ns"] -= 1
        changed = replace(sample, provenance=provenance, elapsed_ns=sample.elapsed_ns - 1)
        invalid = replace(preparation, samples=(changed,) + preparation.samples[1:])
        with self.assertRaisesRegex(ValueError, "phase endpoints"):
            replace(plan, phases=(bootstrap, invalid) + plan.phases[2:]).validate()


class ReductionTest(unittest.TestCase):
    def setUp(self):
        self.plan = controls.reviewed_for_test()
        self.raw = controls.record(self.plan.workload)

    def test_whole_same_invocation_maximum_and_exact_millisecond_rounding(self):
        first, second = deepcopy(self.raw), deepcopy(self.raw)
        first["leaves"][0]["elapsed_ns"], first["leaves"][1]["elapsed_ns"] = 7_000_000, 1_000_000
        second["leaves"][0]["elapsed_ns"], second["leaves"][1]["elapsed_ns"] = 1_000_000, 7_000_001
        self.retime(first)
        self.retime(second)
        phases = clocks.reduce_span_budgets([controls.observed(first), controls.observed(second, "b")])
        self.assertEqual(phases[0].limit_ns, 14_000_000)
        self.assertEqual([row.elapsed_ns for row in phases[0].samples], [12_000_000, 12_000_001])
        self.assertNotEqual(phases[0].limit_ns, clocks.bound(18_000_001))
        self.assertEqual([row.elapsed_ns for row in phases[0].telemetry[0].samples], [8_000_000, 8_000_001])
        sample = phases[0].samples[1]
        self.assertEqual(sample.provenance["clock_span"]["start_ns"], second["leaves"][0]["start_ns"])
        self.assertEqual(sample.provenance["clock_span"]["end_ns"], second["leaves"][5]["end_ns"])

    def retime(self, record):
        cursor = record["origin_ns"]
        for leaf in record["leaves"]:
            leaf.update(start_ns=cursor, end_ns=cursor + leaf["elapsed_ns"])
            cursor = leaf["end_ns"]
        record.update(end_ns=cursor, command_ns=cursor - record["origin_ns"], disjoint_ns=cursor - record["origin_ns"])
        by_key = {row["key"]: row for row in record["leaves"]}
        for sample in record["samples"]:
            delta = by_key[sample["key"]]["start_ns"] - sample["start_ns"]
            sample.update(start_ns=sample["start_ns"] + delta, end_ns=sample["end_ns"] + delta)
        record["samples"][0].update(start_ns=record["leaves"][1]["start_ns"], end_ns=record["leaves"][1]["end_ns"], elapsed_ns=record["leaves"][1]["elapsed_ns"])

    def test_failed_partial_instrumented_or_unclean_invocations_never_size_spans(self):
        for change in ({"measurement_completed": False}, {"failures": [{}]}, {"missing_sample_keys": ["missing"]},
                       {"unresolved_key_sets": ["pending"]}, {"provenance": {"transportLifecycle": True}},
                       {"workload": self.raw["workload"] | {"transportLifecycle": True}}):
            with self.assertRaises(ValueError):
                clocks.reduce_span_budgets([controls.observed(self.raw | change)])
        for field, value in (("eligible", False), ("completed", False), ("cleanup_verified", False), ("censored", True),
                             ("start_ns", self.raw["origin_ns"] + 1), ("elapsed_ns", 0)):
            changed = deepcopy(self.raw)
            changed["leaves"][0][field] = value
            with self.assertRaises(ValueError):
                clocks.reduce_span_budgets([controls.observed(changed)])

    def test_sample_endpoint_forgery_gap_or_mixed_source_rejects(self):
        for field in ("start_ns", "end_ns", "origin_ns", "command_end_ns"):
            phase = self.plan.phases[0]
            sample = phase.samples[0]
            provenance = deepcopy(sample.provenance)
            provenance["clock_span"][field] += 1
            changed = replace(phase, samples=(replace(sample, provenance=provenance),))
            with self.assertRaises(ValueError):
                replace(self.plan, phases=(changed,) + self.plan.phases[1:]).validate()
        phase = self.plan.phases[1]
        sample = phase.samples[0]
        provenance = deepcopy(sample.provenance)
        provenance["fixture_record"]["sha256"] = "b" * 64
        changed = replace(phase, samples=(replace(sample, provenance=provenance),))
        with self.assertRaises(ValueError):
            replace(self.plan, phases=self.plan.phases[:1] + (changed,) + self.plan.phases[2:]).validate()


class CursorTest(unittest.TestCase):
    def setUp(self):
        self.now = 100 * clocks.SECOND
        self.plan = controls.reviewed_for_test()
        self.clock = clocks.Clock(self.plan, now=lambda: self.now)
        self.temporary = tempfile.TemporaryDirectory(prefix="span-cursor-")
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / "clock.json"

    def move_to(self, key):
        for expected in controls.leaf_keys(self.plan)[1:]:
            if self.clock.active_key == key:
                return
            self.now += 1
            self.clock.switch(expected.rsplit("#", 1)[0])
        self.assertEqual(self.clock.active_key, key)

    def test_nested_resume_serving_substeps_never_renew_a_span(self):
        self.move_to("proof:v0.1.249:v0.1.9001#1")
        deadline = self.clock.effective_deadline()
        with self.clock.operation("native-transition:v0.1.249:v0.1.9001"):
            self.now += clocks.MILLISECOND
            self.assertEqual(self.clock.effective_deadline(), deadline)
        self.assertEqual(self.clock.active_key, "proof:v0.1.249:v0.1.9001#2")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        self.move_to_from_active("serve:releases#1")
        deadline = self.clock.effective_deadline()
        self.move_to_from_active("head:v0.1.9001#1")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        self.assertEqual(self.clock.deadline_ns, 100 * clocks.SECOND + self.plan.command_ns)

    def test_bootstrap_members_can_exceed_telemetry_without_renewing_absolute_deadline(self):
        phase = self.plan.phases[0]
        deadline = self.clock.effective_deadline()
        self.clock.switch("signer-build:")
        self.now += phase.telemetry[0].limit_ns + 1
        self.clock.switch("setup")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        with self.clock.operation("signer-start"):
            self.now += phase.telemetry[1].limit_ns + 1
        self.assertEqual(self.clock.effective_deadline(), deadline)
        self.clock.switch("acquire:v0.1.249")
        self.now = deadline - 1
        self.clock.check()
        telemetry = self.clock.recorded_telemetry([clocks.qualify(row, True) for row in self.clock.rows])
        self.assertGreater(telemetry[0]["elapsed_ns"], telemetry[0]["limit_ns"])
        self.assertTrue(all(row["enforced"] is False and row["phase_deadline_ns"] == deadline for row in telemetry))
        self.now = deadline
        with self.assertRaises(clocks.ClockExpired):
            self.clock.switch("setup")
        self.assertEqual(self.clock.span_index, 0)
        cleanup = self.clock.cleanup_deadline_ns
        self.clock.fail(RuntimeError("secondary"))
        self.assertEqual(self.clock.cleanup_deadline_ns, cleanup)
        self.assertEqual(self.clock.failures[0]["type"], "ClockExpired")

    def test_interleaved_bootstrap_proof_nesting_keeps_deadline_and_independent_guard(self):
        self.plan = controls.reviewed_for_test("linux-amd64-adoption-all")
        self.clock = clocks.Clock(self.plan, now=lambda: self.now)
        deadline = self.clock.effective_deadline()
        self.move_to("proof:v0.1.249:v0.1.9001#1")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        with self.clock.operation("native-transition:v0.1.249:v0.1.9001"):
            self.now += clocks.MILLISECOND
            self.assertEqual(self.clock.effective_deadline(), deadline)
            self.clock.stack.append({"key": "independent", "deadline_ns": self.now + 10})
            self.assertEqual(self.clock.effective_deadline(), self.now + 10)
            self.clock.stack.pop()
        self.assertEqual(self.clock.effective_deadline(), deadline)
        self.assertEqual(self.clock.active_key, "proof:v0.1.249:v0.1.9001#2")

    def test_preparation_publication_telemetry_does_not_reset_or_expire_phase(self):
        self.plan = controls.reviewed_for_test("linux-amd64-adoption-interrupt")
        self.clock = clocks.Clock(self.plan, now=lambda: self.now)
        self.move_to("proof:v0.1.255:v0.1.9001#1")
        deadline = self.clock.effective_deadline()
        self.move_to_from_active("publish:v0.1.9001#1")
        publication = self.plan.phases[1].telemetry[1]
        self.assertEqual(publication.limit_ns, 122 * clocks.MILLISECOND)
        self.now += publication.limit_ns + 1
        self.clock.switch("setup")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        self.move_to_from_active("head:v0.1.9001#1")
        self.assertEqual(self.clock.effective_deadline(), deadline)
        telemetry = self.clock.recorded_telemetry([clocks.qualify(row, True) for row in self.clock.rows])
        published = next(row for row in telemetry if row["key"] == publication.key)
        self.assertGreater(published["elapsed_ns"], publication.limit_ns)
        self.assertEqual(published["phase_deadline_ns"], deadline)
        self.assertFalse(published["enforced"])
        self.now = deadline
        with self.assertRaises(clocks.ClockExpired):
            self.clock.switch("setup")
        self.assertEqual(self.clock.span_index, 1)
        cleanup = self.clock.cleanup_deadline_ns
        self.clock.fail(RuntimeError("secondary"))
        self.assertEqual(self.clock.cleanup_deadline_ns, cleanup)

    def move_to_from_active(self, key):
        keys = controls.leaf_keys(self.plan)
        for expected in keys[keys.index(self.clock.active_key) + 1:]:
            self.now += 1
            self.clock.switch(expected.rsplit("#", 1)[0])
            if expected == key:
                return
        self.fail("target key absent")

    def test_equality_and_delayed_alarm_expire_before_a_new_span(self):
        for delay in (0, 1):
            clock = clocks.Clock(self.plan, now=lambda: self.now)
            clock.switch("signer-build:")
            deadline = clock.effective_deadline()
            self.now = deadline + delay
            with self.assertRaises(clocks.ClockExpired):
                clock.switch("setup")
            self.assertEqual(clock.span_index, 0)
            cleanup = clock.cleanup_deadline_ns
            self.now = cleanup
            with patch.object(clocks.signal, "setitimer"), self.assertRaises(clocks.ClockExpired):
                clock.alarm(signal.SIGALRM, None)
            self.assertEqual(clock.cleanup_deadline_ns, cleanup)

    def test_command_and_guard_handoff_minima_cannot_borrow(self):
        self.clock.stack.append({"key": "guard", "deadline_ns": self.now + 10})
        with self.clock.activate(), patch.object(clocks.time, "monotonic_ns", lambda: self.now):
            self.assertEqual(clocks.parent_deadline(600), self.now + 10)
            with clocks.handoff(self.now + 5):
                self.assertEqual(clocks.parent_deadline(480), self.now + 5)
        self.clock.stack.clear()
        self.clock.deadline_ns = self.now + 2
        self.clock.switch("signer-build:")
        self.now += 2
        with self.assertRaises(clocks.ClockExpired):
            self.clock.switch("setup")

    def test_group_resume_and_early_completion_cannot_lend_allowance(self):
        self.plan = controls.reviewed_for_test("linux-amd64-chain-consume-first")
        self.clock = clocks.Clock(self.plan, now=lambda: self.now)
        self.move_to("installed-group:pc:chain-first:0#1")
        end = self.clock.effective_deadline()
        self.move_to_from_active("installed-group:pc:chain-first:0#4")
        self.assertEqual(self.clock.effective_deadline(), end)
        self.now = end
        with self.assertRaises(clocks.ClockExpired):
            self.clock.switch("setup")
        self.assertEqual(self.clock.span_index, 3)

    def test_incorrect_next_key_latches_failure_and_cleanup_is_fixed(self):
        with self.assertRaises(ValueError):
            self.clock.switch("not-declared")
        first, end = self.clock.failures[0], self.clock.cleanup_deadline_ns
        with self.assertRaises(clocks.ClockExpired):
            with self.clock.operation("new-work"):
                pass
        self.now += 1
        self.clock.fail(RuntimeError("secondary"))
        self.assertEqual(self.clock.failures[0], first)
        self.assertEqual(self.clock.cleanup_deadline_ns, end)
        self.assertFalse(self.clock.export(self.path, True, {})["accepted"])

    def test_boundary_timestamp_is_checked_once_and_export_stays_in_final_span(self):
        self.clock.switch("signer-build:")
        now = self.clock.origin_ns + self.plan.phases[0].telemetry[0].limit_ns - 1
        self.clock.now = Mock(return_value=now)
        self.clock.stack.append({"key": "signer-build:#1", "deadline_ns": self.clock.deadline_ns})
        self.clock.finish_operation("signer-build:#1", self.clock.started_ns, "setup")
        self.clock.now.assert_called_once()
        self.clock.now = lambda: self.now
        self.now = now
        self.move_to_from_active("completion#2")
        deadline = self.clock.effective_deadline()
        write = Path.write_text
        def slow(path, data, *args, **kwargs):
            self.now += (deadline - self.now) if not path.exists() else 1
            return write(path, data, *args, **kwargs)
        with patch.object(Path, "write_text", slow):
            value = self.clock.export(self.path, True, {})
        self.assertFalse(value["accepted"])
        self.assertTrue(value["leaves"][-1]["censored"])
        self.assertFalse(value["spans"][-1]["completed"])
        self.assertEqual(value["spans"][-1]["end_ns"], value["leaves"][-1]["end_ns"])
        self.assertFalse(json.loads(self.path.read_text())["accepted"])


if __name__ == "__main__":
    unittest.main()
