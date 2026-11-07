from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
CHECKS = {"app": 3, "hosting": 5}
SOURCES = (
    "cmd/mesh/main.go",
    "internal/apps/app.go",
    "internal/cli/app.go",
    "internal/cli/command.go",
    "internal/cli/serve_command.go",
    "web/app-pill/src/index.ts",
    "internal/apppill/assets/pill.css",
)
HITS = (
    ("internal/apps/app.go", "var _ = apppill.Widget\n", "public dispatch dependency"),
    ("internal/cli/app.go", "var Visibility string\n", "visibility field"),
    ("web/app-pill/src/index.ts", 'const marker = "mesh-app-status";\n', "Mesh sharing controls"),
    ("cmd/mesh/main.go", "var TypeTunnelClaim int\n", "dependency or contract"),
    ("internal/cli/command.go", 'const flag = "wake-on-request"\n', "command or flag"),
)


class PrivateBoundaryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mesh-private-boundary-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for tool in ("bash", "dirname"):
            (self.bin / tool).symlink_to(shutil.which(tool))
        scripts = self.root / "scripts"
        scripts.mkdir()
        for source in ROOT.glob("scripts/*private*boundary*.sh"):
            shutil.copy2(source, scripts / source.name)
        for source in SOURCES:
            self.write(source, "// Clean source.\n")

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def run_check(self, check, **environment):
        return subprocess.run(
            [str(self.bin / "bash"), f"scripts/check-private-{check}-boundary.sh"],
            cwd=self.root,
            env={"PATH": str(self.bin), "LC_ALL": "C", **environment},
            capture_output=True, text=True, timeout=10,
        )

    def use_real_search(self):
        for tool in ("rg", "grep"):
            executable = shutil.which(tool)
            if executable:
                (self.bin / tool).symlink_to(executable)

    def stub_search(self):
        stub = f"#!{sys.executable}\n" + '''import os
from pathlib import Path
import sys
state = Path(os.environ["CALLS"])
count = int(state.read_text()) + 1 if state.exists() else 1
state.write_text(str(count))
status = int(os.environ["STATUS"]) if count == int(os.environ["AT"]) else 1
if status == 2:
    print("search failed", file=sys.stderr)
sys.exit(status)
'''
        for tool in ("rg", "grep"):
            path = self.bin / tool
            path.write_text(stub)
            path.chmod(0o755)

    def assert_refused(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("PASS:", result.stdout + result.stderr)

    def test_missing_search_fails_closed(self):
        for check in CHECKS:
            with self.subTest(check=check):
                result = self.run_check(check)
                self.assert_refused(result)
                self.assertIn("command not found", result.stderr)

    def test_every_search_distinguishes_clean_hit_and_error(self):
        self.stub_search()
        for check, searches in CHECKS.items():
            for status in (0, 1, 2):
                self.check_search_outcomes(check, searches, status)

    def check_search_outcomes(self, check, searches, status):
        calls = self.root / "calls"
        for at in range(1, searches + 1):
            with self.subTest(check=check, status=status, at=at):
                calls.unlink(missing_ok=True)
                result = self.run_check(check, CALLS=str(calls), STATUS=str(status), AT=str(at))
                self.assertEqual(int(calls.read_text()), searches if status == 1 else at)
                self.check_search_result(result, status)

    def check_search_result(self, result, status):
        if status == 1:
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("PASS:", result.stdout)
            return
        self.assert_refused(result)
        if status == 2:
            self.assertIn("search failed", result.stderr)

    def test_real_clean_scan_and_excluded_go_files(self):
        self.use_real_search()
        self.write("internal/apps/app_test.go", "apppill.Widget Visibility TypeTunnelClaim\n")
        self.write("internal/apppill/pill.go", "apppill.Widget TypeTunnelClaim\n")
        for check in CHECKS:
            with self.subTest(check=check):
                result = self.run_check(check)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("PASS:", result.stdout)

    def test_real_findings_at_each_boundary(self):
        self.use_real_search()
        for source, content, message in HITS:
            self.write(source, content)
            self.check_real_finding(source, message)
            self.write(source, "// Clean source.\n")

    def check_real_finding(self, source, message):
        checks = ("hosting",) if source.startswith("cmd/") or source.endswith("command.go") else CHECKS
        for check in checks:
            with self.subTest(check=check, source=source):
                result = self.run_check(check)
                self.assert_refused(result)
                self.assertIn(message, result.stderr)
                self.assertIn(source, result.stdout)

    def test_real_search_error_after_an_earlier_clean_scan(self):
        self.use_real_search()
        (self.root / "internal/cli/app.go").unlink()
        for check in CHECKS:
            with self.subTest(check=check):
                result = self.run_check(check)
                self.assert_refused(result)
                self.assertIn("internal/cli/app.go", result.stderr)

    def test_retired_hosting_paths_still_fail(self):
        self.use_real_search()
        for retired in ("internal/edge", "internal/tunnel", "integration/fixtures/public-edge-renewer.pem"):
            with self.subTest(retired=retired):
                self.write(retired, "retired\n")
                result = self.run_check("hosting")
                self.assert_refused(result)
                self.assertIn(retired, result.stderr)
                (self.root / retired).unlink()


if __name__ == "__main__":
    unittest.main()
