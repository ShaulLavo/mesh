#!/usr/bin/env python3
"""Offline controls for hosted prepared selections, intake and fail-closed cleanup."""
import hashlib
import fnmatch
import io
import importlib.util
import itertools
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import tarfile
from types import SimpleNamespace
import unittest
import zipfile
from unittest.mock import patch

from fixtures import installed_clock, installed_launch, installed_releases, installed_slices, installed_workflow as workflow

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github/workflows"
PREPARED = (WORKFLOWS / "installed-prepared.yml").read_text()
CATCHUP = (WORKFLOWS / "installed-catchup.yml").read_text()
CACHE = (ROOT / ".github/actions/installed-go/action.yml").read_text()


def job(source, name):
    return re.search(rf"^  {name}:\n(.*?)(?=^  [\w-]+:|\Z)", source, re.M | re.S).group(1)


def step(source, name):
    value = source.split("      - name: " + name + "\n", 1)[1].split("      - ", 1)[0]
    command = value.split("        run: ", 1)[1]
    if not command.startswith("|\n"):
        return command.strip()
    return "\n".join(line[10:] for line in command[2:].splitlines())


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

    def test_required_current_continuity_job_runs_real_fresh_update_on_all_native_platforms(self):
        source = job(CATCHUP, "fresh-managed")
        self.assertEqual(re.findall(r"^          - platform: (.+)$", source, re.M),
                         ["linux/amd64", "linux/arm64", "darwin/arm64"])
        self.assertNotIn("worker_artifact_prefix", source.split("\n    name:", 1)[0])
        self.assertNotIn("continue-on-error", source)
        command = step(source, "Prove fresh preparation and same-client update between current source builds")
        output = self.root / "command.json"
        mock = 'python3() { printf "%s\\n" "$@" > "$COMMAND_OUTPUT"; }\n'
        environment = os.environ | {"RUNNER_TEMP": str(self.root), "COMMAND_OUTPUT": str(output)}
        result = subprocess.run(["bash", "-e", "-c", mock + command], env=environment,
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_text().splitlines(), ["scripts/prove-installed-catchup.py",
                         str(self.root / "fresh-proof"), "--native-preflight", "--fresh-managed"])
        self.assertIn("'scripts/fixtures/installed_continuity.py'", CATCHUP)

    def test_fixture_cache_domain_covers_lockfiles_source_imports_and_recipe(self):
        self.assertIn("go-version-file: go.mod", CACHE)
        self.assertIn("cache: true", CACHE)
        for pattern in ("**/go.mod", "**/go.sum", "**/*.go", "scripts/fixtures/installed_fresh.py", "scripts/fixtures/installed_releases.py",
                        "scripts/fixtures/release_transition.py", ".github/actions/installed-go/action.yml"):
            self.assertIn("          " + pattern + "\n", CACHE)
        self.assertEqual(CACHE.count("uses: actions/setup-go@"), 1)
        self.assertEqual(CACHE.count("uses: actions/cache/restore@"), 1)
        self.assertEqual(CACHE.count("if: steps.fixture.outputs.cache-hit != 'true'"), 2)
        self.assertNotIn("restore-keys:", CACHE)
        self.assertNotIn("actions/cache/save", CACHE)
        self.assertIn("hashFiles('go.mod')", CACHE)
        self.assertIn("${{ steps.seed.outputs.modules }}", CACHE)
        self.assertIn("${{ steps.seed.outputs.build }}", CACHE)
        self.assertNotIn("runner.temp", CACHE)
        self.assertNotIn("go build", CACHE)
        for name in ("fresh-managed", "native-adoption", "signed-cohort-chain"):
            self.assertEqual(job(CATCHUP, name).count("uses: ./.github/actions/installed-go"), 1)
        self.assertNotIn("installed-go", job(CATCHUP, "publication-native"))
        self.assertEqual(job(PREPARED, "prepare").count("uses: ./.github/actions/installed-go"), 1)
        self.assertNotIn("installed-go", job(PREPARED, "consume"))
        self.assertIn("'.github/actions/installed-go/**'", CATCHUP)
        self.assertIn("'.github/actions/installed-go/**'", (WORKFLOWS / "ci.yml").read_text())

    def test_actual_seed_command_matches_pinned_setup_go_identity_on_three_hosts(self):
        body = CACHE.split("      run: |\n", 1)[1].split("    - name:", 1)[0]
        script = "\n".join(line[8:] for line in body.splitlines())
        mock = '''go() {
          test "$1" = env || return 73
          case "$2" in
            GOHOSTARCH) echo "$HOST_ARCH" ;;
            GOVERSION) echo go1.27.4 ;;
            GOMODCACHE) echo '/public modules' ;;
            GOCACHE) echo '/public build' ;;
            *) return 74 ;;
          esac
        }
        '''
        for system, arch, image, expected in (("Linux", "amd64", "ubuntu24", "Linux-x64-ubuntu24-"),
                                             ("Linux", "arm64", "ubuntu24", "Linux-arm64-ubuntu24-"),
                                             ("macOS", "arm64", "", "macOS-arm64-")):
            output = self.root / "outputs"
            output.unlink(missing_ok=True)
            env = os.environ | {"RUNNER_OS": system, "HOST_ARCH": arch, "ImageOS": image,
                                "MODULE_HASH": "a" * 64, "GITHUB_OUTPUT": str(output)}
            result = subprocess.run(["bash", "-e", "-c", mock + script], env=env,
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text().splitlines(), ["key=setup-go-" + expected + "go-1.27.4-" + "a" * 64,
                                                             "modules=/public modules", "build=/public build"])


    def test_actual_signer_build_keeps_tagged_test_graph_and_parent_hang_cap(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root)
        self.addCleanup(releases.close)
        with patch.object(releases, "exporter_overlay", return_value=[]) as overlay, \
                patch.object(installed_releases.subprocess, "run") as run, \
                patch.object(installed_clock, "timeout", return_value=installed_clock.HANG_SECONDS) as timeout:
            releases.prepare_signer.__wrapped__(releases)
        overlay.assert_called_once_with()
        command = run.call_args_list[0]
        self.assertEqual(command.args[0], ["go", "test", "-c", "-tags", "mesh_installed_fixture",
                                          "-o", str(self.root / "signer-test"), "./internal/update"])
        self.assertEqual(command.kwargs["timeout"], installed_clock.HANG_SECONDS)
        self.assertEqual(timeout.call_count, 2)

    def test_complete_native_support_matrix_survives_timing_policy_removal(self):
        adoption = job(CATCHUP, "native-adoption")
        fresh = job(CATCHUP, "fresh-managed")
        self.assertEqual(matrix_values(adoption, "scenario"), ["all", "interrupt"])
        self.assertEqual(matrix_values(adoption, "platform"), list(workflow.RUNNERS))
        for source in (adoption, fresh):
            self.assertEqual(re.findall(r"^          - platform: (.+)$", source, re.M), list(workflow.RUNNERS))
        self.assertIn('if [[ -n $PAIRED_SCENARIO ]]; then extra=(--scenario "$PAIRED_SCENARIO"); fi', adoption)
        self.assertIn("--fresh-managed", fresh)


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
        with zipfile.ZipFile(outer, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=1) as archive:
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


    def test_job_graph_and_consumer_commands_keep_complete_profiles_and_no_builds(self):
        for name in ("prepare", "consume"):
            source = job(PREPARED, name)
            self.assertLess(source.index("Record job duration"), source.index("uses: actions/checkout"))
            self.assertEqual(source.count('open(p,"x")'), 1)
            self.assertIn("timeout-minutes: 30", source)
            self.assertNotIn("--clock-", source)
        consumer = job(PREPARED, "consume")
        self.assertIn("needs: [selection, prepare]", consumer)
        self.assertIn("profile: ${{ fromJSON(needs.selection.outputs.profiles) }}", consumer)
        self.assertNotIn("setup-go", consumer)
        self.assertNotIn("go build", consumer)
        self.assertNotIn("--native-helper", consumer)
        self.assertNotIn("worker-registry", consumer)
        aggregate = job(PREPARED, "aggregate")
        self.assertIn("scripts/aggregate-installed-slices.py", aggregate)
        self.assertIn('"$RUNNER_TEMP/preparation/cohort-proof"', aggregate)
        self.assertIn("for profile in none first final", aggregate)
        self.assertNotIn("continue-on-error", PREPARED)

    def test_actual_consumer_shell_emits_complete_mode_and_role(self):
        script = step(job(PREPARED, "consume"), "Run one complete authoritative installed profile using bundled helper")
        control = self.root / "cohort-control"
        control.mkdir()
        for mode in workflow.MODES:
            environment = os.environ | {"RUNNER_TEMP": str(self.root), "SOURCE_HOST": "pc", "PROOF_MODE": mode,
                "PYTHONDONTWRITEBYTECODE": "1"}
            result = subprocess.run(["bash", "-c", "python3() { printf '%s\\n' \"$@\"; };\n" + script],
                env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = result.stdout.splitlines()
            self.assertIn("--format-waypoint" if mode == "waypoint" else "--signed-chain", args)
            self.assertIn("--installed-adoption", args)
            self.assertIn("--prepared-releases", args)
            self.assertIn("--installed-slice", args)
            self.assertFalse(any(argument.startswith("--clock-") for argument in args))

    def test_harness_and_original_preparation_sources_remain_distinct(self):
        self.assertIn("MESH_HARNESS_SOURCE: ${{ inputs.harness_source }}", PREPARED)
        self.assertIn("MESH_PREPARATION_SOURCE: ${{ inputs.worker_artifact_source }}", PREPARED)
        producer = job(PREPARED, "prepare")
        self.assertIn('installed_artifacts.py native "$input" "$MESH_PREPARATION_SOURCE"', producer)
        self.assertIn("PRODUCER_SOURCE: ${{ inputs.worker_artifact_source }}", producer)
        for name in ("prepare", "consume"):
            self.assertIn("ref: ${{ inputs.harness_source }}", job(PREPARED, name))

    def test_small_evidence_uploads_keep_descriptors_without_public_payload(self):
        for role in ("prepare", "consume"):
            with self.subTest(role=role):
                self.check_small_upload(role)

    def check_small_upload(self, role):
        upload = job(PREPARED, role).split("        if: always()\n        with:\n          name: installed-", 1)[1]
        paths = upload.split("          path: |\n", 1)[1].split("          compression-level:", 1)[0]
        patterns = [line.strip().replace("${{ runner.temp }}/", "") for line in paths.splitlines()]
        names = ("cohort-proof/fixture-ownership.json", "cohort-proof/fixture-clock.json",
                 "cohort-proof/prepared-descriptor.json", "cohort-control/tools-cleanup-decision.json",
                 "prepared.noindex/prepared.json", "prepared.noindex/prepared-workers.json", "prepared.noindex/provenance.json")
        for name in names:
            self.assertTrue(self.upload_matches(name, patterns), name)
        payloads = ("prepared.noindex/workers/hash/mesh", "prepared.noindex/releases/version/mesh_linux_arm64.tar.gz",
                    "prepared.noindex/signer-test", "prepared-transfer.noindex/bundle.tar")
        for name in payloads:
            self.assertFalse(self.upload_matches(name, patterns), name)

    def upload_matches(self, name, patterns):
        return any(fnmatch.fnmatchcase(name, pattern) or name.startswith(pattern + "/") for pattern in patterns)

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
            self.assertIn("matrix: ${{ fromJSON(needs.selection.outputs.case) }}", source)
            self.assertNotIn("platform: [", source)
            self.assertNotIn("mode: [", source)
        consumers = job(PREPARED, "consume")
        self.assertIn("include: ${{ fromJSON(needs.selection.outputs.case).include }}", consumers)
        self.assertIn("profile: ${{ fromJSON(needs.selection.outputs.profiles) }}", consumers)
        profiles = workflow.workflow_case("linux/amd64", "chain")["profiles"]
        self.assertEqual(profiles, ["none", "first", "final"])
        actual = {(row["platform"], row["mode"], profile) for row in rows for profile in profiles}
        self.assertEqual(actual, set(itertools.product(workflow.RUNNERS, workflow.MODES, workflow.PROFILES)))
        self.assertEqual(len(actual), 18)
        self.assertIn("needs: [selection, prepare]", consumers)
        self.assertIn("needs: [selection, prepare, consume]", job(PREPARED, "aggregate"))
        self.assertIn("needs: selection", job(PREPARED, "prepare"))
        self.assertIn("fail-fast: false", job(CATCHUP, "prepared-cohort"))
        self.assertIn("mode: [oldest, order]", job(CATCHUP, "signed-cohort-chain"))
        self.assertEqual(job(CATCHUP, "signed-cohort-chain").count("            mode: order"), 2)

    def test_linux_consumers_are_ready_when_mac_preparations_are_cancelled(self):
        cases = outer_cases()
        completed = {(case, "selection") for case in cases} | {(case, "prepare") for case in cases if case[0].startswith("linux/")}
        needs = re.search(r"^    needs: \[(.+)\]$", job(PREPARED, "consume"), re.M).group(1).split(", ")
        ready = [case for case in cases if all((case, dependency) in completed for dependency in needs)]
        self.assertEqual(set(ready), {case for case in cases if case[0].startswith("linux/")})
        self.assertEqual(len(ready) * len(workflow.PROFILES), 12)

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

    def test_actual_selection_step_requires_its_case_and_all_profiles(self):
        command = step(job(PREPARED, "selection"), "Select native case and all complete profiles")
        for platform, mode in outer_cases():
            output = self.root / (platform.replace("/", "-") + "-" + mode)
            environment = os.environ | {"GITHUB_OUTPUT": str(output), "PYTHONDONTWRITEBYTECODE": "1",
                "PYTHONPATH": str(ROOT / "scripts"), "NATIVE_PLATFORM": platform,
                "PROOF_MODE": mode}
            result = subprocess.run(["bash", "-c", command], cwd=ROOT, env=environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            observed = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(json.loads(observed["case"]), workflow.case_matrix(platform, mode))
            self.assertEqual(json.loads(observed["profiles"]), list(workflow.PROFILES))


    def test_cli_bundled_preparer_is_optional_and_waypoint_is_forwarded(self):
        spec = importlib.util.spec_from_file_location("prepared_workflow_cli", ROOT / "scripts/prove-installed-catchup.py")
        cli = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cli)
        args = SimpleNamespace(native_helper=None, export_prepared=None, prepared_releases=Path("/bundle"), installed_slice=Path("/slice"))
        with patch("fixtures.installed_prepared.run_slice") as run:
            cli.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64")
        run.assert_called_once_with(Path("/root"), Path("/out"), "linux/amd64", None, Path("/bundle"), Path("/slice"))


    def test_prepared_cli_forwards_only_to_the_selected_worker_role(self):
        spec = importlib.util.spec_from_file_location("prepared_budget_proof", ROOT / "scripts/prove-installed-catchup.py")
        proof = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(proof)
        worker = SimpleNamespace(run_prepare=unittest.mock.Mock(), run_slice=unittest.mock.Mock())
        args = SimpleNamespace(export_prepared=Path("/export"), prepared_releases=None, source_host="pc",
            worker_registry=Path("/registry"), worker_probe=Path("/probe"), installed_slice=Path("/slice"),
            native_helper=unittest.mock.Mock(), format_waypoint=True)
        import fixtures
        with patch.dict(sys.modules, {"fixtures.installed_prepared": worker}), \
                patch.object(fixtures, "installed_prepared", worker, create=True):
            proof.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64")
            self.assertEqual(worker.run_prepare.call_args.args[3]["name"], "pc")
            self.assertTrue(worker.run_prepare.call_args.kwargs["format_waypoint"])
            self.assertEqual(worker.run_prepare.call_args.kwargs["preparer"], args.native_helper.resolve.return_value)
            worker.run_slice.assert_not_called()
            args.export_prepared, args.prepared_releases = None, Path("/bundle")
            proof.run_prepared_requested(args, Path("/root"), Path("/out"), "linux/amd64")
            worker.run_slice.assert_called_once_with(Path("/root"), Path("/out"), "linux/amd64", args.native_helper.resolve.return_value,
                                                     Path("/bundle"), Path("/slice"))


    def test_prepared_cli_rejects_incomplete_and_mixed_roles(self):
        spec = importlib.util.spec_from_file_location("prepared_argument_proof", ROOT / "scripts/prove-installed-catchup.py")
        proof = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(proof)
        args = {"export_prepared": None, "prepared_releases": Path("/bundle"), "installed_slice": Path("/slice"),
            "installed_adoption": True, "signed_chain": True, "format_waypoint": False, "ordered_hosts": False, "fresh_managed": False,
            "scenario": None, "prepare_only": False, "publication_version": None, "publication_source": None,
            "native_helper": Path("/preparer")}
        parser = unittest.mock.Mock()
        parser.error.side_effect = ValueError("invalid prepared CLI")
        proof.validate_prepared_arguments(parser, SimpleNamespace(**args))
        parser.error.assert_not_called()
        for changes in ({"installed_slice": None}, {"fresh_managed": True}, {"publication_source": "a" * 40},
                        {"installed_adoption": False}, {"format_waypoint": True}, {"export_prepared": Path("/export")}):
            with self.assertRaisesRegex(ValueError, "invalid prepared CLI"):
                proof.validate_prepared_arguments(parser, SimpleNamespace(**(args | changes)))



class LaunchCleanupControls(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="launch-cleanup-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.control = self.root / "cohort-control"
        self.control.mkdir()
        self.output = self.root / "cohort-proof"
        self.launch = self.control / "fixture-launch.json"
        preflight = str(self.root / "preflight")
        self.manager = {"schema": 1, "passed": True, "created": {"root": preflight},
                        "cleanup": {"serviceAbsent": True, "processAbsent": True}}
        (self.control / "native-manager.json").write_text(json.dumps(self.manager))
        (self.control / "scratch-cleanup.json").write_text(json.dumps({"root": preflight, "absent": True}))
        self.names = ("native-worker-input.noindex", "native-tools.noindex", "prepared.noindex", "prepared-transfer.noindex")
        for name in self.names:
            directory = self.root / name
            directory.mkdir()
            (directory / "payload").write_bytes(b"never execute")
        spec = importlib.util.spec_from_file_location("outcome_cleanup_cli", ROOT / "scripts/prove-installed-catchup.py")
        self.cli = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.cli)
        self.argv = ["proof", str(self.output), "--native-preflight", "--fresh-managed", "--launch-record", str(self.launch)]

    def fail_validation(self):
        primary = ValueError("prepared identity validation failed")
        with patch.object(sys, "argv", self.argv), patch.object(self.cli, "validate_invocation", side_effect=primary), \
                patch.object(self.cli, "run_owned") as run, self.assertRaises(ValueError) as caught:
            self.cli.main()
        self.assertIs(caught.exception, primary)
        run.assert_not_called()
        return primary

    def retain(self):
        with self.assertRaisesRegex(RuntimeError, "ownership unconfirmed"):
            workflow.cleanup(self.root, True)
        self.assertTrue(all((self.root / name / "payload").exists() for name in self.names))

    def test_early_validation_failure_cleans_only_owned_inputs_without_job_clock(self):
        self.fail_validation()
        workflow.cleanup(self.root)
        self.assertTrue(all(not (self.root / name).exists() for name in self.names[:2]))
        self.assertTrue(all((self.root / name).exists() for name in self.names[2:]))
        workflow.cleanup(self.root, True)
        workflow.cleanup(self.root, True)
        self.assertTrue(all(not (self.root / name).exists() for name in self.names))
        self.assertTrue(self.launch.exists())

    def test_cold_cli_validation_and_cleanup_do_not_launch_any_payload(self):
        script = """
import importlib.util, sys
from unittest.mock import patch
spec = importlib.util.spec_from_file_location('cold_cli', sys.argv[1])
cli = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cli)
primary = ValueError('cold prepared identity failure')
with patch.object(cli, 'run_owned', side_effect=AssertionError('runtime launched')), patch.object(cli, 'validate_invocation', side_effect=primary):
    sys.argv = ['proof', sys.argv[2], '--native-preflight', '--fresh-managed', '--launch-record', sys.argv[3]]
    try:
        cli.main()
    except ValueError as error:
        assert error is primary
"""
        environment = os.environ | {"PYTHONPATH": str(ROOT / "scripts"), "PYTHONDONTWRITEBYTECODE": "1"}
        subprocess.run([sys.executable, "-c", script, str(ROOT / "scripts/prove-installed-catchup.py"), str(self.output), str(self.launch)],
                       env=environment, check=True, capture_output=True, timeout=180)
        self.assertEqual(installed_launch.document(self.launch)["state"], "validation-failed")
        subprocess.run([sys.executable, str(ROOT / "scripts/fixtures/installed_workflow.py"), "cleanup", str(self.root)],
                       env=environment, check=True, capture_output=True, timeout=180)
        self.assertTrue(all(not (self.root / name).exists() for name in self.names[:2]))
        self.assertTrue(all((self.root / name).exists() for name in self.names[2:]))

    def test_runtime_possible_or_unconfirmed_ownership_retains_inputs(self):
        self.fail_validation()
        self.output.mkdir()
        for name in ("fixture-clock.json", "fixture-ownership.json"):
            path = self.output / name
            for value in ("{", "{}"):
                path.write_text(value)
                self.retain()
            path.unlink()
        installed_launch.Launch(self.launch, self.output).runtime_possible()
        self.retain()

    def test_validation_evidence_requires_matching_preflight_and_output(self):
        self.fail_validation()
        valid = self.launch.read_bytes()
        launch = json.loads(valid)
        for change in ({"schema": True}, {"output": "/other-job/cohort-proof"}, {"state": "runtime-possible"},
                       {"error": ""}, {"errorType": ""}):
            self.launch.write_text(json.dumps(launch | change))
            self.retain()
        self.launch.write_bytes(valid)
        for path in (self.control / "native-manager.json", self.control / "scratch-cleanup.json"):
            original = path.read_bytes()
            for value in ("{", "[]", "{}"):
                path.write_text(value)
                self.retain()
            path.unlink()
            self.retain()
            path.write_bytes(original)
        self.manager["cleanup"]["processAbsent"] = False
        (self.control / "native-manager.json").write_text(json.dumps(self.manager))
        self.retain()

    def test_activation_failure_records_possible_runtime_and_preserves_error(self):
        primary = installed_clock.ClockExpired("watchdog activation failed")
        clock = installed_clock.Clock({})
        with patch.object(sys, "argv", self.argv), patch.object(self.cli, "validate_invocation", return_value=("linux/amd64", clock)), \
                patch.object(clock, "activate", side_effect=primary), self.assertRaises(installed_clock.ClockExpired) as caught:
            self.cli.main()
        self.assertIs(caught.exception, primary)
        self.assertEqual(installed_launch.document(self.launch)["state"], "runtime-possible")
        self.retain()

    def test_failed_pre_runtime_record_keeps_original_error(self):
        original_write = Path.write_text
        def write(path, data, *args, **kwargs):
            if path == self.launch and '"validation-failed"' in data:
                raise OSError("evidence disk failure")
            return original_write(path, data, *args, **kwargs)
        with patch.object(Path, "write_text", write):
            error = self.fail_validation()
        self.assertIn("evidence disk failure", error.__notes__[0])
        self.retain()

    def test_hosted_dispatches_pass_the_same_launch_record(self):
        for source in (job(PREPARED, "prepare"), job(PREPARED, "consume"), job(CATCHUP, "signed-cohort-chain")):
            self.assertEqual(source.count('--launch-record "$RUNNER_TEMP/cohort-control/fixture-launch.json"'), 1)
            self.assertLess(source.index('printf'), source.index("--launch-record"))


if __name__ == "__main__":
    unittest.main()
