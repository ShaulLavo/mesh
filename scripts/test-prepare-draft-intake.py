#!/usr/bin/env python3
"""Offline controls for draft authority and exact release candidate handoff."""
import ast
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from fixtures import draft_intake as INTAKE

ROOT = Path(__file__).resolve().parent.parent
GATE_SPEC = importlib.util.spec_from_file_location("gate", ROOT / "scripts/test-helper-recovery-gate.py")
GATE = importlib.util.module_from_spec(GATE_SPEC)
GATE_SPEC.loader.exec_module(GATE)
RELEASE = (ROOT / ".github/workflows/release.yml").read_text()
PROOFS = (ROOT / ".github/workflows/update-proofs.yml").read_text()
SOURCE, VERSION = "a" * 40, "v0.1.323"


def depth(node, nesting=0):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        nesting = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        nesting += 1
    return max((depth(child, nesting) for child in ast.iter_child_nodes(node)), default=nesting)


class DraftIntakeTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="draft-intake-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.directory = self.root / "candidate"
        self.directory.mkdir()
        self.release = {"id": 445605054, "tag_name": VERSION, "target_commitish": SOURCE, "draft": True, "assets": [],
                        "repository": "ShaulLavo/mesh", "intake_run": 42, "frozen_baselines": [],
                        "url": "https://api.github.com/repos/ShaulLavo/mesh/releases/445605054",
                        "html_url": "https://github.com/ShaulLavo/mesh/releases/tag/" + VERSION}
        checksums = []
        for name in INTAKE.ARCHIVES.values():
            data = ("exact draft " + name).encode()
            (self.directory / name).write_bytes(data)
            checksums.append(hashlib.sha256(data).hexdigest() + "  " + name + "\n")
        (self.directory / "checksums.txt").write_text("".join(checksums))
        for number, name in enumerate(INTAKE.NAMES, 1):
            data = (self.directory / name).read_bytes()
            self.release["assets"].append({"name": name, "id": number, "size": len(data), "state": "uploaded",
                                           "digest": "sha256:" + hashlib.sha256(data).hexdigest()})
        self.write_release()

    def write_release(self):
        (self.directory / "draft.json").write_text(json.dumps(self.release))

    def add_asset(self, directory, release, name, data):
        (directory / name).write_bytes(data)
        release["assets"].append({"name": name, "id": max(item["id"] for item in release["assets"]) + 1,
                                  "size": len(data), "state": "uploaded", "digest": "sha256:" + hashlib.sha256(data).hexdigest()})
        (directory / "draft.json").write_text(json.dumps(release))

    def signed_snapshot(self):
        directory = self.root / VERSION
        shutil.copytree(self.directory, directory)
        release = copy.deepcopy(self.release)
        for name in INTAKE.release_documents.PHYSICAL + ("mesh.rb",):
            self.add_asset(directory, release, name, b'{"offlinePhysicalControl":true}')
        return directory, release

    def fake_api(self, source=None):
        binary = self.root / "bin"
        binary.mkdir(exist_ok=True)
        fake = binary / "gh"
        fake.write_text(f"#!{sys.executable}\n" + '''import json, os, pathlib, sys
root = pathlib.Path(os.environ["FIXTURE"])
source = pathlib.Path(os.environ["FIXTURE_SOURCE"])
args = sys.argv[1:]
with (root / "calls").open("a") as calls:
    calls.write(json.dumps(args) + "\\n")
if args == ["api", "--paginate", "--slurp", "repos/ShaulLavo/mesh/releases?per_page=100"]:
    value = json.loads((source / "draft.json").read_text())
    print(json.dumps([[dict(value, assets=[])]] if os.environ["AUTHORITY"] == "write" else [[]]))
elif args == ["api", "--paginate", "--slurp", "repos/ShaulLavo/mesh/releases/445605054/assets?per_page=100"] and os.environ["AUTHORITY"] == "write":
    value = json.loads((source / "draft.json").read_text())
    middle = len(value["assets"]) // 2
    print(json.dumps([value["assets"][:middle], value["assets"][middle:]]))
elif args[0] == "api" and "/releases/assets/" in args[1] and os.environ["AUTHORITY"] == "write":
    value = json.loads((source / "draft.json").read_text())
    item = next(item for item in value["assets"] if str(item["id"]) == args[1].rsplit("/", 1)[1])
    sys.stdout.buffer.write((source / item["name"]).read_bytes())
else:
    sys.stderr.write("release not found (HTTP 404)\\n")
    sys.exit(1)
''')
        fake.chmod(0o700)
        return dict(os.environ, PATH=str(binary) + os.pathsep + os.environ["PATH"],
                    FIXTURE=str(self.root), FIXTURE_SOURCE=str(source or self.directory), GITHUB_RUN_ID="42",
                    AUTHORITY="write", PYTHONDONTWRITEBYTECODE="1")

    def test_writer_captures_exact_draft_while_read_only_and_tag_paths_cannot(self):
        environment = self.fake_api()
        tag = subprocess.run(["gh", "api", "repos/ShaulLavo/mesh/releases/tags/" + VERSION],
                             env=environment, capture_output=True, check=False)
        self.assertNotEqual(tag.returncode, 0)
        command = [sys.executable, str(ROOT / "scripts/prepare-draft-intake.py"), "capture"]
        denied = subprocess.run(command + [str(self.root / "denied"), VERSION, SOURCE],
                                env=dict(environment, AUTHORITY="read"), capture_output=True, check=False)
        self.assertNotEqual(denied.returncode, 0)
        self.assertIn(b"missing or unavailable", denied.stderr)
        self.assertFalse((self.root / "denied").exists())
        accepted = subprocess.run(command + [str(self.root / "accepted"), VERSION, SOURCE],
                                  env=environment, capture_output=True, check=False)
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        for name in INTAKE.NAMES:
            self.assertEqual((self.root / "accepted" / name).read_bytes(), (self.directory / name).read_bytes())
        with patch.object(INTAKE.subprocess, "check_output", side_effect=AssertionError("reader needs no API")):
            INTAKE.verify(self.root / "accepted", VERSION, SOURCE)

    def test_wrong_source_version_or_published_release_is_refused(self):
        for field, value in (("target_commitish", "b" * 40), ("tag_name", "v0.1.322"), ("draft", False), ("id", 0)):
            changed = dict(self.release, **{field: value})
            with self.assertRaisesRegex(ValueError, "reserved source or version"):
                INTAKE.inventory(changed, VERSION, SOURCE)

    def test_missing_or_repeated_assets_and_missing_digests_are_refused(self):
        changed = copy.deepcopy(self.release)
        changed["assets"].pop()
        with self.assertRaisesRegex(ValueError, "every unique"):
            INTAKE.inventory(changed, VERSION, SOURCE)
        changed["assets"].append(changed["assets"][0])
        with self.assertRaisesRegex(ValueError, "every unique"):
            INTAKE.inventory(changed, VERSION, SOURCE)
        changed = copy.deepcopy(self.release)
        changed["assets"][0]["digest"] = None
        with self.assertRaisesRegex(ValueError, "SHA256"):
            INTAKE.inventory(changed, VERSION, SOURCE)

    def test_missing_or_modified_bytes_are_refused(self):
        archive = self.directory / next(iter(INTAKE.ARCHIVES.values()))
        original = archive.read_bytes()
        archive.write_bytes(b"x" * len(original))
        with self.assertRaisesRegex(ValueError, "exact uploaded bytes"):
            INTAKE.verify(self.directory, VERSION, SOURCE)
        archive.unlink()
        with self.assertRaisesRegex(ValueError, "complete asset inventory"):
            INTAKE.verify(self.directory, VERSION, SOURCE)

    def test_uploaded_archive_must_match_the_uploaded_checksum_file(self):
        path = self.directory / "checksums.txt"
        path.write_text(path.read_text().replace(self.release["assets"][0]["digest"][7:], "0" * 64))
        self.release["assets"][-1]["digest"] = "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
        self.write_release()
        with self.assertRaisesRegex(ValueError, "release checksum"):
            INTAKE.verify(self.directory, VERSION, SOURCE)

    def test_native_report_requires_actual_candidate_source_version_and_platform(self):
        report = self.root / "reader.json"
        build = {"commit": SOURCE, "version": VERSION, "platform": {"os": "linux", "arch": "amd64"}, "modified": False}
        report.write_text(json.dumps({"build": build}))
        INTAKE.native(self.directory, VERSION, SOURCE, "linux/amd64", report)
        for field, value in (("commit", "b" * 40), ("version", "v0.1.322"), ("platform", {"os": "linux", "arch": "arm64"}), ("modified", True)):
            report.write_text(json.dumps({"build": dict(build, **{field: value})}))
            with self.assertRaisesRegex(ValueError, "native candidate differs"):
                INTAKE.native(self.directory, VERSION, SOURCE, "linux/amd64", report)

    def run_preparation(self, draft):
        scripts = self.root / "scripts"
        (scripts / "fixtures").mkdir(parents=True)
        shutil.copyfile(ROOT / "scripts/prepare-draft-intake.py", scripts / "prepare-draft-intake.py")
        shutil.copyfile(ROOT / "scripts/fixtures/draft_intake.py", scripts / "fixtures/draft_intake.py")
        shutil.copytree(ROOT / "scripts/fixtures", scripts / "fixtures", dirs_exist_ok=True,
                        ignore=shutil.ignore_patterns("__pycache__"))
        (scripts / "fixtures/noindex_paths.py").write_text("")
        (scripts / "probe-update-reader.py").write_text(
            'import json, os, pathlib, sys\npathlib.Path(sys.argv[4]).write_text(json.dumps({"build": '
            '{"version": os.environ["VERSION"], "commit": os.environ["SOURCE"], "modified": False, '
            '"platform": {"os": "linux", "arch": "amd64"}}}))\n')
        tools = scripts / "prepare-update-proof-tools.sh"
        tools.write_text('#!/bin/bash\nprintf "%s\\n" "$@" > "$FIXTURE/tool-args"\nmkdir -p "$1"\n')
        tools.chmod(0o700)
        environment = self.fake_api()
        go = self.root / "bin/go"
        go.write_text('#!/bin/bash\ncase "$*" in *GOHOSTOS*) echo linux;; *GOHOSTARCH*) echo amd64;; *) exit 1;; esac\n')
        go.chmod(0o700)
        shutil.move(self.directory, self.root / "release-candidate")
        environment.update(AUTHORITY="read", DRAFT=str(draft).lower(), RUNNER_TEMP=str(self.root), VERSION=VERSION,
                           SOURCE=SOURCE, PLATFORM="linux/amd64", ARCHIVE="mesh_linux_amd64.tar.gz", BASELINE_ASSETS="")
        step = GATE.step(GATE.job(PROOFS, "prepare"), "Build the native candidate and startup-fault fixture once")
        command = GATE.run_script(step).split("short=$(mktemp", 1)[0]
        result = subprocess.run(["bash", "-euo", "pipefail", "-c", command], cwd=self.root,
                                env=environment, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.root / "calls").exists(), "read-only preparation contacted the draft API")
        return (self.root / "tool-args").read_text().splitlines()

    def test_read_only_draft_preparation_uses_the_exact_archive(self):
        arguments = self.run_preparation(True)
        self.assertEqual(arguments[1:], [VERSION, str(self.root / "release-candidate/mesh_linux_amd64.tar.gz")])

    def test_normal_ci_still_builds_its_candidate_without_draft_intake(self):
        arguments = self.run_preparation(False)
        self.assertEqual(arguments[1:], [VERSION])

    def test_workflows_keep_read_only_proofs_and_mandatory_publication_gates(self):
        self.assertNotIn(": write", PROOFS)
        self.assertNotIn("write", GATE.job(RELEASE, "prove-transition"))
        self.assertNotIn("write", GATE.job(RELEASE, "verify-readers"))
        prepare = GATE.job(PROOFS, "prepare")
        self.assertIn("if: inputs.draft\n        uses: actions/download-artifact@", prepare)
        self.assertNotIn("gh release download", prepare)
        build = GATE.job(RELEASE, "build")
        self.assertLess(build.index('gh release upload'), build.index('prepare-draft-intake.py capture'))
        self.assertLess(build.index('prepare-draft-intake.py capture'), build.index('name: release-candidate'))
        publish = GATE.job(RELEASE, "publish")
        self.assertIn("needs: [reserve, build, prove-transition]", publish)
        self.assertLess(publish.index('gh release upload'), publish.index('name: release-signed-draft'))
        self.assertIn('capture "$RUNNER_TEMP/release-signed-draft"', publish)
        self.assertIn('--signed --candidate "$RUNNER_TEMP/release-candidate"', publish)
        self.assertIn("needs: [reserve, publish, verify-readers, publication-native]", GATE.job(RELEASE, "finalize"))
        self.assertIn("name: release-signed-draft", GATE.job(RELEASE, "verify-readers"))
        self.assertNotIn("gh release download", GATE.job(RELEASE, "verify-readers"))

    def test_new_code_keeps_at_most_three_nesting_levels(self):
        for path in (Path(INTAKE.__file__), Path(__file__), ROOT / "scripts/prepare-draft-intake.py"):
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))

    def test_signed_writer_capture_keeps_every_actual_asset_and_same_candidate(self):
        directory, release = self.signed_snapshot()
        environment = self.fake_api(directory)
        output = self.root / "captured-signed"
        result = subprocess.run([sys.executable, str(ROOT / "scripts/prepare-draft-intake.py"), "capture", str(output),
            VERSION, SOURCE, "--signed", "--candidate", str(self.directory)], env=environment, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(INTAKE.verify(output, VERSION, SOURCE, signed=True, run=42), release)
        for item in release["assets"]:
            self.assertEqual((output / item["name"]).read_bytes(), (directory / item["name"]).read_bytes())

    def test_changed_candidate_bytes_asset_id_release_id_and_run_are_refused(self):
        directory, release = self.signed_snapshot()
        for field, changed in (("id", 445605055), ("intake_run", 43)):
            value = dict(release, **{field: changed})
            with self.assertRaisesRegex(ValueError, "transition candidate|repository, release ID"):
                INTAKE.same_candidate(self.directory, value, VERSION, SOURCE)
        release["assets"][0]["id"] += 100
        with self.assertRaisesRegex(ValueError, "transition candidate"):
            INTAKE.same_candidate(self.directory, release, VERSION, SOURCE)
        release["assets"][0]["digest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(ValueError, "transition candidate"):
            INTAKE.same_candidate(self.directory, release, VERSION, SOURCE)
        with self.assertRaisesRegex(ValueError, "writer run"):
            INTAKE.verify(directory, VERSION, SOURCE, signed=True, run=43)

    def test_signed_snapshot_requires_signature_and_complete_actual_json_namespace(self):
        directory, release = self.signed_snapshot()
        (directory / "unexpected.json").write_text("{}")
        with self.assertRaisesRegex(ValueError, "complete asset inventory"):
            INTAKE.verify(directory, VERSION, SOURCE, signed=True)
        (directory / "unexpected.json").unlink()
        (directory / "mesh-update.sig.json").unlink()
        with self.assertRaisesRegex(ValueError, "complete asset inventory"):
            INTAKE.verify(directory, VERSION, SOURCE, signed=True)
        release["assets"] = [item for item in release["assets"] if item["name"] != "mesh-update.sig.json"]
        with self.assertRaisesRegex(ValueError, "required metadata"):
            INTAKE.inventory(release, VERSION, SOURCE, signed=True)

    def test_frozen_custom_baseline_bytes_and_inventory_are_authoritative(self):
        data = b"custom baseline; offline bytes only"
        name = "baseline_linux_amd64_" + hashlib.sha256(data).hexdigest() + ".bin"
        self.release["frozen_baselines"] = [name]
        self.add_asset(self.directory, self.release, name, data)
        self.assertEqual(INTAKE.verify(self.directory, VERSION, SOURCE, baselines=name), self.release)
        with self.assertRaisesRegex(ValueError, "requested frozen"):
            INTAKE.verify(self.directory, VERSION, SOURCE, baselines="")
        self.release["assets"][-1]["digest"] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(ValueError, "filename checksum"):
            INTAKE.inventory(self.release, VERSION, SOURCE)
        self.release["frozen_baselines"] = []
        with self.assertRaisesRegex(ValueError, "frozen custom"):
            INTAKE.inventory(self.release, VERSION, SOURCE)

    def test_blocking_packaging_ci_runs_intake_beside_signing(self):
        packaging = (ROOT / "scripts/check-packaging.sh").read_text()
        self.assertIn("python3 scripts/test-release-signing.py\n  python3 scripts/test-prepare-draft-intake.py", packaging)
        self.assertIn("run: ./scripts/check-packaging.sh dist", (ROOT / ".github/workflows/ci.yml").read_text())


if __name__ == "__main__":
    unittest.main()
