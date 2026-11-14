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
        releases = SimpleNamespace(native="linux/amd64", directory=directory.parent, verification=draft)
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


if __name__ == "__main__":
    unittest.main()
