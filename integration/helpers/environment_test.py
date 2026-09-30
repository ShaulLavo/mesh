#!/usr/bin/env python3
"""The fixture environment must not depend on the developer's agent setup."""

import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
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


if __name__ == "__main__":
    unittest.main()
