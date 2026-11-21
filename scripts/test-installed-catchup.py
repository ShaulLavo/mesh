"""Installed proof path boundaries and real native-service failure cleanup."""
import json
import hashlib
import ast
import http.server
import io
import importlib.util
import os
import shutil
import socket
import struct
import ssl
import socketserver
import http.client
import zipfile
import tempfile
import tarfile
import textwrap
import unittest
import threading
import subprocess
from types import SimpleNamespace
from pathlib import Path
from unittest.mock import MagicMock, Mock, patch

from fixtures import fleet_baselines, installed_paths as paths, native_services, published_releases, installed_cohort, installed_artifacts, installed_accounts, installed_routes, installed_chain, installed_native, publication_origin, installed_publication, installed_storage
from fixtures import installed_review_controls, installed_listener_controls, installed_catalog_controls, installed_order_trace_controls
from fixtures import installed_attachment_controls, installed_publication_controls
from fixtures import installed_releases, release_transition
from fixtures import installed_adoption, installed_clock

PROOF_PATH = Path(__file__).with_name("prove-installed-catchup.py")
SPEC = importlib.util.spec_from_file_location("installed_catchup_proof", PROOF_PATH)
PROOF = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROOF)


def maximum_depth(node, depth):
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        depth = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try, ast.AsyncWith, ast.AsyncFor)):
        depth += 1
    return max((maximum_depth(child, depth) for child in ast.iter_child_nodes(node)), default=depth)


class ExporterFormatTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="exporter-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.clock = installed_clock.Clock({"test": "exporter"})
        active = self.clock.activate()
        active.__enter__()
        self.addCleanup(active.__exit__, None, None, None)

    def test_future_overlay_changes_only_manifest_schema_guard(self):
        replacements = installed_releases.future_manifest_overlay(self.root)
        source = installed_releases.REPO / "internal/release/release.go"
        expected = source.read_text().replace("if m.Schema != ManifestSchema {",
            "if m.Schema != ManifestSchema && m.Schema != 2 {", 1)
        self.assertEqual(list(replacements), [str(source)])
        self.assertEqual(Path(replacements[str(source)]).read_text(), expected)
        self.assertFalse((self.root / "reader-types.go").exists())

    def test_future_overlay_source_drift_fails_closed(self):
        with patch.object(Path, "read_text", return_value="changed validator"):
            with self.assertRaisesRegex(RuntimeError, "exactly one"):
                installed_releases.future_manifest_overlay(self.root)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_explicit_exporter_selection_precedes_launch(self):
        for waypoint, schemas in ((False, (1,)), (True, (1, 2))):
            self.check_selection(waypoint, schemas)

    def check_selection(self, waypoint, schemas):
        with patch.object(installed_releases, "Releases") as factory, patch.object(installed_adoption, "run_prepared"), \
                patch.object(installed_adoption, "retain_producer_logs"):
            installed_adoption.run(self.root, self.root, "linux/amd64", None, format_waypoint=waypoint)
        factory.assert_called_once_with(self.root, "linux/amd64", self.root, manifest_schemas=schemas)
        factory.return_value.start_signer.assert_called_once_with()
        factory.return_value.close.assert_called_once_with()

    def test_unknown_exporter_capability_fails_before_build(self):
        with self.assertRaisesRegex(ValueError, "manifest capability"):
            installed_releases.Releases(self.root, "linux/amd64", self.root, manifest_schemas=(2,))

    def test_exporter_build_selects_only_manifest_overlay_and_records_identity(self):
        for schemas in ((1,), (1, 2)):
            self.check_exporter_build(schemas)

    def check_exporter_build(self, schemas):
        root = self.root / str(len(schemas))
        root.mkdir()
        releases = installed_releases.Releases(root, "linux/amd64", root, manifest_schemas=schemas)
        self.addCleanup(releases.close)
        source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=installed_releases.REPO, text=True).strip()
        with patch.object(installed_releases.subprocess, "run") as run, \
                patch.object(installed_releases.subprocess, "check_output", return_value=source), \
                patch.object(installed_releases.subprocess, "Popen") as launch, \
                patch.object(native_services, "wait_for"):
            releases.start_signer()
        command = run.call_args_list[0].args[0]
        self.assertEqual("-overlay" in command, schemas == (1, 2))
        replacements = json.loads((root / "exporter-overlay.json").read_text())["Replace"]
        self.assertEqual(len(replacements), len(schemas) - 1)
        record = json.loads((root / "fixture-exporter.json").read_text())
        self.assertEqual(record["manifestSchemas"], list(schemas))
        self.assertEqual(record["harnessSource"], source)
        self.assertEqual(record["overlaySha256"], installed_releases.digest((root / "exporter-overlay.json").read_bytes()))
        launch.assert_called_once()
        duration = next(arg for arg in launch.call_args.args[0] if arg.startswith("-test.timeout="))
        self.assertTrue(duration.endswith("ns"))
        self.assertLessEqual(int(duration.removeprefix("-test.timeout=").removesuffix("ns")), installed_clock.HANG_SECONDS * installed_clock.SECOND)
        with self.assertRaisesRegex(RuntimeError, "already launched"):
            releases.start_signer()

    def test_exporter_build_failure_never_launches_or_falls_back(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root, manifest_schemas=(1, 2))
        self.addCleanup(releases.close)
        with patch.object(installed_releases.subprocess, "run", side_effect=RuntimeError("build failed")) as run, \
                patch.object(installed_releases.subprocess, "Popen") as launch:
            with self.assertRaisesRegex(RuntimeError, "build failed"):
                releases.start_signer()
        run.assert_called_once()
        launch.assert_not_called()

    def test_waypoint_exporter_failure_still_closes_without_preparation(self):
        with patch.object(installed_releases, "Releases") as factory, \
                patch.object(installed_adoption, "run_prepared") as prepare, \
                patch.object(installed_adoption, "retain_producer_logs"):
            factory.return_value.start_signer.side_effect = RuntimeError("exporter failed")
            with self.assertRaisesRegex(RuntimeError, "exporter failed"):
                installed_adoption.run(self.root, self.root, "linux/amd64", None, format_waypoint=True)
        factory.return_value.start_signer.assert_called_once_with()
        factory.return_value.close.assert_called_once_with()
        prepare.assert_not_called()

    def test_invalid_exporter_reply_is_a_protocol_runtime_error(self):
        for data in (b"", b"not JSON\n"):
            connection = self.exporter_connection(data)
            with patch.object(installed_releases.installed_origin.socket, "socket", return_value=connection), \
                    self.assertRaisesRegex(RuntimeError, "incomplete or invalid reply") as raised:
                installed_releases.installed_origin.request(self.root, {"stop": True})
            self.assertIsInstance(raised.exception.__cause__, json.JSONDecodeError)

    def test_empty_stop_reply_reaps_exporter_and_preserves_primary_timeout(self):
        releases = installed_releases.Releases(self.root, "linux/amd64", self.root)
        process = releases.signer = Mock()
        process.poll.side_effect = [None, 0]
        log = releases.signer_log = Mock()
        primary = TimeoutError("original serving deadline")
        connection = self.exporter_connection(b"")
        with patch.object(installed_releases.installed_origin.socket, "socket", return_value=connection), \
                self.assertRaises(TimeoutError) as raised:
            try:
                raise primary
            finally:
                releases.close()
        self.assertIs(raised.exception, primary)
        process.kill.assert_called_once_with()
        process.wait.assert_called_once()
        log.close.assert_called_once_with()
        self.assertTrue(releases.closed)
        self.assertTrue(json.loads((self.root / "fixture-ownership.json").read_bytes())["signerAbsent"])

    def exporter_connection(self, data):
        connection = Mock()
        connection.__enter__ = Mock(return_value=connection)
        connection.__exit__ = Mock(return_value=False)
        connection.makefile.return_value = io.BytesIO(data)
        return connection

    def test_failed_export_never_reads_stale_ledger(self):
        case = SimpleNamespace(root=self.root, state=self.root / "state", origin=SimpleNamespace())
        (self.root / "chain-ledger.json").write_text('{"entries":[]}')
        case.origin.ledger = Mock(side_effect=RuntimeError("empty exporter reply"))
        with patch.object(Path, "read_text", side_effect=AssertionError("stale ledger consumed")):
            with self.assertRaisesRegex(RuntimeError, "empty exporter reply"):
                installed_chain.read_ledger(case)

    def test_producer_log_export_enumerates_each_directory_once(self):
        root, output = self.root / "producer", self.root / "output"
        output.mkdir()
        for name in ("root.log", ".hidden.log", "one/one.log", "one/deep/deep.log", "two/two.log", "two/deep/deep.log"):
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(name)
        (root / "linked.log").symlink_to(root / "root.log")
        (root / "linked-directory").symlink_to(root / "one", target_is_directory=True)
        proof = root / "proofs/nested/receipt.json"
        proof.parent.mkdir(parents=True)
        proof.write_text('{"receipt":true}')
        expected = list(root.rglob("*.log"))
        directories = [root] + [path for path in root.rglob("*") if path.is_dir() and not path.is_symlink()]
        with patch.object(installed_adoption.os, "scandir", wraps=os.scandir) as scans, \
                patch.object(installed_native, "copy_log_tail", wraps=installed_native.copy_log_tail) as copy:
            installed_adoption.retain_producer_logs(root, output)
        self.assertCountEqual([call.args[0] for call in copy.call_args_list], expected)
        enumerated = [Path(call.args[0]) for call in scans.call_args_list]
        for directory in directories:
            if directory == root / "proofs" or root / "proofs" in directory.parents:
                continue
            self.assertEqual(enumerated.count(directory), 1, str(directory))
        for path in expected:
            destination = output / "producer-logs" / path.relative_to(root)
            self.assertEqual(destination.read_bytes(), path.read_bytes())
        self.assertEqual((output / "measured-proofs/nested/receipt.json").read_bytes(), proof.read_bytes())

    def test_producer_log_discovery_keeps_directory_and_broken_link_matches(self):
        directory = self.root / "directory.log"
        directory.mkdir()
        (directory / "nested.log").write_text("nested")
        (self.root / "broken.log").symlink_to(self.root / "absent")
        self.assertCountEqual(installed_clock.recursive_suffix_paths(self.root, ".log"), self.root.rglob("*.log"))

    def test_producer_log_export_preserves_colliding_flat_names_in_either_order(self):
        root = self.root / "producer"
        paths = [root / name for name in ("a/b/c-d.log", "a-b/c/d.log")]
        for path in paths:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes((str(path.relative_to(root)) + "\n").encode() * 10000)
        self.check_colliding_logs(root, paths, "first")
        self.check_colliding_logs(root, list(reversed(paths)), "reversed")

    def check_colliding_logs(self, root, paths, name):
        output = self.root / name
        output.mkdir()
        with patch.object(installed_clock, "recursive_suffix_paths", return_value=paths):
            installed_adoption.retain_producer_logs(root, output)
        for path in paths:
            destination = output / "producer-logs" / path.relative_to(root)
            self.assertEqual(destination.read_bytes(), path.read_bytes()[-65536:])


