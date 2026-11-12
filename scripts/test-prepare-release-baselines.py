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

    def shard_sources(self):
        tools, retained = self.root / "tools", self.root / "retained"
        tools.mkdir()
        retained.mkdir()
        (tools / "candidate-reader.json").write_text("{}")
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

    def run_shard(self, tools, listing, shard, count):
        output = self.root / f"proof-{shard}"
        args = ["runner", str(tools), str(listing), "linux/amd64", str(output),
                "--shard", str(shard), "--shards", str(count), "--manifest", str(self.root / "manifest.json")]
        def fake_run(_command, _environment, log):
            log.write_text("--- PASS: TestLegacyWorkerProcessProtocolAndVersion\n")
        with mock.patch.object(sys, "argv", args), mock.patch.object(RUNNER, "run", side_effect=fake_run):
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


if __name__ == "__main__":
    unittest.main()
