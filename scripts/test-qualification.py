#!/usr/bin/env python3
"""Controls for read-only installation of a trusted writer's snapshot."""
import importlib
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

qualification = importlib.import_module("qualify-publication")
ROOT = Path(__file__).resolve().parents[1]
FINALIZER = {"name": "Publish tested main commit / Publish verified release", "status": "completed",
             "conclusion": "skipped", "run_attempt": 2}
WRITER = {"status": "completed", "run_attempt": 2}
NOT_FOUND = subprocess.CompletedProcess(["gh", "api", "--include"], 1, b"HTTP/2 404 Not Found\n\r\n", b"")


class QualificationTests(unittest.TestCase):
    def test_attempt_and_job_window_both_bind_the_upload(self):
        name = qualification.DRAFT
        created = "2026-11-20T14:31:06Z"
        job = {"name": "Publish tested main commit / Sign complete release draft", "status": "completed",
               "conclusion": "success", "run_attempt": 1, "started_at": "2026-11-20T14:20:38Z",
               "completed_at": "2026-11-20T14:31:14Z"}
        qualification.successful_producer(name, [job], 1, created)
        for jobs in ([], [job, job], [job | {"run_attempt": 2}], [job | {"started_at": "2026-11-20T14:31:07Z"}],
                     [job | {"name": "Sign on a different job"}], [job | {"conclusion": "failure"}]):
            with self.subTest(jobs=jobs), self.assertRaisesRegex(ValueError, "unique successful producer"):
                qualification.successful_producer(name, jobs, 1, created)

    def test_archive_pin_requires_exact_id_origin_size_and_digest(self):
        pin = {"id": 8, "size": 1024, "digest": "sha256:" + "a" * 64}
        archive = {"id": 8, "name": qualification.DRAFT, "size_in_bytes": 1024, "digest": pin["digest"],
                   "expired": False, "workflow_run": {"id": 6}}
        qualification.check_artifact(qualification.DRAFT, pin, 6, archive)
        for change in ({"id": 9}, {"name": qualification.TRUST}, {"digest": "sha256:" + "b" * 64},
                       {"size_in_bytes": 1025}, {"expired": True}, {"workflow_run": {"id": 5}}):
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, "pinned live writer archive"):
                qualification.check_artifact(qualification.DRAFT, pin, 6, archive | change)

    def test_read_only_replay_rejects_published_or_potentially_published_releases(self):
        with patch.object(qualification.installed_artifacts, "api", return_value=WRITER), \
                patch.object(qualification.installed_artifacts, "pages") as jobs, \
                patch.object(qualification.subprocess, "run", return_value=NOT_FOUND) as tag:
            jobs.return_value = [FINALIZER]
            qualification.confirm_unpublished("v0.1.347", 6)
            tag.assert_called_once_with(["gh", "api", "--include", "repos/ShaulLavo/mesh/releases/tags/v0.1.347"],
                                        capture_output=True, timeout=30, check=False)
            for conclusion in (None, "success", "cancelled", "failure"):
                jobs.return_value = [FINALIZER, FINALIZER | {"conclusion": conclusion, "run_attempt": 1}]
                with self.subTest(conclusion=conclusion), self.assertRaisesRegex(ValueError, "may have started"):
                    qualification.confirm_unpublished("v0.1.347", 6)
            jobs.return_value = [FINALIZER]
            tag.return_value = subprocess.CompletedProcess(["gh", "api"], 0, b'{"draft": false}', b"")
            with self.assertRaisesRegex(ValueError, "never been published"):
                qualification.confirm_unpublished("v0.1.347", 6)
            tag.return_value = subprocess.CompletedProcess(["gh", "api"], 1, b"HTTP/2 503 Service Unavailable\n\r\n",
                                                           b"gh: Missing release (HTTP 404)")
            with self.assertRaisesRegex(ValueError, "could not establish"):
                qualification.confirm_unpublished("v0.1.347", 6)

    def test_failed_jobs_rerun_uses_exact_handoff_without_guessing_attempt(self):
        with patch.dict(os.environ, GITHUB_RUN_ATTEMPT="2"):
            self.assertEqual(qualification.parse_handoff("7:1"), (7, 1))
            self.assertEqual(qualification.parse_handoff("8:2"), (8, 2))
            for value in ("", "0:2", "8:0", "8:3"):
                with self.subTest(value=value), self.assertRaises(ValueError):
                    qualification.parse_handoff(value)

    def test_partial_job_inventory_cannot_establish_that_the_writer_skipped_publication(self):
        for rows in ([], [FINALIZER | {"run_attempt": 1}], [FINALIZER, FINALIZER]):
            with self.subTest(rows=rows), patch.object(qualification.installed_artifacts, "api", return_value=WRITER), \
                    patch.object(qualification.installed_artifacts, "pages", return_value=rows), \
                    self.assertRaisesRegex(ValueError, "completed skipped finalizer"):
                qualification.confirm_unpublished("v0.1.347", 6)

    def test_writer_must_be_completed_even_when_a_finalizer_is_skipped(self):
        with patch.object(qualification.installed_artifacts, "api", return_value=WRITER | {"status": "in_progress"}), \
                patch.object(qualification.installed_artifacts, "pages", return_value=[FINALIZER]), \
                self.assertRaisesRegex(ValueError, "completed draft writer"):
            qualification.confirm_unpublished("v0.1.347", 6)

    def test_check_run_accepts_main_ci_push_and_release_schedule_or_dispatch(self):
        source = "a" * 40
        spec = {"run": 6, "release": {"version": "v0.1.350", "source": source, "id": 42}}
        base = {"head_repository": {"full_name": qualification.REPO}, "head_branch": "main",
                "head_sha": source, "path": ".github/workflows/ci.yml", "event": "push"}
        with patch.dict(os.environ, GITHUB_RUN_ID="99"):
            for path, event in ((".github/workflows/ci.yml", "push"),
                                (".github/workflows/release.yml", "schedule"),
                                (".github/workflows/release.yml", "workflow_dispatch")):
                with self.subTest(path=path, event=event), \
                        patch.object(qualification.installed_artifacts, "api", return_value=base | {"path": path, "event": event}):
                    qualification.check_run(spec)
            for bad in ({"head_repository": {"full_name": "other/mesh"}}, {"head_branch": "feature"},
                        {"head_sha": "b" * 40}, {"path": ".github/workflows/ci.yml", "event": "pull_request"},
                        {"path": ".github/workflows/ci.yml", "event": "workflow_dispatch"},
                        {"path": ".github/workflows/release.yml", "event": "push"},
                        {"path": ".github/workflows/release.yml", "event": "pull_request"}):
                with self.subTest(bad=bad), \
                        patch.object(qualification.installed_artifacts, "api", return_value=base | bad), \
                        self.assertRaisesRegex(ValueError, "exact main publication run"):
                    qualification.check_run(spec)
        with patch.dict(os.environ, GITHUB_RUN_ID="6"), \
                patch.object(qualification.installed_artifacts, "api", return_value=base), \
                self.assertRaisesRegex(ValueError, "separate read-only evidence run"):
            qualification.check_run(spec)

    def test_qualification_has_only_read_permissions_and_the_release_gate_stays_required(self):
        workflows = ROOT / ".github/workflows"
        replay = (workflows / "qualify-publication.yml").read_text() + (ROOT / "scripts/qualify-publication.sh").read_text()
        for forbidden in (": write", "secrets.", "self-hosted", "finalize-release.sh", "gh release edit", "release.yml"):
            self.assertNotIn(forbidden, replay)
        release = (workflows / "release.yml").read_text()
        final = release.split("\n  finalize:\n")[1]
        self.assertIn("needs: [reserve, publish, verify-readers, publication-native]", final)
        self.assertIn("needs.verify-readers.result == 'success'", final)
        self.assertIn("needs.publication-native.result == 'success'", final)
        self.assertNotIn("qualify-publication", release)


if __name__ == "__main__":
    unittest.main()
