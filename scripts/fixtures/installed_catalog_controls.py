"""Pure controls for passive installed-session evidence; no Mesh binaries."""
import ast
import json
import os
from pathlib import Path
import sqlite3
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from fixtures import installed_adoption, installed_catalog_evidence as evidence, installed_cohort, release_workers


class CatalogEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="catalog-control-", dir=os.environ.get("MESH_SHORT_TMP"))
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.state = self.root / "state"
        self.state.mkdir()
        self.database = sqlite3.connect(self.state / "mesh.db")
        self.addCleanup(self.database.close)
        self.database.executescript("PRAGMA journal_mode=WAL; CREATE TABLE hosts(id TEXT); INSERT INTO hosts VALUES('local'); CREATE TABLE sessions(host_id TEXT,id TEXT,state TEXT,created_at INTEGER);")
        self.observer = evidence.Observer(self.state, self.root / "evidence.json", 1)
        self.observer.host = "local"

    def insert(self, host="local", session="W000", commit=True):
        self.database.execute("INSERT INTO sessions VALUES(?,?,?,?)", (host, session, "detached", 1))
        if commit:
            self.database.commit()

    def meta(self):
        directory = self.state / "s/W000"
        directory.mkdir(parents=True)
        value = {"id": "W000", "pid": 42, "bootId": "boot", "state": "detached", "createdAt": "2026-11-13T00:00:00Z",
                 "command": ["private-command"], "cwd": "/private-cwd", "environment": "private-environment", "privateKey": "private-key"}
        (directory / "meta.json").write_text(json.dumps(value))
        (directory / ".launching").write_text("private-marker-token")
        return directory

    def test_readonly_connection_sees_committed_wal_and_wrong_host_but_not_uncommitted_rows(self):
        self.insert("other")
        self.insert("local", commit=False)
        before = evidence.database_snapshot(self.state, ("W000",), "local")
        self.assertEqual(before["localVisible"], [])
        self.assertEqual(before["rows"][0]["hostId"], "other")
        self.database.commit()
        self.assertTrue((self.state / "mesh.db-wal").exists())
        after = evidence.database_snapshot(self.state, ("W000",), "local")
        self.assertEqual(after["localVisible"], ["W000"])
        self.assertFalse((self.state / "missing.db").exists())
        with self.assertRaises(sqlite3.OperationalError):
            evidence.database_snapshot(self.root, ("W000",), "local")
        self.assertFalse((self.root / "mesh.db").exists())

    def test_metadata_and_marker_allowlists_exclude_commands_cwd_and_secrets(self):
        self.meta()
        result = evidence.session_snapshot(self.state, "W000")
        encoded = json.dumps(result)
        self.assertNotIn("private-", encoded)
        self.assertEqual(result["metadata"]["fields"]["bootId"], "boot")
        self.assertTrue(result["markers"][".launching"]["exists"])
        self.assertTrue(result["metadata"]["commandShape"]["nonemptyStrings"])

    def test_metadata_rejects_symlink_and_oversized_file_without_reading_target(self):
        directory = self.meta()
        (directory / "meta.json").unlink()
        (directory / "meta.json").symlink_to(self.state / "mesh.db")
        self.assertIn("unavailable", evidence.session_snapshot(self.state, "W000")["metadata"])
        (directory / "meta.json").unlink()
        (directory / "meta.json").write_bytes(b"x" * 16385)
        self.assertEqual(evidence.session_snapshot(self.state, "W000")["metadata"]["unavailable"], "ValueError")

    def added(self, session="ORIG"):
        return {"type": "state.event", "stateEvent": {"seq": 2, "kind": "session.added", "payload": {
            "session": {"id": session, "state": "detached", "command": ["private-command"]}}},
            "stateCurrent": {"seq": 2, "sections": {"sessions": {"ageMillis": 0}}}}

    def test_arm_uses_only_passive_topics_on_the_intended_daemon_and_closes_its_stream(self):
        connection, thread = Mock(), Mock()
        thread.is_alive.return_value = False
        initial = {"type": "state.snapshot", "stateSnapshot": {"seq": 1, "host": {"id": "local"}, "current": {"sessions": {"ageMillis": 12}}}}
        with patch.object(evidence.socket, "socket", return_value=connection), patch.object(evidence, "peer_identity", return_value={"pid": 10}), patch.object(evidence, "receive_control", return_value=initial), patch.object(evidence.threading, "Thread", return_value=thread), patch.object(release_workers, "control") as control:
            self.observer.start({"pid": 10})
        self.observer.consume(self.added())
        self.insert(session="ORIG")
        self.observer.prove("ORIG", {"pid": 10}, "local")
        self.observer.stop("control")
        self.assertEqual(control.call_args.args[1], {"type": "state.watch", "requestId": "catalog-evidence", "watch": {"topics": ["host", "sessions"]}})
        connection.connect.assert_called_once_with(str(self.state / "daemon.sock"))
        connection.close.assert_called_once()
        thread.join.assert_called_once_with(timeout=3)

    def test_normal_seed_control_requires_same_daemon_exact_event_and_committed_local_row(self):
        identity = {"pid": 10, "sha256": "exact"}
        self.observer.record["daemon"] = identity
        self.observer.consume(self.added())
        with self.assertRaisesRegex(RuntimeError, "committed local database row"):
            self.observer.prove("ORIG", identity, "local")
        self.insert(session="ORIG")
        self.observer.prove("ORIG", identity, "local")
        self.assertTrue(self.observer.record["positiveControl"]["passed"])
        self.assertNotIn("private-command", json.dumps(self.observer.record))
        with self.assertRaisesRegex(RuntimeError, "positive control failed"):
            self.observer.prove("ORIG", identity | {"pid": 11}, "local")

    def test_coalesced_changed_event_can_prove_normal_control_only_with_its_local_row(self):
        self.observer.record["daemon"] = {"pid": 10}
        value = self.added()
        value["stateEvent"]["kind"] = "session.changed"
        self.observer.consume(value)
        self.insert(session="ORIG")
        self.observer.prove("ORIG", {"pid": 10}, "local")
        self.assertEqual(self.observer.record["positiveControl"]["event"], "session.changed")

    def test_watch_drain_handles_fragmentation_and_retains_age_without_claiming_scan_start(self):
        value = self.added("W000")
        value["stateCurrent"]["sections"]["sessions"] = {"ageMillis": 1234, "failing": True}
        payload = json.dumps(value).encode()
        packet = b"\x01" + struct.pack(">I", len(payload)) + payload
        buffer = bytearray(packet[:7])
        self.observer.drain(buffer)
        self.assertEqual(self.observer.record["events"], [])
        buffer.extend(packet[7:])
        self.observer.drain(buffer)
        saved = self.observer.record["events"][0]
        self.assertEqual(saved["value"]["current"]["sessions"], {"ageMillis": 1234, "failing": True})
        self.assertNotIn("scanStartedAt", json.dumps(saved))
        self.assertEqual(buffer, b"")
        with self.assertRaisesRegex(RuntimeError, "complete watch coverage"):
            self.observer.consume({"type": "state.resync"})

    def test_timeline_stops_at_bound_and_close_is_idempotent(self):
        with patch.object(evidence, "MAX_EVENTS", 1):
            self.observer.add("control", {})
            with self.assertRaises(ValueError):
                self.observer.add("exceeds", {})
        self.observer.stop("failure")
        saved = self.observer.output.read_bytes()
        self.observer.stop("teardown")
        self.assertEqual(saved, self.observer.output.read_bytes())
        self.assertTrue(json.loads(saved)["observerStopped"])

    def cohort(self):
        cohort = object.__new__(installed_cohort.Cohort)
        cohort.record, cohort.state = self.root / "cohort.json", self.state
        cohort.catalog, cohort.selected = self.observer, []
        cohort.record.write_text('{"workers":[],"observations":[]}')
        return cohort

    def test_failed_verifier_is_not_retried_and_keeps_exact_error_full_streams_and_absent_row(self):
        self.meta()
        cohort = self.cohort()
        failure = subprocess.CalledProcessError(1, ["verify"], output="full stdout", stderr="original traceback\n" + "x" * 40000)
        with patch.object(cohort, "environment", return_value={}), patch.object(installed_cohort.subprocess, "run", side_effect=failure) as run:
            with self.assertRaises(subprocess.CalledProcessError) as caught:
                cohort.verify("before-install")
        self.assertIs(caught.exception, failure)
        run.assert_called_once()
        self.assertEqual(run.call_args.kwargs["timeout"], 90)
        self.assertEqual(cohort.record.with_suffix(".failure.stderr").read_text(), failure.stderr)
        self.assertEqual(cohort.record.with_suffix(".failure.stdout").read_text(), failure.stdout)
        saved = json.loads(self.observer.output.read_text())
        final = saved["events"][-1]["value"]
        self.assertEqual(final["label"], "after-failed-io")
        self.assertEqual(final["database"]["localVisible"], [])
        self.assertEqual(final["sessions"]["W000"]["metadata"]["fields"]["pid"], 42)

    def test_capture_failure_and_timeout_cannot_replace_original_verifier_exception(self):
        cohort = self.cohort()
        failures = (subprocess.CalledProcessError(7, ["verify"], stderr="original"), subprocess.TimeoutExpired(["verify"], 90))
        for failure in failures:
            self.check_capture_failure(cohort, failure)

    def test_timeout_exports_exact_partial_bytes_before_case_teardown(self):
        cohort = self.cohort()
        failure = subprocess.TimeoutExpired(["verify"], 90, output=b"partial stdout\x00", stderr=b"partial stderr\xff" + b"x" * 40000)
        with patch.object(cohort, "environment", return_value={}), patch.object(installed_cohort.subprocess, "run", side_effect=failure) as run:
            with self.assertRaises(subprocess.TimeoutExpired) as caught:
                cohort.verify("before-install")
        self.assertIs(caught.exception, failure)
        run.assert_called_once()
        self.assertEqual(run.call_args.kwargs["timeout"], 90)
        self.assertTrue(json.loads(self.observer.output.read_bytes())["observerStopped"])
        self.export_timeout(cohort, failure)

    def export_timeout(self, cohort, failure):
        output = self.root / "export"
        output.mkdir()
        case = Mock(root=self.root, host={"name": "pc"}, cohort=cohort)
        with patch.object(installed_adoption, "collect_service_snapshot"), patch.object(installed_adoption.installed_native, "retain_logs"):
            installed_adoption.finish_case(case, {"passed": False, "error": str(failure)}, output)
        self.assertEqual((output / "pc-cohort.failure.stdout").read_bytes(), failure.stdout)
        self.assertEqual((output / "pc-cohort.failure.stderr").read_bytes(), failure.stderr)
        saved = json.loads((output / "pc-cohort.json").read_bytes())["verificationFailure"]
        self.assertIsNone(saved["returncode"])
        self.assertEqual(saved["timeoutSeconds"], 90)
        self.assertTrue(saved["stderr"]["truncated"])
        self.assertEqual(saved["stderr"]["bytes"], len(failure.stderr))

    def check_capture_failure(self, cohort, failure):
        with patch.object(cohort, "environment", return_value={}), patch.object(installed_cohort.subprocess, "run", side_effect=failure), patch.object(self.observer, "stop", side_effect=OSError("disk full")):
            with self.assertRaises(type(failure)) as caught:
                cohort.verify("before-install")
        self.assertIs(caught.exception, failure)
        self.assertIn("catalog evidence unavailable: OSError", caught.exception.__notes__)

    def test_io_boundaries_record_original_attempt_without_private_payloads(self):
        path = self.root / "io.jsonl"
        with patch.dict(os.environ, {"MESH_CATALOG_IO_TIMELINE": str(path)}):
            evidence.io_boundary("W000", "begin")
            evidence.io_boundary("W000", "failed")
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        self.assertEqual([row["phase"] for row in rows], ["begin", "failed"])
        self.assertLessEqual(rows[0]["monotonicNs"], rows[1]["monotonicNs"])

    def test_actual_verifier_boundary_preserves_original_io_exception_without_retry(self):
        self.meta()
        worker = {"pid": 7, "shellPid": 42, "session": "W000"}
        failure = RuntimeError("sql: no rows in result set")
        path = self.root / "io.jsonl"
        with patch.dict(os.environ, {"MESH_CATALOG_IO_TIMELINE": str(path)}), patch.object(release_workers, "process_info", side_effect=((1, 11), (7, 12))), patch.object(release_workers, "session_io", side_effect=failure) as io:
            with self.assertRaises(RuntimeError) as caught:
                release_workers.verify_one(self.state, worker, "before-install", True)
        self.assertIs(caught.exception, failure)
        io.assert_called_once_with(self.state / "daemon.sock", "W000", "COHORT_before_install_W000")
        self.assertEqual([json.loads(line)["phase"] for line in path.read_text().splitlines()], ["begin", "failed"])

    def test_source_preserves_three_level_nesting(self):
        for path in (Path(evidence.__file__), Path(__file__), Path(installed_cohort.__file__), Path(release_workers.__file__)):
            self.assertLessEqual(depth(ast.parse(path.read_text())), 3, str(path))


def depth(node, nesting=0):
    if isinstance(node, ast.FunctionDef):
        nesting = 1
    elif isinstance(node, (ast.If, ast.For, ast.While, ast.With, ast.Try)):
        nesting += 1
    return max((depth(child, nesting) for child in ast.iter_child_nodes(node)), default=nesting)


if __name__ == "__main__":
    unittest.main()
