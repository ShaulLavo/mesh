#!/usr/bin/env python3
"""Offline controls for hosted prepared selections, intake and fail-closed cleanup."""
from dataclasses import asdict
import hashlib
import io
import importlib.util
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import tarfile
from types import SimpleNamespace
import unittest
import zipfile
from unittest.mock import patch

from fixtures import installed_clock, installed_slices, installed_workflow as workflow

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github/workflows"
PREPARED = (WORKFLOWS / "installed-prepared.yml").read_text()
CATCHUP = (WORKFLOWS / "installed-catchup.yml").read_text()


def job(source, name):
    return re.search(rf"^  {name}:\n(.*?)(?=^  [\w-]+:|\Z)", source, re.M | re.S).group(1)


def step(source, name):
    value = source.split("      - name: " + name + "\n", 1)[1].split("      - ", 1)[0]
    return "\n".join(line[10:] for line in value.split("        run: |\n", 1)[1].splitlines())


def matrix_values(source, name):
    values = re.search(rf"^        {name}: \[(.+)\]$", source, re.M).group(1)
    return [value.strip() for value in values.split(",")]


def outer_cases():
    source = job(CATCHUP, "prepared-cohort")
    return list(itertools.product(matrix_values(source, "platform"), matrix_values(source, "mode")))


def bundle(root, count=7, mode="waypoint"):
    root.mkdir()
    payload = root / "helper"
    payload.write_bytes(b"offline executable payload")
    payload.chmod(0o755)
    value = {"schema": 1, "harnessSource": "a" * 40, "preparationSource": "b" * 40,
             "publishedRegistrySha256": "c" * 64, "mode": mode, "groups": [{"index": index} for index in range(count)],
             "files": {"helper": {"sha256": hashlib.sha256(payload.read_bytes()).hexdigest(),
                                  "size": payload.stat().st_size, "executable": True}}}
    (root / "prepared.json").write_text(json.dumps(value))
    return value


