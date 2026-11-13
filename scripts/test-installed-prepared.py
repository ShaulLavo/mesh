"""Offline prepared release transport, exact slices and consumer routing controls."""
import ast
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_prepared as prepared, installed_slices as slices
from fixtures import installed_releases, installed_waypoint, release_worker_groups


def selection(profile="chain-none", groups=(0,), readers=()):
    return {"schema": 1, "select": [{"profile": profile, "group_indices": list(groups)}], "readers": list(readers)}


class SliceTest(unittest.TestCase):
    def test_canonical_order_and_bounded_schema(self):
        value = {"schema": 1, "select": [{"profile": "chain-final", "group_indices": [2, 0]},
                 {"profile": "chain-none", "group_indices": [1]}], "readers": [2, 0]}
        parsed = slices.parse_slice(value)
        self.assertEqual([row["profile"] for row in parsed["select"]], ["chain-none", "chain-final"])
        self.assertEqual(parsed["select"][1]["group_indices"], [0, 2])
        self.assertEqual(parsed["readers"], [0, 2])
        bad = [selection(groups=(0, 0)), selection(groups=(-1,)), selection(groups=(4096,)),
               selection(groups=(True,)), selection("unregistered"), selection(groups=()), value | {"schema": True},
               value | {"extra": 1}, value | {"select": value["select"] * 2}]
        for item in bad:
            with self.subTest(item=item), self.assertRaises(ValueError):
                slices.parse_slice(item)

    def test_exact_coverage_includes_all_profiles_and_waypoint_readers(self):
        partitions = [slices.parse_slice(selection(profile, (0, 1))) for profile in slices.PROFILES]
        slices.validate_partition(partitions, 2, False)
        with self.assertRaisesRegex(ValueError, "omit or duplicate"):
            slices.validate_partition(partitions, 2, True)
        readers = {"schema": 1, "select": [], "readers": [0, 1]}
        slices.validate_partition(partitions + [readers], 2, True)
        with self.assertRaisesRegex(ValueError, "omit or duplicate"):
            slices.validate_partition(partitions + [partitions[0]], 2, False)
        with self.assertRaises(ValueError):
            slices.validate_selection(readers, 2, False)

    def test_group_labels_retain_complete_existing_transition(self):
        self.assertEqual(slices.group_labels("chain-first"),
                         ["before-install", "native-first-hop-rollback", "native-failed-edge-restart"])
        self.assertEqual(slices.group_labels("chain-final")[-1], "native-failed-edge-restart")


class PreparedTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="prepared-control-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.root, self.output = self.base / "producer", self.base / "output"
        self.root.mkdir()
        self.output.mkdir()
        self.bundle = self.base / "bundle"
        self.pin = "a" * 40
        self.host = {"name": "fixture", "platform": "linux/amd64"}
        self.releases, self.pair, self.previous, self.groups = self.fixture()
        self.addCleanup(patch.stopall)
        patch.object(prepared, "source_pin", return_value=self.pin).start()
        patch.object(prepared.installed_origin, "request", side_effect=self.export_origin).start()

    def fixture(self):
        builds = {}
        for version in installed_releases.VERSIONS:
            path = self.root / "build" / version / "linux-amd64" / "mesh"
            self.write(path, version.encode(), executable=True)
            builds[version] = {"path": str(path), "digest": prepared.file_digest(path)}
            self.write(self.root / "releases" / version / "mesh-update.json", b"signed metadata")
        for name in ("signer-test", "fixture-ca.pem", "bootstrap-root.json", "trusted-root.json", "provenance.json", "worker-probe"):
            self.write(self.root / name, name.encode(), executable=name in ("signer-test", "worker-probe"))
        self.write(self.output / "fixture-exporter.json", json.dumps({"harnessSource": self.pin}).encode())
        worker = builds[installed_releases.VERSIONS[0]]
        record = {"version": installed_releases.VERSIONS[0], "protocol": 1,
            "binary": str(Path(worker["path"]).relative_to(self.root)),
            "artifact": {"binarySha256": worker["digest"], "platform": {"os": "linux", "arch": "amd64"}}}
        registry = self.root / "fixture-workers.json"
        self.write(registry, json.dumps({"schema": 1, "platform": {"os": "linux", "arch": "amd64"}, "workers": [record]}).encode())
        image = self.root / "public" / "v0.1.1" / "mesh"
        self.write(image, b"actual baseline", executable=True)
        self.write(image.with_name("mesh-release.json"), b"public manifest")
        baseline = {"executable": image, "artifact": {"binarySha256": prepared.file_digest(image)},
                    "manifest": {"compatibility": {"workerWrite": 1}}}
        group = {"index": 0, "members": [record | {"binary": worker["path"], "imageBytes": Path(worker["path"]).stat().st_size}],
                 "imageBytes": image.stat().st_size + Path(worker["path"]).stat().st_size}
        releases = SimpleNamespace(root=self.root, output=self.output, native="linux/amd64", directory=self.root / "releases",
            builds=builds, manifests={}, metadata={}, registry=registry, probe=self.root / "worker-probe", published_registry_sha256="b" * 64)
        return releases, {"daemon": baseline, "helper": baseline}, baseline, [group]

    def write(self, path, data, executable=False):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        path.chmod(0o700 if executable else 0o600)

    def export_origin(self, root, request):
        (root / request["export"]).write_text('{"files":{"tuf/immutable":"bytes"}}')

    def export(self, waypoint=False):
        return prepared.export(self.releases, self.host, self.pair, self.previous, self.groups, self.bundle, waypoint=waypoint)

    def test_export_is_allowlisted_and_relocatable_without_rebuild_or_live_state(self):
        self.write(self.root / "state" / "installation.json", b"live state must stay")
        self.write(self.root / "issuer.key", b"signing authority must stay")
        self.write(self.root / "build-cache" / "cached", b"cache must stay")
        identity = self.export()
        descriptor, actual = prepared.read_descriptor(self.bundle)
        self.assertEqual(identity, actual)
        self.assertFalse(any(name.startswith(("state/", "build-cache/")) or name == "issuer.key" for name in descriptor["files"]))
        relocated = self.base / "relocated"
        self.bundle.rename(relocated)
        root = self.base / "consumer"
        root.mkdir()
        with patch.object(installed_releases.Releases, "build", side_effect=AssertionError("consumer built")), \
                patch.object(installed_releases.Releases, "prove", side_effect=AssertionError("consumer ran native preparation")):
            releases, value, loaded, selected, pair, previous, groups = prepared.load(
                root, self.output, "linux/amd64", relocated, selection(), None)
        self.assertEqual(loaded, identity)
        self.assertEqual([release_worker_groups.public_group(group) for group in groups], descriptor["groups"])
        self.assertTrue(Path(releases.builds[installed_releases.VERSIONS[0]]["path"]).is_relative_to(root))
        self.assertEqual(previous["executable"].read_bytes(), b"actual baseline")
        self.assertEqual(selected, selection())

    def test_changed_asset_source_or_partition_rejects_before_server(self):
        self.export()
        value, _ = prepared.read_descriptor(self.bundle)
        (self.bundle / value["builds"][installed_releases.VERSIONS[0]]["path"]).write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "asset changed"):
            prepared.validate_files(self.bundle, value)
        with patch.object(prepared, "source_pin", return_value="c" * 40), self.assertRaisesRegex(ValueError, "source or platform"):
            prepared.load(self.base / "not-created", self.output, "linux/amd64", self.bundle, selection(), None)

    def test_waypoint_negative_packets_are_signed_only_during_preparation(self):
        negative = {"pre-signed": "negative reader packet"}
        with patch.object(installed_waypoint, "prepare_negative_packet", return_value=negative) as signing:
            self.export(waypoint=True)
        signing.assert_called_once_with(self.releases, self.groups[0])
        value, _ = prepared.read_descriptor(self.bundle)
        replay = SimpleNamespace(root=self.bundle, reader_packets=value["readerPackets"])
        with patch.object(installed_waypoint, "prepare_negative_packet", side_effect=AssertionError("consumer signed")):
            self.assertEqual(installed_waypoint.negative_packet(replay, self.groups[0]), negative)

    def test_preparer_is_a_validated_reusable_bundle_tool(self):
        helper = self.base / "native-helper"
        self.write(helper, b"once-built native fixture", executable=True)
        prepared.export(self.releases, self.host, self.pair, self.previous, self.groups, self.bundle, preparer=helper)
        value, _ = prepared.read_descriptor(self.bundle)
        prepared.validate_files(self.bundle, value)
        self.assertEqual(prepared.resolve_preparer(self.bundle, value, None, selection()).read_bytes(), helper.read_bytes())
        self.assertEqual(prepared.resolve_preparer(self.bundle, value, helper, selection()), helper)
        helper.write_bytes(b"different source tool")
        with self.assertRaisesRegex(ValueError, "differs"):
            prepared.resolve_preparer(self.bundle, value, helper, selection())
        with self.assertRaisesRegex(ValueError, "requires"):
            prepared.resolve_preparer(self.bundle, value | {"preparer": None}, None, selection())

    def test_clock_identity_is_actual_manifest_hash_and_selection_is_canonical(self):
        identity = self.export()
        self.assertEqual(prepared.clock_workload(self.bundle, selection()),
                         {"preparedRole": "consume", "preparedId": identity, "installedSlice": selection()})

    def test_consumer_clock_cannot_change_selection_registry_or_source_context(self):
        self.export()
        value, _ = prepared.read_descriptor(self.bundle)
        workload = {"preparedRole": "consume", "installedSlice": selection(), "platform": value["native"],
                    "host": self.host["name"], "mode": value["mode"], "publishedRegistrySha256": value["publishedRegistrySha256"]}
        prepared.validate_clock_workload(SimpleNamespace(plan=SimpleNamespace(workload=workload)), value, selection())
        for change in ({"mode": "waypoint"}, {"installedSlice": selection("chain-final")}, {"publishedRegistrySha256": "wrong"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                prepared.validate_clock_workload(SimpleNamespace(plan=SimpleNamespace(workload=workload | change)), value, selection())

    def test_prepared_server_uses_remaining_original_command_without_build(self):
        plan = SimpleNamespace(validate=Mock())
        clock = SimpleNamespace(plan=plan, deadline_ns=42000100)
        consumer_root = self.base / "consumer"
        consumer_root.mkdir()
        with patch.object(prepared.installed_clock, "current", return_value=clock):
            releases = prepared.PreparedReleases(consumer_root, "linux/amd64", self.output, clock_plan=plan)
        self.addCleanup(lambda: releases.signer_log.close() if releases.signer_log else None)
        with patch.object(installed_releases.time, "monotonic_ns", return_value=100), \
                patch.object(installed_releases.subprocess, "Popen") as launch, \
                patch.object(installed_releases.native_services, "wait_for"), \
                patch.object(releases, "prepare_signer", side_effect=AssertionError("consumer built")):
            releases.start_prepared_server()
        self.assertEqual(launch.call_args.args[0][-2:], ["^TestInstalledPreparedServe$", "-test.timeout=42000000ns"])
        self.assertEqual(launch.call_args.kwargs["env"]["MESH_INSTALLED_PREPARED_DIR"], str(consumer_root))
        self.assertIs(releases.clock, clock)
        self.assertEqual(releases.server_kind, "prepared-origin")

    def test_selected_routing_runs_whole_existing_groups_and_original_readers(self):
        chosen = selection("chain-final", readers=(0,))
        with patch.object(installed_waypoint, "verify_readers") as readers, patch.object(prepared.installed_chain, "run_groups") as groups:
            prepared.run_selected(self.root, self.output, "helper", self.releases, "origin", self.host,
                                  self.pair, self.previous, self.groups, chosen)
        readers.assert_called_once_with(self.releases, self.groups[0])
        groups.assert_called_once_with(self.root, self.output, self.host, self.pair, "helper", "origin",
                                       self.releases, self.previous, "final", self.groups)

    def test_actual_partition_drift_rejects(self):
        self.export()
        value, _ = prepared.read_descriptor(self.bundle)
        value["groups"][0]["imageBytes"] += 1
        installed_releases.write_json(self.bundle / "prepared.json", value)
        root = self.base / "consumer"
        root.mkdir()
        with self.assertRaisesRegex(ValueError, "partition differs"):
            prepared.load(root, self.output, "linux/amd64", self.bundle, selection(), None)

    def aggregate_inputs(self, *, diagnostic=False, waypoint=False):
        self.export(waypoint=waypoint)
        descriptor, identity = prepared.read_descriptor(self.bundle)
        common = {"platform": descriptor["native"], "host": self.host["name"], "mode": descriptor["mode"]}
        clock = {"mode": "diagnostic" if diagnostic else "frozen", "accepted": not diagnostic,
                 "measurement_completed": True, "failures": [], "provenance": {"preparedId": identity}}
        producer = self.base / "preparation-output"
        producer.mkdir()
        installed_releases.write_json(producer / "prepared-export.json", {"preparedId": identity,
            "harnessSource": self.pin, "groups": descriptor["groups"]})
        installed_releases.write_json(producer / "fixture-clock.json", clock | {"workload": common | {"preparedRole": "prepare"}})
        outputs = []
        for profile in slices.PROFILES:
            output = self.base / profile
            output.mkdir()
            self.write_aggregate_output(output, identity, common, clock, selection(profile, readers=(0,) if waypoint and profile == "chain-none" else ()))
            outputs.append(output)
        return producer, outputs

    def write_aggregate_output(self, output, identity, common, clock, selected):
        installed_releases.write_json(output / "prepared-slice.json", {"preparedId": identity,
            "harnessSource": self.pin, "selection": selected, "completed": True})
        installed_releases.write_json(output / "fixture-clock.json", clock | {
            "workload": common | {"preparedRole": "consume", "installedSlice": selected}})
        if selected["readers"]:
            installed_releases.write_json(output / "original-format-readers-group-0.json",
                                          {"oldRejectedManifestSchema2": "manifest schema 2 is unsupported"})

    def observed(self, directory, *_args):
        return {"observations": [{"original": {"stateRun": str(directory)}}]}

    def test_aggregate_revalidates_groups_and_exact_coverage(self):
        producer, outputs = self.aggregate_inputs()
        with patch.object(prepared.installed_adoption, "validate_installed_group", side_effect=self.observed) as checked:
            result = slices.aggregate(self.bundle, outputs, producer)
            self.assertEqual(checked.call_count, 3)
            self.assertTrue(result["accepted"])
            self.assertEqual(result["kind"], "installed-acceptance")
            with self.assertRaisesRegex(ValueError, "omit or duplicate"):
                slices.aggregate(self.bundle, outputs[:-1], producer)
            with self.assertRaisesRegex(ValueError, "reused"):
                slices.aggregate(self.bundle, outputs + [outputs[0]], producer)

    def test_diagnostic_and_different_bundle_never_become_acceptance(self):
        producer, outputs = self.aggregate_inputs(diagnostic=True)
        with patch.object(prepared.installed_adoption, "validate_installed_group", side_effect=self.observed):
            result = slices.aggregate(self.bundle, outputs, producer)
            self.assertTrue(result["measurement_completed"])
            self.assertFalse(result["accepted"])
            self.assertEqual(result["kind"], "diagnostic-installed-measurement")
            path = outputs[0] / "prepared-slice.json"
            report = json.loads(path.read_bytes())
            installed_releases.write_json(path, report | {"preparedId": "d" * 64})
            with self.assertRaisesRegex(ValueError, "identity"):
                slices.aggregate(self.bundle, outputs, producer)

    def test_incomplete_cleanup_and_missing_reader_cannot_aggregate(self):
        with patch.object(installed_waypoint, "prepare_negative_packet", return_value={}):
            producer, outputs = self.aggregate_inputs(waypoint=True)
        with patch.object(prepared.installed_adoption, "validate_installed_group", side_effect=self.observed):
            self.assertTrue(slices.aggregate(self.bundle, outputs, producer)["accepted"])
            (outputs[0] / "original-format-readers-group-0.json").unlink()
            with self.assertRaises(FileNotFoundError):
                slices.aggregate(self.bundle, outputs, producer)
            path = producer / "fixture-clock.json"
            value = json.loads(path.read_bytes())
            installed_releases.write_json(path, value | {"measurement_completed": False})
            with self.assertRaisesRegex(ValueError, "cleanup"):
                slices.aggregate(self.bundle, outputs, producer)


class StructureTest(unittest.TestCase):
    def test_new_code_nesting_is_at_most_three(self):
        for module in (prepared, slices):
            tree = ast.parse(Path(module.__file__).read_text())
            self.assertLessEqual(depth(tree), 3, module.__file__)


def depth(node, current=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        current = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        current += 1
    return max([current] + [depth(child, current) for child in ast.iter_child_nodes(node)])


if __name__ == "__main__":
    unittest.main()