class SocketPathsTest(unittest.TestCase):
    def setUp(self):
        base = os.environ.get("MESH_SHORT_TMP", "/tmp")
        self.temp = tempfile.TemporaryDirectory(prefix="ip-", dir=base)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()

    def test_every_generated_address_binds(self):
        evidence = paths.validate(self.root)
        for row in evidence["paths"]:
            address = Path(row["path"])
            address.parent.mkdir(parents=True, exist_ok=True)
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(str(address))

    def test_platform_exact_byte_limits(self):
        for system, maximum in (("Darwin", 103), ("Linux", 107)):
            suffix = max(len(os.fsencode(path)) for path in paths.socket_paths(self.root))
            root = self.root / ("x" * (maximum - suffix - 1))
            self.assertEqual(paths.validate(root, system)["longestSocketBytes"], maximum)
            with self.assertRaisesRegex(ValueError, "socket requires"):
                paths.validate(root.with_name(root.name + "x"), system)

    def test_long_tmpdir_and_symlink_cannot_hide_physical_length(self):
        long = self.root / ("long-tmp-" * 12)
        long.mkdir()
        alias = self.root / "link"
        alias.symlink_to(long, target_is_directory=True)
        before = set(long.iterdir())
        with patch.dict(os.environ, {"MESH_SHORT_TMP": str(alias)}):
            with self.assertRaisesRegex(ValueError, "socket requires"):
                paths.create()
        self.assertEqual(set(long.iterdir()), before)

    def test_filesystem_bytes_are_counted(self):
        with self.assertRaisesRegex(ValueError, "socket requires"):
            paths.validate(self.root / ("é" * 40))

    def test_missing_account_cleanup_retains_actual_order_scratch(self):
        root, output = self.root / "scratch", self.root / "evidence"
        root.mkdir()
        output.mkdir()
        (root / "ordered-input.json").write_text("{}")
        (output / "native-manager.json").write_text(json.dumps({"cleanup": {"serviceAbsent": True, "processAbsent": True}}))
        with self.assertRaisesRegex(RuntimeError, "missing"):
            PROOF.clean_scratch(root, output)
        self.assertTrue(root.exists())
        self.assertFalse((output / "scratch-cleanup.json").exists())

    def test_unmounted_work_drive_cannot_receive_fixture_data(self):
        with (patch.dict(os.environ, {"MESH_SHORT_TMP": "/work/tmp"}), patch.object(Path, "is_mount", return_value=False)):
            with self.assertRaisesRegex(RuntimeError, "not mounted"):
                paths.create()

    def test_unconfirmed_fixture_owner_retains_scratch(self):
        root, output = self.root / "owned", self.root / "evidence"
        root.mkdir()
        output.mkdir()
        (output / "fixture-ownership.json").write_text(json.dumps({"signerAbsent": True, "transitionsAbsent": False}))
        with self.assertRaisesRegex(RuntimeError, "fixture owner cleanup unconfirmed"):
            PROOF.clean_scratch(root, output)
        self.assertTrue(root.exists())

    def test_python_fixture_blocks_remain_at_most_three_deep(self):
        scripts = PROOF_PATH.parent
        files = (PROOF_PATH, Path(__file__), scripts / "test-installed-clock.py", *(scripts / "fixtures" / name for name in
            ("release_worker_groups.py", "release_worker_index.py", "release_worker_completion.py", "noindex_paths.py", "installed_paths.py", "native_services.py", "installed_listener.py", "installed_listener_evidence.py", "installed_listener_controls.py", "installed_attachment_evidence.py", "installed_attachment_controls.py", "installed_continuity.py", "installed_native.py", "fleet_baselines.py",
             "installed_clock.py", "installed_origin.py", "installed_releases.py", "installed_adoption.py", "installed_cohort.py", "installed_catalog_evidence.py", "installed_catalog_controls.py", "installed_chain.py", "installed_waypoint.py", "installed_artifacts.py", "installed_accounts.py", "installed_order.py", "installed_order_host.py", "installed_order_trace.py", "installed_order_trace_controls.py", "installed_fresh.py", "installed_routes.py", "installed_storage.py", "publication_origin.py", "installed_publication.py", "installed_account_run.py", "installed_review_controls.py", "installed_publication_controls.py")))
        for path in files:
            self.assertLessEqual(maximum_depth(ast.parse(path.read_text()), 0), 3, str(path))

    def test_native_tmpdir_is_ignored(self):
        environment = dict(os.environ)
        environment.pop("MESH_SHORT_TMP", None)
        environment["TMPDIR"] = str(self.root / ("long-" * 30))
        with patch.dict(os.environ, environment, clear=True):
            root = paths.create()
        self.addCleanup(shutil.rmtree, root)
        self.assertEqual(root.parent, Path("/tmp").resolve())

    def test_cleanup_retains_root_when_native_stop_is_unconfirmed(self):
        output = self.root / "output"
        output.mkdir()
        retained = self.root / "retained"
        retained.mkdir()
        (retained / "sentinel").write_text("test owner retains this until native cleanup is known")
        (output / "native-manager.json").write_text(json.dumps({"cleanupError": "stop failed"}))
        with self.assertRaisesRegex(RuntimeError, "cleanup unconfirmed"):
            PROOF.clean_scratch(retained, output)
        self.assertFalse((output / "scratch-cleanup.json").exists())
        self.assertTrue((retained / "sentinel").exists())


class FleetBaselinesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="fb-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.platform = {"os": "linux", "arch": "amd64"}
        # Checksum fixtures are deliberately not executable release history.
        self.binary = b"checksum unit-test data; never executed"
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
            member = tarfile.TarInfo("mesh")
            member.size = len(self.binary)
            archive.addfile(member, io.BytesIO(self.binary))
        self.archive = buffer.getvalue()
        self.artifact = {"platform": self.platform, "archive": "mesh_linux_amd64.tar.gz",
                         "sha256": published_releases.digest(self.archive), "binarySha256": published_releases.digest(self.binary)}
        self.manifest = {"schema": 1, "version": "v0.1.249", "artifacts": [self.artifact]}

    def fetch(self, address, _maximum):
        return json.dumps(self.manifest).encode() if address.endswith("mesh-release.json") else self.archive

    def test_fixture_records_exact_observed_pairs_without_inventing_first_updater(self):
        fixture = fleet_baselines.load()
        observed = [(host["name"], host["daemon"], host["helper"], host["helperRunning"]) for host in fixture["hosts"]]
        self.assertEqual(observed, [("pc", "v0.1.249", "v0.1.196", False), ("pi", "v0.1.255", "v0.1.230", True),
                                    ("vps", "v0.1.255", "v0.1.196", True), ("mac", "v0.1.257", "v0.1.196", True)])
        with self.assertRaisesRegex(RuntimeError, "PR-source label cannot replace"):
            fleet_baselines.first_updater(fixture | {"firstPublishedUpdater": None})
        self.assertEqual(fleet_baselines.first_updater(fixture | {"firstPublishedUpdater": "v0.1.999"}), "v0.1.999")

    def test_acquisition_checks_both_hashes_and_inventory_binding(self):
        result = fleet_baselines.acquire(self.root, "v0.1.249", self.platform, self.artifact["binarySha256"], self.fetch)
        self.assertEqual(result["executable"].read_bytes(), self.binary)
        with self.assertRaisesRegex(RuntimeError, "observed installed fleet"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, "a" * 64, self.fetch)

    def test_bad_binary_never_creates_an_executable(self):
        self.artifact["binarySha256"] = "a" * 64
        with self.assertRaisesRegex(RuntimeError, "executable digest mismatch"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)
        self.assertFalse((self.root / "v0.1.249" / "mesh").exists())

    def test_cached_archive_is_rechecked(self):
        fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)
        (self.root / "v0.1.249" / self.artifact["archive"]).write_bytes(b"corrupted cache")
        with self.assertRaisesRegex(RuntimeError, "archive digest mismatch"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)

    def test_wrong_native_platform_and_manifest_version_reject(self):
        with self.assertRaisesRegex(RuntimeError, "one exact native archive"):
            fleet_baselines.acquire(self.root, "v0.1.249", {"os": "darwin", "arch": "arm64"}, fetcher=self.fetch)
        self.manifest["version"] = "v0.1.196"
        with self.assertRaisesRegex(RuntimeError, "schema or version differs"):
            fleet_baselines.acquire(self.root, "v0.1.249", self.platform, fetcher=self.fetch)


