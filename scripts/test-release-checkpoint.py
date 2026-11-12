#!/usr/bin/env python3
"""Check the integration's measured manifest contract before native builds."""
import hashlib
import importlib.util
import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts/fixtures"))
SPEC = importlib.util.spec_from_file_location("checkpoint", ROOT / "scripts/fixtures/release_checkpoint.py")
FIXTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURE)


class CandidateContextTests(unittest.TestCase):
    def inputs(self, root):
        for target in FIXTURE.PLATFORMS:
            directory = root / target.replace("/", "-")
            directory.mkdir()
            (directory / "mesh").write_bytes(b"measured fixture image " + target.encode())
        source = root / "source"
        source.write_bytes(b"measured fixture source")
        native = FIXTURE.native_platform()
        goos, goarch = native.split("/")
        build = {"version": "v0.1.9001", "commit": "a" * 40, "modified": False,
                 "platform": {"os": goos, "arch": goarch}, "stateVersion": 18, "workerProtocol": 1,
                 "digest": FIXTURE.digest(root / native.replace("/", "-") / "mesh")}
        baseline = dict(build, version="v0.1.9000", digest=FIXTURE.digest(source), stateVersion=16)
        return source, build, baseline

    def output(self, build, baseline, arguments, **_):
        if arguments[0] != "go":
            return json.dumps(baseline if arguments[0].endswith("/source") else build).encode()
        goos, goarch = Path(arguments[-1]).parent.name.split("-")
        return ("fixture\n\tbuild\tGOOS=" + goos + "\n\tbuild\tGOARCH=" + goarch
                + "\n\tbuild\tvcs.revision=" + build["commit"] + "\n\tbuild\tvcs.modified=false"
                + "\n\tbuild\t-ldflags=-X github.com/shaul/mesh/internal/release.Version=" + build["version"] + "\n")

    def run_context(self, root, source, build, baseline):
        with mock.patch.object(FIXTURE.subprocess, "check_output", side_effect=lambda args, **kwargs:
                               self.output(build, baseline, args, **kwargs)), mock.patch("builtins.print"):
            FIXTURE.context(ROOT, root, source)

    def test_manifest_binds_every_actual_archive_and_source_range(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source, build, baseline = self.inputs(root)
            self.run_context(root, source, build, baseline)
            manifest = json.loads((root / "manifest.json").read_bytes())
            self.assertEqual(manifest["compatibility"]["stateReadMin"], 16)
            self.assertEqual(manifest["compatibility"]["stateWrite"], 18)
            self.assertEqual(manifest["compatibility"]["transitions"], [])
            for artifact in manifest["artifacts"]:
                archive = root / (artifact["platform"]["os"] + "-" + artifact["platform"]["arch"]) / artifact["archive"]
                self.check_archive(archive, artifact)

    def check_archive(self, archive, artifact):
        self.assertEqual(FIXTURE.digest(archive), artifact["sha256"])
        with tarfile.open(archive) as package:
            image = package.extractfile("mesh").read()
        self.assertEqual(hashlib.sha256(image).hexdigest(), artifact["binarySha256"])

    def test_missing_cross_image_and_changed_native_image_emit_no_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source, build, baseline = self.inputs(root)
            native = root / FIXTURE.native_platform().replace("/", "-") / "mesh"
            native.write_bytes(b"changed image")
            with self.assertRaisesRegex(ValueError, "native fixture report"):
                self.run_context(root, source, build, baseline)
            self.assertFalse((root / "manifest.json").exists())
            build["digest"] = FIXTURE.digest(native)
            foreign = next(target for target in FIXTURE.PLATFORMS if target != FIXTURE.native_platform())
            (root / foreign.replace("/", "-") / "mesh").unlink()
            with self.assertRaises(FileNotFoundError):
                self.run_context(root, source, build, baseline)
            self.assertFalse((root / "manifest.json").exists())

    def test_changed_source_report_emits_no_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source, build, baseline = self.inputs(root)
            baseline["digest"] = "b" * 64
            with self.assertRaisesRegex(ValueError, "source report"):
                self.run_context(root, source, build, baseline)
            self.assertFalse((root / "manifest.json").exists())


if __name__ == "__main__":
    unittest.main()
