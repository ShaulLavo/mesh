#!/usr/bin/env python3
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("rollback_context", Path(__file__).with_name("prepare-rollback-context.py"))
CONTEXT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CONTEXT)


class NativeContextTests(unittest.TestCase):
    def records(self, root):
        actual = json.loads((Path(__file__).parent.parent / "internal/update/testdata/discovery/v0.1.306/mesh-release.json").read_bytes())
        for index, artifact in enumerate(actual["artifacts"]):
            directory = root / str(index)
            directory.mkdir()
            value = {"schema": 1, "artifact": artifact, "build": {
                "version": actual["version"], "commit": actual["commit"], "platform": artifact["platform"],
                "digest": artifact["binarySha256"], "stateVersion": actual["compatibility"]["stateWrite"],
                "workerProtocol": actual["compatibility"]["workerWrite"], "modified": False}}
            value["sources"] = [{"stateVersion": 13, "workerProtocol": 1}]
            (directory / "rollback-artifact.json").write_text(json.dumps(value))
        return actual

    def test_context_keeps_all_actual_artifacts_without_transition_claims(self):
        with tempfile.TemporaryDirectory(prefix="rollback-artifacts-") as directory:
            root = Path(directory)
            actual = self.records(root)
            value = CONTEXT.assemble(root, root / "context.json")
            self.assertEqual(value["artifacts"], actual["artifacts"])
            self.assertEqual(value["commit"], actual["commit"])
            self.assertEqual(value["compatibility"]["transitions"], [])
            self.assertEqual(value["compatibility"]["stateReadMin"], 13)

    def test_missing_or_mismatched_platform_never_emits_context(self):
        with tempfile.TemporaryDirectory(prefix="rollback-artifacts-") as directory:
            root = Path(directory)
            self.records(root)
            path = root / "2/rollback-artifact.json"
            value = json.loads(path.read_bytes())
            value["build"]["digest"] = "a" * 64
            path.write_text(json.dumps(value))
            with self.assertRaisesRegex(ValueError, "binding"):
                CONTEXT.assemble(root, root / "context.json")
            self.assertFalse((root / "context.json").exists())
            path.unlink()
            with self.assertRaisesRegex(ValueError, "all three"):
                CONTEXT.assemble(root, root / "context.json")


if __name__ == "__main__":
    unittest.main()
