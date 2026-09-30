#!/usr/bin/env python3
"""The fixture environment must not depend on the developer's agent setup."""

import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from terminal_window import Fixture


class FixtureEnvironmentTest(unittest.TestCase):
    def test_caller_environment_is_not_inherited(self):
        caller = {
            "PATH": "/usr/bin:/bin",
            "HOME": "/caller-home",
            "TMPDIR": tempfile.gettempdir(),
            "TERM": "caller-term",
            "LANG": "C",
            "XDG_RUNTIME_DIR": "/caller-runtime",
            "DBUS_SESSION_BUS_ADDRESS": "unix:path=/caller-runtime/bus",
            "ANTHROPIC_BASE_URL": "http://127.0.0.1:9",
            "ANTHROPIC_API_KEY": "fixture-anthropic-key",
            "OPENAI_BASE_URL": "http://127.0.0.1:9",
            "OPENAI_API_KEY": "fixture-openai-key",
            "CLAUDE_CONFIG_DIR": "/caller-claude",
            "CODEX_HOME": "/caller-codex",
            "HTTP_PROXY": "http://127.0.0.1:9",
            "https_proxy": "http://127.0.0.1:9",
            "ALL_PROXY": "http://127.0.0.1:9",
            "NO_PROXY": "caller.invalid",
            "BASH_ENV": "/caller-startup",
            "MESH_SESSION_ID": "CALLER",
            "MESH_TEST_AGENT_ROLE": "caller",
            "UNRELATED_CALLER_SETTING": "caller",
        }
        selected = {"PATH", "HOME", "TMPDIR", "TERM", "LANG", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
        with tempfile.TemporaryDirectory(prefix="m-env-") as root:
            with patch.dict(os.environ, caller, clear=True):
                fixture = Fixture("/bin/true", root)
            environment = fixture.environment
            for name in caller.keys() - selected:
                self.assertNotIn(name, environment)
            for name in selected - {"PATH", "HOME", "TERM"}:
                self.assertEqual(environment[name], caller[name])
            self.assertTrue(environment["PATH"].endswith(os.pathsep + caller["PATH"]))
            self.assertNotEqual(environment["HOME"], caller["HOME"])
            self.assertTrue(Path(environment["HOME"]).is_dir())
            self.assertTrue(Path(environment["HOME"]).is_relative_to(root))
            self.assertEqual(environment["TERM"], "xterm-256color")
            fixture.environment["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:7"
            self.assertEqual(fixture.environment["ANTHROPIC_BASE_URL"], "http://127.0.0.1:7")

    def test_fixture_bypasses_home_dependent_python_shim(self):
        with tempfile.TemporaryDirectory(prefix="m-python-env-") as temporary:
            root = Path(temporary)
            tools = root / "shims"
            tools.mkdir()
            shim = tools / "python3"
            shim.write_text("#!/bin/sh\nexit 126\n")
            shim.chmod(0o700)
            with patch.dict(os.environ, {"PATH": str(tools) + os.pathsep + os.defpath}):
                fixture = Fixture("/bin/true", root)
            command = root / "provider"
            command.write_text("#!/usr/bin/env python3\nprint('fixture-python')\n")
            command.chmod(0o700)
            result = subprocess.run([str(command)], env=fixture.environment, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "fixture-python\n")


class VerifierEnvironmentTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="m-verify-env-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "scripts").mkdir()
        (self.root / "integration").mkdir()
        self.tools = self.root / "toolchain" / "bin"
        self.tools.mkdir(parents=True)
        shutil.copyfile(Path(__file__).parents[2] / "scripts" / "verify.sh", self.root / "scripts" / "verify.sh")
        self.build_capture = self.root / "build.env"
        self.test_capture = self.root / "test.env"
        self.caller_home = self.root / "caller-home"
        self.caller_home.mkdir()
        go_root = subprocess.check_output(["go", "env", "GOROOT"], text=True).strip()
        go = self.tools / "go"
        go.write_text(
            "#!/usr/bin/env bash\nset -euo pipefail\n"
            "if [[ $1 == env ]]; then\n"
            f"  case $2 in GOROOT) echo {shlex.quote(str(self.tools.parent))} ;; "
            f"GOCACHE) echo {shlex.quote(str(self.root / 'cache'))} ;; "
            f"GOMODCACHE) echo {shlex.quote(str(self.root / 'modules'))} ;; "
            f"*) exec {shlex.quote(str(Path(go_root) / 'bin' / 'go'))} \"$@\" ;; esac\n"
            f"else env > {shlex.quote(str(self.build_capture))}; fi\n"
        )
        go.chmod(0o700)
        self.probe = self.root / "integration" / "probe.sh"
        self.probe.write_text(f"env > {shlex.quote(str(self.test_capture))}\n")
        self.caller = {"PATH": str(self.tools) + os.pathsep + os.defpath, "HOME": str(self.caller_home),
                       "TMPDIR": str(self.root), "TERM": "dumb", "LANG": "C"}

    def run_verifier(self):
        return subprocess.run(["bash", str(self.root / "scripts" / "verify.sh")], env=self.caller,
                              cwd=self.root, capture_output=True, text=True, timeout=10)

    def captured_environments(self):
        result = self.run_verifier()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        build = dict(line.split("=", 1) for line in self.build_capture.read_text().splitlines())
        test = dict(line.split("=", 1) for line in self.test_capture.read_text().splitlines())
        return build, test

    def test_build_network_policy_does_not_reach_tests(self):
        network = {"HTTP_PROXY": "http://127.0.0.1:9", "HTTPS_PROXY": "http://127.0.0.1:9",
                   "NO_PROXY": "fixture.invalid", "https_proxy": "http://127.0.0.1:9",
                   "GOPROXY": "https://fixture.invalid", "GONOSUMDB": "fixture.invalid/*", "GOFLAGS": ""}
        provider = {"ANTHROPIC_BASE_URL": "http://127.0.0.1:9", "CLAUDE_CONFIG_DIR": "/caller-claude"}
        self.caller.update(network | provider)
        build, test = self.captured_environments()
        for name, value in network.items():
            self.assertEqual(build.get(name), value)
            self.assertNotIn(name, test)
        for name in provider:
            self.assertNotIn(name, build)
            self.assertNotIn(name, test)
        self.assertNotEqual(build["HOME"], self.caller["HOME"])
        self.assertNotEqual(test["HOME"], self.caller["HOME"])
        self.assertNotEqual(build["HOME"], test["HOME"])

    def test_saved_go_policy_is_resolved_before_replacing_home(self):
        policy = {"GOPROXY": "http://127.0.0.1:9", "GONOSUMDB": "fixture.invalid/*",
                  "GONOPROXY": "direct.fixture.invalid/*", "GOPRIVATE": "private.fixture.invalid/*",
                  "GOFLAGS": "-mod=readonly", "GOINSECURE": "insecure.fixture.invalid/*", "GOSUMDB": "off"}
        config = self.caller_home / ".config" / "go" / "env"
        config.parent.mkdir(parents=True)
        config.write_text("".join(f"{name}={value}\n" for name, value in policy.items()))
        build, test = self.captured_environments()
        for name, value in policy.items():
            self.assertEqual(build.get(name), value, name)
            self.assertNotIn(name, test)
        self.caller["GOFLAGS"] = ""
        build, _ = self.captured_environments()
        self.assertEqual(build["GOFLAGS"], "")

    def test_build_trust_and_netrc_do_not_reach_tests(self):
        netrc = self.caller_home / ".netrc"
        netrc.write_text("machine fixture.invalid login fixture password synthetic\n")
        trust = {"SSL_CERT_FILE": str(self.root / "synthetic-ca.pem"), "SSL_CERT_DIR": str(self.root / "synthetic-certs")}
        self.caller.update(trust)
        for explicit in (None, "caller-home/.netrc"):
            with self.subTest(netrc=explicit):
                if explicit is not None:
                    self.caller["NETRC"] = explicit
                build, test = self.captured_environments()
                self.assertEqual(build.get("NETRC"), str(netrc))
                self.assertNotIn("NETRC", test)
                for name, value in trust.items():
                    self.assertEqual(build.get(name), value)
                    self.assertNotIn(name, test)

    def test_home_dependent_python_shim_is_resolved(self):
        shim = self.tools / "python3"
        shim.write_text(
            "#!/bin/sh\n"
            f"[ \"$HOME\" = {shlex.quote(str(self.caller_home))} ] || exit 126\n"
            f"exec {shlex.quote(str(Path(sys.executable).resolve()))} \"$@\"\n"
        )
        shim.chmod(0o700)
        self.probe.write_text("#!/usr/bin/env bash\npython3 -c 'print(\"resolved-python\")'\n")
        result = self.run_verifier()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_home_dependent_shell_shims_are_resolved(self):
        for name in ("bash", "sh"):
            shim = self.tools / name
            shim.write_text(
                "#!/bin/sh\n"
                f"[ \"$HOME\" = {shlex.quote(str(self.caller_home))} ] || exit 126\n"
                f"exec /bin/{name} \"$@\"\n"
            )
            shim.chmod(0o700)
        self.probe.write_text("bash -c 'sh -c true'\n")
        result = self.run_verifier()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_user_bus_reaches_scripts(self):
        selected = {"XDG_RUNTIME_DIR": "/fixture-runtime", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/fixture-runtime/bus"}
        self.caller.update(selected)
        _, test = self.captured_environments()
        for name, value in selected.items():
            self.assertEqual(test.get(name), value, name)

    def test_supported_overrides_reach_scripts(self):
        selected = {"MESH_TEST_ZSH": "/fixture/zsh", "MESH_SHORT_TMP": str(self.root)}
        self.caller.update(selected | {"MESH_AGENT_NATIVE": "fixture", "MESH_SESSION_ID": "CALLER"})
        _, test = self.captured_environments()
        for name, value in selected.items():
            self.assertEqual(test.get(name), value, name)
        for name in ("MESH_AGENT_NATIVE", "MESH_SESSION_ID"):
            self.assertNotIn(name, test)

    def test_scope_cannot_skip_a_working_caller_user_bus(self):
        busctl = self.tools / "busctl"
        busctl.write_text(
            "#!/bin/sh\n"
            f"[ \"$HOME\" = {shlex.quote(str(self.caller_home))} ]\n"
        )
        busctl.chmod(0o700)
        self.probe.rename(self.root / "integration" / "session_scope.sh")
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("FAIL: session_scope.sh lost the caller's user bus", result.stderr)


if __name__ == "__main__":
    unittest.main()
