import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
import runpy
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]


def workflow_command():
    lines = (ROOT / ".github/workflows/ci.yml").read_text().splitlines()
    start = lines.index("      - name: Require main as the repository default branch")
    command = next(line.split("run: ", 1)[1] for line in lines[start + 1:] if line.startswith("        run: "))
    return command.replace("${{ github.repository }}", "ShaulLavo/mesh")


@unittest.skipUnless(os.name == "posix" and shutil.which("bash"), "command fixtures require POSIX and Bash")
class DefaultBranchTests(unittest.TestCase):
    def run_check(self, responses):
        with tempfile.TemporaryDirectory(prefix="mesh-default-branch-") as directory:
            root = Path(directory)
            state = root / "state.json"
            state.write_text(json.dumps({"calls": 0, "responses": responses}))
            gh = root / "gh"
            gh.write_text(f"#!{sys.executable}\n" + '''import json, os, sys
from pathlib import Path
path = Path(os.environ["MESH_TEST_BRANCH_STATE"])
state = json.loads(path.read_text())
response = state["responses"][min(state["calls"], len(state["responses"]) - 1)]
state["calls"] += 1
path.write_text(json.dumps(state))
print(response.get("stdout", ""), end="")
print(response.get("stderr", ""), end="", file=sys.stderr)
sys.exit(response["code"])
''')
            gh.chmod(0o755)
            result = subprocess.run(
                ["bash", "-ec", workflow_command()], cwd=ROOT,
                env={**os.environ, "PATH": str(root) + os.pathsep + os.environ.get("PATH", ""), "MESH_TEST_BRANCH_STATE": str(state)},
                text=True, capture_output=True, timeout=10,
            )
            return result, json.loads(state.read_text())["calls"]

    def test_main_is_accepted_once(self):
        result, calls = self.run_check([{"code": 0, "stdout": "main\n"}])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, 1)

    def test_503_recovers_without_waiving_branch_assertion(self):
        result, calls = self.run_check([{"code": 1, "stderr": "gh: unavailable (HTTP 503)\n"}, {"code": 0, "stdout": "main\n"}])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, 2)

    def test_permanent_http_errors_stop(self):
        for status in (401, 403, 404, 501):
            with self.subTest(status=status):
                result, calls = self.run_check([{"code": 1, "stderr": f"gh: refused (HTTP {status})\n"}])
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, 1)

    def test_wrong_branch_stops(self):
        result, calls = self.run_check([{"code": 0, "stdout": "other\n"}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, 1)

    def test_transient_attempts_are_bounded(self):
        result, calls = self.run_check([{"code": 1, "stderr": "gh: unavailable (HTTP 503)\n"}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, 3)

    def test_per_attempt_timeout_uses_remaining_total_budget(self):
        check = runpy.run_path(str(ROOT / "scripts/check-default-branch.py"))["check"]
        now = [0.0]
        timeouts = []

        def run(_command, **options):
            timeouts.append(options["timeout"])
            now[0] += options["timeout"]
            return subprocess.CompletedProcess([], 1, "", "gh: unavailable (HTTP 503)")

        def sleep(delay):
            now[0] += delay

        with patch("subprocess.run", run), patch("time.monotonic", lambda: now[0]), patch("time.sleep", sleep):
            with self.assertRaises(RuntimeError):
                check("ShaulLavo/mesh")
        self.assertEqual(timeouts, [20, 20, 18.5])
        self.assertEqual(now[0], 60)

    def test_command_timeout_does_not_retry(self):
        check = runpy.run_path(str(ROOT / "scripts/check-default-branch.py"))["check"]
        with patch("subprocess.run", side_effect=subprocess.TimeoutExpired("gh", 20)) as run:
            with self.assertRaises(subprocess.TimeoutExpired):
                check("ShaulLavo/mesh")
        self.assertEqual(run.call_count, 1)


if __name__ == "__main__":
    unittest.main()
