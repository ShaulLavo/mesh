#!/usr/bin/env python3
"""Controlled group, asset-closure and signed-context regressions; no native launch."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from fixtures import noindex_paths, release_worker_groups as groups, release_worker_index as index
from fixtures import release_worker_completion as completion, installed_paths

PLATFORM = {"os": "linux", "arch": "amd64"}
SOURCE = "a" * 40


def selected(count, platform=PLATFORM):
    return [{"version": "v0.1." + str(number + 1), "protocol": 1, "commit": "a" * 40,
             "artifact": {"platform": platform, "binarySha256": format(number + 1, "064x"),
                          "archive": "mesh.tar.gz", "sha256": "b" * 64}, "imageBytes": 5}
            for number in range(count)]


class Bundle:
    def __init__(self, root, workers, profile="full", platform=PLATFORM, registry_digest="b" * 64):
        self.root, self.workers = root, workers
        self.context = {"harnessSource": SOURCE, "preparationSource": SOURCE, "registrySha256": registry_digest,
                        "sourceDigest": workers[-1]["artifact"]["binarySha256"], "candidateDigest": "c" * 64,
                        "platform": platform, "profile": profile, "firstState": 1, "lastState": 2,
                        "sourceVersion": workers[-1]["version"], "sourceProtocol": 1, "candidateProtocol": 2,
                        "sourceImageBytes": 5, "startupDigest": "d" * 64, "rollbackManifestSha256": "e" * 64}
        self.forward = groups.encode({"schema": 1, "platform": platform, "fromDigest": self.context["sourceDigest"],
            "toDigest": "c" * 64, "stateReadMin": 1, "stateReadMax": 2, "stateWrite": 2,
            "workerMin": 1, "workerMax": 2, "workerWrite": 2, "journalVersion": 1,
            "candidateOpenedRetainedState": True, "sessionsPreserved": True, "recoveryRecordsPreserved": True})
        entries = [self.entry(group) for group in groups.partition(workers, 5)]
        self.value = {"schema": 1, "context": self.context, "originalBytes": 5, "groups": entries}

    def entry(self, group):
        number = group["index"]
        rows = [{"session": "W" + str(position), "pid": 1000 + number * 100 + position, "shellPid": 2000 + number * 100 + position,
                 "digest": item["artifact"]["binarySha256"], "protocol": item["protocol"], "version": item["version"]}
                for position, item in enumerate(group["members"])]
        original = {"stateRun": "state-" + str(number), "session": "ORIG", "pid": 9000 + number, "shellPid": 9500 + number,
                    "digest": self.context["sourceDigest"], "protocol": 1}
        observations = [{"label": label, "workers": len(rows), "processes": 2 * len(rows), "identities": rows,
                         "original": original} for label in groups.labels(self.context["profile"], 1, 2)]
        entry = {"group": groups.public_group(group), "stateRun": original["stateRun"],
            "observations": groups.leaf(self.root, {"workers": rows, "observations": observations}, "observations"),
            "cleanup": groups.leaf(self.root, {"stateRun": original["stateRun"], "processesAbsent": True, "stateAbsent": True}, "cleanup"),
            "forward": groups.leaf(self.root, {"bytes": self.forward.decode(), "sha256": groups.digest(self.forward)}, "forward"), "rollback": None}
        if self.context["profile"] == "full":
            entry["rollback"] = groups.leaf(self.root, self.rollback(group), "rollback")
            entry["startup"] = groups.leaf(self.root, self.startup(), "startup")
        return entry

    def rollback(self, group):
        return {"schema": 1, "platform": self.context["platform"], "fromDigest": self.context["sourceDigest"],
            "toDigest": "c" * 64, "forward": groups.digest(self.forward),
            "workers": sorted({self.context["sourceDigest"]} | {row["artifact"]["binarySha256"] for row in group["members"]}),
            "boundaries": [{"state": state, "candidateWrote": True, "sourceStarted": True, "sessionIO": True, "recoveryPreserved": True} for state in (1, 2)]}

    def startup(self):
        target = {"version": "v0.1.99", "commit": SOURCE, "platform": self.context["platform"], "stateVersion": 2, "workerProtocol": 2, "digest": "c" * 64}
        value = {"source": {"digest": self.context["sourceDigest"]}, "release": target, "failedImage": target | {"digest": "d" * 64},
                 "startupCompanion": True, "manifestSha256": "e" * 64}
        return {label.removeprefix("rollback-"): value for label in groups.labels("full", 1, 2) if label.startswith("rollback-startup-")}

    def reference(self):
        raw = groups.encode(self.value)
        name = groups.digest(raw) + ".worker-groups.json"
        (self.root / name).write_bytes(raw)
        return {"name": name, "sha256": groups.digest(raw), "size": len(raw)}

    def validate(self):
        return groups.validate_group_assets(self.context, self.workers, self.reference(), groups.file_resolver(self.root))

    def mutate_leaf(self, field, action, position=0):
        entry = self.value["groups"][position]
        value = groups.typed_leaf(entry[field], field, groups.file_resolver(self.root))
        action(value)
        entry[field] = groups.leaf(self.root, value, field)


class WorkerGroupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="worker-groups-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def test_31_and_32_preserve_every_equal_protocol_member(self):
        for count, expected in ((31, [31]), (32, [31, 1]), (63, [31, 31, 1])):
            workers = selected(count)
            result = groups.partition(workers, 5)
            self.assertEqual([len(group["members"]) for group in result], expected)
            self.assertEqual([item for group in result for item in group["members"]], workers)

    def test_byte_cap_includes_original_and_splits_consecutively(self):
        workers = [item | {"imageBytes": 128 << 20} for item in selected(8)]
        result = groups.partition(workers, 128 << 20)
        self.assertEqual([len(group["members"]) for group in result], [7, 1])
        self.assertEqual(result[0]["imageBytes"], 1 << 30)
        with self.assertRaises(ValueError):
            groups.partition([workers[0] | {"imageBytes": (128 << 20) + 1}], 1)

    def test_frozen_custom_source_retains_distinct_full_hash(self):
        image = self.root / "source"
        image.write_bytes(b"source")
        worker = selected(1)[0] | {"binary": str(image)}
        custom = {"version": "v0.1.2", "protocol": 1, "binary": str(image),
                  "artifact": {"platform": PLATFORM, "binarySha256": groups.digest(image.read_bytes())}}
        with mock.patch.object(groups.release_workers, "applicable", return_value=[worker, custom]):
            frozen = groups.freeze("registry", "v0.1.2", image)
        self.assertEqual([groups.member(item) for item in frozen], [groups.member(worker), groups.member(custom)])
        self.assertEqual([item["imageBytes"] for item in frozen], [6, 6])
        self.assertNotIn("commit", frozen[-1])

    def test_full_group_union_keeps_actual_forward_bytes(self):
        bundle = Bundle(self.root, selected(32))
        verified = bundle.validate()
        self.assertEqual(verified["forward"], bundle.forward)
        self.assertEqual(verified["rollback"]["workers"], sorted(item["artifact"]["binarySha256"] for item in bundle.workers))
        self.assertEqual(verified["rollback"]["boundaries"], bundle.rollback(groups.partition(bundle.workers, 5)[0])["boundaries"])

    def test_forward_only_requires_both_original_boundaries(self):
        bundle = Bundle(self.root, selected(1), "forward-only")
        self.assertIsNone(bundle.validate()["rollback"])
        bundle.mutate_leaf("observations", lambda value: value["observations"].pop())
        with self.assertRaisesRegex(ValueError, "labels"):
            bundle.validate()

    def test_missing_member_group_or_boundary_never_passes(self):
        for corruption in ("member", "group", "boundary", "startup"):
            bundle = Bundle(self.root, selected(32))
            self.corrupt(bundle, corruption)
            with self.subTest(corruption=corruption), self.assertRaises(ValueError):
                bundle.validate()

    def corrupt(self, bundle, corruption):
        if corruption == "member":
            bundle.value["groups"][0]["group"]["members"].pop()
        if corruption == "group":
            bundle.value["groups"].pop()
        if corruption == "boundary":
            bundle.mutate_leaf("observations", lambda value: value["observations"].pop(2))
        if corruption == "startup":
            bundle.mutate_leaf("startup", lambda value: value.pop(next(iter(value))))

    def test_changed_or_cross_group_pid_identity_rejected(self):
        bundle = Bundle(self.root, selected(32))
        bundle.mutate_leaf("observations", lambda value: value["observations"][0]["identities"][0].update(pid=1))
        with self.assertRaisesRegex(ValueError, "identity changed"):
            bundle.validate()

    def test_reused_state_identity_rejected(self):
        bundle = Bundle(self.root, selected(32))
        bundle.value["groups"][1]["stateRun"] = bundle.value["groups"][0]["stateRun"]
        with self.assertRaises(ValueError):
            bundle.validate()

    def test_cleanup_refusal_and_failed_startup_rejected(self):
        bundle = Bundle(self.root, selected(1))
        bundle.mutate_leaf("cleanup", lambda value: value.update(processesAbsent=False))
        with self.assertRaisesRegex(ValueError, "cleanup"):
            bundle.validate()
        bundle = Bundle(self.root, selected(1))
        bundle.mutate_leaf("observations", lambda value: value["workers"][0].update(startupFailure={"errno": "EPERM"}))
        with self.assertRaisesRegex(ValueError, "identities"):
            bundle.validate()

    def test_missing_swapped_and_traversing_assets_rejected(self):
        bundle = Bundle(self.root, selected(1))
        ref = bundle.value["groups"][0]["observations"]
        (self.root / ref["name"]).write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "changed or swapped"):
            bundle.validate()
        bundle = Bundle(self.root, selected(1))
        bundle.value["groups"][0]["observations"]["name"] = "../outside.worker-group.json"
        with self.assertRaisesRegex(ValueError, "flat"):
            bundle.validate()

    def test_wrong_context_or_profile_rejected(self):
        bundle = Bundle(self.root, selected(1))
        reference = bundle.reference()
        for field in ("candidateDigest", "sourceDigest", "profile", "registrySha256", "harnessSource", "preparationSource"):
            context = bundle.context | {field: "other"}
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "context"):
                groups.validate_group_assets(context, bundle.workers, reference, groups.file_resolver(self.root))

    def test_failed_group_starts_no_later_group_and_publishes_no_proof(self):
        image = self.root / "source"
        image.write_bytes(b"image")
        context = {"sourceDigest": "a" * 64}
        build = groups.encode({"version": "v0.1.32"})
        with mock.patch.dict(os.environ, MESH_WORKER_REGISTRY="registry"), mock.patch.object(groups, "context", return_value=context), mock.patch.object(groups, "freeze", return_value=selected(32)), mock.patch.object(groups.subprocess, "check_output", return_value=build), mock.patch.object(groups, "run_group", side_effect=RuntimeError("cleanup unconfirmed")) as run:
            with self.assertRaisesRegex(RuntimeError, "cleanup unconfirmed"):
                groups.run(image, image, "linux/amd64", self.root / "output")
        self.assertEqual(run.call_count, 1)
        self.assertEqual(list((self.root / "output").glob("*.json")), [])

    def test_direct_darwin_root_keeps_physical_socket_bound(self):
        root = Path("/private/tmp/mg-123456.noindex")
        self.assertEqual(installed_paths.validate(root, "Darwin")["longestSocketBytes"], 97)
        with self.assertRaisesRegex(ValueError, "109"):
            installed_paths.validate(root / "mi-abcdefgh", "Darwin")
        owned = self.root / "owned.noindex"
        owned.mkdir()
        self.assertEqual(noindex_paths.require(owned, "Darwin")["ownedRoot"], str(owned))
        with self.assertRaisesRegex(ValueError, "noindex"):
            noindex_paths.require(self.root, "Darwin")

    def test_unavailable_published_updater_has_no_invented_route(self):
        with mock.patch.object(completion.fleet_baselines, "load", return_value={"firstPublishedUpdater": None}):
            completion.validate_routes(self.root, {"firstPublishedUpdater": None, "routes": []}, {})
            with self.assertRaisesRegex(ValueError, "invented"):
                completion.validate_routes(self.root, {"firstPublishedUpdater": None, "routes": [{}]}, {})


class IndexTests(unittest.TestCase):
    setUp = WorkerGroupTests.setUp

    def build_index(self):
        proofs = self.root / "proofs"
        leaves = proofs / "groups" / "leaves"
        leaves.mkdir(parents=True)
        selection = {"workers": [], "baselines": []}
        references, registries, transitions, artifacts = [], {}, [], []
        for platform in index.PLATFORMS:
            workers = selected(1, platform)
            selection["workers"].extend(groups.member(item) for item in workers)
            selection["baselines"].append({"version": workers[0]["version"], "artifact": workers[0]["artifact"], "rollback": True})
            registry = groups.encode({"schema": 1, "platform": platform, "workers": workers})
            registries[json.dumps(platform, sort_keys=True)] = {"bytes": registry.decode(), "sha256": groups.digest(registry)}
            bundle = Bundle(leaves, workers, platform=platform, registry_digest=groups.digest(registry))
            ref = bundle.reference()
            (leaves / ref["name"]).rename(proofs / ref["name"])
            references.append(ref)
            verified = groups.validate_group_assets(bundle.context, workers, ref, index.resolver(proofs))
            (proofs / (groups.digest(bundle.forward) + ".json")).write_bytes(bundle.forward)
            rollback = groups.encode(verified["rollback"])
            (proofs / "rollback").mkdir(exist_ok=True)
            (proofs / "rollback" / (groups.digest(rollback) + ".rollback.json")).write_bytes(rollback)
            transitions.append({"platform": platform, "fromDigest": bundle.context["sourceDigest"], "toDigest": "c" * 64, "proof": groups.digest(bundle.forward)})
            artifacts.append({"platform": platform, "binarySha256": "c" * 64})
        dist = self.root / "dist"
        dist.mkdir()
        manifest = {"version": "v0.1.99", "commit": SOURCE, "artifacts": artifacts, "compatibility": {"transitions": transitions}}
        (dist / "mesh-release.json").write_bytes(groups.encode(manifest))
        frozen = {"selection": selection, "sources": references, "registries": registries}
        index.stage(dist, proofs, frozen, SOURCE)
        return dist, proofs, manifest

    def test_index_requires_exact_native_matrix_and_asset_closure(self):
        dist, proofs, manifest = self.build_index()
        value = index.validate_index(dist / "mesh-worker-groups.json", manifest, proofs)
        self.assertEqual(len(value["sources"]), 3)
        value["sources"].pop()
        (dist / "mesh-worker-groups.json").write_bytes(groups.encode(value))
        with self.assertRaisesRegex(ValueError, "matrix is incomplete"):
            index.validate_index(dist / "mesh-worker-groups.json", manifest, proofs)

    def test_signed_but_wrong_context_is_not_coverage(self):
        dist, proofs, manifest = self.build_index()
        path = dist / "mesh-worker-groups.json"
        value = json.loads(path.read_bytes())
        value["context"]["version"] = "v0.1.98"
        path.write_bytes(groups.encode(value))
        with self.assertRaisesRegex(ValueError, "wrong release"):
            index.validate_index(path, manifest, proofs)
        path.unlink()
        with self.assertRaisesRegex(ValueError, "missing"):
            index.validate_index(path, manifest, proofs)

    def test_flat_reacquisition_requires_leaf_bytes(self):
        dist, proofs, manifest = self.build_index()
        value = json.loads((dist / "mesh-worker-groups.json").read_bytes())
        resolve = index.resolver(proofs)
        for name in index.closure(value, resolve):
            (dist / name).write_bytes(resolve(name))
        for source in list(proofs.glob("*.json")) + list((proofs / "rollback").glob("*.json")):
            (dist / source.name).write_bytes(source.read_bytes())
        index.validate_index(dist / "mesh-worker-groups.json", manifest)
        missing = next(dist.glob("*.worker-group.json"))
        missing.unlink()
        with self.assertRaisesRegex(ValueError, "missing"):
            index.validate_index(dist / "mesh-worker-groups.json", manifest)

    def test_registries_and_frozen_members_cannot_be_replaced(self):
        dist, proofs, manifest = self.build_index()
        value = json.loads((dist / "mesh-worker-groups.json").read_bytes())
        value["selection"]["workers"][0]["artifact"]["binarySha256"] = "f" * 64
        (dist / "mesh-worker-groups.json").write_bytes(groups.encode(value))
        with self.assertRaisesRegex(ValueError, "authoritative"):
            index.validate_index(dist / "mesh-worker-groups.json", manifest, proofs)

    def test_installed_completion_requires_every_platform_and_bound_case(self):
        inventory = {"firstPublishedUpdater": None, "hosts": [{"name": host, "platform": native} for host, native in
                     (("pc", "linux/amd64"), ("pi", "linux/arm64"), ("mac", "darwin/arm64"))]}
        with mock.patch.object(completion.fleet_baselines, "load", return_value=inventory):
            for native in ("linux/amd64", "linux/arm64", "darwin/arm64"):
                output = self.root / native.replace("/", "-")
                output.mkdir()
                refs = []
                for name in completion.expected_pairs(native):
                    path = output / name
                    path.write_bytes(groups.encode({"passed": True, "cleanup": {"servicesAbsent": True, "processesAbsent": True}}))
                    refs.append(completion.asset(output, path))
                value = {"schema": 1, "version": "v0.1.99", "releaseSource": SOURCE, "platform": native,
                         "signedIndexSha256": "b" * 64, "firstPublishedUpdater": None, "installedHarness": SOURCE, "cases": refs, "routes": []}
                (output / "worker-completion.json").write_bytes(groups.encode(value))
            completion.validate(self.root, "v0.1.99", SOURCE, "b" * 64, SOURCE, {})
            with self.assertRaisesRegex(ValueError, "wrong index"):
                completion.validate(self.root, "v0.1.99", SOURCE, "c" * 64, SOURCE, {})
            (self.root / "darwin-arm64/worker-completion.json").unlink()
            with self.assertRaisesRegex(ValueError, "missing"):
                completion.validate(self.root, "v0.1.99", SOURCE, "b" * 64, SOURCE, {})


if __name__ == "__main__":
    unittest.main()
