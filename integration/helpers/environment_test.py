#!/usr/bin/env python3
"""The fixture environment must not depend on the developer's agent setup."""

import os
from pathlib import Path
import shlex
import shutil
import subprocess
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
        with tempfile.TemporaryDirectory(prefix="m-env-") as root:
            with patch.dict(os.environ, caller, clear=True):
                fixture = Fixture("/bin/true", root)
            environment = fixture.environment
            for name in caller.keys() - {"PATH", "HOME", "TMPDIR", "TERM", "LANG"}:
                self.assertNotIn(name, environment)
            for name in ("PATH", "TMPDIR", "LANG"):
                self.assertEqual(environment[name], caller[name])
            self.assertNotEqual(environment["HOME"], caller["HOME"])
            self.assertTrue(Path(environment["HOME"]).is_dir())
            self.assertTrue(Path(environment["HOME"]).is_relative_to(root))
            self.assertEqual(environment["TERM"], "xterm-256color")
            fixture.environment["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:7"
            self.assertEqual(fixture.environment["ANTHROPIC_BASE_URL"], "http://127.0.0.1:7")


class VerifierEnvironmentTest(unittest.TestCase):
    def test_build_network_policy_does_not_reach_tests(self):
        with tempfile.TemporaryDirectory(prefix="m-verify-env-") as temporary:
            root = Path(temporary)
            (root / "scripts").mkdir()
            (root / "integration").mkdir()
            tools = root / "toolchain" / "bin"
            tools.mkdir(parents=True)
            shutil.copyfile(Path(__file__).parents[2] / "scripts" / "verify.sh", root / "scripts" / "verify.sh")
            build_capture = root / "build.env"
            test_capture = root / "test.env"
            go = tools / "go"
            go.write_text(
                "#!/usr/bin/env bash\nset -euo pipefail\n"
                "if [[ $1 == env ]]; then\n"
                f"  case $2 in GOROOT) echo {shlex.quote(str(tools.parent))} ;; "
                f"GOCACHE) echo {shlex.quote(str(root / 'cache'))} ;; "
                f"GOMODCACHE) echo {shlex.quote(str(root / 'modules'))} ;; esac\n"
                f"else env > {shlex.quote(str(build_capture))}; fi\n"
            )
            go.chmod(0o700)
            (root / "integration" / "probe.sh").write_text(f"env > {shlex.quote(str(test_capture))}\n")
            network = {
                "HTTP_PROXY": "http://127.0.0.1:9",
                "HTTPS_PROXY": "http://127.0.0.1:9",
                "NO_PROXY": "fixture.invalid",
                "https_proxy": "http://127.0.0.1:9",
                "GOPROXY": "https://fixture.invalid",
                "GONOSUMDB": "fixture.invalid/*",
                "GOFLAGS": "",
            }
            provider = {"ANTHROPIC_BASE_URL": "http://127.0.0.1:9", "CLAUDE_CONFIG_DIR": "/caller-claude"}
            caller = {"PATH": str(tools) + os.pathsep + os.defpath, "HOME": "/caller-home", "TMPDIR": str(root),
                      "TERM": "dumb", "LANG": "C", **network, **provider}
            result = subprocess.run(["bash", str(root / "scripts" / "verify.sh")], env=caller,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            build = dict(line.split("=", 1) for line in build_capture.read_text().splitlines())
            test = dict(line.split("=", 1) for line in test_capture.read_text().splitlines())
            for name, value in network.items():
                self.assertEqual(build.get(name), value)
                self.assertNotIn(name, test)
            for name in provider:
                self.assertNotIn(name, build)
                self.assertNotIn(name, test)
            self.assertNotEqual(build["HOME"], caller["HOME"])
            self.assertNotEqual(test["HOME"], caller["HOME"])
            self.assertNotEqual(build["HOME"], test["HOME"])


if __name__ == "__main__":
    unittest.main()