class InstalledModeShellTest(unittest.TestCase):
    def test_workflow_modes_pass_exact_arguments_under_nounset(self):
        source = (PROOF_PATH.parent.parent / ".github/workflows/installed-catchup.yml").read_text()
        start = source.index("          mode=(--signed-chain)")
        command = textwrap.dedent(source[start:].split("\n      - name:", 1)[0])
        command = command.replace("python3 scripts/prove-installed-catchup.py", "capture_args")
        prefix = 'set -euo pipefail\ninput="$RUNNER_TEMP/native-worker-input.noindex"\ncapture_args() { printf "%s\\0" "$@"; }\n'
        temporary = tempfile.TemporaryDirectory(dir=os.environ.get("MESH_SHORT_TMP"))
        self.addCleanup(temporary.cleanup)
        (Path(temporary.name) / "cohort-control").mkdir()
        for mode, flags in (("oldest", []), ("chain", ["--signed-chain"]), ("waypoint", ["--format-waypoint"]), ("order", ["--ordered-hosts"])):
            environment = os.environ | {"PROOF_MODE": mode, "SOURCE_HOST": "mac", "RUNNER_TEMP": temporary.name}
            result = subprocess.run(["/bin/bash", "-c", prefix + command], env=environment, capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, (mode, result.stderr))
            arguments = result.stdout.decode().split("\0")[:-1]
            position = arguments.index("--source-host")
            self.assertEqual(arguments[position:], ["--source-host", "mac"] + flags)
            self.assertIn("--worker-registry", arguments)
        self.assertTrue(json.loads((Path(temporary.name) / "cohort-control/fixture-launch.json").read_text())["started"])


class AdoptionCleanupTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="adoption-cleanup-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "output"
        self.output.mkdir()
        self.case = Mock(root=self.root, state=self.root / "state", host={"name": "owned"}, cohort=None, prior=None)

    def test_snapshot_failure_still_records_successful_native_cleanup(self):
        record = {"passed": True}
        with patch.object(installed_adoption, "collect_service_snapshot", side_effect=RuntimeError("empty inspection reply")), \
                patch.object(installed_native, "retain_logs"), self.assertRaisesRegex(RuntimeError, "empty inspection reply"):
            installed_adoption.finish_case(self.case, record, self.output)
        self.case.close.assert_called_once_with()
        self.assertFalse(record["passed"])
        retained = json.loads((self.output / "owned-native-pair.json").read_text())
        self.assertEqual(retained["cleanup"], {"servicesAbsent": True, "processesAbsent": True})
        self.assertEqual(retained["cleanupError"], "empty inspection reply")

    def test_snapshot_failure_preserves_original_case_failure(self):
        primary = installed_clock.ClockExpired("original installed case hang cap")
        self.case.prepare.side_effect = primary
        with patch.object(installed_adoption, "AdoptionCase", return_value=self.case), \
                patch.object(installed_adoption, "collect_service_snapshot", side_effect=RuntimeError("empty inspection reply")), \
                patch.object(installed_native, "retain_logs"), self.assertRaises(installed_clock.ClockExpired) as caught:
            installed_adoption.run_case(self.root, self.output, {"name": "owned"}, {}, None, None, None, "adopt")
        self.assertIs(caught.exception, primary)
        self.assertIn("empty inspection reply", primary.__notes__[0])
        self.case.close.assert_called_once_with()

    def test_failed_native_close_never_records_cleanup_success(self):
        self.case.close.side_effect = RuntimeError("native services remain")
        record = {"passed": True}
        with patch.object(installed_adoption, "collect_service_snapshot"), patch.object(installed_native, "retain_logs"), \
                self.assertRaisesRegex(RuntimeError, "native services remain"):
            installed_adoption.finish_case(self.case, record, self.output)
        retained = json.loads((self.output / "owned-native-pair.json").read_text())
        self.assertNotIn("cleanup", retained)
        self.assertFalse(retained["passed"])
        self.assertEqual(retained["cleanupError"], "native services remain")

    def write_failure_state(self, relative, data):
        path = self.case.state / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return path

    def run_failed_case(self, primary, inspect=None):
        self.case.prepare.side_effect = primary
        with patch.object(installed_adoption, "AdoptionCase", return_value=self.case), \
                patch.object(installed_adoption, "collect_service_snapshot", side_effect=inspect), \
                patch.object(installed_native, "retain_logs"), self.assertRaises(type(primary)) as caught:
            installed_adoption.run_case(self.root, self.output, {"name": "owned"}, {}, None, None, None, "adopt")
        self.assertIs(caught.exception, primary)
        self.case.close.assert_called_once_with()
        return json.loads((self.output / "owned-native-pair.json").read_text())

    def test_failure_state_keeps_exact_bytes_before_inspection_and_teardown(self):
        adoption = b'{ "phase" : "activating" }\n\n'
        installation = b'{"phase":"granted", "unparsed":"\xff"}\n'
        operation = self.write_failure_state("updates/host-adoption.json", adoption)
        journal = self.write_failure_state("update/installation.json", installation)
        self.case.close.side_effect = lambda: shutil.rmtree(self.case.state)
        record = self.run_failed_case(RuntimeError("primary failure"), lambda case: operation.write_bytes(b"mutated"))
        files = record["failureState"]
        self.assertEqual((self.output / files["updates/host-adoption.json"]["path"]).read_bytes(), adoption)
        self.assertEqual((self.output / files["update/installation.json"]["path"]).read_bytes(), installation)
        self.assertEqual(files["update/installation.json"]["bytes"], len(installation))
        self.assertFalse(operation.exists())
        self.assertFalse(journal.exists())

    def test_missing_failure_state_is_explicit(self):
        record = self.run_failed_case(RuntimeError("primary failure"))
        self.assertEqual(record["failureState"], {"updates/host-adoption.json": {"absent": True},
                                                  "update/installation.json": {"absent": True}})

    def test_retention_write_failure_preserves_primary_and_native_cleanup(self):
        self.write_failure_state("updates/host-adoption.json", b'{"phase":"waiting"}')
        primary = installed_clock.ClockExpired("original installed case hang cap")
        with patch.object(Path, "write_bytes", side_effect=OSError("retention disk full")):
            record = self.run_failed_case(primary)
        self.assertIn("retention disk full", primary.__notes__[0])
        self.assertIn("retention disk full", record["failureState"]["updates/host-adoption.json"]["retentionError"])
        self.assertEqual(record["cleanup"], {"servicesAbsent": True, "processesAbsent": True})

    def test_oversized_failure_state_records_error_without_truncating(self):
        self.write_failure_state("updates/host-adoption.json", b"x" * ((1 << 20) + 1))
        record = self.run_failed_case(RuntimeError("primary failure"))
        self.assertIn("1 MiB", record["failureState"]["updates/host-adoption.json"]["retentionError"])
        self.assertFalse((self.output / "owned-failure-host-adoption.json").exists())

    def test_nonregular_failure_state_does_not_block_native_cleanup(self):
        source = self.case.state / "updates/host-adoption.json"
        source.parent.mkdir(parents=True)
        os.mkfifo(source)
        record = self.run_failed_case(RuntimeError("primary failure"))
        self.assertIn("not a regular file", record["failureState"]["updates/host-adoption.json"]["retentionError"])
        self.assertEqual(record["cleanup"], {"servicesAbsent": True, "processesAbsent": True})

    def test_success_does_not_capture_failure_state(self):
        self.case.prepare.return_value = {}
        self.case.start_adoption.return_value = {"pin": "expected"}
        self.case.validate_result.return_value = {}
        self.write_failure_state("updates/host-adoption.json", b"canonical")
        with patch.object(installed_adoption, "AdoptionCase", return_value=self.case), \
                patch.object(installed_adoption, "collect_service_snapshot"), patch.object(installed_native, "retain_logs"):
            installed_adoption.run_case(self.root, self.output, {"name": "owned"}, {}, None, None, None, "adopt")
        record = json.loads((self.output / "owned-native-pair.json").read_text())
        self.assertTrue(record["passed"])
        self.assertNotIn("failureState", record)
        self.assertFalse((self.output / "owned-failure-host-adoption.json").exists())


