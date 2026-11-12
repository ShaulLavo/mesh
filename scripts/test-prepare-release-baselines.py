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
from unittest import mock

SPEC = importlib.util.spec_from_file_location("baselines", Path(__file__).with_name("prepare-release-baselines.py"))
BASELINES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BASELINES)


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


if __name__ == "__main__":
    unittest.main()