class WorkflowControls(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="prepared-workflow-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_three_profiles_cover_actual_partition_and_waypoint_readers_once(self):
        for mode, count in (("chain", 2), ("waypoint", 7)):
            directory = self.root / mode
            bundle(directory, count, mode)
            selections = [workflow.selection(directory, profile) for profile in workflow.PROFILES]
            installed_slices.validate_partition(selections, count, mode == "waypoint")
            self.assertEqual([len(row["select"][0]["group_indices"]) for row in selections], [count] * 3)
            self.assertEqual([row["readers"] for row in selections],
                             [list(range(count)), [], []] if mode == "waypoint" else [[], [], []])

    def test_descriptor_indices_cannot_change_the_full_partition(self):
        directory = self.root / "bundle"
        value = bundle(directory)
        value["groups"][0]["index"] = 10
        (directory / "prepared.json").write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError, "not contiguous"):
            workflow.selection(directory, "none")

    def test_artifact_transfer_preserves_internal_modes_and_rechecks_exact_bytes(self):
        directory = self.root / "bundle"
        bundle(directory)
        transfer = self.root / "transfer"
        workflow.pack(directory, transfer)
        outer = self.root / "artifact.zip"
        with zipfile.ZipFile(outer, "w") as archive:
            archive.write(transfer / "bundle.tar", "bundle.tar")
        downloaded = self.root / "download.noindex"
        downloaded.mkdir()
        workflow.installed_artifacts.extract_archive(outer, downloaded)
        (downloaded / "bundle.tar").chmod(0o644)
        loaded = self.root / "loaded.noindex"
        workflow.unpack(downloaded / "bundle.tar", loaded)
        self.assertEqual((loaded / "helper").stat().st_mode & 0o777, 0o755)
        self.assertEqual((loaded / "helper").read_bytes(), (directory / "helper").read_bytes())
        self.assertEqual((loaded / "prepared.json").read_bytes(), (directory / "prepared.json").read_bytes())
        (directory / "helper").write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "asset changed"):
            workflow.pack(directory, self.root / "changed-transfer")

    def test_tar_rejects_nonlocal_nonregular_and_duplicate_members_before_extraction(self):
        for label in ("../outside", "/outside", "symlink", "duplicate"):
            archive_path = self.root / (label.replace("/", "_") + ".tar")
            self.write_invalid_tar(archive_path, label)
            destination = self.root / (label.replace("/", "_") + ".noindex")
            with self.assertRaisesRegex(ValueError, "nonlocal, repeated or nonregular"):
                workflow.unpack(archive_path, destination)
            self.assertFalse(any(destination.iterdir()))

    def write_invalid_tar(self, archive_path, label):
        with tarfile.open(archive_path, "w:") as archive:
            item = tarfile.TarInfo(label)
            if label == "symlink":
                item.type, item.linkname = tarfile.SYMTYPE, "outside"
            archive.addfile(item, io.BytesIO())
            if label == "duplicate":
                archive.addfile(item, io.BytesIO())

    def test_tar_mode_mismatch_is_rejected_without_changing_it_to_expected(self):
        directory = self.root / "bundle"
        bundle(directory)
        bad = self.root / "bad.tar"
        with tarfile.open(bad, "w:") as archive:
            archive.add(directory / "prepared.json", arcname="prepared.json")
            member = archive.gettarinfo(directory / "helper", arcname="helper")
            member.mode = 0o644
            with (directory / "helper").open("rb") as source:
                archive.addfile(member, source)
        loaded = self.root / "loaded.noindex"
        with self.assertRaisesRegex(ValueError, "asset changed"):
            workflow.unpack(bad, loaded)
        self.assertFalse((loaded / "helper").stat().st_mode & 0o111)

    def test_intake_checks_unique_pinned_archive_and_space_before_any_transfer(self):
        artifact = {"id": 12, "name": "prepared", "size_in_bytes": 20, "digest": "sha256:" + "a" * 64, "expired": False}
        control = self.root / "control"
        with patch.object(workflow.installed_artifacts, "pages", return_value=[artifact]), \
                patch.object(workflow.installed_storage, "preflight", side_effect=RuntimeError("space")) as preflight, \
                patch.object(workflow.installed_artifacts, "download") as download:
            with self.assertRaisesRegex(RuntimeError, "space"):
                workflow.fetch("owner/repo", 42, "prepared", self.root / "bundle", control)
        preflight.assert_called_once()
        download.assert_not_called()
        pin = json.loads((control / "prepared-pin.json").read_bytes())
        self.assertEqual(pin["run"], 42)
        self.assertEqual(pin["artifacts"]["prepared"]["digest"], artifact["digest"])
        with patch.object(workflow.installed_artifacts, "pages", return_value=[artifact, artifact]):
            with self.assertRaisesRegex(ValueError, "exactly one"):
                workflow.fetch("owner/repo", 42, "prepared", self.root / "bundle", control)

    def test_missing_or_negative_ownership_retains_all_payloads(self):
        control, proof = self.root / "cohort-control", self.root / "cohort-proof"
        control.mkdir()
        proof.mkdir()
        (control / "fixture-launch.json").write_text('{"started":true}')
        names = ("native-worker-input.noindex", "native-tools.noindex", "prepared.noindex", "prepared-transfer.noindex")
        for name in names:
            (self.root / name).mkdir()
        for ownership in (None, {"signerAbsent": False, "transitionsAbsent": True},
                          {"signerAbsent": True, "transitionsAbsent": True}):
            if ownership is not None:
                (proof / "fixture-ownership.json").write_text(json.dumps(ownership))
            with self.assertRaisesRegex(RuntimeError, "ownership unconfirmed"):
                workflow.cleanup(self.root, True)
            self.assertTrue(all((self.root / name).exists() for name in names))
        (proof / "fixture-clock.json").write_text('{"provenance":{"retained_root":null}}')
        workflow.cleanup(self.root)
        self.assertTrue((self.root / "prepared.noindex").exists())
        workflow.cleanup(self.root, True)
        self.assertTrue(all(not (self.root / name).exists() for name in names))

    def test_diagnostic_physical_planning_never_reads_an_index(self):
        with patch.object(workflow.installed_budgets, "select_plan") as select:
            result = workflow.physical_plans(self.root / "absent.json", True, "linux/amd64", "chain")
        select.assert_not_called()
        self.assertEqual(len(result["minutes"]), 4)
        self.assertEqual(set(result["minutes"].values()), {30})
        self.assertEqual(set(result["plans"].values()), {""})
        with self.assertRaisesRegex(ValueError, "inactive"):
            workflow.physical_plans(self.root / "absent.json", False, "linux/amd64", "chain")

    def test_frozen_physical_roles_require_independent_plans_and_round_admission(self):
        phase = installed_clock.PhaseBudget("setup#1", installed_clock.bound(installed_clock.SECOND), {"x": 1},
            (installed_clock.SampleRef({"test": True}, {"x": 1}, installed_clock.SECOND, True, True),))
        base = installed_clock.ClockPlan(mode="frozen", workload={"placeholder": True}, phases=(phase,),
            command_ns=phase.limit_ns, external_ns=phase.limit_ns, emergency_ns=installed_clock.SECOND,
            eligibility_review="offline test", external_budget=phase, admission_ns=1175 * installed_clock.SECOND)
        index = self.root / "index.json"
        entries = {}
        self.write_physical_plans(index, entries, base, "linux/amd64", "chain")
        index.write_text(json.dumps({"schema": 1, "plans": entries}))
        with patch.object(workflow.installed_budgets, "ROOT", self.root):
            result = workflow.physical_plans(index, False, "linux/amd64", "chain")
        self.assertEqual(len(result["plans"]), 4)
        self.assertEqual(set(result["minutes"].values()), {20})
        path = self.root / result["plans"]["linux-amd64-chain-consume-first"]
        value = json.loads(path.read_bytes())
        value["workload"]["installedSlice"]["select"][0]["profile"] = "chain-final"
        path.write_text(json.dumps(value))
        with patch.object(workflow.installed_budgets, "ROOT", self.root), self.assertRaisesRegex(ValueError, "another profile"):
            workflow.physical_plans(index, False, "linux/amd64", "chain")

    def write_physical_plans(self, index, entries, base, platform, mode):
        for role, profile in (("prepare", None), ("consume", "none"), ("consume", "first"), ("consume", "final")):
            key = workflow.physical_key(platform, mode, role, profile)
            workload = {"platform": platform, "host": workflow.installed_budgets.PLATFORMS[platform][1], "mode": mode,
                        "preparedRole": role, "grouped": True, "scenario": None, "catalogEvidence": False}
            if profile:
                workload["installedSlice"] = {"schema": 1, "select": [{"profile": "chain-" + profile, "group_indices": [0]}], "readers": []}
            value = asdict(base) | {"workload": workload}
            (index.parent / (key + ".json")).write_text(json.dumps(value))
            entries[key] = {"path": key + ".json"}

    def test_unsplit_selection_preserves_three_oldest_order_and_support(self):
        args = SimpleNamespace(diagnostic=False, publication=False, cohort=True, adoption_scenario=None,
                               catalog=True, index=self.root / "index.json")
        with patch.object(workflow.installed_budgets, "select_plan", return_value=(None, SimpleNamespace(admission_ns=900 * installed_clock.SECOND))) as select:
            result = workflow.unsplit_plans(args)
        self.assertEqual(len(result), 10)
        self.assertEqual(sum("oldest" in key for key in result), 3)
        self.assertIn("linux-amd64-order", result)
        self.assertFalse(any("-chain" in key or "-waypoint" in key for key in result))
        self.assertEqual(select.call_count, 10)

    def test_job_graph_and_consumer_commands_preserve_original_clock_and_no_builds(self):
        for name in ("prepare", "consume"):
            source = job(PREPARED, name)
            self.assertLess(source.index("Record original job clock"), source.index("uses: actions/checkout"))
            self.assertEqual(source.count('open(p,"x")'), 1)
            self.assertIn("needs.plans.outputs.minutes", source)
            self.assertIn("--clock-plan", source)
            self.assertIn("--clock-job-seconds 1800 --clock-command-seconds 1380 --clock-external-seconds 360 --clock-emergency-seconds 60", source)
        consumer = job(PREPARED, "consume")
        self.assertIn("needs: [plans, prepare]", consumer)
        self.assertIn("profile: [none, first, final]", consumer)
        self.assertNotIn("setup-go", consumer)
        self.assertNotIn("go build", consumer)
        self.assertNotIn("--native-helper", consumer)
        self.assertNotIn("worker-registry", consumer)
        aggregate = job(PREPARED, "aggregate")
        self.assertIn("scripts/aggregate-installed-slices.py", aggregate)
        self.assertIn('"$RUNNER_TEMP/preparation/cohort-proof"', aggregate)
        self.assertIn("for profile in none first final", aggregate)
        self.assertNotIn("continue-on-error", PREPARED)

    def test_actual_consumer_shell_emits_explicit_mode_role_and_diagnostic_bounds(self):
        script = step(job(PREPARED, "consume"), "Run one complete authoritative installed profile using bundled helper")
        control = self.root / "cohort-control"
        control.mkdir()
        for mode in workflow.MODES:
            environment = os.environ | {"RUNNER_TEMP": str(self.root), "SOURCE_HOST": "pc", "PROOF_MODE": mode,
                "CLOCK_CALIBRATION": "true", "CLOCK_PLAN": "", "PYTHONDONTWRITEBYTECODE": "1"}
            result = subprocess.run(["bash", "-c", "python3() { printf '%s\\n' \"$@\"; };\n" + script],
                env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = result.stdout.splitlines()
            self.assertIn("--format-waypoint" if mode == "waypoint" else "--signed-chain", args)
            self.assertIn("--installed-adoption", args)
            self.assertIn("--prepared-releases", args)
            self.assertIn("--installed-slice", args)
            self.assertEqual(args[args.index("--clock-command-seconds") + 1], "1380")
            self.assertEqual(args[args.index("--clock-external-seconds") + 1], "360")
            self.assertEqual(sum(int(args[args.index(flag) + 1]) for flag in
                ("--clock-command-seconds", "--clock-external-seconds", "--clock-emergency-seconds")), 1800)
            self.assertNotIn("--clock-plan", args)

    def test_calibration_alias_and_all_nine_support_profiles_are_preserved(self):
        source = (WORKFLOWS / "installed-clock-calibration.yml").read_text()
        self.assertEqual(source, (WORKFLOWS / "rollback-proof-investigation.yml").read_text())
        self.assertIn("profile: [adoption-all, adoption-interrupt, fresh]", job(source, "support"))
        self.assertIn("calibration_only: true", job(source, "support"))
        self.assertIn("inputs.calibration_support, inputs.calibration_scope", CATCHUP)
        self.assertIn("name: installed-catchup-${{ inputs.calibration_support || 'ordinary' }}-", CATCHUP)
        self.assertIn("mode: [oldest, order]", job(CATCHUP, "signed-cohort-chain"))
        self.assertIn("uses: ./.github/workflows/installed-prepared.yml", job(CATCHUP, "prepared-cohort"))
        ci = (WORKFLOWS / "ci.yml").read_text()
        for script in ("test-installed-prepared.py", "test-installed-workflow.py"):
            self.assertIn("python3 scripts/" + script, ci)
            self.assertIn("scripts/" + script, CATCHUP)

    def test_harness_and_original_preparation_sources_remain_distinct(self):
        self.assertIn("MESH_HARNESS_SOURCE: ${{ inputs.harness_source }}", PREPARED)
        self.assertIn("MESH_PREPARATION_SOURCE: ${{ inputs.worker_artifact_source }}", PREPARED)
        producer = job(PREPARED, "prepare")
        self.assertIn('installed_artifacts.py native "$input" "$MESH_PREPARATION_SOURCE"', producer)
        self.assertIn("PRODUCER_SOURCE: ${{ inputs.worker_artifact_source }}", producer)
        for name in ("prepare", "consume"):
            self.assertIn("ref: ${{ inputs.harness_source }}", job(PREPARED, name))

    def test_one_case_mapping_rejects_unknown_context_and_inventory_mismatch(self):
        for platform, mode in (("linux/386", "chain"), ("linux/amd64", "oldest"), ("", "waypoint")):
            with self.assertRaisesRegex(ValueError, "supported native platform and mode"):
                workflow.case_matrix(platform, mode)
        with patch.object(workflow.fleet_baselines, "load", return_value={"hosts": [{"name": "pc", "platform": "darwin/arm64"}]}):
            with self.assertRaisesRegex(ValueError, "actual native inventory"):
                workflow.case_matrix("linux/amd64", "chain")

    def test_six_outer_cases_have_single_prepares_and_complete_consumer_union(self):
        cases = outer_cases()
        self.assertEqual(set(cases), set(itertools.product(workflow.RUNNERS, workflow.MODES)))
        self.assertEqual(len(cases), 6)
        rows = [workflow.case_matrix(platform, mode)["include"][0] for platform, mode in cases]
        self.assertEqual({row["host"] for row in rows}, {"pc", "pi", "mac"})
        for name in ("prepare", "aggregate"):
            source = job(PREPARED, name)
            self.assertIn("matrix: ${{ fromJSON(needs.plans.outputs.case) }}", source)
            self.assertNotIn("platform: [", source)
            self.assertNotIn("mode: [", source)
        consumers = job(PREPARED, "consume")
        self.assertIn("include: ${{ fromJSON(needs.plans.outputs.case).include }}", consumers)
        profiles = matrix_values(consumers, "profile")
        self.assertEqual(profiles, ["none", "first", "final"])
        actual = {(row["platform"], row["mode"], profile) for row in rows for profile in profiles}
        self.assertEqual(actual, set(itertools.product(workflow.RUNNERS, workflow.MODES, workflow.PROFILES)))
        self.assertEqual(len(actual), 18)
        self.assertIn("needs: [plans, prepare]", consumers)
        self.assertIn("needs: [plans, prepare, consume]", job(PREPARED, "aggregate"))
        self.assertIn("needs: plans", job(PREPARED, "prepare"))
        self.assertIn("fail-fast: false", job(CATCHUP, "prepared-cohort"))
        self.assertIn("mode: [oldest, order]", job(CATCHUP, "signed-cohort-chain"))
        self.assertEqual(job(CATCHUP, "signed-cohort-chain").count("            mode: order"), 2)

    def test_linux_consumers_are_ready_when_mac_preparations_are_cancelled(self):
        cases = outer_cases()
        completed = {(case, "plans") for case in cases} | {(case, "prepare") for case in cases if case[0].startswith("linux/")}
        needs = re.search(r"^    needs: \[(.+)\]$", job(PREPARED, "consume"), re.M).group(1).split(", ")
        ready = [case for case in cases if all((case, dependency) in completed for dependency in needs)]
        self.assertEqual(set(ready), {case for case in cases if case[0].startswith("linux/")})
        self.assertEqual(len(ready) * len(matrix_values(job(PREPARED, "consume"), "profile")), 12)

    def test_required_reusable_inputs_and_case_concurrency_are_distinct(self):
        header = PREPARED.split("\npermissions:", 1)[0]
        caller = job(CATCHUP, "prepared-cohort")
        for name, expression in (("native_platform", "matrix.platform"), ("mode", "matrix.mode")):
            self.assertIn(name + ":\n        required: true\n        type: string", header)
            self.assertIn(name + ": ${{ " + expression + " }}", caller)
        group = re.search(r"^  group: (.+)$", PREPARED, re.M).group(1)
        self.assertIn("${{ inputs.native_platform }}", group)
        self.assertIn("${{ inputs.mode }}", group)
        groups = {group.replace("${{ inputs.native_platform }}", platform).replace("${{ inputs.mode }}", mode)
                  for platform, mode in outer_cases()}
        self.assertEqual(len(groups), 6)
        self.assertIn("cancel-in-progress: false", PREPARED)

    def test_actual_plan_step_selects_only_its_case_without_loading_diagnostic_budgets(self):
        command = step(job(PREPARED, "plans"), "Require independently reviewed physical job plans or explicit measurement")
        for platform, mode in outer_cases():
            output = self.root / (platform.replace("/", "-") + "-" + mode)
            environment = os.environ | {"GITHUB_OUTPUT": str(output), "PYTHONDONTWRITEBYTECODE": "1",
                "PYTHONPATH": str(ROOT / "scripts"), "DIAGNOSTIC": "true", "NATIVE_PLATFORM": platform, "PROOF_MODE": mode}
            result = subprocess.run(["bash", "-c", command], cwd=ROOT, env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            observed = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(json.loads(observed["case"]), workflow.case_matrix(platform, mode))
            self.assertEqual(len(json.loads(observed["minutes"])), 4)
            key = workflow.installed_budgets.PLATFORMS[platform][0] + "-" + mode + "-"
            self.assertTrue(all(name.startswith(key) for name in json.loads(observed["plans"])))
            self.assertEqual(set(json.loads(observed["plans"]).values()), {""})

    def test_cli_bundled_preparer_is_optional_and_waypoint_is_forwarded(self):
        spec = importlib.util.spec_from_file_location("prepared_workflow_cli", ROOT / "scripts/prove-installed-catchup.py")
        cli = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cli)
        args = SimpleNamespace(native_helper=None, export_prepared=None, prepared_releases=Path("/bundle"), installed_slice=Path("/slice"))
        with patch("fixtures.installed_prepared.run_slice") as run:
            cli.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64", "plan")
        run.assert_called_once_with(Path("/root"), Path("/out"), "linux/amd64", None, Path("/bundle"), Path("/slice"), clock_plan="plan")


if __name__ == "__main__":
    unittest.main()
