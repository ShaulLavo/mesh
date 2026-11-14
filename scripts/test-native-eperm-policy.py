#!/usr/bin/env python3
"""Pure/mock controls for the three parallel normal AMD64 proof jobs."""
import ast
from copy import deepcopy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from fixtures import linux_eperm_control as control, release_worker_eperm as eperm


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


policy = module("native_eperm", "native-eperm-policy.py")
runner = module("ordinary_proofs", "run-update-proofs.py")
summary = module("native_eperm_summary", "summarize-native-eperm.py")
REPO = Path(__file__).resolve().parents[1]


def snapshot_value():
    same = {name: {"same": True, "original": name + ":[1]", "observed": name + ":[1]"} for name in ("user", "mnt")}
    return {"cleanupConfirmed": True, "mounts": [{"mountId": "1", "options": "rw", "filesystem": "ext4"}] * 3,
            "sysctls": dict.fromkeys(control.snapshot.SYSCTLS, "123\n"),
            "namespacesComparedToLaunch": dict.fromkeys(("workerAtObservation", "workerAtFailure", "launcherAtFailure"), same),
            "kernelJournal": {"arguments": ["journalctl", "-k", "--since", "@1", "--until", "@2"],
                              "returncode": 0, "output": "-- No entries --\n",
                              "cleanup": {"pid": 9753, "reaped": True, "stdoutClosed": True}}}


def control_value():
    return {"passed": True, "cleanupConfirmed": True, "knownChildPid": 9752, "childReturncode": 0,
            "snapshot": snapshot_value()}


class PolicyTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="eperm-ci-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name)
        self.enterContext(patch.dict(os.environ, GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="1", GITHUB_JOB="prove"))
        self.root = self.parent / "update-proof"
        self.root.mkdir()
        self.native = self.parent / "mesh-native.noindex"
        self.native.mkdir()
        self.payload = self.native / "original"
        self.payload.write_bytes(b"retained input, never executed")
        policy.write(self.root / "eperm-state.json", {"controlAttempted": True, "proofAttempted": False})
        policy.write(self.root / "positive-control.json", control_value())
        self.ledger = self.root / "eperm-ledger.json"
        policy.write(self.ledger, {"schema": 1, "cap": None, "events": []})

    def initialize(self):
        (self.root / "eperm-state.json").unlink()
        self.ledger.unlink()
        with patch.object(policy, "require_host"), patch.object(control, "positive_control") as healthy:
            policy.initialize(self.root, 0)
        healthy.assert_called_once_with(self.root / "positive-control.json")

    def test_exclusive_initialization_precedes_control_and_cannot_reset(self):
        self.initialize()
        original = self.ledger.read_bytes()
        inode = self.ledger.stat().st_ino
        with patch.object(policy, "require_host"), patch.object(control, "positive_control") as healthy:
            with self.assertRaises(FileExistsError):
                policy.initialize(self.root, 0)
        healthy.assert_not_called()
        self.assertEqual(self.ledger.read_bytes(), original)
        self.assertEqual(self.ledger.stat().st_ino, inode)

    def test_preexisting_ledger_is_never_truncated_even_without_state(self):
        (self.root / "eperm-state.json").unlink()
        original = b'{"schema":1,"cap":2,"events":[{"original":"failure"}]}'
        self.ledger.write_bytes(original)
        with patch.object(policy, "require_host"), patch.object(control, "positive_control") as healthy:
            with self.assertRaises(FileExistsError):
                policy.initialize(self.root, 0)
        healthy.assert_not_called()
        self.assertEqual(self.ledger.read_bytes(), original)

    def test_control_failure_keeps_evidence_and_prevents_begin(self):
        (self.root / "eperm-state.json").unlink()
        self.ledger.unlink()
        with patch.object(policy, "require_host"), patch.object(control, "positive_control", side_effect=RuntimeError("healthy failed")):
            with self.assertRaisesRegex(RuntimeError, "healthy failed"):
                policy.initialize(self.root, 0)
        policy.write(self.root / "positive-control.json", {"passed": False})
        with self.assertRaisesRegex(ValueError, "healthy control"):
            policy.begin(self.root, "/tmp/ms-mock.noindex")
        self.assertFalse(policy.read_state(self.root)["proofAttempted"])

    def test_runtime_refuses_local_host(self):
        with patch.dict(os.environ, {}, clear=True), self.assertRaisesRegex(ValueError, "hosted-only"):
            policy.require_host()

    def test_begin_inherits_one_shared_ledger_without_reset(self):
        original = self.ledger.read_bytes()
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)):
            policy.begin(self.root, "/tmp/ms-mock.noindex")
            with self.assertRaisesRegex(ValueError, "already attempted"):
                policy.begin(self.root, "/tmp/ms-other.noindex")
        self.assertEqual(self.ledger.read_bytes(), original)
        self.assertEqual(policy.read_state(self.root)["scratch"], "/tmp/ms-mock.noindex")

    def test_begin_refuses_missing_corrupt_and_already_used_ledger(self):
        self.ledger.unlink()
        self.check_bad_begin()
        for data in ("invalid", "{}", '{"schema":1,"cap":2,"events":[{}]}'):
            self.ledger.write_text(data)
            self.check_bad_begin()

    def check_bad_begin(self):
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)), self.assertRaises((OSError, ValueError)):
            policy.begin(self.root, "/tmp/ms-mock.noindex")
        self.assertFalse(policy.read_state(self.root)["proofAttempted"])

    def test_other_ledger_and_foreign_scratch_refuse_begin(self):
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.root / "other.json")), self.assertRaises(ValueError):
            policy.begin(self.root, "/tmp/ms-mock.noindex")
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)), self.assertRaisesRegex(ValueError, "ownership path"):
            policy.begin(self.root, "/tmp/unrelated")

    def test_clean_prelaunch_removal_keeps_proofs_control_ledger_and_decision(self):
        proof = self.root / "original-proof.json"
        proof.write_bytes(b"proof")
        policy.cleanup(self.root, self.native)
        policy.cleanup(self.root, self.native)
        self.assertFalse(self.native.exists())
        self.assertEqual(proof.read_bytes(), b"proof")
        self.assertTrue(self.ledger.exists())
        self.assertTrue(json.loads((self.root / "eperm-cleanup.json").read_bytes())["payloadsAbsent"])

    def confirmed_native(self):
        short = Path(self.enterContext(tempfile.TemporaryDirectory(prefix="ms-", suffix=".noindex", dir="/tmp")))
        state = policy.read_state(self.root)
        state.update(proofAttempted=True, proofReturned=True, proofStatus=0, scratch=str(short))
        policy.write(self.root / "eperm-state.json", state)
        group = self.root / "proofs/groups" / ("a" * 64) / "forward-only/0"
        group.mkdir(parents=True)
        policy.write(group / "selected.json", {})
        policy.write(group / "cleanup.json", {"processesAbsent": True, "stateAbsent": True, "stateRun": str(short / "removed")})
        return short, group

    def test_successful_complete_proof_removes_payload_and_short_scratch_only(self):
        short, group = self.confirmed_native()
        policy.cleanup(self.root, self.native)
        policy.cleanup(self.root, self.native)
        self.assertFalse(short.exists())
        self.assertFalse(self.native.exists())
        self.assertTrue((group / "selected.json").exists())

    def test_incomplete_native_traps_preserve_payload_without_signals(self):
        short, group = self.confirmed_native()
        (group / "cleanup.json").unlink()
        self.assert_preserved()
        self.assertTrue(short.exists())

    def test_failed_or_interrupted_proof_preserves_even_when_group_cleanup_passed(self):
        self.confirmed_native()
        policy.returned(self.root, 1)
        self.assert_preserved()
        state = policy.read_state(self.root)
        state["proofReturned"] = False
        policy.write(self.root / "eperm-state.json", state)
        self.assert_preserved()

    def test_leftover_native_scratch_is_preserved(self):
        short, _ = self.confirmed_native()
        (short / "unconfirmed-state").mkdir()
        self.assert_preserved()

    def assert_preserved(self):
        with self.assertRaisesRegex(RuntimeError, "preserved"), patch.object(policy.shutil, "rmtree") as remove:
            policy.cleanup(self.root, self.native)
        remove.assert_not_called()
        self.assertTrue(self.payload.exists())
        result = json.loads((self.root / "eperm-cleanup.json").read_bytes())
        self.assertFalse(result["payloadsMayBeRemoved"])
        return result

    def test_missing_and_malformed_required_ledger_preserve_payload(self):
        self.ledger.unlink()
        self.assert_preserved()
        for data in ("broken", "[]", "{}", '{"schema":1,"cap":2,"events":[{}]}'):
            self.ledger.write_text(data)
            self.assert_preserved()

    def test_diagnostic_pid_error_records_survive_native_success_and_bad_state(self):
        self.confirmed_native()
        value = snapshot_value()
        value["kernelJournal"]["cleanup"].update(reaped=False, error={"unavailable": "wait timed out", "errno": None})
        policy.write(self.ledger, {"schema": 1, "cap": 2, "events": [{"failure": {"snapshot": value}}]})
        result = self.assert_preserved()
        expected = value["kernelJournal"]["cleanup"]
        self.assertEqual(result["diagnosticCleanup"]["snapshots"][0]["commands"][0]["cleanup"], expected)
        (self.root / "eperm-state.json").write_text("invalid")
        result = self.assert_preserved()
        self.assertEqual(result["diagnosticCleanup"]["snapshots"][0]["commands"][0]["cleanup"], expected)
        self.assertEqual(json.loads((self.root / "eperm-counter.json").read_bytes())["events"], 1)

    def test_control_header_cannot_hide_its_unreaped_kernel_reader(self):
        value = control_value()
        value["snapshot"]["kernelJournal"]["cleanup"]["reaped"] = False
        policy.write(self.root / "positive-control.json", value)
        result = self.assert_preserved()
        self.assertEqual(result["controlCleanup"]["snapshot"]["commands"][0]["cleanup"]["pid"], 9753)

    def test_each_diagnostic_command_requires_reap_and_stdout_records(self):
        for name in ("kernelJournal", "kernelDmesg", "hostedSudoKernelJournal"):
            self.check_command_flags(name)

    def check_command_flags(self, name):
        for field in ("reaped", "stdoutClosed"):
            value = snapshot_value()
            value[name] = deepcopy(value["kernelJournal"])
            value[name]["cleanup"][field] = False
            self.assertFalse(eperm.snapshot_cleanup(value)["confirmed"])

    def test_empty_or_missing_snapshot_cannot_grant_cleanup(self):
        for value in ({}, {"cleanupConfirmed": True}, {"kernelJournal": {}}):
            self.assertFalse(eperm.snapshot_cleanup(value)["confirmed"])

    def test_missing_control_or_state_preserves_inputs(self):
        (self.root / "positive-control.json").unlink()
        self.assert_preserved()
        (self.root / "eperm-state.json").unlink()
        self.assert_preserved()

    def test_successful_retry_still_counts_and_allows_confirmed_cleanup(self):
        self.confirmed_native()
        event = {"session": "W00D", "version": "v0.1.254", "digest": "a" * 64,
                 "failure": {"snapshot": snapshot_value(), "pid": 9754, "outcome": "EPERM", "exception": {"message": "original EPERM"}},
                 "retry": {"outcome": "started", "pid": 9755}}
        policy.write(self.ledger, {"schema": 1, "cap": 2, "events": [event]})
        with patch.object(policy.sys, "stdout", new_callable=io.StringIO) as log:
            policy.cleanup(self.root, self.native)
        counter = json.loads((self.root / "eperm-counter.json").read_bytes())
        self.assertEqual(counter["events"], 1)
        self.assertEqual(counter["recurrences"][0]["originalOutcome"], "EPERM")
        self.assertEqual(counter["recurrences"][0]["retry"], event["retry"])
        self.assertIn("W00D", log.getvalue())
        self.assertIn("original EPERM", log.getvalue())
        self.assertIn('"outcome": "started"', log.getvalue())
        self.assertEqual(json.loads(self.ledger.read_bytes())["events"], [event])

    def test_optional_bounded_cap_blocks_later_source_profile_and_group_without_reset(self):
        policy.write(self.ledger, {"schema": 1, "cap": 2, "events": []})
        event = {"failure": {"snapshot": snapshot_value()}}
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)):
            self.fill_shared_cap(event)
        self.assertEqual(len(json.loads(self.ledger.read_bytes())["events"]), 2)

    def fill_shared_cap(self, event):
        for _ in range(2):
            with eperm.ledger() as state:
                state[1]["events"].append(event)
                policy.write(state[0], state[1])
        with self.assertRaisesRegex(RuntimeError, "cap reached"), eperm.ledger():
            self.fail("later source/group must never spawn")

    def test_normal_runner_passes_same_ledger_across_all_profiles_and_sources(self):
        tools, listing = self.runner_inputs()
        environments = []
        def run(command, env, log):
            environments.append(dict(env))
            log.write_text("--- PASS: TestLegacyWorkerProcessProtocolAndVersion\n")
        arguments = ["runner", str(tools), str(listing), "linux/amd64", str(self.root), "--manifest", str(self.native / "manifest.json")]
        with patch.dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(self.ledger)), patch.object(policy.sys, "argv", arguments), patch.object(runner, "run", side_effect=run), patch.object(runner.signal, "signal"):
            runner.main()
        self.assertEqual(len(environments), 8)
        self.assertEqual({env["MESH_WORKER_EPERM_LEDGER"] for env in environments}, {str(self.ledger)})
        self.assertEqual([env["MESH_TRANSITION_PROOF_MODE"] for env in environments[1::2]], ["full", "forward-only", "forward-only", "forward-only"])
        self.assertEqual(json.loads(self.ledger.read_bytes())["events"], [])

    def runner_inputs(self):
        tools = self.native / "tools"
        tools.mkdir()
        policy.write(tools / "candidate-reader.json", {})
        policy.write(tools / "preparation.json", {"source": {"commit": "a" * 40}})
        retained = self.native / "retained"
        retained.mkdir()
        policy.write(retained / "workers.json", {})
        binaries = [self.runner_source(index) for index in range(4)]
        listing = self.native / "retained.list"
        listing.write_text("\n".join(map(str, binaries)))
        return tools, listing

    def runner_source(self, index):
        directory = self.native / f"source-{index}"
        directory.mkdir()
        binary = directory / "mesh"
        binary.write_bytes(str(index).encode())
        policy.write(directory / "source.json", {"version": f"v0.1.{index}", "rollback": index == 0,
                     "artifact": {"binarySha256": hashlib.sha256(binary.read_bytes()).hexdigest()}})
        return binary


    def test_normal_initialization_has_null_cap_and_exact_physical_shard_provenance(self):
        self.initialize()
        value = json.loads(self.ledger.read_bytes())
        self.assertIsNone(value["cap"])
        self.assertEqual(value["provenance"], {"run": "123", "attempt": "1", "job": "prove", "platform": "linux/amd64", "shard": 0})

    def test_bounded_diagnostic_ledger_cannot_enable_normal_ci(self):
        policy.write(self.ledger, {"schema": 1, "cap": 2, "events": []})
        self.check_bad_begin()

    def test_three_normal_events_audit_and_cleanup_without_a_count_gate(self):
        self.confirmed_native()
        events = [{"session": f"W{index}", "version": "v0.1.308", "digest": "a" * 64,
                   "failure": {"snapshot": snapshot_value(), "cohort": f"/tmp/mesh-transition.MOCK-{index}/cohort.json",
                               "groupSelection": str(self.root / "proofs/groups" / ("b" * 64) / f"forward-only/{index}/selected.json")},
                   "retry": {"outcome": "started"}} for index in range(3)]
        policy.write(self.ledger, {"schema": 1, "cap": None, "events": events})
        policy.cleanup(self.root, self.native)
        counter = json.loads((self.root / "eperm-counter.json").read_bytes())
        self.assertEqual(counter["events"], 3)
        self.assertIsNone(counter["cap"])
        self.assertEqual([row["sourceId"] for row in counter["recurrences"]], ["b" * 64] * 3)
        self.assertFalse(self.native.exists())
        self.assertEqual(json.loads(self.ledger.read_bytes())["events"], events)

    def test_three_shards_own_independent_exclusive_ledgers(self):
        paths = []
        for shard in range(3):
            root = self.parent / f"physical-{shard}" / "update-proof"
            self.initialize_shard(root, shard)
            value = json.loads((root / "eperm-ledger.json").read_bytes())
            self.assertIsNone(value["cap"])
            self.assertEqual(value["provenance"]["shard"], shard)
            paths.append(root / "eperm-ledger.json")
        self.assertEqual(len({path.stat().st_ino for path in paths}), 3)
        policy.write(paths[0], {"schema": 1, "cap": None, "events": [{"retained": "original"}]})
        self.assertEqual(json.loads(paths[1].read_bytes())["events"], [])
        self.assertEqual(json.loads(paths[2].read_bytes())["events"], [])

    def initialize_shard(self, root, shard):
        with patch.object(policy, "require_host"), patch.object(control, "positive_control") as healthy:
            policy.initialize(root, shard)
        healthy.assert_called_once_with(root / "positive-control.json")

    def test_snapshot_gap_does_not_erase_the_count_or_original_event(self):
        value = {"schema": 1, "cap": None, "events": [{"session": "W00D", "failure": {"pid": 9753}}]}
        policy.write(self.ledger, value)
        counter = policy.recurrence_counter(self.root, eperm.diagnostic_cleanup(self.ledger, required=True))
        self.assertEqual(counter["events"], 1)
        self.assertEqual(counter["recurrences"][0]["originalPid"], 9753)
        self.assertFalse(counter["ledgerConfirmed"])

    def test_explicit_null_and_bounded_cap_validation_rejects_missing_or_invalid_modes(self):
        for cap in (None, 2):
            value = {"schema": 1, "cap": cap, "events": []}
            policy.write(self.ledger, value)
            eperm.validate_ledger(value)
            self.assertTrue(eperm.diagnostic_cleanup(self.ledger, required=True)["confirmed"])
        for value in ({"schema": 1, "events": []}, {"schema": 1, "cap": 0, "events": []},
                      {"schema": 1, "cap": True, "events": []}, {"schema": 1, "cap": "none", "events": []}):
            policy.write(self.ledger, value)
            with self.assertRaises(ValueError):
                eperm.validate_ledger(value)
            self.assertFalse(eperm.diagnostic_cleanup(self.ledger, required=True)["confirmed"])


class SummaryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="eperm-summary-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.paths = []
        for shard in range(3):
            directory = self.root / f"update-proofs-linux-amd64-{shard}"
            directory.mkdir()
            path = directory / "eperm-counter.json"
            policy.write(path, {"schema": 1, "cap": None, "events": 1, "ledgerConfirmed": True,
                         "provenance": {"run": "123", "attempt": "1", "platform": "linux/amd64", "shard": shard, "job": "prove"},
                         "recurrences": [{"index": 1, "sourceId": "b" * 64, "version": "v0.1.308", "digest": "a" * 64,
                                          "session": "W00D", "cohort": f"/owned/proofs/groups/{'b' * 64}/forward-only/{shard}/cohort.json"}]})
            self.paths.append(path)

    def aggregate(self):
        return summary.aggregate(self.root, "123", "1")

    def test_three_shards_are_counted_once_even_with_shared_source_and_session_ids(self):
        result = self.aggregate()
        self.assertTrue(result["complete"])
        self.assertEqual(result["total"], 3)
        self.assertEqual(result["sourceIds"], ["b" * 64])
        self.assertEqual([row["shard"] for row in result["recurrences"]], [0, 1, 2])
        self.assertTrue(result["investigationRequired"])
        self.assertIsNone(result["cap"])

    def test_zero_event_run_completes_without_investigation(self):
        for path in self.paths:
            self.change(path, lambda value: value.update(events=0, recurrences=[]))
        result = self.aggregate()
        self.assertTrue(result["complete"])
        self.assertEqual(result["total"], 0)
        self.assertFalse(result["investigationRequired"])
        self.assertEqual(result["sourceIds"], [])

    def test_real_policy_counters_feed_the_run_summary(self):
        for shard, path in enumerate(self.paths):
            ledger = {"schema": 1, "cap": None, "provenance": {"run": "123", "attempt": "1", "platform": "linux/amd64", "shard": shard, "job": "prove"},
                      "events": [{"session": "W00D", "version": "v0.1.308", "digest": "a" * 64,
                                  "failure": {"snapshot": snapshot_value(), "cohort": "/tmp/mesh-transition.MOCK/cohort.json",
                                              "groupSelection": str(path.parent / "proofs/groups" / ("b" * 64) / "forward-only/3/selected.json")},
                                  "retry": {"outcome": "started"}}]}
            policy.write(path.parent / "eperm-ledger.json", ledger)
            audit = eperm.diagnostic_cleanup(path.parent / "eperm-ledger.json", required=True)
            policy.write(path, policy.recurrence_counter(path.parent, audit))
        result = self.aggregate()
        self.assertTrue(result["complete"])
        self.assertEqual(result["total"], 3)
        self.assertEqual(result["sourceIds"], ["b" * 64])

    def test_actual_mock_launch_keeps_scratch_cohort_and_separate_proof_source(self):
        retry = module("summary_retry_controls", "test-release-worker-eperm.py")
        case = retry.RetryControls("test_successful_retry_keeps_original_failure_and_same_image_arguments_environment_deadline")
        case.setUp()
        self.addCleanup(case.doCleanups)
        case.path = case.root / "mesh-transition.MOCK" / "cohort.json"
        case.path.parent.mkdir()
        case.diagnostic.return_value = snapshot_value()
        selection = self.paths[0].parent / "proofs/groups" / ("b" * 64) / "forward-only/3/selected.json"
        provenance = {"run": "123", "attempt": "1", "platform": "linux/amd64", "shard": 0, "job": "prove"}
        policy.write(case.ledger, {"schema": 1, "cap": None, "provenance": provenance, "events": []})
        with patch.dict(os.environ, MESH_WORKER_GROUP=str(selection)):
            case.launch()
        ledger = json.loads(case.ledger.read_bytes())
        policy.write(self.paths[0].parent / "eperm-ledger.json", ledger)
        audit = eperm.diagnostic_cleanup(case.ledger, required=True)
        policy.write(self.paths[0], policy.recurrence_counter(self.paths[0].parent, audit))
        result = self.aggregate()
        self.assertTrue(result["complete"])
        self.assertEqual(result["sourceIds"], ["b" * 64])
        event = result["recurrences"][0]
        self.assertEqual(event["cohort"], str(case.path.resolve()))
        self.assertEqual(event["groupSelection"], str(selection))
        self.assertNotEqual(event["sourceId"], event["digest"])
        self.assertEqual(ledger["events"][0]["failure"]["groupSelection"], str(selection))

    def test_missing_or_invalid_source_digest_never_claims_complete(self):
        for identity in (None, "", "unknown", "a" * 63):
            self.change(self.paths[0], lambda value: value["recurrences"][0].update(sourceId=identity))
            self.assertFalse(self.aggregate()["complete"])

    def test_identical_counter_copies_are_deduplicated(self):
        duplicate = self.root / "copy"
        duplicate.mkdir()
        (duplicate / "eperm-counter.json").write_bytes(self.paths[0].read_bytes())
        result = self.aggregate()
        self.assertTrue(result["complete"])
        self.assertEqual(result["total"], 3)

    def test_conflicting_same_run_attempt_shard_counter_is_reported_without_double_count(self):
        duplicate = self.root / "copy"
        duplicate.mkdir()
        value = json.loads(self.paths[0].read_bytes())
        value["recurrences"][0]["version"] = "v0.1.309"
        policy.write(duplicate / "eperm-counter.json", value)
        result = self.aggregate()
        self.assertFalse(result["complete"])
        self.assertEqual(result["total"], 3)
        self.assertEqual(len(result["errors"]), 1)

    def test_previous_attempt_is_not_counted_in_current_attempt(self):
        self.change(self.paths[0], lambda value: value["provenance"].update(attempt="2"))
        result = self.aggregate()
        self.assertEqual(result["total"], 2)
        self.assertFalse(result["complete"])
        self.assertEqual(result["missingShards"], [0])

    def test_all_source_ids_are_reported_without_suppressing_later_events(self):
        self.change(self.paths[2], lambda value: value["recurrences"][0].update(sourceId="c" * 64))
        result = self.aggregate()
        self.assertEqual(result["sourceIds"], ["b" * 64, "c" * 64])
        self.assertEqual(result["total"], 3)
        self.assertIn("event 1", summary.render(result))
        self.assertIn("does not fail CI", summary.render(result))

    def test_missing_counter_is_reported_with_remaining_recurrences(self):
        self.paths[1].unlink()
        result = self.aggregate()
        self.assertFalse(result["complete"])
        self.assertEqual(result["total"], 2)
        self.assertTrue(result["investigationRequired"])
        self.assertEqual(result["missingShards"], [1])

    def test_duplicate_event_index_or_wrong_total_cannot_hide_or_duplicate_events(self):
        self.change(self.paths[0], lambda value: value.update(events=2))
        self.assertFalse(self.aggregate()["complete"])
        self.change(self.paths[0], lambda value: value["recurrences"].append(value["recurrences"][0]))
        self.assertFalse(self.aggregate()["complete"])
        self.assertEqual(self.aggregate()["total"], 2)

    def test_unknown_diagnostic_pid_evidence_is_preserved_in_summary(self):
        self.change(self.paths[0], lambda value: value.update(ledgerConfirmed=False, ledgerError="unknown PID 9753"))
        result = self.aggregate()
        self.assertEqual(result["counters"][0]["ledgerError"], "unknown PID 9753")
        self.assertEqual(result["total"], 3)

    def test_summary_command_succeeds_above_investigation_threshold(self):
        output = self.root / "result.json"
        markdown = self.root / "step-summary.md"
        with patch.object(summary, "aggregate", return_value=self.aggregate()), patch.object(policy.sys, "argv", ["summary", str(self.root), str(output), "--run", "123", "--attempt", "1"]), patch.dict(os.environ, GITHUB_STEP_SUMMARY=str(markdown)):
            summary.main()
        self.assertEqual(json.loads(output.read_bytes())["total"], 3)
        self.assertIn("parent investigation", markdown.read_text())

    def change(self, path, operation):
        value = json.loads(path.read_bytes())
        operation(value)
        policy.write(path, value)


