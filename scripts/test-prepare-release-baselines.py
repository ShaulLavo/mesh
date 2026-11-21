#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
import sys
from unittest import mock

SPEC = importlib.util.spec_from_file_location("baselines", Path(__file__).with_name("prepare-release-baselines.py"))
BASELINES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BASELINES)
RUN_SPEC = importlib.util.spec_from_file_location("proof_runner", Path(__file__).with_name("run-update-proofs.py"))
RUNNER = importlib.util.module_from_spec(RUN_SPEC)
RUN_SPEC.loader.exec_module(RUNNER)
COLLECT_SPEC = importlib.util.spec_from_file_location("collect_proofs", Path(__file__).with_name("collect-update-proofs.py"))
COLLECT = importlib.util.module_from_spec(COLLECT_SPEC)
COLLECT_SPEC.loader.exec_module(COLLECT)


class BaselineTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mb-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.output = self.root / "output"
        self.cache = self.root / "cache"
        self.output.mkdir()
        self.cache.mkdir()
        self.calls = 0
        self.failures = 0

    def download(self, command, **_):
        self.calls += 1
        if self.calls <= self.failures:
            return subprocess.CompletedProcess(command, 1)
        name = command[command.index("--pattern") + 1]
        destination = Path(command[command.index("--dir") + 1]) / name
        destination.write_bytes(self.archive)
        return subprocess.CompletedProcess(command, 0)

    def fixture(self, name="mesh", data=b"original published executable"):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as package:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            package.addfile(info, io.BytesIO(data))
        self.archive = stream.getvalue()
        return hashlib.sha256(self.archive).hexdigest(), hashlib.sha256(data).hexdigest()

    def acquire(self, digest):
        with mock.patch.object(BASELINES.subprocess, "run", side_effect=self.download), mock.patch.object(BASELINES.time, "sleep"):
            return BASELINES.acquire("v0.1.249", "mesh_linux_amd64.tar.gz", digest, self.output, self.cache)

    def test_two_acquisition_failures_then_success_and_real_cache_hit(self):
        archive, binary = self.fixture()
        self.failures = 2
        path = self.acquire(archive)
        self.assertEqual(self.calls, 3)
        BASELINES.unpack(path, binary, self.output / "mesh")
        self.assertEqual((self.output / "mesh").read_bytes(), b"original published executable")
        self.assertEqual(self.acquire(archive), path)
        self.assertEqual(self.calls, 3)

    def test_bounded_failure_never_drops_a_source(self):
        archive, _ = self.fixture()
        self.failures = 10
        with self.assertRaisesRegex(RuntimeError, "required baseline acquisition failed"):
            self.acquire(archive)
        self.assertEqual(self.calls, 3)
        self.assertFalse((self.cache / archive).exists())

    def test_changed_archive_binary_or_immutable_cache_fails(self):
        archive, binary = self.fixture()
        with self.assertRaisesRegex(ValueError, "digest changed"):
            self.acquire("0" * 64)
        path = self.acquire(archive)
        with self.assertRaisesRegex(ValueError, "executable differs"):
            BASELINES.unpack(path, "0" * 64, self.output / "mesh")
        path.write_bytes(b"changed cached bytes")
        with self.assertRaisesRegex(ValueError, "immutable baseline cache changed"):
            self.acquire(archive)
        self.assertNotEqual(binary, "0" * 64)

    def test_archive_cannot_substitute_a_link(self):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as package:
            info = tarfile.TarInfo("mesh")
            info.type = tarfile.SYMTYPE
            info.linkname = "elsewhere"
            package.addfile(info)
        self.archive = stream.getvalue()
        path = self.acquire(hashlib.sha256(self.archive).hexdigest())
        with self.assertRaisesRegex(ValueError, "regular mesh executable"):
            BASELINES.unpack(path, "0" * 64, self.output / "mesh")

    def test_previous_release_is_required_even_with_frozen_binary(self):
        selection = self.root / "selection.json"
        selection.write_text(json.dumps({"baselines": []}))
        with mock.patch.dict(os.environ, {"MESH_RELEASE_SELECTION": str(selection)}):
            with self.assertRaisesRegex(ValueError, "previous published release"):
                BASELINES.main(["v0.1.250", "v0.1.249", "linux/amd64", "", str(self.output), str(self.root / "list")])

    def test_custom_baseline_uses_exact_frozen_writer_bytes_without_candidate_tag_download(self):
        controls = importlib.import_module("test-prepare-draft-intake")
        fixture = controls.DraftIntakeTests()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        data = b"custom source image; offline control"
        name = "baseline_linux_amd64_" + hashlib.sha256(data).hexdigest() + ".bin"
        fixture.release["frozen_baselines"] = [name]
        fixture.add_asset(fixture.directory, fixture.release, name, data)
        published = self.output / "published/mesh"
        published.parent.mkdir()
        published.write_bytes(b"published baseline; offline control")
        selection = self.root / "selection.json"
        selection.write_text(json.dumps({"baselines": [{"version": "v0.1.309", "artifact": {"platform": {"os": "linux", "arch": "amd64"}}}]}))
        environment = {"MESH_RELEASE_SELECTION": str(selection), "MESH_DRAFT_INTAKE": str(fixture.directory),
                       "MESH_DRAFT_SOURCE": controls.SOURCE, "GITHUB_RUN_ID": "42"}
        with mock.patch.dict(os.environ, environment), mock.patch.object(BASELINES, "prepare_item", return_value=published), \
                mock.patch.object(BASELINES, "prepare_workers"), mock.patch.object(BASELINES, "acquire", side_effect=AssertionError("no draft download")):
            BASELINES.main([controls.VERSION, "v0.1.309", "linux/amd64", name, str(self.output), str(self.root / "list")])
        self.assertEqual((self.output / name).read_bytes(), data)
        self.assertIn(str(self.output / name), (self.root / "list").read_text().splitlines())
        (fixture.directory / name).write_bytes(b"changed")
        with mock.patch.dict(os.environ, environment), self.assertRaisesRegex(ValueError, "exact uploaded bytes"):
            BASELINES.custom_intake(controls.VERSION, name)

    def shard_sources(self):
        tools, retained = self.root / "tools", self.root / "retained"
        tools.mkdir()
        retained.mkdir()
        (tools / "candidate-reader.json").write_text("{}")
        (tools / "preparation.json").write_text(json.dumps({"source": {"commit": "a" * 40, "clean": True}}))
        (retained / "workers.json").write_text("{}")
        selected, paths = [], []
        for index in range(8):
            item = retained / f"release-{index}"
            item.mkdir()
            binary = item / "mesh"
            binary.write_bytes(str(index).encode())
            source = {"version": f"v0.1.{200 + index}", "rollback": index < 3, "artifact": {
                "platform": {"os": "linux", "arch": "amd64"}, "binarySha256": BASELINES.sha256(binary)}}
            (item / "source.json").write_text(json.dumps(source))
            selected.append(source)
            paths.append(str(binary))
        custom = retained / ("baseline_linux_amd64_" + hashlib.sha256(b"custom").hexdigest() + ".bin")
        custom.write_bytes(b"custom")
        paths.append(str(custom))
        listing = self.root / "retained.list"
        listing.write_text("\n".join(paths) + "\n")
        return {"baselines": selected}, tools, custom, listing

    def run_shard(self, tools, listing, shard, count, batch=False):
        output = self.root / f"proof-{shard}"
        args = ["runner", str(tools), str(listing), "linux/amd64", str(output),
                "--shard", str(shard), "--shards", str(count), "--manifest", str(self.root / "manifest.json")]
        def fake_run(_command, _environment, log):
            log.write_text("--- PASS: TestLegacyWorkerProcessProtocolAndVersion\n")
        if batch:
            args.append("--batch")
        with mock.patch.object(sys, "argv", args), mock.patch.object(RUNNER, "run", side_effect=fake_run), \
                mock.patch.dict(os.environ, {"GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2"}):
            RUNNER.main()
        return json.loads((output / "timings.json").read_bytes())

    def test_pinned_custom_full_source_gets_its_own_shard(self):
        selected, tools, custom, listing = self.shard_sources()
        shards = BASELINES.proof_shards(selected, custom.name)
        self.assertEqual(shards, [0, 1, 2, 3])
        results = [self.run_shard(tools, listing, shard, len(shards)) for shard in shards]
        self.assertEqual([sum(row["rollback"] for row in result) for result in results], [1, 1, 1, 1])
        sources = [row["source"] for result in results for row in result]
        self.assertEqual(len(sources), 9)
        self.assertEqual(len(set(sources)), 9)

    def test_custom_matrix_freezes_actual_asset_digest_without_published_identity(self):
        selected, _, custom, _ = self.shard_sources()
        path = self.root / "selection.json"
        path.write_text(json.dumps(selected | {"workers": []}))
        BASELINES.freeze_pins(path, custom.name + ",irrelevant.txt")
        frozen = json.loads(path.read_bytes())
        self.assertEqual(frozen["baselines"], selected["baselines"])
        self.assertEqual(frozen["workers"], [])
        self.assertEqual(frozen["customBaselines"], [{"artifact": {"platform": {"os": "linux", "arch": "amd64"},
            "binarySha256": hashlib.sha256(b"custom").hexdigest()}, "asset": custom.name, "rollback": True}])

    def test_shard_count_handles_platforms_and_duplicate_pins(self):
        selected, _, custom, _ = self.shard_sources()
        for index in range(3):
            selected["baselines"].append({"version": f"v0.1.{200 + index}", "rollback": True,
                "artifact": {"platform": {"os": "darwin", "arch": "arm64"}}})
        pins = [custom.name, custom.name, "irrelevant.txt"]
        pins += ["baseline_darwin_arm64_" + value * 64 + ".bin" for value in "abc"]
        self.assertEqual(BASELINES.proof_shards(selected, ",".join(pins)), list(range(6)))

    def test_runner_rejects_undersized_matrix_before_running_a_proof(self):
        _, tools, _, listing = self.shard_sources()
        args = ["runner", str(tools), str(listing), "linux/amd64", str(self.root / "proof"), "--shards", "3", "--manifest", str(self.root / "manifest.json")]
        with mock.patch.object(sys, "argv", args), mock.patch.object(RUNNER, "run") as run:
            with self.assertRaisesRegex(ValueError, "too few shards"):
                RUNNER.main()
        run.assert_not_called()

    def test_release_limits_batches_but_proves_the_same_sources_and_profiles(self):
        selected, tools, custom, listing = self.shard_sources()
        shards = BASELINES.proof_shards(selected, custom.name)
        matrix = BASELINES.prove_matrix(shards, True)["include"]
        self.assertEqual(sorted((row["platform"], row["shard"]) for row in matrix),
                         sorted((platform, shard) for platform in ("linux/amd64", "linux/arm64", "darwin/arm64") for shard in (0, 1)))
        self.assertEqual(len(BASELINES.prove_matrix([0], True)["include"]), 3)
        original = [row for shard in shards for row in self.run_shard(tools, listing, shard, len(shards))]
        results = [self.run_shard(tools, listing, shard, 2, batch=True) for shard in range(2)]
        self.assertEqual([sum(row["rollback"] for row in result) for result in results], [2, 2])
        self.assertCountEqual([{key: value for key, value in row.items() if key != "seconds"} for row in original],
                              [{key: value for key, value in row.items() if key != "seconds"} for row in results[0] + results[1]])
        record = json.loads((self.root / "proof-1/proof-batch.json").read_bytes())
        self.assertEqual(record, {"attempt": "2", "platform": "linux/amd64", "run": "42", "shard": 1})

    def test_pull_request_keeps_every_linux_shard_and_one_darwin_shard(self):
        matrix = BASELINES.prove_matrix([0, 1, 2, 3], False)["include"]
        rows = [(row["platform"], row["shard"]) for row in matrix]
        self.assertEqual([row for row in rows if row[0] == "darwin/arm64"], [("darwin/arm64", 0)])
        for platform in ("linux/amd64", "linux/arm64"):
            self.assertEqual([shard for name, shard in rows if name == platform], [0, 1, 2, 3])
        self.assertIn({"platform": "linux/arm64", "key": "linux-arm64", "runner": "ubuntu-24.04-arm", "shard": 3}, matrix)

    def test_prove_matrix_cli_rejects_unknown_modes_and_gapped_shards(self):
        script = str(Path(__file__).with_name("prepare-release-baselines.py"))
        result = subprocess.run([sys.executable, script, "--prove-matrix", "[0,1]", "false"], capture_output=True, text=True, timeout=30, check=True)
        self.assertEqual(len(json.loads(result.stdout)["include"]), 5)
        for shards in ([], [1, 2], [0, 0], {"0": 1}):
            with self.assertRaisesRegex(ValueError, "0 through N-1"):
                BASELINES.prove_matrix(shards, True)
        main_mode = subprocess.run([sys.executable, script, "--prove-matrix", "[0]", "yes"], capture_output=True, text=True, timeout=30)
        self.assertNotEqual(main_mode.returncode, 0)


class ReleaseBatchIntakeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="release-batches-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.matrix = BASELINES.prove_matrix([0, 1], True)
        self.manifest = {"version": "v0.1.9000", "commit": "a" * 40, "artifacts": []}
        self.uploads = {}
        for index, row in enumerate(self.matrix["include"], start=1):
            platform = dict(zip(("os", "arch"), row["platform"].split("/")))
            digest = hashlib.sha256(row["platform"].encode()).hexdigest()
            if not row["shard"]:
                self.manifest["artifacts"].append({"platform": platform, "binarySha256": digest})
            build = {"platform": platform, "digest": digest, "modified": False,
                     "version": self.manifest["version"], "commit": self.manifest["commit"]}
            key = f"{row['key']}-{row['shard']}"
            self.uploads[key] = str(index)
            self.batch(key, row["platform"], row["shard"], str(1 + row["shard"]), build)

    def batch(self, key, platform, shard, attempt, build):
        path = self.root / f"update-proofs-{key}-attempt-{attempt}"
        path.mkdir()
        (path / "proof-batch.json").write_text(json.dumps({"run": "42", "attempt": attempt,
            "platform": platform, "shard": shard}))
        (path / "candidate-reader.json").write_text(json.dumps({"build": build}))

    def check(self, attempt="2"):
        COLLECT.check_batches(self.root, self.matrix, "42", attempt, self.manifest)

    def test_exact_attempt_set_accepts_carried_successes_from_same_run(self):
        self.assertEqual(COLLECT.batch_artifact_ids(self.matrix, self.uploads), "1,2,3,4,5,6")
        self.check()
        with self.assertRaisesRegex(ValueError, "wrong run, attempt or platform"):
            self.check(attempt="1")

    def test_missing_extra_and_duplicate_upload_ids_fail(self):
        absent = {key: value for key, value in self.uploads.items() if key != "linux-amd64-0"}
        for uploads in (absent, self.uploads | {"unexpected": "7"}, self.uploads | {"linux-amd64-0": "2"}):
            with self.subTest(uploads=uploads), self.assertRaises(ValueError):
                COLLECT.batch_artifact_ids(self.matrix, uploads)

    def test_wrong_run_platform_or_candidate_cannot_reach_signing(self):
        path = self.root / "update-proofs-linux-amd64-0-attempt-1"
        record = path / "proof-batch.json"
        original = json.loads(record.read_bytes())
        for invalid in ({"run": "43"}, {"platform": "darwin/arm64"}, {"attempt": "2"}):
            record.write_text(json.dumps(original | invalid))
            with self.assertRaisesRegex(ValueError, "wrong run, attempt or platform"):
                self.check()
        record.write_text(json.dumps(original))
        report = path / "candidate-reader.json"
        candidate = json.loads(report.read_bytes())
        candidate["build"]["digest"] = "0" * 64
        report.write_text(json.dumps(candidate))
        with self.assertRaisesRegex(ValueError, "another candidate"):
            self.check()


if __name__ == "__main__":
    unittest.main()
