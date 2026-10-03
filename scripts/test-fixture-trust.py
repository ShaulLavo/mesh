#!/usr/bin/env python3
"""Exercise the native trust fixture with an in-memory Security tool boundary."""
import ast
import copy
import plistlib
import shlex
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

SOURCE = Path(__file__).with_name("prove-device-auth-cutover.py")


class SecurityFixture:
    def __init__(self, root):
        self.root = root
        self.prior = ["/fixture/login.keychain-db", "/fixture/other keychain-db"]
        self.search = self.prior[:]
        self.keychains = set()
        self.right = {"class": "rule", "rule": ["authenticate-admin"], "timeout": 300}
        self.original_right = copy.deepcopy(self.right)
        self.trusted = False
        self.calls = []
        self.fail_remove = False
        self.ignore_restore = False
        self.ignore_delete = False
        self.fail_add = False
        self.ignore_right_restore = False
        self.ignore_trust_remove = False

    def run(self, command, **kwargs):
        administrator = command[:3] == ["sudo", "-n", "security"]
        args = command[3:] if administrator else command[1:]
        self.calls.append(tuple(args))
        action = args[0]
        output = b""
        if action == "authorizationdb":
            if args[1] == "read":
                output = plistlib.dumps(self.right)
            elif args[-1] == "allow":
                self.right = {"class": "rule", "rule": ["allow"]}
            elif not self.ignore_right_restore:
                self.right = plistlib.loads(kwargs["input"])
        elif action == "list-keychains":
            if "-s" in args:
                if not self.ignore_restore or args[args.index("-s") + 1:] != self.prior:
                    self.search = args[args.index("-s") + 1:]
            else:
                output = ("\n".join(shlex.quote(path) for path in self.search) + "\n").encode()
        elif action == "create-keychain":
            self.keychains.add(args[-1])
            Path(args[-1]).touch()
        elif action == "unlock-keychain":
            pass
        elif action == "add-trusted-cert":
            self.trusted = True
            if self.fail_add:
                return SimpleNamespace(returncode=1, stdout=b"", stderr=b"fixture partial add failure")
        elif action == "remove-trusted-cert":
            if self.right.get("rule") != ["allow"]:
                raise subprocess.TimeoutExpired("security remove-trusted-cert", 15)
            if self.fail_remove:
                return SimpleNamespace(returncode=1, stdout=b"", stderr=b"fixture removal failure")
            if not self.ignore_trust_remove:
                self.trusted = False
        elif action == "dump-trust-settings":
            output = b"Number of trusted certs = 1\n" if self.trusted else b"No Trust Settings were found.\n"
        elif action == "find-certificate":
            if self.trusted:
                output = b"fixture certificate"
            else:
                return SimpleNamespace(returncode=1, stdout=b"", stderr=b"certificate absent")
        elif action == "trust-settings-export":
            Path(args[-1]).write_bytes(plistlib.dumps({"trustVersion": 1, "trustList": {"fixture": {}} if self.trusted else {}}))
        elif action == "delete-keychain":
            if not self.ignore_delete:
                self.keychains.remove(args[-1])
                Path(args[-1]).unlink()
        else:
            raise AssertionError("unexpected Security action " + action)
        return SimpleNamespace(returncode=0, stdout=output, stderr=b"")

    def check_output(self, command, **kwargs):
        result = self.run(command, **kwargs)
        if result.returncode:
            raise subprocess.CalledProcessError(result.returncode, command)
        return result.stdout


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class NativeTrustTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = SecurityFixture(self.root)
        self.tree = ast.parse(SOURCE.read_text())
        definitions = [node for node in self.tree.body if isinstance(node, ast.FunctionDef)
                       and node.name in ("install_native_fixture_trust", "cleanup_native_fixture_trust")]
        self.namespace = {
            "ROOT": self.root, "OLD_BUILD": {"platform": {"os": "darwin"}},
            "os": SimpleNamespace(environ={"GITHUB_ACTIONS": "true", "RUNNER_OS": "macOS"}),
            "subprocess": SimpleNamespace(run=self.fixture.run, check_output=self.fixture.check_output,
                                          SubprocessError=subprocess.SubprocessError),
            "shlex": shlex, "plistlib": plistlib, "sys": sys, "require": require,
            "event": lambda *args, **kwargs: None, "trust_cleanup": [],
        }
        exec(compile(ast.Module(body=definitions, type_ignores=[]), str(SOURCE), "exec"), self.namespace)

    def install(self):
        self.namespace["install_native_fixture_trust"]()

    def cleanup(self):
        server = SimpleNamespace(shutdown=lambda: None, server_close=lambda: None)
        self.namespace.update(hosts=[], services=server, proxy=server)
        exec(compile(ast.Module(body=self.tree.body[-1].finalbody, type_ignores=[]), str(SOURCE), "exec"), self.namespace)

    def test_last_admin_certificate_removal_is_noninteractive_and_restored(self):
        self.install()
        self.cleanup()
        self.assertFalse(self.fixture.trusted)
        self.assertEqual(self.fixture.search, self.fixture.prior)
        self.assertFalse(self.fixture.keychains)
        self.assertEqual(self.fixture.right, self.fixture.original_right)
        self.assertIn(("authorizationdb", "read", "com.apple.trust-settings.admin"), self.fixture.calls)

    def test_primary_failure_survives_cleanup_failure(self):
        self.install()
        self.fixture.fail_remove = True
        primary = RuntimeError("primary cutover failure")
        try:
            try:
                raise primary
            finally:
                self.cleanup()
        except RuntimeError as error:
            self.assertIs(error, primary)
            self.assertTrue(any("fixture removal failure" in note for note in error.__notes__))
        else:
            self.fail("primary failure disappeared")
        self.assertEqual(self.fixture.right, self.fixture.original_right)
        self.assertEqual(self.fixture.search, self.fixture.prior)
        self.assertFalse(self.fixture.keychains)

    def test_cleanup_failure_fails_successful_proof(self):
        self.install()
        self.fixture.fail_remove = True
        with self.assertRaisesRegex(RuntimeError, "fixture removal failure"):
            self.cleanup()
        self.assertEqual(self.fixture.right, self.fixture.original_right)

    def test_search_restore_is_read_back(self):
        self.install()
        self.fixture.ignore_restore = True
        with self.assertRaisesRegex(RuntimeError, "search list"):
            self.cleanup()
        self.assertEqual(self.fixture.right, self.fixture.original_right)
        self.assertFalse(self.fixture.keychains)

    def test_keychain_removal_is_read_back(self):
        self.install()
        self.fixture.ignore_delete = True
        with self.assertRaisesRegex(RuntimeError, "keychain.*remov"):
            self.cleanup()
        self.assertEqual(self.fixture.right, self.fixture.original_right)

    def test_authorization_restore_is_read_back(self):
        self.install()
        self.fixture.ignore_right_restore = True
        with self.assertRaisesRegex(RuntimeError, "authorization.*restor"):
            self.cleanup()
        self.assertFalse(self.fixture.keychains)

    def test_trust_removal_is_read_back(self):
        self.install()
        self.fixture.ignore_trust_remove = True
        with self.assertRaisesRegex(RuntimeError, "trust.*restor"):
            self.cleanup()
        self.assertEqual(self.fixture.right, self.fixture.original_right)
        self.assertFalse(self.fixture.keychains)

    def test_partial_trust_install_is_cleaned(self):
        self.fixture.fail_add = True
        with self.assertRaisesRegex(RuntimeError, "partial add failure"):
            try:
                self.install()
            finally:
                self.cleanup()
        self.assertFalse(self.fixture.trusted)
        self.assertEqual(self.fixture.search, self.fixture.prior)
        self.assertEqual(self.fixture.right, self.fixture.original_right)
        self.assertFalse(self.fixture.keychains)

    def test_guard_refuses_non_ci_runner_before_security(self):
        self.namespace["os"].environ = {}
        with self.assertRaisesRegex(RuntimeError, "disposable GitHub"):
            self.install()
        self.assertEqual(self.fixture.calls, [])


if __name__ == "__main__":
    unittest.main()
