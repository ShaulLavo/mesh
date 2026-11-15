#!/usr/bin/env python3
"""Fake positional tool controls; the Go suite owns real archive/trust validation."""
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

from fixtures import release_documents, release_tool

SOURCE = "a" * 40


def fake_tool(directory):
    directory.mkdir()
    tool = directory / "mesh-release-sign"
    tool.write_text(f"#!{sys.executable}\n" + '''import hashlib, json, os, pathlib, sys
me = pathlib.Path(__file__)
args = sys.argv[1:]
with pathlib.Path(os.environ["TOOL_CALLS"]).open("a") as calls:
    calls.write(json.dumps(args) + "\\n")
if args == ["identity"]:
    print(json.dumps({"schema":1,"source":os.environ["TOOL_SOURCE"],"target":json.loads(os.environ["TOOL_TARGET"]),
        "sha256":hashlib.sha256(me.read_bytes()).hexdigest(),"modified":False}))
elif args[0] == "materialize-documents" and len(args) == 4:
    snapshot, binding, destination = map(pathlib.Path, args[1:])
    expected = json.loads(binding.read_text())
    fixture = json.loads((snapshot / "fake-documents.json").read_text())
    if fixture["binding"] != expected: raise SystemExit("binding differs")
    destination.mkdir()
    entries = []
    for name, text in fixture["documents"].items():
        data = text.encode()
        (destination / name).write_bytes(data)
        entries.append({"name":name,"size":len(data),"sha256":hashlib.sha256(data).hexdigest()})
    print(json.dumps({"schema":1,"binding":expected,"documents":entries}))
else:
    raise SystemExit("unsupported positional command")
''')
    tool.chmod(0o755)
    return tool


class ReleaseToolTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="release-tool-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.tool = fake_tool(self.root / "tools")
        (self.root / "trust").mkdir()
        patcher = patch.dict(os.environ, {"TOOL_CALLS": str(self.root / "calls"), "TOOL_SOURCE": SOURCE,
            "TOOL_TARGET": json.dumps(release_tool.native_target()), "MESH_RELEASE_TOOL": str(self.tool),
            "MESH_RELEASE_TOOL_SOURCE": SOURCE, "MESH_RELEASE_TOOL_RUN": "42", "MESH_RELEASE_TOOL_ROLE": "harness",
            "MESH_RELEASE_TRUST_CACHE": str(self.root / "trust")})
        patcher.start()
        self.addCleanup(patcher.stop)
        release_tool.record(self.tool.parent, SOURCE, 42, "harness", release_tool.native_target())

    def record(self):
        return json.loads((self.tool.parent / "release-tool.json").read_bytes())

    def changed_record(self, **changes):
        value = self.record() | changes
        (self.tool.parent / "release-tool.json").write_text(json.dumps(value))

    def test_explicit_clean_source_target_digest_mode_and_role(self):
        self.assertEqual(release_tool.configured(), self.tool)
        for changes in ({"source": "b" * 40}, {"run": 43}, {"role": "source"}, {"modified": True},
                        {"sha256": "0" * 64}, {"target": {"os": "wrong", "arch": "wrong"}}):
            original = self.record()
            self.changed_record(**changes)
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                release_tool.configured()
            (self.tool.parent / "release-tool.json").write_text(json.dumps(original))
        self.tool.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "executable regular file"):
            release_tool.configured()

    def test_missing_path_or_source_provenance_has_no_path_fallback(self):
        with patch.dict(os.environ, MESH_RELEASE_TOOL="mesh-release-sign"):
            with self.assertRaisesRegex(ValueError, "absolute"):
                release_tool.configured()
        with patch.dict(os.environ, MESH_RELEASE_TOOL_SOURCE=""):
            with self.assertRaises(ValueError):
                release_tool.configured()

    def test_materialize_uses_exact_positional_binding_and_separate_inventory(self):
        snapshot = self.root / "physical"
        snapshot.mkdir()
        release = {"tag_name": "v0.1.324", "target_commitish": SOURCE, "id": 19, "intake_run": 42}
        documents = {name: "exact logical " + name for name in release_documents.FIXED + (release_documents.ENVELOPE,)}
        (snapshot / "fake-documents.json").write_text(json.dumps({"binding": release_tool.binding(release), "documents": documents}))
        output = self.root / "logical"
        inventory = self.root / "logical-inventory.json"
        before = sorted(path.name for path in snapshot.iterdir())
        value = release_tool.materialize(snapshot, release, output, inventory)
        self.assertEqual(set(documents), {path.name for path in output.iterdir()})
        self.assertEqual(sorted(path.name for path in snapshot.iterdir()), before)
        self.assertEqual(value, json.loads(inventory.read_text()))
        self.assertTrue(all("id" not in item for item in value["documents"]))
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        invocation = calls[-1]
        self.assertEqual(invocation[0], "materialize-documents")
        self.assertEqual(invocation[1], str(snapshot))
        self.assertEqual(invocation[3], str(output))
        self.assertEqual(len(invocation), 4)

    def test_materialized_bytes_cannot_change_after_authenticated_inventory(self):
        output = self.root / "logical"
        output.mkdir()
        entries = []
        for name in release_documents.FIXED + (release_documents.ENVELOPE,):
            (output / name).write_bytes(b"exact")
            entries.append({"name": name, "size": 5, "sha256": hashlib.sha256(b"exact").hexdigest()})
        release_tool.validate_logical_inventory(output, entries)
        (output / entries[0]["name"]).write_bytes(b"changed")
        with self.assertRaisesRegex(ValueError, "authenticated reference"):
            release_tool.validate_logical_inventory(output, entries)

    def test_artifact_modes_are_explicit(self):
        self.assertEqual(stat.S_IMODE(self.tool.stat().st_mode), 0o755)


if __name__ == "__main__":
    unittest.main()
