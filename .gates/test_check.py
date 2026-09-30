import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import check


class BaselineTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.reports = self.root / "reports"
        self.reports.mkdir()
        (self.root / "sample.go").write_text("package sample\n\nfunc sample() {}\n")
        self.baseline = self.root / "baseline.json"
        self.write_baseline([])
        self.write_reports()

    def write_baseline(self, entries):
        self.baseline.write_text(json.dumps({"version": 1, "entries": entries}))

    def write_reports(self, issues=None):
        (self.reports / "golangci.json").write_text(json.dumps({"Issues": issues or []}))
        for gate in ("deadcode", "shellcheck", "ruff"):
            (self.reports / f"{gate}.json").write_text("[]")

    def issue(self, line=3, text="duplicated with sample.go:3-8"):
        return {"Pos": {"Filename": "sample.go", "Line": line}, "FromLinter": "dupl", "Text": text}

    def entry(self, count=1):
        return {"gate": "golangci", "file": "sample.go", "rule": "dupl", "text": "duplicated with sample.go:<location>", "source": "func sample() {}", "count": count, "reason": "Existing clone; extraction is assigned to Phase 4."}

    def run_check(self, **kwargs):
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return check.check(self.reports, self.root, self.baseline, **kwargs)

    def test_new_finding_fails(self):
        self.write_reports([self.issue()])
        self.assertEqual(self.run_check(), 1)

    def test_update_refuses_addition_without_partial_removal(self):
        self.write_baseline([self.entry()])
        self.write_reports([self.issue(text="new clone")])
        before = self.baseline.read_bytes()
        self.assertEqual(self.run_check(update=True), 1)
        self.assertEqual(self.baseline.read_bytes(), before)

    def test_fixed_entry_fails_until_removed(self):
        self.write_baseline([self.entry()])
        self.assertEqual(self.run_check(), 1)
        self.assertEqual(self.run_check(update=True), 0)
        self.assertEqual(json.loads(self.baseline.read_text())["entries"], [])

    def test_duplicate_count_is_not_an_unlimited_exception(self):
        self.write_baseline([self.entry()])
        self.write_reports([self.issue(), self.issue()])
        self.assertEqual(self.run_check(), 1)

    def test_count_can_only_decrease(self):
        self.write_baseline([self.entry(count=2)])
        self.write_reports([self.issue()])
        self.assertEqual(self.run_check(update=True), 0)
        self.assertEqual(json.loads(self.baseline.read_text())["entries"][0]["count"], 1)

    def test_line_movement_preserves_key(self):
        self.write_baseline([self.entry()])
        (self.root / "sample.go").write_text("package sample\n\n\nfunc sample() {}\n")
        self.write_reports([self.issue(line=4, text="duplicated with sample.go:4-9")])
        self.assertEqual(self.run_check(), 0)

    def test_changed_source_is_a_new_finding(self):
        self.write_baseline([self.entry()])
        (self.root / "sample.go").write_text("package sample\n\nfunc different() {}\n")
        self.write_reports([self.issue()])
        self.assertEqual(self.run_check(), 1)

    def test_blank_reason_is_invalid(self):
        entry = self.entry()
        entry["reason"] = " "
        self.write_baseline([entry])
        with self.assertRaisesRegex(ValueError, "needs a reason"):
            check.baseline_entries(self.baseline)

    def test_duplicate_key_is_invalid(self):
        self.write_baseline([self.entry(), self.entry()])
        with self.assertRaisesRegex(ValueError, "duplicate baseline"):
            check.baseline_entries(self.baseline)

    def test_partial_scan_does_not_remove_unscanned_entries(self):
        self.write_baseline([self.entry()])
        self.assertEqual(self.run_check(scope={"internal/other"}), 0)
        self.assertEqual(len(json.loads(self.baseline.read_text())["entries"]), 1)

    def test_third_party_is_excluded(self):
        issue = self.issue()
        issue["Pos"]["Filename"] = "third_party/sample.go"
        self.write_reports([issue])
        self.assertEqual(self.run_check(), 0)

    def test_reports_must_not_escape_root(self):
        issue = self.issue()
        issue["Pos"]["Filename"] = "../sample.go"
        self.write_reports([issue])
        with self.assertRaisesRegex(ValueError, "escapes repository"):
            self.run_check()

    def test_malformed_report_is_not_a_clean_scan(self):
        (self.reports / "golangci.json").write_text("{}")
        with self.assertRaises(KeyError):
            self.run_check()


if __name__ == "__main__":
    unittest.main()