class HealthyTests(unittest.TestCase):
    def test_readability_control_uses_actual_reviewed_collector_boundary_with_mocks(self):
        value = control_value()
        with tempfile.TemporaryDirectory(prefix="eperm-healthy-") as short:
            self.run_healthy_mock(Path(short), value)

    def run_healthy_mock(self, short, value):
        process = Mock(pid=9752, returncode=0)
        process.poll.return_value = 0
        with patch.object(control.subprocess, "Popen", return_value=process), patch.object(control, "capture_child", return_value={"snapshot": value["snapshot"]}), patch.object(control.evidence, "stop_control") as stop:
            result = control.positive_control(short / "control.json")
        self.assertTrue(result["passed"])
        self.assertTrue(result["cleanupConfirmed"])
        self.assertEqual(result["kernelAccess"]["kernelJournal"], "empty")
        stop.assert_called_once_with(process)

    def test_permission_empty_and_unavailable_kernel_gaps_remain_honest(self):
        for result, expected in (({"returncode": 0, "output": "Permission denied"}, "permission-gap"),
                                 ({"returncode": 0, "output": ""}, "empty"),
                                 ({"returncode": 1}, "unavailable"), ({"returncode": 0, "output": "event"}, "available")):
            self.assertEqual(control.kernel_access(result), expected)

    def test_required_mount_sysctl_namespace_and_kernel_attempt_gaps_fail(self):
        for key in ("mounts", "sysctls", "namespacesComparedToLaunch", "kernelJournal"):
            value = snapshot_value()
            value[key] = {} if key != "mounts" else []
            with self.assertRaises(RuntimeError):
                control.verify_snapshot(value)


