"""Native Unix socket controls for the historical helper proof's short root."""
import json
import os
import shutil
import socket
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fixtures import helper_recovery_paths as paths


class HelperRecoveryPathsTest(unittest.TestCase):
    def setUp(self):
        self.short_base = paths.selected_base().resolve()
        self.temp = tempfile.TemporaryDirectory(prefix="hp-", dir=self.short_base)
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.long = self.base / ("long-native-tmpdir-" * 7)
        self.long.mkdir()

    def test_default_base_ignores_native_tmpdir(self):
        environment = os.environ | {"TMPDIR": str(self.long)}
        environment.pop("MESH_SHORT_TMP", None)
        with patch.dict(os.environ, environment, clear=True):
            self.assertEqual(paths.selected_base(), Path("/tmp"))

    def test_previous_tmpdir_root_fails_before_short_root_binds(self):
        with tempfile.TemporaryDirectory(prefix="mesh-helper-proof.", dir=self.long) as old:
            old_root = Path(old) / "proof"
            with self.assertRaisesRegex(ValueError, "socket path requires"):
                paths.validate_socket_root(old_root)
            old_socket = old_root / "repair" / "state" / ".d-4294967295"
            old_socket.parent.mkdir(parents=True)
            with socket.socket(socket.AF_UNIX) as connection, self.assertRaises(OSError):
                connection.bind(str(old_socket))
            print(json.dumps({"control": "previous-long-TMPDIR-root-rejected",
                              "socketBytes": len(os.fsencode(old_socket))}), flush=True)
        with patch.dict(os.environ, {"TMPDIR": str(self.long), "MESH_SHORT_TMP": str(self.short_base)}):
            root = paths.create_root()
        self.addCleanup(shutil.rmtree, root)
        evidence = paths.validate_socket_root(root / "proof")
        self.assertEqual(root.parent, self.short_base)
        self.assertLessEqual(evidence["longestSocketBytes"], paths.MAX_SOCKET_BYTES)
        for row in evidence["socketPaths"]:
            address = Path(row["path"])
            address.parent.mkdir(parents=True, exist_ok=True)
            with socket.socket(socket.AF_UNIX) as connection:
                connection.bind(str(address))
        print(json.dumps({"control": "short-root-native-sockets-bound", **evidence}), flush=True)

    def test_overlong_configured_base_fails_closed_and_cleans_owned_root(self):
        before = set(self.long.iterdir())
        with (
            patch.dict(os.environ, {"TMPDIR": str(self.base), "MESH_SHORT_TMP": str(self.long)}),
            self.assertRaisesRegex(ValueError, "socket path requires"),
        ):
            paths.create_root()
        self.assertEqual(set(self.long.iterdir()), before)

    def test_configured_symlink_uses_resolved_path_bound(self):
        alias = self.base / "a"
        alias.symlink_to(self.long, target_is_directory=True)
        with (
            patch.dict(os.environ, {"MESH_SHORT_TMP": str(alias)}),
            self.assertRaisesRegex(ValueError, "socket path requires"),
        ):
            paths.create_root()
        self.assertEqual(list(self.long.iterdir()), [])

    def test_exact_portable_limit_is_accepted_and_next_byte_rejected(self):
        base = Path("/tmp").resolve()
        suffix_bytes = max(len(os.fsencode(path)) for path in paths.socket_paths(base))
        root = base / ("a" * (paths.MAX_SOCKET_BYTES - suffix_bytes - 1))
        self.assertEqual(paths.validate_socket_root(root)["longestSocketBytes"], paths.MAX_SOCKET_BYTES)
        with self.assertRaisesRegex(ValueError, "socket path requires"):
            paths.validate_socket_root(root.with_name(root.name + "a"))

    def test_filesystem_bytes_and_longest_retained_worker_path_are_bounded(self):
        root = Path("/tmp") / ("é" * 30)
        longest = max(paths.socket_paths(root), key=lambda path: len(os.fsencode(path)))
        self.assertLessEqual(len(str(longest)), paths.MAX_SOCKET_BYTES)
        self.assertGreater(len(os.fsencode(longest)), paths.MAX_SOCKET_BYTES)
        self.assertEqual(longest.name, "sock")
        with self.assertRaisesRegex(ValueError, "socket path requires"):
            paths.validate_socket_root(root)


if __name__ == "__main__":
    unittest.main()
