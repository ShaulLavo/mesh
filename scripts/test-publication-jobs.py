#!/usr/bin/env python3
"""Offline real-shape publication producers, selectors and finalizer controls."""
import copy
import ast
import importlib
import json
import os
from pathlib import Path
import shutil
import sys
import unittest
from unittest import mock
from types import SimpleNamespace

from fixtures import installed_publication, release_worker_completion as completion
from fixtures import release_publication_platform as platforms, release_documents, draft_intake
from fixtures import release_worker_groups as groups

controls = importlib.import_module("test-release-worker-groups")


class PublicationJobsTests(unittest.TestCase):
    def setUp(self):
        self.fixture = controls.PublishedCompletionTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        self.directory = self.root / "draft-assets"
        self.directory.mkdir()
        self.inputs = self.root / "jobs"
        self.inputs.mkdir()
        self.output = self.root / "aggregate"

    def configure(self, first=True, staged=False):
        baseline = completion.fleet_baselines.load()
        baseline["firstPublishedUpdater"] = "v0.1.1" if first else None
        history = [controls.retained_updater(1), controls.retained_updater(2)] if staged else [controls.retained_updater(1)] if first else []
        selection = {"workers": [groups.member(worker) for platform in controls.index.PLATFORMS for worker in controls.selected(2, platform)]}
        sources = [controls.Bundle(self.directory, controls.selected(2, platform), platform=platform).reference()
                   for platform in controls.index.PLATFORMS]
        manifest = {"version": "v0.1.99", "commit": controls.SOURCE, "artifacts": self.fixture.release["artifacts"],
                    "compatibility": {"stateWrite": 2, "workerWrite": 2}}
        for name, value in (("mesh-release.json", manifest), ("mesh-worker-groups.json", {"selection": selection, "sources": sources}),
                            ("mesh-retention.json", history), ("mesh-update.json", {"offline": "real-shape control"}),
                            ("mesh-update.sig.json", {"offline": "never signature acceptance"})):
            (self.directory / name).write_bytes(groups.encode(value))
        self.context = completion.release_context(self.directory)
        self.fixture.release, self.fixture.selection = self.context, selection
        for platform in controls.index.PLATFORMS:
            self.fixture.write_completion(platform)
            if first:
                self.fixture.install_platform_routes(platform, self.context["routeContract"])
        self.inventory = completion.task_inventory(self.directory, "linux/amd64", controls.SOURCE)
        return self.inventory

    def emit(self):
        source = self.root / "linux-amd64"
        for number, task in enumerate(self.inventory["tasks"]):
            output = self.inputs / str(number)
            output.mkdir()
            for name in ("real-draft.json", "harness-source.json", "first-published-updater.json"):
                shutil.copyfile(source / name, output / name)
            if "case" in task:
                shutil.copyfile(source / task["case"], output / task["case"])
            else:
                relative = Path("groups") / task["scenario"] / str(task["group"]["index"])
                shutil.copytree(source / relative, output / relative)
            completion.record_task(output, self.inventory, task, self.context, self.fixture.selection)

    def aggregate(self):
        completion.aggregate_tasks(self.directory, "linux/amd64", controls.SOURCE, self.inputs, self.output)

    def test_inventory_has_each_legacy_profile_fresh_and_only_available_routes(self):
        for first, staged, route_count in ((False, False, 0), (True, False, 2), (True, True, 4)):
            self.check_inventory(first, staged, route_count)

    def check_inventory(self, first, staged, route_count):
        inventory = self.configure(first, staged)
        self.assertEqual(len(inventory["tasks"]), 4 + route_count)
        self.assertEqual(len({task["id"] for task in inventory["tasks"]}), len(inventory["tasks"]))
        self.assertEqual(inventory, completion.task_inventory(self.directory, "linux/amd64", controls.SOURCE))
        if not first:
            return
        for platform in controls.index.PLATFORMS:
            shutil.rmtree(self.root / (platform["os"] + "-" + platform["arch"]) / "groups")

    def test_producer_to_aggregate_to_existing_schema2_finalizer(self):
        self.configure(staged=True)
        self.emit()
        self.aggregate()
        header = json.loads((self.output / "worker-completion.json").read_bytes())
        self.assertEqual(header["schema"], 2)
        original = self.root / "linux-amd64"
        self.assertEqual(header, json.loads((original / "worker-completion.json").read_bytes()))
        shutil.rmtree(original)
        self.output.rename(original)
        completion.validate(self.root, self.context, controls.SOURCE, self.fixture.selection)

    def test_partial_and_duplicate_tasks_cannot_write_completion(self):
        self.configure()
        self.emit()
        path = next(self.inputs.rglob("publication-task.json"))
        saved = path.read_bytes()
        path.unlink()
        with self.assertRaisesRegex(ValueError, "incomplete"):
            self.aggregate()
        self.assertFalse(self.output.exists())
        other = next(self.inputs.rglob("publication-task.json"))
        shutil.copytree(other.parent, path.parent, dirs_exist_ok=True)
        with self.assertRaisesRegex(ValueError, "duplicates"):
            self.aggregate()
        path.write_bytes(saved)
        self.aggregate()

    def test_first_updater_aggregates_without_inventing_public_routes(self):
        self.configure(first=False)
        self.emit()
        self.aggregate()
        header = json.loads((self.output / "worker-completion.json").read_bytes())
        self.assertEqual(header["routes"], [])
        self.assertEqual(header["routeContract"]["multihop"], "unavailable-first-updater")

    def test_actual_selected_fresh_producer_emits_only_a_task_receipt(self):
        self.configure()
        output = self.inputs / "fresh-run"
        output.mkdir()
        source = self.root / "linux-amd64"
        (output / "harness-source.json").write_bytes((source / "harness-source.json").read_bytes())
        draft = json.loads((source / "real-draft.json").read_bytes())
        root = self.root / "native-run"
        directory = root / "releases" / self.context["version"]
        shutil.copytree(self.directory, directory)
        releases = SimpleNamespace(native="linux/amd64", directory=directory.parent, physical_directory=directory.parent, verification=draft)
        (root / "publication-reader").write_bytes(b"provided H inspector control")
        case = SimpleNamespace(host={"name": "fresh-managed"}, root=root, retained=lambda: {}, snapshots={},
            prepare=lambda: {"helperBeforeDaemon": {"sha256": draft["build"]["digest"]},
                "firstNativeStart": {"mappedImage": {"sha256": draft["build"]["digest"]}}}, seed=lambda: None,
            initialize_preserved_baseline=lambda: {}, restart_control=lambda: {}, inspect_retained=lambda: None,
            control=lambda _operation: {"host": {"build": draft["build"]}}, close=lambda: None)
        origin = mock.Mock()
        origin.close.return_value = {"processesAbsent": True}
        with mock.patch.dict(os.environ, MESH_HARNESS_SOURCE=controls.SOURCE), \
                mock.patch.object(installed_publication, "stage", side_effect=lambda *_args: self.stage_fixture(output, source, releases)), \
                mock.patch.object(installed_publication.subprocess, "run"), \
                mock.patch.object(installed_publication.release_tool, "configured", return_value=root / "mesh-release-sign"), \
                mock.patch.object(installed_publication.publication_origin, "Origin", return_value=origin), \
                mock.patch.object(installed_publication.installed_fresh, "FreshCase", return_value=case), \
                mock.patch.object(installed_publication.installed_fresh, "collect_diagnostics"), \
                mock.patch.object(installed_publication.installed_fresh, "retain_install_logs"):
            installed_publication.prove(root, output, "linux/amd64", self.context["version"], controls.SOURCE,
                                        None, task_id="fresh-managed")
        receipt = json.loads((output / "publication-task.json").read_bytes())
        completion.validate_task(output, receipt, self.inventory, self.context, self.fixture.selection)
        self.assertFalse((output / "worker-completion.json").exists())
        origin.close.assert_called_once()

    def stage_fixture(self, output, source, releases):
        shutil.copyfile(source / "real-draft.json", output / "real-draft.json")
        return releases

    def test_mixed_and_diagnostic_receipts_reject_before_publication(self):
        self.configure(staged=True)
        self.emit()
        path = next(self.inputs.rglob("publication-task.json"))
        original = json.loads(path.read_bytes())
        for changes in ({"inventorySha256": "e" * 64}, {"diagnostic": True}, {"schema": 0},
                        {"task": self.inventory["tasks"][-1]}):
            path.write_bytes(groups.encode(original | changes))
            self.reject_aggregate()
        path.write_bytes(groups.encode(original))

    def reject_aggregate(self):
        with self.assertRaises((ValueError, KeyError)):
            self.aggregate()
        self.assertFalse(self.output.exists())

    def test_signed_retention_signature_harness_and_baseline_context_cannot_mix(self):
        self.configure()
        self.emit()
        for name in ("mesh-retention.json", "mesh-update.sig.json"):
            path = self.directory / name
            data = path.read_bytes()
            path.write_bytes(data + b"\n")
            with self.assertRaisesRegex(ValueError, "inventory"):
                self.aggregate()
            path.write_bytes(data)
        with self.assertRaisesRegex(ValueError, "inventory"):
            completion.aggregate_tasks(self.directory, "linux/amd64", "e" * 40, self.inputs, self.output)
        completion.fleet_baselines.load()["extraControl"] = True
        with self.assertRaisesRegex(ValueError, "inventory"):
            self.aggregate()
        self.assertFalse(self.output.exists())

    def test_tampered_case_is_not_coverage(self):
        self.configure()
        self.emit()
        number, task = next((number, task) for number, task in enumerate(self.inventory["tasks"]) if "case" in task)
        receipt = self.inputs / str(number) / "publication-task.json"
        value = json.loads(receipt.read_bytes())
        self.assertEqual(value["case"]["name"], task["case"])
        leaf = receipt.parent / value["case"]["name"]
        leaf.write_bytes(leaf.read_bytes() + b"\n")
        with self.assertRaisesRegex(ValueError, "changed"):
            self.aggregate()
        self.assertFalse(self.output.exists())

    def test_missing_group_observation_is_not_coverage(self):
        self.configure()
        self.emit()
        path = next(self.inputs.rglob("*-cohort.json"))
        path.unlink()
        with self.assertRaises(FileNotFoundError):
            self.aggregate()
        self.assertFalse(self.output.exists())

    def test_source_defined_partition_keeps_every_bounded_group(self):
        self.configure()
        platform = controls.PLATFORM
        workers = controls.selected(65, platform)
        first = workers[-1]["version"]
        completion.fleet_baselines.load()["firstPublishedUpdater"] = first
        index_path = self.directory / "mesh-worker-groups.json"
        index = json.loads(index_path.read_bytes())
        index["selection"]["workers"] = [groups.member(item) for item in workers]
        index["sources"] = [controls.Bundle(self.directory, workers).reference()]
        index_path.write_bytes(groups.encode(index))
        history = [controls.retained_updater(65)]
        (self.directory / "mesh-retention.json").write_bytes(groups.encode(history))
        inventory = completion.task_inventory(self.directory, "linux/amd64", controls.SOURCE)
        routes = [task for task in inventory["tasks"] if "group" in task]
        self.assertEqual([task["id"] for task in routes], [f"{name}:{number}" for name in ("published", "published-rollback") for number in range(3)])
        self.assertEqual([len(task["group"]["members"]) for task in routes], [31, 31, 3, 31, 31, 3])

    def test_selected_staged_route_runs_wholly_in_one_native_job(self):
        self.configure(staged=True)
        task = completion.select_task(self.inventory, "published-chain-rb:0")
        releases = SimpleNamespace(verification={"routeContract": self.context["routeContract"]}, native="linux/amd64",
            builds={}, directory=self.directory.parent)
        baseline = {"executable": self.directory / "original", "artifact": {"binarySha256": "b" * 64}}
        build = {"modified": False, "version": "v0.1.1", "digest": "b" * 64}
        private_group = copy.deepcopy(task["group"])
        private_group["members"][0]["binary"] = "/owned/original"
        with mock.patch.object(installed_publication.fleet_baselines, "acquire", return_value=baseline), \
                mock.patch.object(installed_publication.subprocess, "check_output", return_value=groups.encode(build)), \
                mock.patch.object(installed_publication.installed_cohort, "stage", return_value="owned-registry"), \
                mock.patch.object(installed_publication.installed_adoption, "installed_groups", return_value=[private_group]), \
                mock.patch.object(installed_publication, "run_published_groups") as run:
            installed_publication.run_task(self.root, self.inputs, "v0.1.99", None, releases, None, "registry", "probe", task)
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[-4:], (True, [private_group], "published-chain-rb", "v0.1.2"))
        self.assertEqual(run.call_args.kwargs, {"complete": False})

    def test_cli_inventory_uses_the_same_deterministic_contract(self):
        self.configure(staged=True)
        # The CLI subprocess reads the real fleet inventory, so bind it to this fixture in-process.
        cli = importlib.import_module("publication-jobs")
        with mock.patch.object(sys, "argv", ["publication-jobs.py", "inventory", str(self.directory), "linux/amd64", controls.SOURCE]), \
                mock.patch("builtins.print") as output:
            cli.main()
        self.assertEqual(json.loads(output.call_args.args[0]), self.inventory)

    def test_selector_rejects_diagnostic_and_supplemental_inputs_before_native_work(self):
        cli = importlib.import_module("prove-installed-catchup")
        base = ["prove-installed-catchup.py", str(self.root / "unused"), "--native-preflight", "--publication-task", "fresh-managed",
                "--publication-version", "v0.1.99", "--publication-source", controls.SOURCE]
        for options in (["--clock-mode", "diagnostic"], ["--signed-chain"], ["--source-host", "pc"], ["--fresh-managed"]):
            with mock.patch.object(sys, "argv", base + options), mock.patch.object(cli, "run_owned") as run, \
                    mock.patch("sys.stderr"), self.assertRaises(SystemExit) as failure:
                cli.main()
            self.assertEqual(failure.exception.code, 2)
            run.assert_not_called()
        self.assertFalse((self.root / "unused").exists())

    def test_new_selector_and_controls_stay_at_most_three_blocks_deep(self):
        depth = importlib.import_module("test-installed-catchup").maximum_depth
        for path in (Path(__file__), Path(__file__).with_name("publication-jobs.py")):
            self.assertLessEqual(depth(ast.parse(path.read_text()), 0), 3, str(path))