class ExactCohortInputTest(unittest.TestCase):
    def test_failed_verification_retains_streams_and_original_exception(self):
        cohort = object.__new__(installed_cohort.Cohort)
        cohort.catalog = None
        cohort.record, cohort.state = self.root / "cohort.json", self.root / "state"
        cohort.record.write_text(json.dumps({"workers": [], "observations": []}))
        failure = subprocess.CalledProcessError(1, ["verify"], output="outcome", stderr="inner traceback\n" + "x" * 40000)
        with patch.object(cohort, "environment", return_value={}), patch.object(installed_cohort.subprocess, "run", side_effect=failure):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                cohort.verify("before-install")
        self.assertIs(caught.exception, failure)
        retained = json.loads(cohort.record.read_text())["verificationFailure"]
        self.assertEqual((retained["label"], retained["returncode"]), ("before-install", 1))
        self.assertEqual(retained["stdout"], {"tail": "outcome", "bytes": 7, "truncated": False})
        self.assertEqual(len(retained["stderr"]["tail"].encode()), 32768)
        self.assertTrue(retained["stderr"]["truncated"])
        output = self.root / "export"
        output.mkdir()
        case = SimpleNamespace(root=self.root, host={"name": "owned"}, cohort=cohort, close=lambda: None)
        from fixtures import installed_adoption
        with patch.object(installed_adoption, "collect_service_snapshot"), patch.object(installed_native, "retain_logs"):
            installed_adoption.finish_case(case, {"passed": False}, output)
        self.assertEqual(json.loads((output / "owned-cohort.json").read_text())["verificationFailure"], retained)
        self.assertEqual((output / "owned-cohort.failure.stdout").read_text(), failure.stdout)
        self.assertEqual((output / "owned-cohort.failure.stderr").read_text(), failure.stderr)

    def test_failed_verification_export_keeps_exception_when_record_unavailable(self):
        cohort = object.__new__(installed_cohort.Cohort)
        cohort.catalog = None
        cohort.record, cohort.state = self.root / "absent/cohort.json", self.root / "state"
        failure = subprocess.CalledProcessError(1, ["verify"], output="", stderr="inner failure")
        with patch.object(cohort, "environment", return_value={}), patch.object(installed_cohort.subprocess, "run", side_effect=failure):
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                cohort.verify("before-install")
        self.assertIs(caught.exception, failure)
        self.assertIn("FileNotFoundError", caught.exception.__notes__[0])

    def test_copy_controls_obey_darwin_exclusion(self):
        with patch("fixtures.noindex_paths.platform.system", return_value="Darwin"):
            self.test_owned_copy_preserves_exact_digest_and_borrowed_permissions()
            self.test_changed_bytes_and_escaping_paths_fail_closed()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-", suffix=".noindex", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.binary = self.source / "mesh"
        self.binary.write_bytes(b"checksum fixture only; never executed")
        self.binary.chmod(0o640)
        self.platform = {"os": "linux", "arch": "amd64"}
        self.record = {"version": "v0.1.249", "protocol": 1, "binary": "mesh",
            "artifact": {"platform": self.platform, "binarySha256": published_releases.digest(self.binary.read_bytes())}}
        self.registry = self.source / "workers.json"
        self.registry.write_text(json.dumps({"schema": 1, "platform": self.platform, "workers": [self.record]}))

    def test_owned_copy_preserves_exact_digest_and_borrowed_permissions(self):
        staged = installed_cohort.stage(self.registry, self.root / "owned", self.platform)
        selected = json.loads(staged.read_text())["workers"]
        binary = staged.parent / selected[0]["binary"]
        self.assertEqual(binary.read_bytes(), self.binary.read_bytes())
        binary.chmod(0o755)
        self.assertEqual(self.binary.stat().st_mode & 0o777, 0o640)

    def test_changed_bytes_and_escaping_paths_fail_closed(self):
        self.binary.write_bytes(b"changed fixture bytes")
        with self.assertRaisesRegex(RuntimeError, "published identity"):
            installed_cohort.stage(self.registry, self.root / "changed", self.platform)
        with self.assertRaisesRegex(RuntimeError, "relative beneath"):
            installed_cohort.copy_record(self.registry, self.root, self.record | {"binary": "../outside"})

    def test_wrong_platform_does_not_stage_binaries(self):
        with self.assertRaisesRegex(RuntimeError, "schema/platform"):
            installed_cohort.stage(self.registry, self.root / "wrong", {"os": "darwin", "arch": "arm64"})
        self.assertFalse((self.root / "wrong").exists())


@unittest.skipUnless(os.environ.get("MESH_TEST_NATIVE_MANAGER") == "1", "native control explicitly selected")
class NativeServiceTest(unittest.TestCase):
    def setUp(self):
        self.root = paths.create()
        self.addCleanup(self.clean)
        self.report = self.root / "evidence.json"

    def clean(self):
        if not self.report.exists():
            raise RuntimeError(f"native cleanup record missing; test owner retains {self.root}")
        cleanup = json.loads(self.report.read_text()).get("cleanup", {})
        if not cleanup.get("serviceAbsent") or not cleanup.get("processAbsent"):
            raise RuntimeError(f"native cleanup unconfirmed; test owner retains {self.root}")
        shutil.rmtree(self.root)

    def test_real_manager_pid_and_stop(self):
        evidence = native_services.positive_control(self.root, self.report)
        self.assertTrue(evidence["passed"])
        self.assertTrue(evidence["cleanup"]["serviceAbsent"])
        self.assertTrue(evidence["cleanup"]["processAbsent"])

    def test_exception_after_launch_stops_real_service(self):
        with patch.object(native_services.json, "loads", side_effect=RuntimeError("injected after launch")):
            with self.assertRaisesRegex(RuntimeError, "injected after launch"):
                native_services.positive_control(self.root, self.report)
        evidence = json.loads(self.report.read_text())
        self.assertFalse(evidence["passed"])
        self.assertTrue(evidence["cleanup"]["serviceAbsent"])
        self.assertTrue(evidence["cleanup"]["processAbsent"])


class RouteHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        try:
            while not self.server.stopping.is_set():
                self.wfile.write((str(self.server.listener_pid) + "\n").encode())
                self.wfile.flush()
                self.server.stopping.wait(0.02)
        except OSError:
            pass

    def log_message(self, *_arguments):
        pass


