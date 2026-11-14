#!/usr/bin/env python3
"""Pure and mocked vehicle controls; never collect local kernel context."""
import ast
from copy import deepcopy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from fixtures import linux_eperm_control as control


SPEC = importlib.util.spec_from_file_location("eperm_vehicle", Path(__file__).with_name("linux-eperm-vehicle.py"))
vehicle = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(vehicle)


def snapshot_value():
    same = {name: {"same": True, "original": name + ":[1]", "observed": name + ":[1]"} for name in ("user", "mnt")}
    return {"cleanupConfirmed": True, "mounts": [{"mountId": "1", "options": "rw", "filesystem": "ext4"}] * 3,
            "sysctls": dict.fromkeys(control.snapshot.SYSCTLS, "123\n"),
            "namespacesComparedToLaunch": dict.fromkeys(("workerAtObservation", "workerAtFailure", "launcherAtFailure"), same),
            "kernelJournal": {"arguments": ["journalctl", "-k", "--since", "@1", "--until", "@2"],
                              "returncode": 0, "output": "-- No entries --\n", "cleanup": {"reaped": True, "stdoutClosed": True}}}


class VehicleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="eperm-vehicle-control-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.expected = json.loads(vehicle.PIN.read_bytes())

    def test_exact_live_complete_metadata(self):
        vehicle.verify_live_pin(self.expected, self.expected)
        for key in vehicle.intake.KEYS + ("rollback-manifest",):
            changed = deepcopy(self.expected)
            changed["artifacts"][key]["id"] += 1
            with self.subTest(key=key), self.assertRaisesRegex(ValueError, "complete producer"):
                vehicle.verify_live_pin(changed, self.expected)

    def test_source_roles_remain_distinct(self):
        self.assertEqual(self.expected["run"], 43549723644)
        self.assertEqual(self.expected["source"], self.expected["runHead"])
        self.assertNotEqual(self.expected["actualPreparationSource"], self.expected["runHead"])
        self.assertEqual(self.expected["actualPreparationSource"], self.expected["preparation"]["source"]["commit"])
        self.assertNotEqual(vehicle.BASE, self.expected["actualPreparationSource"])

    def test_ref_and_composition(self):
        source = "a" * 40
        vehicle.verify_ref(source, source, list(vehicle.ALLOWED))
        for expected, actual, changed in (("main", source, []), (source, "b" * 40, []),
                                           (source, source, ["scripts/fixtures/release_workers.py"]),
                                           (source, source, ["go.mod"])):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                vehicle.verify_ref(expected, actual, changed)

    def test_local_runtime_is_refused(self):
        with patch.dict(vehicle.os.environ, {}, clear=True), self.assertRaisesRegex(ValueError, "hosted-only"):
            vehicle.require_host()

    def test_preselected_or_clock_context_is_refused(self):
        for key in ("MESH_WORKER_GROUP", "MESH_INSTALLED_CLOCK_HANDOFF", "MESH_WORKER_EPERM_LEDGER"):
            env = {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", key: "inherited"}
            with patch.dict(vehicle.os.environ, env, clear=True), self.assertRaisesRegex(ValueError, "inherit"):
                vehicle.require_host()

    def selection(self):
        members = [{"version": "v0.1.226", "protocol": 1, "artifact": {"binarySha256": "f" * 64}, "imageBytes": 12}]
        partition = [{"index": 0, "imageBytes": 24, "members": members}]
        expected = {"sourceVersion": "v0.1.227", "sourceDigest": "a" * 64, "sourceBytes": 12,
                    "groups": vehicle.describe_groups(partition)}
        return {"version": "v0.1.227", "rollback": False}, partition, expected

    def test_selection_positive(self):
        evidence, partition, expected = self.selection()
        vehicle.verify_selection(evidence, "a" * 64, 12, partition, expected)

    def test_selection_refuses_member_loss_and_partition_change(self):
        evidence, partition, expected = self.selection()
        for changed in ([], partition + partition, [{"index": 0, "imageBytes": 24, "members": []}]):
            with self.subTest(changed=changed), self.assertRaisesRegex(ValueError, "partition"):
                vehicle.verify_selection(evidence, "a" * 64, 12, changed, expected)

    def test_selection_refuses_profile_source_digest_and_size_changes(self):
        evidence, partition, expected = self.selection()
        for item, digest, size in ((evidence | {"rollback": True}, "a" * 64, 12),
                                  (evidence | {"version": "v0.1.226"}, "a" * 64, 12),
                                  (evidence, "b" * 64, 12), (evidence, "a" * 64, 13)):
            with self.subTest(item=item), self.assertRaises(ValueError):
                vehicle.verify_selection(item, digest, size, partition, expected)

    def test_original_partition_has_all_49_workers(self):
        self.assertEqual([group["members"] for group in self.expected["groups"]], [31, 18])
        self.assertEqual(self.expected["sourceVersion"], "v0.1.227")

    def test_single_source_list_and_normal_runner_command(self):
        command = vehicle.proof_command(self.root, self.root / "one.list", self.root / "proof")
        self.assertIn(str(vehicle.REPO / "scripts/run-update-proofs.py"), command)
        self.assertEqual(command[command.index("--shard") + 1], "0")
        self.assertEqual(command[command.index("--shards") + 1], "1")
        self.assertEqual(command[-1], str(self.root / "mesh-release-context.json"))

    def test_manifest_retains_all_platforms_and_actual_candidate(self):
        (self.root / "tools").mkdir()
        artifact = {"platform": {"os": "linux", "arch": "amd64"}, "binarySha256": "d" * 64}
        manifest = {"commit": self.expected["actualPreparationSource"], "version": self.expected["candidateReader"]["build"]["version"],
                    "artifacts": [artifact, {"platform": {"os": "linux", "arch": "arm64"}}, {"platform": {"os": "darwin", "arch": "arm64"}}]}
        vehicle.write(self.root / "tools/rollback-artifact.json", {"build": self.expected["candidateReader"]["build"], "artifact": artifact})
        vehicle.write(self.root / "mesh-release-context.json", manifest)
        vehicle.verify_manifest(self.root, self.expected)
        for changed in (manifest | {"commit": self.expected["runHead"]}, manifest | {"artifacts": [artifact]}):
            vehicle.write(self.root / "mesh-release-context.json", changed)
            with self.assertRaises(ValueError):
                vehicle.verify_manifest(self.root, self.expected)

    def test_positive_control_failure_prevents_download_and_cohort(self):
        with patch.object(vehicle, "require_host"), patch.object(vehicle, "source_pin"), patch.object(
                control, "positive_control", side_effect=RuntimeError("control failed")), patch.object(
                vehicle, "acquire") as acquire, patch.object(vehicle, "run_proof") as run:
            with self.assertRaises(RuntimeError):
                vehicle.execute(self.root, "a" * 40, False)
        acquire.assert_not_called()
        run.assert_not_called()
        self.assertFalse(json.loads((self.root / "cleanup.json").read_bytes())["payloadsMayBeRemoved"])

    def test_deadline_and_shared_ledger_without_reset(self):
        state = {"scratch": str(self.root)}
        with patch.object(vehicle.subprocess, "Popen", return_value=Mock(pid=123)), patch.object(
                vehicle.installed_clock, "NativeOwnership") as observer, patch.object(
                vehicle.installed_releases, "wait_transition") as wait, patch.object(vehicle.time, "monotonic_ns", return_value=10):
            vehicle.run_proof(self.root, self.root, self.root / "one.list", state, "a" * 40)
        self.assertEqual(wait.call_args.args[2], 10 + 1220 * vehicle.installed_clock.SECOND)
        self.assertTrue(state["nativeCleanupConfirmed"])
        observer.return_value.save.assert_called_once()
        self.assertEqual(json.loads((self.root / "eperm-ledger.json").read_bytes()), {"schema": 1, "cap": 2, "events": []})

    def test_runtime_failure_and_unknown_ownership_are_not_relabelled_clean(self):
        state = {"scratch": str(self.root)}
        with patch.object(vehicle.subprocess, "Popen", return_value=Mock(pid=123)), patch.object(
                vehicle.installed_clock, "NativeOwnership"), patch.object(vehicle.installed_releases, "wait_transition",
                side_effect=RuntimeError("original native failure")), patch.object(vehicle.installed_releases,
                "stop_transition", side_effect=RuntimeError("ownership unknown")):
            with self.assertRaisesRegex(RuntimeError, "ownership unknown"):
                vehicle.run_proof(self.root, self.root, self.root / "one.list", state, "a" * 40)
        self.assertFalse(state["nativeCleanupConfirmed"])

    def test_unconfirmed_native_cleanup_preserves_inputs(self):
        state = {"launchAttempted": True, "nativeCleanupConfirmed": False}
        vehicle.write(self.root / "state.json", state)
        payload = self.root / "native.noindex"
        payload.mkdir()
        with self.assertRaisesRegex(RuntimeError, "preserved"):
            vehicle.cleanup(self.root)
        self.assertTrue(payload.exists())

    def test_confirmed_and_prelaunch_cleanup_is_idempotent(self):
        vehicle.write(self.root / "state.json", {"launchAttempted": False})
        (self.root / "native.noindex").mkdir()
        vehicle.cleanup(self.root)
        vehicle.cleanup(self.root)
        self.assertTrue(json.loads((self.root / "cleanup.json").read_bytes())["payloadsAbsent"])

    def test_missing_ownership_and_foreign_scratch_refuse_deletion(self):
        (self.root / "native.noindex").mkdir()
        with self.assertRaises(RuntimeError):
            vehicle.cleanup(self.root)
        vehicle.write(self.root / "state.json", {"scratch": "/tmp/unrelated"})
        with self.assertRaisesRegex(RuntimeError, "ownership path"):
            vehicle.cleanup(self.root)

    def test_unreadable_ownership_preserves_inputs_and_records_gap(self):
        (self.root / "native.noindex").mkdir()
        (self.root / "state.json").write_text("incomplete")
        with self.assertRaisesRegex(RuntimeError, "ownership evidence unreadable"):
            vehicle.cleanup(self.root)
        self.assertFalse(json.loads((self.root / "cleanup.json").read_bytes())["payloadsMayBeRemoved"])
        self.assertTrue((self.root / "native.noindex").exists())

    def test_recurrences_count_even_after_successful_retry(self):
        vehicle.write(self.root / "eperm-ledger.json", {"events": [{"outcome": "EPERM"}, {"outcome": "EPERM"}]})
        vehicle.counter(self.root)
        value = json.loads((self.root / "counter.json").read_bytes())
        self.assertEqual(value["events"], 2)
        self.assertFalse(value["acceptanceEstablished"])

    def test_workflow_is_one_manual_hosted_job_with_always_upload(self):
        workflow = (vehicle.REPO / ".github/workflows/rollback-proof-investigation.yml").read_text()
        for text in ("workflow_dispatch:", "timeout-minutes: 30", "fetch-depth: 0", "if: always()", "eperm-diagnostic/proof"):
            self.assertIn(text, workflow)
        for text in ("matrix:", "calibration", "workflow_call:", "cache@", "setup-go@"):
            self.assertNotIn(text, workflow)

    def test_nesting_at_most_three(self):
        for path in (Path(vehicle.__file__), Path(control.__file__), Path(__file__)):
            tree = ast.parse(path.read_text())
            self.check_depth(tree, 0)

    def check_depth(self, node, depth):
        blocks = (ast.FunctionDef, ast.AsyncFunctionDef, ast.If, ast.For, ast.While, ast.With, ast.Try)
        current = depth + isinstance(node, blocks)
        self.assertLessEqual(current, 3, getattr(node, "lineno", None))
        for child in ast.iter_child_nodes(node):
            self.check_depth(child, current)


class SnapshotControlTests(unittest.TestCase):
    def test_readable_control_with_empty_kernel_window(self):
        control.verify_snapshot(snapshot_value())
        self.assertEqual(control.kernel_access(snapshot_value()["kernelJournal"]), "empty")

    def test_kernel_access_labels_are_honest(self):
        for result, label in (({"returncode": 0, "output": "kernel event\n"}, "available"),
                              ({"returncode": 0, "output": ""}, "empty"),
                              ({"returncode": 1, "output": "Operation not permitted"}, "permission-gap"),
                              ({"unavailable": "FileNotFoundError"}, "unavailable")):
            self.assertEqual(control.kernel_access(result), label)

    def test_kernel_permission_gap_is_explicit_and_does_not_invent_coverage(self):
        value = snapshot_value()
        value["kernelJournal"].update(returncode=1, output="Permission denied")
        control.verify_snapshot(value)
        self.assertEqual(control.kernel_access(value["kernelJournal"]), "permission-gap")

    def test_missing_proc_mount_sysctl_namespace_or_cleanup_blocks_control(self):
        for field, replacement in (("mounts", {}), ("sysctls", {}), ("cleanupConfirmed", False),
                                    ("namespacesComparedToLaunch", {}), ("kernelJournal", {})):
            value = snapshot_value() | {field: replacement}
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                control.verify_snapshot(value)

    def test_deadline_exhaustion_is_no_runtime_kernel_attempt(self):
        value = snapshot_value()
        value["kernelJournal"]["unavailable"] = "startup deadline exhausted"
        with self.assertRaisesRegex(RuntimeError, "never attempted"):
            control.verify_snapshot(value)

    def test_actual_collector_is_called_with_known_context_and_deadline(self):
        observed = Mock(before={"stat": {"startTicks": 1}, "status": dict.fromkeys(
            ("Uid", "Gid", "NoNewPrivs", "Seccomp", "TracerPid", "CapEff"), "0")})
        manager = Mock()
        manager.__enter__ = Mock(return_value=observed)
        manager.__exit__ = Mock(return_value=False)
        with patch.object(control.evidence, "launcher_snapshot", return_value={"namespaces": {"user": "user:[1]"}}), patch.object(
                control.evidence, "Startup", return_value=manager), patch.object(control.evidence, "verify_control_context"), patch.object(
                control.evidence, "verify_control_image"), patch.object(control.snapshot, "capture", return_value=snapshot_value()) as collect:
            value = control.capture_child(Mock(pid=123), 456)
        self.assertEqual(value["knownChildPid"], 123)
        self.assertEqual(collect.call_args.args[2], 456)
        self.assertIs(collect.call_args.args[0]["workerAtObservation"], observed.before)

    def test_positive_control_success_requires_cleanup_and_retains_kernel_gap(self):
        value = snapshot_value()
        value["kernelJournal"].update(returncode=1, output="Permission denied")
        with tempfile.TemporaryDirectory(prefix="eperm-control-mock-") as temporary:
            path = Path(temporary) / "control.json"
            process = Mock(returncode=0, pid=123)
            process.poll.return_value = 0
            self.run_mock_control(path, process, value)
            result = json.loads(path.read_bytes())
            self.assertTrue(result["passed"])
            self.assertTrue(result["cleanupConfirmed"])
            self.assertEqual(result["kernelAccess"]["kernelJournal"], "permission-gap")

    def run_mock_control(self, path, process, value):
        with patch.object(control.subprocess, "Popen", return_value=process), patch.object(
                control, "capture_child", return_value={"snapshot": value}), patch.object(control.evidence, "stop_control"):
            control.positive_control(path)

    def test_cleanup_failure_is_retained_and_fatal(self):
        with tempfile.TemporaryDirectory(prefix="eperm-control-mock-") as temporary:
            path = Path(temporary) / "control.json"
            process = Mock()
            process.returncode = None
            process.poll.return_value = None
            with patch.object(control.evidence, "stop_control"), self.assertRaisesRegex(RuntimeError, "unconfirmed"):
                control.finish_control(process, {"passed": True, "snapshot": snapshot_value()}, path)
            value = json.loads(path.read_bytes())
            self.assertFalse(value["passed"])
            self.assertFalse(value["cleanupConfirmed"])

    def test_reap_exception_never_leaves_a_passing_control(self):
        with tempfile.TemporaryDirectory(prefix="eperm-control-mock-") as temporary:
            path = Path(temporary) / "control.json"
            self.failing_reap(path)
            value = json.loads(path.read_bytes())
            self.assertFalse(value["passed"])
            self.assertFalse(value["cleanupConfirmed"])
            self.assertIn("cleanupError", value)

    def failing_reap(self, path):
        with patch.object(control.evidence, "stop_control", side_effect=RuntimeError("not reaped")), self.assertRaises(RuntimeError):
            control.finish_control(Mock(), {"passed": True}, path)


if __name__ == "__main__":
    unittest.main()
