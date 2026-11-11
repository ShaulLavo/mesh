"""Installed proof path boundaries and real native-service failure cleanup."""
import json
import os
import shutil
import socket
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fixtures import installed_paths as paths, native_services


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

    def test_native_tmpdir_is_ignored(self):
        environment = dict(os.environ)
        environment.pop("MESH_SHORT_TMP", None)
        environment["TMPDIR"] = str(self.root / ("long-" * 30))
        with patch.dict(os.environ, environment, clear=True):
            root = paths.create()
        self.addCleanup(shutil.rmtree, root)
        self.assertEqual(root.parent, Path("/tmp").resolve())


@unittest.skipUnless(os.environ.get("MESH_TEST_NATIVE_MANAGER") == "1", "native control explicitly selected")
class NativeServiceTest(unittest.TestCase):
    def setUp(self):
        self.root = paths.create()
        self.addCleanup(shutil.rmtree, self.root)
        self.report = self.root / "evidence.json"

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


if __name__ == "__main__":
    unittest.main()