class ReducedPublicationTests(unittest.TestCase):
    RUN = 179
    ATTEMPTS = dict(zip(platforms.PLATFORMS, (2, 1, 2), strict=True))

    def setUp(self):
        self.old = PublicationJobsTests()
        self.old.setUp()
        self.addCleanup(self.old.doCleanups)
        for artifact in self.old.fixture.release["artifacts"]:
            native = artifact["platform"]
            artifact["archive"] = draft_intake.ARCHIVES[native["os"] + "/" + native["arch"]]
        self.root, self.logical = self.old.root, self.old.directory
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.packet_root = self.root / "packets"

    def configure(self, *, first=True, staged=True, evidence_run=None):
        self.old.configure(first=first, staged=staged)
        self.context = self.old.context
        fleet = completion.fleet_baselines.load()
        for host in fleet["hosts"]:
            host.update(helper="v0.1.2", helperRunning=host["name"] != "pc")
        fleet["hosts"].append({"name": "vps", "platform": "linux/amd64", "daemon": "v0.1.2",
                               "helper": "v0.1.1", "helperRunning": True})
        self.index = json.loads((self.logical / "mesh-worker-groups.json").read_bytes())
        self.snapshot = self.physical_snapshot()
        for native in platforms.PLATFORMS:
            self.write_reader_index(native, first)
        self.plan = platforms.plan(self.logical, controls.SOURCE, self.snapshot, self.packet_root, self.context, self.index, evidence_run)
        self.plan_path = self.root / "plan.json"
        self.plan_path.write_bytes(groups.encode(self.plan))
        self.sha = groups.digest(self.plan_path.read_bytes())

    def physical_snapshot(self):
        names = set(draft_intake.NAMES) | set(release_documents.PHYSICAL) | {"mesh.rb"}
        assets = [{"id": number, "name": name, "state": "uploaded", "size": 1, "digest": "sha256:" + groups.digest(name.encode())}
                  for number, name in enumerate(sorted(names), 1)]
        repository = "https://github.com/ShaulLavo/mesh"
        return {"id": 1, "intake_run": self.RUN, "repository": "ShaulLavo/mesh", "draft": True,
                "tag_name": self.context["version"], "target_commitish": self.context["source"], "frozen_baselines": [],
                "url": "https://api.github.com/repos/ShaulLavo/mesh/releases/1",
                "html_url": repository + "/releases/tag/" + self.context["version"], "assets": assets}

    def write_reader_index(self, native, first):
        platform = dict(zip(("os", "arch"), native.split("/"), strict=True))
        directory = self.packet_root / platforms.platform_key(native)
        directory.mkdir(parents=True)
        digest = groups.digest(native.encode())
        invocation = {"artifact": {"platform": platform, "binarySha256": "b" * 64}, "binary": "c" * 64,
                      "source": "v0.1.1", "metadata": self.context["metadata"], "sha256": digest,
                      "packet": digest + ".reader.json"}
        (directory / "index.json").write_bytes(groups.encode(
            {"schema": 1, "target": self.context["version"], "platform": platform,
             "invocations": [invocation] if first else []}))

    def test_reduced_contract_keeps_exact_release_shapes(self):
        self.configure()
        expected = {
            "linux/amd64": ["fresh-managed", "pc-publication", "pc-publication-interrupt", "pc-rollback", "vps-publication",
                            "published:0", "published-rollback:0", "published-chain:0", "published-chain-rb:0"],
            "linux/arm64": ["fresh-managed", "pi-publication", "published-chain:0", "published-chain-rb:0"],
            "darwin/arm64": ["fresh-managed", "mac-publication", "mac-publication-interrupt", "mac-rollback",
                             "published-chain:0", "published-chain-rb:0"],
        }
        actual = {native: [task["id"] for task in inventory["tasks"]] for native, inventory in self.plan["platforms"].items()}
        self.assertEqual(actual, expected)
        self.assertEqual(self.plan, platforms.plan(self.logical, controls.SOURCE, self.snapshot, self.packet_root, self.context, self.index))
        self.assertEqual(self.plan["platforms"]["linux/amd64"]["tasks"][1]["prior"]["helperRunning"], False)
        self.assertEqual(self.plan["platforms"]["linux/amd64"]["tasks"][4]["prior"]["daemon"]["version"], "v0.1.2")

    def test_first_updater_has_no_routes_and_first_successor_has_direct_on_each_platform(self):
        for first, route_names in ((False, []), (True, ["published", "published-rollback"])):
            with self.subTest(first=first):
                self.check_first_routes(first, route_names)

    def check_first_routes(self, first, route_names):
        self.configure(first=first, staged=False)
        for inventory in self.plan["platforms"].values():
            self.assertEqual([task["scenario"] for task in inventory["tasks"] if "group" in task], route_names)
        shutil.rmtree(self.packet_root)

    def test_representative_group_prefers_an_older_worker_protocol(self):
        self.configure()
        workers = [worker | {"protocol": 2} for worker in controls.selected(65, controls.PLATFORM)]
        workers[33]["protocol"] = 1
        contract = self.context["routeContract"] | {"first": workers[-1]["version"], "intermediate": None}
        context = self.context | {"routeContract": contract}
        selection = {"workers": [groups.member(worker) for worker in workers]}
        index = {"selection": selection, "sources": [controls.Bundle(self.logical, workers).reference()]}
        routes = platforms.route_tasks(self.logical, "linux/amd64", context, index)
        self.assertEqual([task["group"]["index"] for task in routes], [1, 1])
        self.assertEqual(len(routes[0]["group"]["members"]), 31)

    def emit(self):
        for native, attempt in self.ATTEMPTS.items():
            output = self.directory(native)
            output.mkdir()
            planned = self.plan["platforms"][native]
            run = self.plan["evidenceRun"]
            binding = platforms.job_binding(native, run, attempt, self.sha)
            self.emit_installed(output, native, planned, binding)
            dist = output / platforms.READERS
            dist.parent.mkdir()
            dist.write_bytes(groups.encode(planned["readers"]["outcome"] | {"publication": binding}))
            (output / "scratch-cleanup.json").write_bytes(groups.encode({"root": str(self.root / "removed"), "absent": True}))
            (output / "publication-transport-cleanup.json").write_bytes(groups.encode({"proxyStopped": True, "testSignatureRootUsed": False}))
            platforms.record_platform(output, self.plan, self.sha, native, run, attempt)

    def emit_installed(self, output, native, planned, binding):
        source = self.root / platforms.platform_key(native)
        for filename in ("real-draft.json", "harness-source.json", "first-published-updater.json"):
            shutil.copyfile(source / filename, output / filename)
        draft = json.loads((output / "real-draft.json").read_bytes())
        publication = {"release": completion.release_binding(self.context), "candidate": draft["build"], "harness": controls.SOURCE}
        for task in planned["tasks"]:
            self.emit_task(output, source, native, publication, task)
        platforms.record_installed(output, planned, binding, self.context, self.index["selection"])

    def emit_task(self, output, source, native, publication, task):
        if "case" not in task:
            relative = Path("groups") / task["scenario"] / str(task["group"]["index"])
            shutil.copytree(source / relative, output / relative)
            return
        expected = completion.expected_case(task["case"], native, publication, self.index["selection"])
        self.old.fixture.write_case(output, task["case"], expected)
        if "prior" not in task:
            return
        path = output / task["case"]
        record = json.loads(path.read_bytes())
        record["priorPair"] = task["prior"]
        if expected["scenario"] == "rollback":
            record["activationFault"] = {"bound": True, "released": True, "error": ""}
        if expected["scenario"] == "publication-interrupt":
            record["interruption"].update(confirmedBeforeInterruption=False, killedNativePID=12)
            record["result"]["helper"]["pid"] = 18
        path.write_bytes(groups.encode(record))

    def directory(self, native):
        return self.evidence / platforms.artifact_name(native, self.ATTEMPTS[native])

    def rewrite(self, path, changes):
        original = path.read_bytes()
        path.write_bytes(groups.encode(json.loads(original) | changes))
        return original

    def validate(self, *, qualification=False):
        finalizer = importlib.import_module("verify-worker-publication")
        with (
            mock.patch.object(finalizer.draft_intake, "documents", side_effect=self.materialize),
            mock.patch.object(finalizer.release_worker_index, "validate_index", return_value=self.index),
            mock.patch.object(finalizer.installed_publication, "verify_signature"),
            mock.patch.object(finalizer.draft_intake, "compare_live") as compare,
            mock.patch.object(importlib.import_module("qualify-publication"), "confirm_unpublished") as unpublished,
        ):
            finalizer.verify(self.context["version"], controls.SOURCE, self.evidence, self.root / "draft", self.RUN,
                             platform_plan=self.plan_path, plan_sha256=self.sha, harness=controls.SOURCE,
                             attempts=self.ATTEMPTS, qualification=qualification)
        if qualification:
            unpublished.assert_called_once_with(self.context["version"], self.RUN)
            compare.assert_not_called()
            return
        compare.assert_called_once()
        unpublished.assert_not_called()

    def materialize(self, _snapshot, _version, _source, target, _run):
        shutil.copytree(self.logical, target)
        return self.snapshot

    def test_finalizer_accepts_exact_fresh_and_surviving_jobs_from_a_failed_jobs_rerun(self):
        self.configure()
        self.emit()
        self.validate()

    def test_cross_run_replay_rejects_bad_evidence_and_cannot_authorize_publication(self):
        self.configure(evidence_run=180)
        self.emit()
        with self.assertRaisesRegex(ValueError, "cannot authorize publication"):
            self.validate()
        with mock.patch.dict(os.environ, GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted",
                             GITHUB_REPOSITORY="ShaulLavo/mesh", MESH_PUBLICATION_QUALIFICATION="true",
                             GITHUB_RUN_ID="180"):
            self.validate(qualification=True)
            self.check_platform_rejections(qualification=True)

    def test_finalizer_refuses_missing_duplicate_other_attempt_and_wrong_platform_evidence(self):
        self.configure()
        self.emit()
        self.check_platform_rejections()

    def check_platform_rejections(self, *, qualification=False):
        output = self.directory("linux/amd64")
        path = output / platforms.RECEIPT
        missing = path.read_bytes()
        path.unlink()
        with self.assertRaisesRegex(ValueError, "missing, duplicate"):
            self.validate(qualification=qualification)
        path.write_bytes(missing)
        duplicate = output / "duplicate"
        duplicate.mkdir()
        (duplicate / platforms.RECEIPT).write_bytes(missing)
        with self.assertRaisesRegex(ValueError, "missing, duplicate"):
            self.validate(qualification=qualification)
        shutil.rmtree(duplicate)
        for changes, error in (({"attempt": 1}, "producer attempt"), ({"run": 177}, "producer attempt"),
                               ({"platform": "darwin/arm64"}, "native platform")):
            self.reject_platform_claim(path, changes, error, qualification)
        self.validate(qualification=qualification)

    def reject_platform_claim(self, path, changes, error, qualification):
        original = self.rewrite(path, changes)
        with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, error):
            self.validate(qualification=qualification)
        path.write_bytes(original)

    def test_finalizer_refuses_other_attempt_inside_a_current_receipt(self):
        self.configure()
        self.emit()
        output = self.directory("linux/amd64")
        readers = output / platforms.READERS
        original = json.loads(readers.read_bytes())
        self.rewrite(readers, {"publication": original["publication"] | {"attempt": 1}})
        self.rewrite(output / platforms.RECEIPT, {"readers": completion.asset(output, readers)})
        with self.assertRaisesRegex(ValueError, "original reader"):
            self.validate()

    def test_finalizer_requires_each_original_reader_and_exact_physical_draft(self):
        self.configure()
        self.emit()
        output = self.directory("linux/amd64")
        readers = output / platforms.READERS
        reader_original = json.loads(readers.read_bytes())
        for count in (0, 2):
            self.rewrite(readers, {"readers": reader_original["readers"] * count})
            self.rewrite(output / platforms.RECEIPT, {"readers": completion.asset(output, readers)})
            with self.assertRaisesRegex(ValueError, "original reader"):
                self.validate()
        readers.write_bytes(groups.encode(reader_original))
        self.rewrite(output / platforms.RECEIPT, {"readers": completion.asset(output, readers)})
        self.snapshot["assets"][0]["id"] += 100
        with self.assertRaisesRegex(ValueError, "authenticated draft"):
            self.validate()

    def test_reduced_receipts_cannot_relabel_a_fresh_or_legacy_pair(self):
        self.configure()
        self.emit()
        native = "linux/amd64"
        output = self.directory(native)
        installed = output / platforms.INSTALLED
        case = output / "pc-publication-native-pair.json"
        record = json.loads(case.read_bytes())
        self.rewrite(case, {"priorPair": record["priorPair"] | {"helperRunning": True}})
        header = json.loads(installed.read_bytes())
        for task in header["tasks"]:
            if task["id"] == "pc-publication":
                task["case"] = completion.asset(output, case)
        installed.write_bytes(groups.encode(header))
        self.rewrite(output / platforms.RECEIPT, {"installed": completion.asset(output, installed)})
        with self.assertRaisesRegex(ValueError, "prior daemon/helper"):
            self.validate()

    def test_reader_runner_checks_the_signer_index_before_running_any_reader(self):
        self.configure()
        runner = importlib.import_module("run-original-update-readers")
        args = SimpleNamespace(publication_platform_plan=self.plan_path, publication_plan_sha256=self.sha, platform="linux/amd64")
        raw = (self.packet_root / "linux-amd64/index.json").read_bytes()
        with (
            mock.patch.object(installed_publication, "require_main"),
            mock.patch.object(runner.release_tool, "native_target", return_value=controls.PLATFORM),
            mock.patch.dict(os.environ, GITHUB_RUN_ID=str(self.RUN), GITHUB_RUN_ATTEMPT="2"),
        ):
            binding = runner.platform_publication(args, raw)
            self.assertEqual(binding, platforms.job_binding(args.platform, self.RUN, 2, self.sha))
            self.reject_reader_index(runner, args, raw + b"\n")

    def reject_reader_index(self, runner, args, raw):
        with self.assertRaisesRegex(ValueError, "exact publication plan"):
            runner.platform_publication(args, raw)

    def test_native_driver_consumes_one_draft_and_only_its_planned_cases(self):
        self.configure()
        native = "linux/amd64"
        output = self.root / "native-driver"
        output.mkdir()
        (output / "draft-intake.json").write_bytes(groups.encode(self.snapshot))
        version = self.context["version"]
        releases = SimpleNamespace(directory=self.root / "logical")
        shutil.copytree(self.logical, releases.directory / version)
        with (
            mock.patch.object(installed_publication, "stage", return_value=releases) as stage,
            mock.patch.object(installed_publication, "require_main"),
            mock.patch.object(installed_publication, "start_origin") as origin,
            mock.patch.object(installed_publication, "run_fresh") as fresh,
            mock.patch.object(installed_publication, "run_pairs") as pairs,
            mock.patch.object(installed_publication, "run_first_published") as routes,
            mock.patch.object(platforms, "record_installed") as record,
            mock.patch.dict(os.environ, MESH_HARNESS_SOURCE=controls.SOURCE, GITHUB_RUN_ID=str(self.RUN),
                            GITHUB_RUN_ATTEMPT="2", MESH_PUBLICATION_RUN=str(self.RUN)),
        ):
            installed_publication.prove_platform(self.root, output, native, version, controls.SOURCE,
                                                 "helper", "registry", "probe", self.plan_path, self.sha)
        stage.assert_called_once()
        origin.assert_called_once()
        fresh.assert_called_once()
        self.assertEqual(set(pairs.call_args.kwargs["planned"]), {"pc-publication", "pc-publication-interrupt", "pc-rollback", "vps-publication"})
        planned = self.plan["platforms"][native]
        self.assertEqual(routes.call_args.kwargs["tasks"], [task for task in planned["tasks"] if "group" in task])
        record.assert_called_once_with(output, planned, platforms.job_binding(native, self.RUN, 2, self.sha),
                                       self.context, self.index["selection"])



if __name__ == "__main__":
    unittest.main()
