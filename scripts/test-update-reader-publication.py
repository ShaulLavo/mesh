#!/usr/bin/env python3
import hashlib
import fnmatch
import importlib.util
import json
import re
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(file))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


PROBE = module("reader_probe", "probe-update-reader.py")
STAGE = module("reader_stage", "stage-release-publication.py")
ORIGINAL = module("original_readers", "run-original-update-readers.py")


class ReaderPublicationTests(unittest.TestCase):
    def test_reader_only_changes_require_native_proofs(self):
        workflow = (Path(__file__).resolve().parents[1] / ".github/workflows/ci.yml").read_text()
        section = workflow.split("id: update_paths\n", 1)[1].split("\n  app-pill:", 1)[0]
        patterns = re.findall(r"^\s+- '([^']+)'$", section, re.M)
        self.assertTrue(patterns)
        for path in ("internal/cli/update_reader.go", "internal/cli/command.go", "cmd/mesh/main.go",
                     "internal/update/host_updater.go", "db/migrations/00017_example.sql"):
            with self.subTest(path=path):
                self.assertTrue(any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns), path)

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mrp-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.binary = self.root / "published-binary"
        self.binary.write_bytes(b"immutable published executable bytes")
        self.digest = hashlib.sha256(self.binary.read_bytes()).hexdigest()
        self.value = {"schema": 1, "epoch": 1, "build": {"version": "v0.1.307", "commit": "a" * 40,
                      "digest": self.digest, "platform": {"os": "linux", "arch": "amd64"}, "modified": False}}

    def probe(self):
        result = subprocess.CompletedProcess([], 0, json.dumps(self.value).encode(), b"")
        with mock.patch.object(PROBE.subprocess, "run", return_value=result) as command:
            value = PROBE.probe(self.binary, "v0.1.307", "linux/amd64")
            self.assertEqual(command.call_args.args[0], [str(self.binary), "update-reader", "-capabilities"])
        return value

    def test_capability_protocol_binds_actual_executable_bytes(self):
        self.assertEqual(self.probe(), self.value)
        self.binary.write_bytes(b"other executable")
        with self.assertRaisesRegex(ValueError, "executing release artifact"):
            self.probe()

    def test_false_or_absent_reader_never_grants_capability(self):
        self.value["schema"] = True
        with self.assertRaisesRegex(ValueError, "implemented updater reader"):
            self.probe()
        self.value["schema"] = 1
        self.value["build"]["modified"] = True
        with self.assertRaisesRegex(ValueError, "executing release artifact"):
            self.probe()
        with mock.patch.object(PROBE.subprocess, "run", side_effect=subprocess.CalledProcessError(2, "update-reader")):
            with self.assertRaises(subprocess.CalledProcessError):
                PROBE.probe(self.binary, "v0.1.307", "linux/amd64")

    def test_native_shards_cannot_substitute_another_candidate(self):
        for name in ("source-a", "source-b"):
            directory = self.root / name
            directory.mkdir()
            (directory / "candidate-reader.json").write_text(json.dumps(self.value))
        self.assertEqual(STAGE.native_readers(self.root), [self.value])
        changed = json.loads(json.dumps(self.value))
        changed["build"]["digest"] = "b" * 64
        (self.root / "source-b/candidate-reader.json").write_text(json.dumps(changed))
        with self.assertRaisesRegex(ValueError, "shards disagree"):
            STAGE.native_readers(self.root)

    def test_changed_packet_fails_before_original_binary_acquisition(self):
        path = self.root / ("a" * 64 + ".reader.json")
        path.write_bytes(b"changed packet")
        args = mock.Mock(packets=self.root)
        item = {"packet": path.name, "sha256": "a" * 64}
        with mock.patch.object(ORIGINAL.BASELINES, "acquire") as acquire:
            with self.assertRaisesRegex(ValueError, "packet bytes changed"):
                ORIGINAL.verify_invocation(item, args, self.root, {})
            acquire.assert_not_called()


if __name__ == "__main__":
    unittest.main()