class WiringTests(unittest.TestCase):
    def setUp(self):
        self.workflow = (REPO / ".github/workflows/update-proofs.yml").read_text()
        self.prove = self.workflow.split("\n  prove:\n", 1)[1].split("\n  eperm-summary:\n", 1)[0]

    def test_all_three_parallel_amd64_jobs_enable_policy(self):
        condition = "matrix.platform == 'linux/amd64'"
        self.assertIn("if: " + condition, self.prove.split("Initialize this AMD64", 1)[1].split("- name: Create", 1)[0])
        self.assertEqual(self.prove.count("native-eperm-policy.py initialize"), 1)
        self.assertIn('--shard "$SHARD"', self.prove)
        self.assertIn("SHARD: ${{ matrix.shard }}", self.prove)
        self.assertIn("platform: [linux/amd64, linux/arm64, darwin/arm64]", self.prove)
        self.assertIn("fail-fast: false", self.prove)
        self.assertIn("name: update-proofs-${{ matrix.key }}-${{ matrix.shard }}", self.prove)
        self.assertIn("path: ${{ runner.temp }}/update-proof", self.prove)
        self.assertEqual(self.workflow.count("MESH_WORKER_EPERM_LEDGER="), 1)
        enabled = [(platform, shard) for platform in ("linux/amd64", "linux/arm64", "darwin/arm64") for shard in range(3)
                   if platform == "linux/amd64"]
        self.assertEqual(enabled, [("linux/amd64", shard) for shard in range(3)])
        self.assertNotIn("workflow_dispatch:", self.workflow)

    def test_healthy_control_before_intake_and_cleanup_before_always_upload(self):
        self.assertLess(self.prove.index("native-eperm-policy.py initialize"), self.prove.index("Download native proof inputs"))
        self.assertLess(self.prove.index("native-eperm-policy.py cleanup"), self.prove.index("id: proof-upload"))
        self.assertIn("id: proof-upload\n        if: always()", self.prove)
        self.assertIn("if: always() && matrix.platform == 'linux/amd64'", self.prove)
        self.assertIn("steps.eperm-cleanup.outcome == 'success' && steps.proof-upload.outcome == 'success'", self.prove)

    def test_download_one_retry_budgets_and_original_reader_coverage_stay(self):
        self.assertEqual(self.prove.count("timeout-minutes: 2\n"), 2)
        self.assertEqual(self.prove.count("name: Retry native proof inputs once"), 1)
        self.assertIn("timeout-minutes: 15\n", self.prove)
        self.assertIn('status = process.wait(timeout=600)', Path(runner.__file__).read_text())
        self.assertIn('--shard "$SHARD" --shards "$count" --manifest', self.prove)
        reader = self.workflow.split("\n  reader:\n", 1)[1]
        self.assertIn("needs: [policy, prepare, prove]", reader)
        self.assertIn("scripts/collect-update-proofs.py", reader)
        self.assertIn("scripts/prove-legacy-release-reader.py", reader)

    def test_single_existing_blocking_gate_contains_integration_controls(self):
        gates = (REPO / "scripts/check-packaging.sh").read_text()
        self.assertEqual(gates.count("python3 scripts/test-native-eperm-policy.py"), 1)
        self.assertIn("python3 scripts/test-release-worker-eperm.py", gates)
        self.assertIn("python3 scripts/test-release-worker-groups.py", gates)

    def test_actual_proof_shell_keeps_default_cleanup_when_policy_disabled(self):
        self.run_shell_control(False, 0)
        self.run_shell_control(False, 1)

    def test_actual_proof_shell_records_status_and_preserves_scratch_when_enabled(self):
        self.run_shell_control(True, 0)
        self.run_shell_control(True, 1)

    def run_shell_control(self, enabled, status):
        with tempfile.TemporaryDirectory(prefix="eperm-shell-") as directory:
            self.check_shell(Path(directory), enabled, status)

    def check_shell(self, root, enabled, status):
        scratch = root / "owned-scratch"
        scratch.mkdir()
        calls = root / "calls"
        snippet = self.prove.split('          scratch=$(mktemp -d /tmp/ms-XXXXXX.noindex)', 1)[1].split('          root="$RUNNER_TEMP/mesh-native.noindex"', 1)[0]
        # Execute the actual workflow branch with a mock Python boundary and owned files.
        script = 'set -euo pipefail\nscratch="$OWNED_SCRATCH"\npython3() { printf "%s\\n" "$*" >> "$CALLS"; }\n' + snippet
        script += "\ntrue\n" if status == 0 else "\nfalse\n"
        environment = dict(os.environ, OWNED_SCRATCH=str(scratch), CALLS=str(calls), RUNNER_TEMP=str(root),
                           MESH_WORKER_EPERM_LEDGER=str(root / "eperm-ledger.json") if enabled else "")
        result = subprocess.run(["bash", "-c", script], env=environment, capture_output=True, check=False)
        self.assertEqual(result.returncode, status, result.stderr)
        self.assertEqual(scratch.exists(), enabled)
        if not enabled:
            self.assertFalse(calls.exists())
            return
        self.assertEqual(calls.read_text().splitlines(), [
            f"scripts/native-eperm-policy.py begin {root}/update-proof {scratch}",
            f"scripts/native-eperm-policy.py returned {root}/update-proof {status}"])

    def test_summary_waits_for_all_shards_and_uploads_outside_proof_reader_prefix(self):
        section = self.workflow.split("\n  eperm-summary:\n", 1)[1].split("\n  reader:\n", 1)[0]
        self.assertIn("needs: prove\n    if: always()", section)
        self.assertIn("pattern: native-eperm-counter-linux-amd64-*", section)
        self.assertIn("name: native-eperm-counter-linux-amd64-${{ matrix.shard }}", self.prove)
        self.assertIn("steps.counter-upload.outcome == 'success'", self.prove)
        self.assertIn("name: native-eperm-run-summary", section)
        self.assertIn(' --run "$GITHUB_RUN_ID" --attempt "$GITHUB_RUN_ATTEMPT"', section)
        self.assertIn("if: always()\n        with:\n          name: native-eperm-run-summary", section)
        self.assertNotIn("timeout-minutes: 25", self.workflow)
        self.assertNotIn("prove-amd64:", self.workflow)

    def test_added_code_stays_within_three_block_levels(self):
        for path in (Path(policy.__file__), Path(control.__file__), Path(eperm.__file__), Path(summary.__file__), Path(__file__)):
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))


def depth(node, nesting=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        nesting = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        nesting += 1
    return max((depth(child, nesting) for child in ast.iter_child_nodes(node)), default=nesting)


if __name__ == "__main__":
    unittest.main()
