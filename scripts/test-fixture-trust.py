#!/usr/bin/env python3
"""Native fixture boundaries and real TLS verification controls, without host trust."""
import ast
import hashlib
import os
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from fixtures import native_tls
from fixtures.native_tls import SOURCES, fixture_certificates, macho_signature_flags, probe_tls, verify_injectable_binaries

SOURCE = Path(__file__).with_name("prove-device-auth-cutover.py")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def macho(flags):
    blob = struct.pack(">III", 0xfade0cc0, 12 + len(flags) * 24, len(flags))
    for index in range(len(flags)):
        blob += struct.pack(">II", index * 0x1000, 12 + len(flags) * 8 + index * 16)
    for value in flags:
        blob += struct.pack(">IIII", 0xfade0c02, 16, 0x20400, value)
    header = bytearray(32)
    header[:4] = b"\xcf\xfa\xed\xfe"
    struct.pack_into("<I", header, 16, 1)
    return bytes(header) + struct.pack("<IIII", 0x1d, 16, 48, len(blob)) + blob


class SignatureTest(unittest.TestCase):
    def test_adhoc_linker_signature_allows_fixture(self):
        self.assertEqual(macho_signature_flags(macho([0x20002])), [0x20002])

    def test_unsigned_binary_allows_fixture(self):
        data = bytearray(32)
        data[:4] = b"\xcf\xfa\xed\xfe"
        self.assertEqual(macho_signature_flags(data), [])

    def test_runtime_signature_in_any_directory_refuses_fixture(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "mesh"
            path.write_bytes(macho([0x20002, 0x10000]))
            with self.assertRaisesRegex(RuntimeError, "hardened-runtime"):
                verify_injectable_binaries([path])

    def test_restricted_signature_refuses_fixture(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "mesh"
            for flag in (0x800, 0x2000):
                with self.subTest(flag=hex(flag)):
                    path.write_bytes(macho([flag]))
                    with self.assertRaisesRegex(RuntimeError, "restricted"):
                        verify_injectable_binaries([path])

    def test_restrict_segment_refuses_fixture(self):
        data = bytearray(56)
        data[:4] = b"\xcf\xfa\xed\xfe"
        struct.pack_into("<I", data, 16, 1)
        struct.pack_into("<II", data, 32, 0x19, 24)
        data[40:50] = b"__RESTRICT"
        with self.assertRaisesRegex(RuntimeError, "restricted"):
            macho_signature_flags(data)

    def test_invalid_signature_refuses_fixture(self):
        with self.assertRaises((ValueError, struct.error)):
            macho_signature_flags(macho([0x20002])[:-1])

    def test_wrong_binary_format_refuses_fixture(self):
        with self.assertRaisesRegex(ValueError, "Mach-O"):
            macho_signature_flags(b"not a Mach-O")


class NativeTrustTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binaries = [self.root / name for name in ("old", "new", "fleet", "control")]
        for binary in self.binaries:
            binary.write_bytes(binary.name.encode())
        self.tree = ast.parse(SOURCE.read_text())
        definitions = [node for node in self.tree.body if isinstance(node, ast.FunctionDef)
                       and node.name in ("install_native_fixture_trust", "cleanup_native_fixture_trust")]
        self.prepare = Mock(return_value={"DYLD_INSERT_LIBRARIES": str(self.root / "fixture-anchor.dylib")})
        self.security = Mock(return_value=SimpleNamespace(returncode=1, stdout=b"", stderr=b"NO (-60005)\n"))
        self.environment = {"GITHUB_ACTIONS": "true", "RUNNER_OS": "macOS"}
        self.namespace = {
            "ROOT": self.root, "OLD_BUILD": {"platform": {"os": "darwin"}},
            "OLD": self.binaries[0], "NEW": self.binaries[1], "FLEET": self.binaries[2], "CONTROL_CLIENT": self.binaries[3],
            "os": SimpleNamespace(environ=self.environment), "subprocess": SimpleNamespace(run=self.security,
                check_output=self.security, SubprocessError=subprocess.SubprocessError),
            "sys": sys, "require": require, "event": Mock(), "trust_cleanup": [], "native_tls_environment": {},
            "fixture_tls": SimpleNamespace(prepare_native_tls=self.prepare), "digest": lambda data: hashlib.sha256(data).hexdigest(),
        }
        exec(compile(ast.Module(body=definitions, type_ignores=[]), str(SOURCE), "exec"), self.namespace)

    def install(self):
        self.namespace["install_native_fixture_trust"]()

    def cleanup(self):
        server = SimpleNamespace(shutdown=lambda: None, server_close=lambda: None)
        self.namespace.update(hosts=[], services=server, proxy=server)
        exec(compile(ast.Module(body=self.tree.body[-1].finalbody, type_ignores=[]), str(SOURCE), "exec"), self.namespace)

    def test_runner_denies_authorization_writes_without_blocking_cleanup(self):
        self.install()
        self.cleanup()
        self.security.assert_not_called()
        self.assertEqual(self.namespace["native_tls_environment"], {})

    def test_injection_is_child_only_and_inputs_stay_unchanged(self):
        before = [path.read_bytes() for path in self.binaries]
        self.install()
        self.assertNotIn("DYLD_INSERT_LIBRARIES", self.environment)
        self.assertIn("DYLD_INSERT_LIBRARIES", self.namespace["native_tls_environment"])
        self.cleanup()
        self.assertEqual(before, [path.read_bytes() for path in self.binaries])
        self.assertEqual(self.namespace["native_tls_environment"], {})

    def test_changed_artifact_fails_cleanup_and_clears_injection(self):
        self.install()
        self.binaries[0].write_bytes(b"changed")
        with self.assertRaisesRegex(RuntimeError, "changed an input artifact"):
            self.cleanup()
        self.assertEqual(self.namespace["native_tls_environment"], {})

    def test_primary_failure_survives_cleanup_failure(self):
        self.install()
        self.binaries[0].write_bytes(b"changed")
        primary = RuntimeError("primary cutover failure")
        with self.assertRaises(RuntimeError) as caught:
            try:
                raise primary
            finally:
                self.cleanup()
        self.assertIs(caught.exception, primary)
        self.assertTrue(any("changed an input artifact" in note for note in primary.__notes__))
        self.assertEqual(self.namespace["native_tls_environment"], {})

    def test_setup_error_retains_original_failure(self):
        primary = RuntimeError("original TLS verification failure")
        self.prepare.side_effect = primary
        with self.assertRaises(RuntimeError) as caught:
            try:
                self.install()
            finally:
                self.cleanup()
        self.assertIs(caught.exception, primary)
        self.assertEqual(self.namespace["native_tls_environment"], {})
        self.security.assert_not_called()

    def test_guard_refuses_non_ci_runner_before_setup(self):
        self.environment.clear()
        with self.assertRaisesRegex(RuntimeError, "disposable GitHub"):
            self.install()
        self.prepare.assert_not_called()
        self.security.assert_not_called()

    def test_linux_needs_no_native_injection(self):
        self.namespace["OLD_BUILD"]["platform"]["os"] = "linux"
        self.install()
        self.cleanup()
        self.prepare.assert_not_called()
        self.assertEqual(self.namespace["native_tls_environment"], {})


@unittest.skipUnless(sys.platform == "linux", "Darwin controls run inside the native cutover fixture")
class TLSControlsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)
        fixture_certificates(cls.root)
        (cls.root / "empty-certs").mkdir()
        cls.probe = cls.root / "tls-probe"
        subprocess.run(["go", "build", "-o", str(cls.probe), str(SOURCES / "probe")],
                       check=True, capture_output=True, timeout=60)
        cls.environment = os.environ | {"SSL_CERT_FILE": str(cls.root / "cert.pem"),
            "SSL_CERT_DIR": str(cls.root / "empty-certs")}

    def test_existing_certificate_files_need_no_native_interposer(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "RUNNER_OS": "macOS"}), \
                patch.object(native_tls, "verify_injectable_binaries", return_value=[]):
            environment = native_tls.prepare_native_tls(self.root, [], Mock())
        self.assertEqual(environment, {"SSL_CERT_FILE": str(self.root / "cert.pem"),
            "SSL_CERT_DIR": str(self.root / "empty-certs")})

    def test_fixture_ca_is_trusted(self):
        probe_tls(self.root, self.probe, self.environment, "server.pem", "server.key", "trusted")

    def test_wrong_ca_is_rejected(self):
        probe_tls(self.root, self.probe, self.environment, "wrong-ca-server.pem", "server.key", "certificate")

    def test_self_signed_leaf_is_rejected(self):
        probe_tls(self.root, self.probe, self.environment, "self-signed.pem", "self-signed.key", "certificate")

    def test_wrong_hostname_is_rejected(self):
        probe_tls(self.root, self.probe, self.environment, "wrong-host.pem", "wrong-host.key", "hostname")

    def test_missing_fixture_ca_retains_tls_error(self):
        environment = self.environment | {"SSL_CERT_FILE": str(self.root / "missing.pem")}
        probe_tls(self.root, self.probe, environment, "server.pem", "server.key", "certificate")


if __name__ == "__main__":
    unittest.main()
