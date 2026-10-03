"""Supported certificate-file controls and unchanged executable boundaries."""
import hashlib
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock

from fixtures import tls

SOURCE = Path(__file__).with_name("prove-device-auth-cutover.py")


class ArtifactBoundaryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.binary = Path(self.temp.name) / "mesh"
        self.binary.write_bytes(b"unchanged executable")
        self.original = {self.binary: hashlib.sha256(self.binary.read_bytes()).hexdigest()}
        self.event = Mock()

    def verify(self, primary_error=None):
        tls.verify_artifacts(self.original, primary_error, self.event)

    def test_unchanged_inputs_pass(self):
        self.verify()
        self.event.assert_called_once_with("fixture-artifacts-unchanged", trustStoreModified=False)

    def test_changed_input_fails(self):
        self.binary.write_bytes(b"changed")
        with self.assertRaisesRegex(RuntimeError, "changed an input artifact"):
            self.verify()

    def test_primary_failure_survives_artifact_failure(self):
        self.binary.write_bytes(b"changed")
        primary = RuntimeError("original proof failure")
        self.verify(primary)
        self.assertIn("changed an input artifact", primary.__notes__[0])

    def test_missing_input_preserves_primary_failure(self):
        self.binary.unlink()
        primary = RuntimeError("original setup failure")
        self.verify(primary)
        self.assertIn("fixture artifact validation", primary.__notes__[0])

    def test_no_native_trust_or_injection_mutation(self):
        source = SOURCE.read_text() + Path(tls.__file__).read_text()
        for forbidden in ("DYLD_INSERT_LIBRARIES", "authorizationdb", "remove-trusted-cert", "add-trusted-cert"):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, source)


class TLSControlsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)
        tls.fixture_certificates(cls.root)
        (cls.root / "empty-certs").mkdir()
        cls.probe = cls.root / "tls-probe"
        subprocess.run(["go", "build", "-o", str(cls.probe), str(tls.SOURCES)],
            check=True, capture_output=True, timeout=60)
        cls.environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
        cls.environment.update(HOME=str(cls.root), **tls.certificate_environment(cls.root))

    def test_existing_certificate_files_need_no_native_interposer(self):
        parent = dict(os.environ)
        events = []
        environment = tls.prepare_fixture_tls(self.root, lambda kind, **fields: events.append((kind, fields)))
        self.assertEqual(environment, tls.certificate_environment(self.root))
        self.assertEqual(dict(os.environ), parent)
        controls = [fields for kind, fields in events if kind == "tls-control"]
        self.assertEqual(len(controls), 10)
        self.assertEqual({row["launcher"] for row in controls}, {"direct", "production-shell"})
        self.assertEqual({row["control"] for row in controls},
            {"fixture-ca", "missing-ca", "wrong-ca", "self-signed", "wrong-hostname"})
        self.assertTrue(all("returncode" in row and "class" in row for row in controls))

    def check(self, certificate, key, expected, environment=None):
        for launcher in ("direct", "production-shell"):
            with self.subTest(launcher=launcher):
                result = tls.probe_tls(self.root, self.probe, environment or self.environment,
                    certificate, key, expected, launcher)
                self.assertEqual(result["class"], expected)
                self.assertEqual(result["returncode"], 0 if expected == "trusted" else 1)

    def test_fixture_ca_is_trusted(self):
        self.check("server.pem", "server.key", "trusted")

    def test_wrong_ca_is_rejected(self):
        self.check("server.pem", "server.key", "certificate",
            self.environment | {"SSL_CERT_FILE": str(self.root / "wrong-ca.pem")})

    def test_self_signed_leaf_is_rejected(self):
        self.check("self-signed.pem", "self-signed.key", "certificate")

    def test_wrong_hostname_is_rejected(self):
        self.check("wrong-host.pem", "wrong-host.key", "hostname")

    def test_missing_fixture_ca_retains_tls_error(self):
        self.check("server.pem", "server.key", "certificate",
            self.environment | {"SSL_CERT_FILE": str(self.root / "missing.pem")})

    def test_failed_control_records_exit_stdout_and_stderr(self):
        with self.assertRaisesRegex(RuntimeError, '"returncode": 0.*"class": "trusted".*"stdout":'):
            tls.probe_tls(self.root, self.probe, self.environment, "server.pem", "server.key", "certificate")


if __name__ == "__main__":
    unittest.main()
