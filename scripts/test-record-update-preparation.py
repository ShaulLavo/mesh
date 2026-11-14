#!/usr/bin/env python3
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("preparation", Path(__file__).with_name("record-update-preparation.py"))
PREPARATION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PREPARATION)


class PreparationTests(unittest.TestCase):
    def test_records_exact_checkout_platform_and_all_tool_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in PREPARATION.TOOLS:
                (root / name).write_bytes(name.encode())
            with patch.object(PREPARATION.subprocess, "check_output", side_effect=["a" * 40, "", "linux\narm64\n"]):
                value = PREPARATION.record(root, "a" * 40)
            self.assertEqual(value, json.loads((root / "preparation.json").read_bytes()))
            self.assertEqual(value["source"], {"commit": "a" * 40, "clean": True})
            self.assertEqual(value["platform"], {"os": "linux", "arch": "arm64"})
            self.assertEqual(value["sha256"], {name: hashlib.sha256(name.encode()).hexdigest() for name in PREPARATION.TOOLS})

    def test_dirty_or_changed_checkout_never_emits_record(self):
        for source, status in [("a" * 40, " M go.mod"), ("b" * 40, "")]:
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                with patch.object(PREPARATION.subprocess, "check_output", side_effect=[source, status]):
                    with self.assertRaisesRegex(ValueError, "changed or is not clean"):
                        PREPARATION.record(root, "a" * 40)
                self.assertFalse((root / "preparation.json").exists())

    def test_missing_tool_never_emits_record(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(PREPARATION.subprocess, "check_output", side_effect=["a" * 40, "", "linux\namd64\n"]):
                with self.assertRaises(FileNotFoundError):
                    PREPARATION.record(root, "a" * 40)
            self.assertFalse((root / "preparation.json").exists())


if __name__ == "__main__":
    unittest.main()