class RouteObservationTest(unittest.TestCase):
    def server(self, port=0, pid=None):
        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), RouteHandler)
        server.stopping = threading.Event()
        server.listener_pid = os.getpid() if pid is None else pid
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        self.addCleanup(self.stop_server, server, thread)
        thread.start()
        return server, thread

    def stop_server(self, server, thread):
        server.stopping.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)

    def test_live_stream_loss_and_reconnection_is_measured(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        server, thread = self.server()
        port = server.server_port
        observer = installed_routes.Observer(port, {"pid": os.getpid()}, Path(temporary.name) / "route.json")
        self.addCleanup(observer.close)
        observer.start()
        self.stop_server(server, thread)
        native_services.wait_for(lambda: not observer.snapshot()["connected"], "observer missed the actual TCP stream loss")
        self.server(port)
        observer.recovered()
        value = observer.snapshot()
        self.assertGreater(value["pulses"], 1)
        self.assertGreater(len(value["gaps"]), 0)
        self.assertTrue(all(gap["seconds"] >= 0 for gap in value["gaps"]))

    def test_another_listener_is_rejected(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        server, _thread = self.server(pid=os.getpid() + 1)
        observer = installed_routes.Observer(server.server_port, {"pid": os.getpid()}, Path(temporary.name) / "route.json")
        self.addCleanup(observer.close)
        with self.assertRaisesRegex(RuntimeError, "another listener"):
            observer.start()


class PublicationConsumerTest(unittest.TestCase):
    def test_non_main_publication_is_rejected_before_acquisition(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REF": "refs/pull/435/merge"}):
            with self.assertRaisesRegex(RuntimeError, "main-only"):
                installed_publication.require_main()

    def test_transport_serves_the_original_asset_bytes_over_verified_tls(self):
        temporary = tempfile.TemporaryDirectory(prefix="ipub-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        release = root / "releases/v0.1.1"
        release.mkdir(parents=True)
        actual = b"publication transport control; never a signed release proof"
        (release / "asset.json").write_bytes(actual)
        (release / "draft.json").write_text(json.dumps({"assets": [{"name": "asset.json", "size": len(actual),
            "digest": "sha256:" + hashlib.sha256(actual).hexdigest()}]}))
        origin = publication_origin.Origin(root, release.parent, root / "unused-reader")
        self.addCleanup(origin.close)
        origin.start()
        data = publication_request(origin, "/ShaulLavo/mesh/releases/download/v0.1.1/asset.json")
        self.assertEqual(data, actual)
        self.assertFalse(origin.close()["testSignatureRootUsed"])

    def test_candidate_origin_denies_loose_members_and_network_fallback_over_tls(self):
        temporary = tempfile.TemporaryDirectory(prefix="ipub-layout-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        release = root / "releases/v0.1.1"
        release.mkdir(parents=True)
        actual = b"actual opaque bundle bytes"
        (release / "mesh-documents.tar.gz").write_bytes(actual)
        (release / "mesh-update.json").write_text("must never be served as a loose candidate asset")
        (release / "draft.json").write_text(json.dumps({"assets": [{"name": "mesh-documents.tar.gz", "size": len(actual),
            "digest": "sha256:" + hashlib.sha256(actual).hexdigest()}]}))
        origin = publication_origin.Origin(root, release.parent, root / "unused-reader")
        self.addCleanup(origin.close)
        origin.start()
        with patch.object(publication_origin.published_releases, "download", side_effect=AssertionError("candidate network fallback")) as download:
            self.assertEqual(publication_request(origin, "/ShaulLavo/mesh/releases/download/v0.1.1/mesh-documents.tar.gz"), actual)
            with self.assertRaisesRegex(RuntimeError, "exact asset"):
                publication_request(origin, "/ShaulLavo/mesh/releases/download/v0.1.1/mesh-update.json")
            with self.assertRaisesRegex(RuntimeError, "exact asset"):
                publication_request(origin, "/ShaulLavo/mesh/releases/download/v0.1.1/missing.json")
        download.assert_not_called()

    def test_owned_activation_port_fault_releases_at_actual_rollback_phase(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        state = Path(temporary.name)
        (state / "update").mkdir()
        previous = socket.socket()
        self.addCleanup(previous.close)
        previous.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        previous.bind(("127.0.0.1", 0))
        previous.listen(1)
        case = SimpleNamespace(state=state, port=previous.getsockname()[1])
        journal = state / "update/installation.json"
        journal.write_text(json.dumps({"phase": "activating", "request": {"manifest": {"version": "control-only"}}}))
        fault = installed_publication.PortFault(case, "control-only")
        self.addCleanup(fault.close)
        fault.start()
        self.assertFalse(fault.bound)
        previous.close()
        native_services.wait_for(lambda: fault.bound, "owned activation port was not acquired")
        temporary_journal = journal.with_suffix(".tmp")
        temporary_journal.write_text(json.dumps({"phase": "rolling_back"}))
        temporary_journal.replace(journal)
        native_services.wait_for(lambda: fault.released, "owned activation port was not released for rollback")
        fault.close()
        self.assertEqual(fault.error, "")

    def test_public_trust_tls_is_tunneled_without_proxy_termination(self):
        temporary = tempfile.TemporaryDirectory(prefix="iptuf-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        origin = publication_origin.Origin(root, root / "releases", root / "unused")
        self.addCleanup(origin.close)
        origin.start()
        upstream = root / "upstream"
        upstream.mkdir()
        publication_origin.tls.fixture_certificates(upstream)
        server = socketserver.TCPServer(("127.0.0.1", 0), TrustTLSHandler)
        server.context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server.context.load_cert_chain(upstream / "server.pem", upstream / "server.key")
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        self.addCleanup(stop_trust_server, server, thread)
        thread.start()
        connect = socket.create_connection
        def redirected(address, *args, **kwargs):
            return connect(server.server_address if address == ("tuf-repo-cdn.sigstore.dev", 443) else address, *args, **kwargs)
        with patch.object(publication_origin.socket, "create_connection", side_effect=redirected):
            context = ssl.create_default_context(cafile=str(upstream / "cert.pem"))
            context.verify_flags |= ssl.VERIFY_X509_STRICT
            data = trust_tunnel_request(origin, context)
        self.assertEqual(data, b"public TLS remained end to end")
        self.assertEqual(origin.trust_connections, 1)


class TrustTLSHandler(socketserver.BaseRequestHandler):
    def handle(self):
        with self.server.context.wrap_socket(self.request, server_side=True) as secure:
            secure.sendall(b"public TLS remained end to end")


def stop_trust_server(server, thread):
    server.shutdown()
    server.server_close()
    thread.join(timeout=2)


def trust_tunnel_request(origin, context):
    with socket.create_connection(("127.0.0.1", origin.server.server_port), timeout=5) as connection:
        connection.sendall(b"CONNECT tuf-repo-cdn.sigstore.dev:443 HTTP/1.1\r\nHost: tuf-repo-cdn.sigstore.dev:443\r\n\r\n")
        read_connect_response(connection)
        with context.wrap_socket(connection, server_hostname="github.com") as secure:
            return secure.recv(128)


class NativeZIPPreflightTest(unittest.TestCase):
    def test_inventory_reads_only_bounded_directory_ranges(self):
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_STORED) as package:
            package.writestr("retained/worker", b"x" * (1 << 20))
        raw = buffer.getvalue()
        remote = installed_storage.RemoteZIP("https://unused.fixture.invalid/unit-input", len(raw))
        with patch.object(remote, "fetch", side_effect=lambda _request, size, _end: raw[remote.position:remote.position + size]):
            with zipfile.ZipFile(remote) as package:
                self.assertEqual(sum(item.file_size for item in package.infolist()), 1 << 20)
        self.assertLess(remote.transferred, 128 << 10)


def publication_request(origin, path):
    context = ssl.create_default_context(cafile=str(origin.bundle))
    context.verify_flags |= ssl.VERIFY_X509_STRICT
    with socket.create_connection(("127.0.0.1", origin.server.server_port), timeout=5) as connection:
        connection.sendall(b"CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n")
        read_connect_response(connection)
        with context.wrap_socket(connection, server_hostname="github.com") as secure:
            secure.sendall(("GET " + path + " HTTP/1.0\r\nHost: github.com\r\n\r\n").encode())
            return read_publication_response(secure)


def read_publication_response(secure):
    response = http.client.HTTPResponse(secure)
    response.begin()
    if response.status != 200:
        raise RuntimeError("publication transport did not serve its exact asset")
    return response.read()


def read_connect_response(connection):
    data = b""
    while not data.endswith(b"\r\n\r\n"):
        data += connection.recv(1)
        if len(data) > 8192:
            raise RuntimeError("publication proxy header exceeded its control bound")
    if not data.startswith(b"HTTP/1.0 200"):
        raise RuntimeError("publication proxy rejected the approved release host")


class ChainObservationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="chain-observation-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source, self.middle, self.final = installed_releases.VERSIONS
        self.builds = {version: {"version": version, "digest": str(number) * 64}
                       for number, version in enumerate(installed_releases.VERSIONS, 1)}
        self.case = object.__new__(installed_adoption.AdoptionCase)
        self.case.root, self.case.state = self.root, self.root / "state"
        self.case.releases = SimpleNamespace(builds=self.builds)
        self.case.fault = self.root / "fault.json"
        self.case.write_fault({"holdHelper": [self.middle], "daemon": []})
        self.case.inspect_retained = Mock()
        self.case.cohort = SimpleNamespace(verify=Mock(return_value=["retained-worker"]))
        self.record = {"hops": []}
        self.ledger = {"entries": [{"version": self.middle, "origin": "host"}]}
        self.image = {"sha256": self.builds[self.middle]["digest"]}
        self.socket_factory = self.enter_patch(patch.object(installed_native.controls.socket, "socket"))
        self.ledger_reader = self.enter_patch(patch.object(installed_chain, "read_ledger", return_value=self.ledger))
        self.helper_reader = self.enter_patch(patch.object(installed_chain, "ready_helper", return_value=self.image))
        self.checkpoint = self.enter_patch(patch.object(installed_chain.installed_transport_lifecycle, "checkpoint"))

    def enter_patch(self, context):
        value = context.start()
        self.addCleanup(context.stop)
        return value

    def connection(self, version=None, error=None):
        connection = MagicMock()
        connection.__enter__.return_value = connection
        connection.connect.side_effect = error
        payload = json.dumps({"type": "host.info.result", "host": {"build": self.builds[version or self.source]}}).encode()
        connection.recv.side_effect = [b"\x01" + struct.pack(">I", len(payload)), payload]
        return connection

    def observe(self):
        return installed_chain.observe_hop(self.case, self.record, self.middle)

    def assert_unobserved(self):
        self.assertEqual(self.record, {"hops": []})
        self.case.inspect_retained.assert_not_called()
        self.case.cohort.verify.assert_not_called()
        self.checkpoint.assert_not_called()
        self.assertEqual(json.loads(self.case.fault.read_text())["holdHelper"], [self.middle])

    def test_source_response_is_not_followed_by_a_second_connect_during_stop(self):
        source = self.connection()
        missing = self.connection(error=FileNotFoundError(2, "daemon.sock removed by stop"))
        self.socket_factory.side_effect = [source, missing]
        self.assertFalse(self.observe())
        self.socket_factory.assert_called_once_with(socket.AF_UNIX)
        source.connect.assert_called_once_with(str(self.case.state / "daemon.sock"))
        missing.connect.assert_not_called()
        self.ledger_reader.assert_not_called()
        self.assert_unobserved()

    def test_intermediate_uses_the_same_response_and_runs_every_checkpoint(self):
        connection = self.connection(self.middle)
        self.socket_factory.side_effect = [connection, self.connection(error=FileNotFoundError(2, "stopped"))]
        self.assertTrue(self.observe())
        self.socket_factory.assert_called_once_with(socket.AF_UNIX)
        self.case.inspect_retained.assert_called_once_with()
        self.checkpoint.assert_called_once_with(self.case, self.middle, "observed intermediate hop")
        self.case.cohort.verify.assert_called_once_with("native-hop-" + self.middle)
        self.assertEqual(self.record["hops"], [{"version": self.middle, "helper": self.image, "workers": ["retained-worker"]}])
        self.assertEqual(json.loads(self.case.fault.read_text()), {"daemon": []})

    def test_missing_socket_is_only_an_unobserved_sample(self):
        self.socket_factory.return_value = self.connection(error=FileNotFoundError(2, "not yet listening"))
        self.assertFalse(self.observe())
        self.socket_factory.assert_called_once_with(socket.AF_UNIX)
        self.ledger_reader.assert_not_called()
        self.assert_unobserved()

    def test_other_connect_errors_propagate(self):
        for error in (PermissionError("denied"), ConnectionRefusedError("refused"), TimeoutError("expired")):
            self.check_connect_error(error)

    def check_connect_error(self, error):
        self.socket_factory.return_value = self.connection(error=error)
        with self.assertRaises(type(error)):
            self.observe()
        self.assert_unobserved()

    def test_empty_header_is_only_an_unobserved_sample(self):
        connection = self.connection()
        connection.recv.side_effect = [b""]
        self.socket_factory.return_value = connection
        self.assertFalse(self.observe())
        self.socket_factory.assert_called_once_with(socket.AF_UNIX)
        connection.recv.assert_called_once_with(5)
        connection.__exit__.assert_called_once()
        self.ledger_reader.assert_not_called()
        self.assert_unobserved()

    def test_receive_classifies_only_zero_byte_header_close(self):
        connection = Mock()
        connection.recv.return_value = b""
        with self.assertRaises(installed_native.controls.EmptyResponse):
            installed_native.controls.receive(connection, 5, response_header=True)
        with self.assertRaises(RuntimeError) as caught:
            installed_native.controls.receive(connection, 5)
        self.assertNotIsInstance(caught.exception, installed_native.controls.EmptyResponse)

    def test_zero_byte_header_reset_is_only_an_unobserved_sample(self):
        connection = self.connection()
        connection.recv.side_effect = ConnectionResetError(104, "daemon replaced")
        self.socket_factory.side_effect = [connection, self.connection(self.middle)]
        self.assertFalse(self.observe())
        self.assert_unobserved()
        self.assertTrue(self.observe())
        self.checkpoint.assert_called_once_with(self.case, self.middle, "observed intermediate hop")
        self.assertEqual(len(self.record["hops"]), 1)

    def test_direct_control_keeps_preheader_reset_as_a_failure_with_its_cause(self):
        connection = self.connection()
        error = ConnectionResetError(104, "daemon replaced")
        connection.recv.side_effect = error
        self.socket_factory.return_value = connection
        with self.assertRaises(installed_native.controls.EmptyResponse) as caught:
            self.case.control("host.info")
        self.assertIs(caught.exception.__cause__, error)
        self.assertIn("0 response bytes", error.__notes__[0])

    def test_partial_header_and_payload_resets_still_fail(self):
        header = b"\x01" + struct.pack(">I", 2)
        for size in range(1, len(header)):
            self.check_frame_error([header[:size], ConnectionResetError(104, "reset")], ConnectionResetError)
        self.check_frame_error([header, ConnectionResetError(104, "reset")], ConnectionResetError)
        self.check_frame_error([header, b"{", ConnectionResetError(104, "reset")], ConnectionResetError)

    def test_connect_and_send_resets_still_fail(self):
        self.check_connect_error(ConnectionResetError(104, "reset"))
        self.check_exchange_error("sendall", ConnectionResetError(104, "reset"))

    def test_partial_header_and_body_closes_propagate(self):
        header = b"\x01" + struct.pack(">I", 2)
        for size in range(1, len(header)):
            self.check_frame_error([header[:size], b""], RuntimeError)
        self.check_frame_error([header, b""], RuntimeError)
        self.check_frame_error([header, b"{", b""], RuntimeError)

    def test_malformed_frames_propagate(self):
        self.check_frame_error([b"\x02" + struct.pack(">I", 2)], RuntimeError)
        self.check_frame_error([b"\x01" + struct.pack(">I", (4 << 20) + 1)], RuntimeError)
        self.check_frame_error([b"\x01" + struct.pack(">I", 0)], json.JSONDecodeError)
        self.check_frame_error([b"\x01" + struct.pack(">I", 1), b"{"], json.JSONDecodeError)
        self.check_frame_error([b"\x01" + struct.pack(">I", 1), b"\xff"], UnicodeDecodeError)

    def check_frame_error(self, chunks, expected):
        connection = self.connection()
        connection.recv.side_effect = chunks
        self.socket_factory.return_value = connection
        with self.assertRaises(expected) as caught:
            self.observe()
        self.assertNotIsInstance(caught.exception, installed_native.controls.EmptyResponse)
        self.ledger_reader.assert_not_called()
        self.assert_unobserved()

    def test_send_and_receive_errors_propagate(self):
        for operation in ("sendall", "recv"):
            self.check_exchange_error(operation, OSError("exchange failed"))
            self.check_exchange_error(operation, RuntimeError("control failed"))

    def check_exchange_error(self, operation, error):
        connection = self.connection()
        getattr(connection, operation).side_effect = error
        self.socket_factory.return_value = connection
        with self.assertRaises(type(error)) as caught:
            self.observe()
        self.assertIs(caught.exception, error)
        self.assert_unobserved()

    def test_empty_then_complete_response_still_requires_all_target_evidence(self):
        empty = self.connection()
        empty.recv.side_effect = [b""]
        missing = self.connection(error=FileNotFoundError(2, "socket absent"))
        self.socket_factory.side_effect = [missing, empty, self.connection(self.source),
                                         self.connection(self.middle), self.connection(self.middle), self.connection(self.middle)]
        for _ in range(3):
            self.assertFalse(self.observe())
            self.assert_unobserved()
        self.ledger["entries"] = []
        self.assertFalse(self.observe())
        self.assert_unobserved()
        self.helper_reader.assert_not_called()
        self.ledger["entries"] = [{"version": self.middle, "origin": "host"}]
        self.helper_reader.return_value = None
        self.assertFalse(self.observe())
        self.assert_unobserved()
        self.helper_reader.return_value = self.image
        self.assertTrue(self.observe())
        self.assertEqual(self.socket_factory.call_count, 6)
        self.case.inspect_retained.assert_called_once_with()
        self.checkpoint.assert_called_once_with(self.case, self.middle, "observed intermediate hop")
        self.case.cohort.verify.assert_called_once_with("native-hop-" + self.middle)
        self.assertEqual(self.record["hops"], [{"version": self.middle, "helper": self.image, "workers": ["retained-worker"]}])
        self.assertEqual(json.loads(self.case.fault.read_text()), {"daemon": []})

    def test_proof_errors_propagate_without_accepting_a_hop(self):
        for boundary in (self.ledger_reader, self.helper_reader, self.case.inspect_retained,
                         self.checkpoint, self.case.cohort.verify):
            self.check_proof_error(boundary)

    def check_proof_error(self, boundary):
        error = RuntimeError("real proof failed")
        boundary.side_effect = error
        self.socket_factory.return_value = self.connection(self.middle)
        with self.assertRaises(RuntimeError) as caught:
            self.observe()
        self.assertIs(caught.exception, error)
        self.assertEqual(self.record, {"hops": []})
        self.assertEqual(json.loads(self.case.fault.read_text())["holdHelper"], [self.middle])
        boundary.side_effect = None

    def test_empty_response_type_is_not_caught_from_proof_checks(self):
        self.socket_factory.return_value = self.connection(self.middle)
        self.helper_reader.side_effect = installed_native.controls.EmptyResponse("proof error")
        with self.assertRaises(installed_native.controls.EmptyResponse):
            self.observe()
        self.assert_unobserved()

    def test_missing_helper_evidence_is_not_a_missing_socket(self):
        self.socket_factory.return_value = self.connection(self.middle)
        self.helper_reader.side_effect = FileNotFoundError(2, "installed.json missing")
        with self.assertRaises(FileNotFoundError):
            self.observe()
        self.assert_unobserved()

    def test_missing_socket_cannot_extend_the_existing_chain_bound(self):
        self.check_chain_bound(self.connection(error=FileNotFoundError(2, "still absent")))

    def test_empty_header_cannot_extend_the_existing_chain_bound(self):
        connection = self.connection()
        connection.recv.side_effect = [b""]
        self.check_chain_bound(connection)

    def test_empty_header_cannot_restart_the_absolute_exchange_deadline(self):
        connection = self.connection()
        connection.recv.side_effect = [b""]
        self.socket_factory.return_value = connection
        with patch.object(installed_clock.time, "monotonic_ns", return_value=0) as now, \
                installed_clock.handoff(installed_clock.SECOND):
            self.assertFalse(self.observe())
            self.assert_unobserved()
            self.assertTrue(all(call.args[0] == 1 for call in connection.settimeout.call_args_list))
            now.return_value = installed_clock.SECOND
            with self.assertRaises(installed_clock.ClockExpired):
                self.observe()
        self.socket_factory.assert_called_once_with(socket.AF_UNIX)
        self.assert_unobserved()

    def check_chain_bound(self, connection):
        self.socket_factory.return_value = connection
        with patch.object(installed_chain, "read_journal", return_value={}), \
                patch.object(installed_chain.time, "monotonic", side_effect=[0, 0, installed_clock.HANG_SECONDS]), \
                patch.object(installed_chain.time, "sleep") as pause:
            with self.assertRaisesRegex(RuntimeError, "signed native chain did not finish"):
                installed_chain.await_chain(self.case, self.record, self.middle, self.final, False)
        pause.assert_called_once_with(0.05)
        self.assert_unobserved()

    def test_final_graded_host_info_still_rejects_a_missing_daemon(self):
        self.socket_factory.return_value = self.connection(error=FileNotFoundError(2, "final daemon absent"))
        self.ledger["entries"].append({"version": self.final, "origin": "host"})
        with self.assertRaises(FileNotFoundError):
            installed_chain.validate_terminal(self.case, self.record, {"phase": "committed"},
                                             {self.middle}, self.middle, self.final, False)
        self.assertNotIn("running", self.record)

    def test_final_graded_host_info_still_rejects_an_empty_response(self):
        connection = self.connection()
        connection.recv.side_effect = [b""]
        self.socket_factory.return_value = connection
        self.ledger["entries"].append({"version": self.final, "origin": "host"})
        with self.assertRaises(installed_native.controls.EmptyResponse):
            installed_chain.validate_terminal(self.case, self.record, {"phase": "committed"},
                                             {self.middle}, self.middle, self.final, False)
        self.assertNotIn("running", self.record)


class EmptyLedgerTest(unittest.TestCase):
    def test_before_first_commit_has_no_entries(self):
        empty = json.loads('{"entries":null,"state":{"schema":1,"phase":"idle"},"failed":[],"peers":null,"dependents":null}')
        value = installed_chain.normalize_ledger(empty)
        self.assertEqual(value["entries"], [])
        self.assertEqual(value["failed"], [])
        self.assertEqual(value["state"]["phase"], "idle")

    def test_malformed_collection_does_not_become_an_empty_commit(self):
        with self.assertRaisesRegex(RuntimeError, "malformed"):
            installed_chain.normalize_ledger({"entries": {"version": "invented"}})


class FailingTerminal:
    def close(self):
        raise RuntimeError("injected terminal I/O failure")


@unittest.skipUnless(os.environ.get("MESH_TEST_NATIVE_MANAGER") == "1", "requires actual native user manager")
class InstalledCleanupTest(unittest.TestCase):
    def test_terminal_failure_still_stops_actual_listener_and_both_units(self):
        temporary = tempfile.TemporaryDirectory(prefix="ic-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        managers = [self.start_manager(root, index) for index in range(2)]
        listener = subprocess.Popen(["/bin/sleep", "30"])
        self.addCleanup(self.stop_process, listener)
        case = object.__new__(installed_native.Installation)
        case.route_observer, case.stream, case.response = None, None, None
        case.listener, case.worker, case.shell = {"pid": listener.pid}, None, None
        case.terminal, case.cleaned = FailingTerminal(), False
        case.process_stamps = {listener.pid: installed_native.process_stamp(listener.pid)}
        case.units = SimpleNamespace(close=lambda: installed_native.cleanup_all([manager.close for manager in managers]))
        case.verify_session_stopped = lambda: listener.wait(timeout=5)
        with self.assertRaisesRegex(RuntimeError, "injected terminal I/O"):
            case.close()
        self.assertIsNotNone(listener.poll())
        self.assertTrue(all(not manager.exists() and not native_services.alive(manager.pid) for manager in managers))

    def start_manager(self, root, index):
        directory = root / str(index)
        directory.mkdir()
        manager = native_services.Manager(directory)
        self.addCleanup(manager.close)
        marker = directory / "started.json"
        manager.start(marker)
        native_services.wait_for(marker.exists, "actual cleanup control did not start")
        manager.pid = manager.manager_pid()
        return manager

    def stop_process(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)


class AccountIsolationTest(unittest.TestCase):
    def test_account_creation_is_disabled_outside_disposable_hosted_root(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), patch.object(installed_accounts, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "disposable GitHub-hosted"):
                installed_accounts.Accounts(Path("/unused"), Path("/unused"))
        command.assert_not_called()

    def test_preexisting_uid_resources_are_never_stopped(self):
        accounts = object.__new__(installed_accounts.Accounts)
        record = {"name": "test-only", "uid": 12345, "home": "/unused", "created": True, "ownershipVerified": False}
        with patch.object(accounts, "owned"), patch.object(accounts, "persist"), patch.object(installed_accounts, "account_exists", side_effect=[SimpleNamespace(), None]):
            with patch.object(installed_accounts, "command") as command, patch.object(accounts, "stop_processes") as stop:
                accounts.remove_account(record)
        command.assert_called_once_with(["/usr/sbin/userdel", "test-only"])
        stop.assert_not_called()
        self.assertTrue(record["absent"])


class ArtifactPinTest(unittest.TestCase):
    def test_archive_controls_obey_darwin_exclusion(self):
        with patch("fixtures.noindex_paths.platform.system", return_value="Darwin"):
            self.test_pinned_id_archive_is_verified_before_extraction()
            self.test_pinned_archive_rejects_nonlocal_members()

    def inputs(self):
        jobs = [{"name": "Native proofs / prepare (" + key.replace("-", "/") + ", runner)",
                 "status": "completed", "conclusion": "success"} for key in installed_artifacts.KEYS]
        return [{"head_sha": "b" * 40}, {"jobs": jobs}, {"artifacts": [
            {"id": index, "name": "update-native-" + key, "expired": False, "size_in_bytes": 1234, "digest": "sha256:" + "b" * 64,
             "workflow_run": {"id": 123}, "created_at": "2026-11-19T10:00:00Z"}
            for index, key in enumerate(installed_artifacts.KEYS, 1)]}]

    def serve(self, values):
        """Answers the run, its jobs and each name listing; the whole run's artifacts are never listed."""
        meta, jobs, store = values[0], values[1], values[2]["artifacts"]
        def api(endpoint):
            if "/jobs?" in endpoint:
                return jobs
            if "/artifacts?name=" in endpoint:
                name = endpoint.split("?name=", 1)[1].split("&", 1)[0]
                return {"artifacts": [item for item in store if item["name"] == name]}
            self.assertNotIn("/artifacts", endpoint, "verify_set must list each native name, not the whole run")
            return meta
        return api

    def test_a_rerun_upload_of_one_platform_replaces_the_older_one(self):
        values = self.inputs()
        rerun = values[2]["artifacts"][0] | {"id": 99, "created_at": "2026-11-19T11:00:00Z"}
        values[2]["artifacts"].append(rerun)
        with patch.object(installed_artifacts, "api", side_effect=self.serve(values)):
            result = installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        self.assertEqual(result["artifacts"][installed_artifacts.KEYS[0]]["id"], 99)

    def test_an_upload_repeated_by_a_shifting_listing_counts_once(self):
        values = self.inputs()
        values[2]["artifacts"].append(dict(values[2]["artifacts"][0]))
        with patch.object(installed_artifacts, "api", side_effect=self.serve(values)):
            result = installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        self.assertEqual(result["artifacts"][installed_artifacts.KEYS[0]]["id"], values[2]["artifacts"][0]["id"])

    def test_named_listing_queries_one_name_and_keeps_paging(self):
        full = [{"id": number} for number in range(installed_artifacts.PAGE_SIZE)]
        with patch.object(installed_artifacts, "api", side_effect=[{"artifacts": full}, {"artifacts": [{"id": 99}]}]) as api:
            listed = installed_artifacts.named_artifacts("ShaulLavo/mesh", 123, "installed prepared/x")
        self.assertEqual(len(listed), installed_artifacts.PAGE_SIZE + 1)
        self.assertEqual([call.args[0] for call in api.call_args_list],
                         [f"repos/ShaulLavo/mesh/actions/runs/123/artifacts?name=installed%20prepared%2Fx&per_page={installed_artifacts.PAGE_SIZE}&page={page}"
                          for page in (1, 2)])

    def test_tied_or_foreign_uploads_fail_the_native_set(self):
        for change in (lambda items: items.append(items[0] | {"id": 99}),
                       lambda items: items[0].update(workflow_run={"id": 124}),
                       lambda items: items[0].update(workflow_run=None)):
            values = self.inputs()
            change(values[2]["artifacts"])
            with self.subTest(change), patch.object(installed_artifacts, "api", side_effect=self.serve(values)), self.assertRaises(RuntimeError):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

    def test_complete_three_platform_set_is_pinned(self):
        with patch.object(installed_artifacts, "api", side_effect=self.serve(self.inputs())):
            result = installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        self.assertEqual(set(result["artifacts"]), set(installed_artifacts.KEYS))
        self.assertEqual(result["run"], 123)
        self.assertEqual(result["source"], "a" * 40)
        self.assertEqual(result["runHead"], "b" * 40)

    def test_pending_preparation_and_missing_or_expired_platform_fail(self):
        values = self.inputs()
        values[1]["jobs"][0].update(status="in_progress", conclusion=None)
        with patch.object(installed_artifacts, "api", side_effect=self.serve(values)):
            with self.assertRaisesRegex(RuntimeError, "not completed"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        values = self.inputs()
        values[2]["artifacts"][-1]["expired"] = True
        with patch.object(installed_artifacts, "api", side_effect=self.serve(values)):
            with self.assertRaisesRegex(RuntimeError, "darwin-arm64"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

    def gh_result(self, code, stdout=b"", stderr=b""):
        return subprocess.CompletedProcess(["gh", "api"], code, stdout, stderr)

    def test_pages_use_small_pages_until_a_partial_page(self):
        size = installed_artifacts.PAGE_SIZE
        batches = [{"jobs": list(range(size))}, {"jobs": list(range(size))}, {"jobs": [0]}]
        with patch.object(installed_artifacts, "api", side_effect=batches) as api:
            result = installed_artifacts.pages("repos/o/r/actions/runs/7/jobs", "jobs")
        self.assertEqual(len(result), 2 * size + 1)
        self.assertEqual([call.args[0] for call in api.call_args_list],
                         [f"repos/o/r/actions/runs/7/jobs?per_page={size}&page={page}" for page in (1, 2, 3)])

    def test_api_retries_server_errors_then_returns(self):
        outcomes = [self.gh_result(1, stderr=b"gh: HTTP 502: Server Error (https://api.github.com/x)"),
                    self.gh_result(1, stderr=b"HTTP 503: Service Unavailable"),
                    self.gh_result(0, stdout=b'{"jobs": []}')]
        with patch.object(installed_artifacts.subprocess, "run", side_effect=outcomes) as run, \
                patch.object(installed_artifacts.time, "sleep") as sleep:
            self.assertEqual(installed_artifacts.api("repos/o/r/actions/runs/7/jobs"), {"jobs": []})
        self.assertEqual(run.call_count, 3)
        self.assertEqual([call.args[0] for call in sleep.call_args_list], list(installed_artifacts.SERVER_ERROR_RETRIES[:2]))

    def test_api_does_not_retry_client_errors(self):
        with patch.object(installed_artifacts.subprocess, "run", return_value=self.gh_result(1, stderr=b"HTTP 404: Not Found")) as run, \
                patch.object(installed_artifacts.time, "sleep") as sleep:
            with self.assertRaises(subprocess.CalledProcessError):
                installed_artifacts.api("repos/o/r/actions/runs/7/jobs")
        run.assert_called_once()
        sleep.assert_not_called()

    def test_api_gives_up_after_bounded_server_error_retries(self):
        failure = self.gh_result(1, stderr=b"HTTP 502: Server Error")
        with patch.object(installed_artifacts.subprocess, "run", return_value=failure) as run, \
                patch.object(installed_artifacts.time, "sleep"):
            with self.assertRaises(subprocess.CalledProcessError) as raised:
                installed_artifacts.api("repos/o/r/actions/runs/7/jobs")
        self.assertEqual(run.call_count, len(installed_artifacts.SERVER_ERROR_RETRIES) + 1)
        self.assertIn(b"HTTP 502", raised.exception.stderr)

    def test_missing_archive_digest_is_rejected_during_intake(self):
        values = self.inputs()
        values[2]["artifacts"][0]["digest"] = None
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "pinned archive SHA-256"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

    def archive(self, value, name="tools/control"):
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w") as package:
            package.writestr(name, value)
        return buffer.getvalue()

    def download_control(self, root, expected, received):
        destination = root / "input"
        destination.mkdir()
        artifact = {"id": 41, "name": "same-mutable-name", "size": len(expected),
                    "digest": "sha256:" + published_releases.digest(expected)}
        pin = root / "pin.json"
        pin.write_text(json.dumps({"artifacts": {"linux-amd64": artifact}}))
        with patch.object(installed_storage, "archive_url", return_value="https://artifact.invalid/pinned-41") as url, patch.object(installed_artifacts.urllib.request, "urlopen", return_value=io.BytesIO(received)):
            try:
                installed_artifacts.download("ShaulLavo/mesh", pin, "linux-amd64", destination, root / "download.json")
            finally:
                url.assert_called_once_with("ShaulLavo/mesh", 41)

    def test_pinned_id_archive_is_verified_before_extraction(self):
        data = self.archive(b"actual pinned bytes")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            self.download_control(root, data, data)
            self.assertEqual((root / "input/tools/control").read_bytes(), b"actual pinned bytes")
            receipt = json.loads((root / "download.json").read_text())
            self.assertEqual(receipt, {"id": 41, "size": len(data), "digest": "sha256:" + published_releases.digest(data)})
            self.assertEqual(sorted(path.name for path in root.iterdir()), ["download.json", "input", "pin.json"])

    def test_replacement_after_intake_fails_before_extraction(self):
        original, replacement = self.archive(b"artifact A"), self.archive(b"artifact B")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "pinned size or SHA-256"):
                self.download_control(root, original, replacement)
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertEqual(sorted(path.name for path in root.iterdir()), ["input", "pin.json"])

    def test_pinned_archive_rejects_nonlocal_members(self):
        data = self.archive(b"not extracted", "../outside")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "nonlocal path"):
                self.download_control(root, data, data)
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertFalse((root / "outside").exists())

    def test_truncated_archive_fails_before_extraction(self):
        data = self.archive(b"original pinned archive")
        with tempfile.TemporaryDirectory(suffix=".noindex") as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "pinned size or SHA-256"):
                self.download_control(root, data, data[:-4])
            self.assertEqual(list((root / "input").iterdir()), [])
            self.assertFalse((root / "download.json").exists())

    def test_tool_source_and_actual_bytes_must_agree(self):
        with tempfile.TemporaryDirectory() as temporary:
            self.check_tool_provenance(Path(temporary))

    def check_tool_provenance(self, root):
        platform = {"os": "linux", "arch": "amd64"}
        tools = root / "tools"
        tools.mkdir()
        digests = {}
        for name in ("candidate", "worker-probe", "startup-fault-candidate", "boundary"):
            (tools / name).write_bytes(name.encode())
            digests[name] = published_releases.digest(name.encode())
        path = tools / "preparation.json"
        value = {"schema": 1, "source": {"commit": "a" * 40, "clean": True}, "platform": platform, "sha256": digests}
        path.write_text(json.dumps(value))
        installed_artifacts.verify_tools(root, "a" * 40, platform)
        with self.assertRaisesRegex(RuntimeError, "source"):
            installed_artifacts.verify_tools(root, "b" * 40, platform)
        (tools / "worker-probe").write_bytes(b"another observer")
        with self.assertRaisesRegex(RuntimeError, "worker-probe"):
            installed_artifacts.verify_tools(root, "a" * 40, platform)


class UnpublishedRollbackContextTest(unittest.TestCase):
    def manifest(self):
        return {"artifacts": [], "compatibility": {"stateReadMin": 16, "stateReadMax": 16, "stateWrite": 16,
            "workerMin": 1, "workerMax": 1, "workerWrite": 1, "journalVersion": 1,
            "transitions": [{"proof": "existing-completed-receipt"}]}}

    def test_actual_source_range_is_only_added_to_the_private_zero_claim_context(self):
        manifest = self.manifest()
        before = json.loads(json.dumps(manifest))
        source = {"stateVersion": 13, "workerProtocol": 1}
        with self.assertRaisesRegex(ValueError, "actual source database schema"):
            release_transition.verify_source_range(manifest, source)
        context = installed_releases.rollback_context(manifest, source)
        release_transition.verify_source_range(context, source)
        self.assertEqual(context["compatibility"]["stateReadMin"], 13)
        self.assertEqual(context["compatibility"]["stateWrite"], 16)
        self.assertEqual(context["compatibility"]["transitions"], [])
        self.assertIs(context["artifacts"], manifest["artifacts"])
        self.assertEqual(manifest, before)

    def test_source_above_compiled_capabilities_is_rejected(self):
        for source in ({"stateVersion": 17, "workerProtocol": 1}, {"stateVersion": 13, "workerProtocol": 2}):
            with self.assertRaisesRegex(RuntimeError, "actual candidate context capabilities"):
                installed_releases.rollback_context(self.manifest(), source)

    def test_executing_source_report_must_match_actual_image_and_platform(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "source"
            source.write_bytes(b"source measurement control")
            build = {"digest": published_releases.digest(source.read_bytes()), "modified": False,
                     "platform": {"os": "linux", "arch": "amd64"}, "stateVersion": 13, "workerProtocol": 1}
            self.check_source_report(source, build, True)
            self.check_source_report(source, build | {"digest": "0" * 64}, False)
            self.check_source_report(source, build | {"modified": True}, False)
            self.check_source_report(source, build | {"platform": {"os": "darwin", "arch": "arm64"}}, False)

    def check_source_report(self, source, build, accepted):
        result = subprocess.CompletedProcess([], 0, json.dumps(build), "")
        with patch.object(installed_releases.subprocess, "run", return_value=result):
            if accepted:
                self.assertEqual(installed_releases.source_build(source, {}, "linux/amd64"), build)
                return
            self.reject_source_report(source)

    def reject_source_report(self, source):
        with self.assertRaisesRegex(RuntimeError, "actual unmodified native image"):
            installed_releases.source_build(source, {}, "linux/amd64")


class PublicationModeTest(unittest.TestCase):
    def arguments(self, version, source):
        return SimpleNamespace(publication_version=version, publication_source=source, native_helper=None,
                               fresh_managed=False, installed_adoption=False, acquire_baselines=False,
                               worker_registry=None, worker_probe=None, export_prepared=None, prepared_releases=None)

    def test_explicit_empty_publication_inputs_fail_without_consumption(self):
        for version, source in (("", ""), ("v0.1.1", ""), ("", "a" * 40), (None, "a" * 40)):
            self.reject_publication(version, source)

    def reject_publication(self, version, source):
        with patch.object(installed_publication, "prove") as consume:
            with self.assertRaisesRegex(RuntimeError, "exact release version, source"):
                PROOF.run_requested(self.arguments(version, source), Path("/unused"), Path("/unused"), "linux/amd64")
        consume.assert_not_called()

    def test_omitted_publication_mode_remains_only_a_manager_preflight(self):
        with patch.object(installed_publication, "prove") as consume:
            PROOF.run_requested(self.arguments(None, None), Path("/unused"), Path("/unused"), "linux/amd64")
        consume.assert_not_called()

    def test_valid_publication_inputs_call_the_actual_draft_consumer(self):
        with tempfile.TemporaryDirectory() as temporary:
            helper = Path(temporary) / "helper"
            helper.touch()
            arguments = self.arguments("v0.1.1", "a" * 40)
            arguments.native_helper = helper
            with patch.object(installed_publication, "prove") as consume:
                PROOF.run_requested(arguments, Path("/unused"), Path("/output"), "linux/amd64")
            consume.assert_called_once_with(Path("/unused"), Path("/output"), "linux/amd64", "v0.1.1", "a" * 40,
                                            helper.resolve(), None, None)


def load_tests(loader, tests, _pattern):
    return unittest.TestSuite((tests, loader.loadTestsFromModule(installed_review_controls),
                               loader.loadTestsFromModule(installed_listener_controls), loader.loadTestsFromModule(installed_attachment_controls),
                               loader.loadTestsFromModule(installed_catalog_controls), loader.loadTestsFromModule(installed_order_trace_controls),
                               loader.loadTestsFromModule(installed_publication_controls)))


if __name__ == "__main__":
    unittest.main()
