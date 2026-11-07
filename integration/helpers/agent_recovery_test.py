"""Invocation filenames become visible only after their JSON is complete."""

import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from agent_recovery import PROVIDER


class InvocationPublicationTest(unittest.TestCase):
    def assert_publication(self, resumed):
        with tempfile.TemporaryDirectory(prefix="m-ap-") as temporary:
            self.check_provider(Path(temporary), resumed)

    def check_provider(self, root, resumed):
        identity = "conversation one"
        argv = ["claude", "--model", identity]
        if resumed:
            argv.append("--resume=" + identity)
        (root / "allow-resume").touch()
        expected = {"argv": argv[1:], "id": identity, "cwd": os.getcwd(), "root": str(root / "claude-state")}
        destination = root / "launch-test-session.json"
        environment = {"MESH_TEST_AGENT_ROOT": str(root), "MESH_SESSION_ID": "test-session",
                       "CLAUDE_CONFIG_DIR": expected["root"], "MESH_TEST_AGENT_BINARY": "fixture-mesh"}

        def interrupted_write(path, text):
            with path.open("w") as output:
                output.write(text[:1])
                output.flush()
                self.assertEqual(list(root.glob("launch-*.json")), [], "incomplete invocation is visible")
                output.write(text[1:])
                output.flush()
                self.assertFalse(destination.exists(), "invocation published before writer closes")
            return len(text)

        def acknowledge(command, *, input, check):
            self.assertEqual(command, ["fixture-mesh", "agent-hook", "claude"])
            self.assertTrue(check)
            self.assertEqual(json.loads(input)["session_id"], identity)
            self.assertEqual(json.loads(destination.read_text()), expected)
            self.assertCountEqual(list(root.iterdir()), [root / "allow-resume", destination])

        with patch.dict(os.environ, environment), patch.object(sys, "argv", argv), \
                patch.object(sys, "stdin", io.StringIO("\n")), patch.object(sys, "stdout", io.StringIO()), \
                patch.object(Path, "write_text", interrupted_write), patch("subprocess.run", side_effect=acknowledge) as hook:
            exec(compile(PROVIDER, "fake-claude", "exec"), {})
        hook.assert_called_once()
        self.assertEqual(json.loads(destination.read_text()), expected)

    def test_startup_invocation_is_published_complete(self):
        self.assert_publication(False)

    def test_resume_invocation_is_published_complete(self):
        self.assert_publication(True)


if __name__ == "__main__":
    unittest.main()
