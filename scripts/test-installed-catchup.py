"""Installed proof path boundaries and real native-service failure cleanup."""
import json
import ast
import io
import importlib.util
import os
import shutil
import socket
import tempfile
import tarfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fixtures import fleet_baselines, installed_paths as paths, native_services, published_releases, installed_cohort, installed_artifacts

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

    def test_unmounted_work_drive_cannot_receive_fixture_data(self):
        with (patch.dict(os.environ, {"MESH_SHORT_TMP": "/work/tmp"}), patch.object(Path, "is_mount", return_value=False)):
            with self.assertRaisesRegex(RuntimeError, "not mounted"):
                paths.create()

    def test_python_fixture_blocks_remain_at_most_three_deep(self):
        scripts = PROOF_PATH.parent
        files = (PROOF_PATH, Path(__file__), *(scripts / "fixtures" / name for name in
            ("installed_paths.py", "native_services.py", "installed_listener.py", "installed_native.py", "fleet_baselines.py",
             "installed_origin.py", "installed_releases.py", "installed_adoption.py", "installed_cohort.py", "installed_chain.py", "installed_waypoint.py", "installed_artifacts.py")))
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


class ExactCohortInputTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-", dir=os.environ.get("MESH_SHORT_TMP", "/tmp"))
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


class ArtifactPinTest(unittest.TestCase):
    def inputs(self):
        jobs = [{"name": "Native proofs / prepare (" + key.replace("-", "/") + ", runner)",
                 "status": "completed", "conclusion": "success"} for key in installed_artifacts.KEYS]
        return [{"head_sha": "b" * 40}, {"jobs": jobs}, {"artifacts": [
            {"id": index, "name": "update-native-" + key, "expired": False, "digest": "sha256:" + "b" * 64}
            for index, key in enumerate(installed_artifacts.KEYS, 1)]}]

    def test_complete_three_platform_set_is_pinned(self):
        with patch.object(installed_artifacts, "api", side_effect=self.inputs()):
            result = installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        self.assertEqual(set(result["artifacts"]), set(installed_artifacts.KEYS))
        self.assertEqual(result["run"], 123)
        self.assertEqual(result["source"], "a" * 40)
        self.assertEqual(result["runHead"], "b" * 40)

    def test_pending_preparation_and_missing_or_expired_platform_fail(self):
        values = self.inputs()
        values[1]["jobs"][0].update(status="in_progress", conclusion=None)
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "not completed"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")
        values = self.inputs()
        values[2]["artifacts"][-1]["expired"] = True
        with patch.object(installed_artifacts, "api", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "darwin-arm64"):
                installed_artifacts.verify_set("ShaulLavo/mesh", 123, "a" * 40, "update-native")

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


if __name__ == "__main__":
    unittest.main()
