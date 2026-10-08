#!/usr/bin/env python3
"""Printed naming I/O markers require complete output lines, not echoed input."""
import json
import os
from pathlib import Path
import sqlite3
import sys
import tempfile
from types import SimpleNamespace
import unittest

from machine_naming import printed_marker
from naming_transition import RetainedCatalog, catalog_snapshot, image_digest


class PrintedMarkerTest(unittest.TestCase):
    marker = "BEFORE_NAME_IO"
    echoed = b"MESH_PROMPT> printf 'BEFORE_NAME_IO\\n'\r\n"

    def test_actual_darwin_output_boundary(self):
        output = self.echoed + b"BEFORE_NAME_IO\r\nMESH_PROMPT> "
        self.assertTrue(printed_marker(output, self.marker))

    def test_actual_linux_readline_boundary(self):
        output = self.echoed + b"\x1b[?2004l\rBEFORE_NAME_IO\r\n\x1b[?2004hMESH_PROMPT> "
        self.assertTrue(printed_marker(output, self.marker))

    def test_wrong_marker_is_rejected(self):
        for output in (b"WRONG_BEFORE_NAME_IO\r\n", b"BEFORE_NAME_IO_WRONG\r\n"):
            with self.subTest(output=output):
                self.assertFalse(printed_marker(self.echoed + output, self.marker))

    def test_absent_marker_is_rejected(self):
        self.assertFalse(printed_marker(b"MESH_PROMPT> ", self.marker))

    def test_echo_only_is_rejected(self):
        self.assertFalse(printed_marker(self.echoed, self.marker))

    def test_partial_line_is_rejected(self):
        self.assertFalse(printed_marker(self.echoed + b"BEFORE_NAME_IO", self.marker))


class RetainedCatalogTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="mesh-name-guard-")
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.fixture = SimpleNamespace(remote=root / "remote", local=root / "local")
        self.fixture.remote.mkdir()
        self.fixture.local.mkdir()
        self.database = self.fixture.remote / "mesh.db"
        with sqlite3.connect(self.database) as database:
            database.executescript("""
                CREATE TABLE hosts (id TEXT, alias TEXT);
                CREATE TABLE services (name TEXT);
                CREATE TABLE cached_services (name TEXT);
                CREATE TABLE sessions (id TEXT, host_id TEXT, command TEXT, cwd TEXT,
                                       created_at INTEGER, state TEXT);
                CREATE TABLE goose_db_version (version_id INTEGER, is_applied INTEGER);
                INSERT INTO goose_db_version VALUES (10, 1);
                INSERT INTO hosts VALUES ('destination', 'legacy-owner');
                INSERT INTO sessions VALUES ('7K3D', 'destination', '["sh"]', '/fixture', 1, 'running');
            """)
        for path in (self.fixture.remote / "identity.key", self.fixture.remote / "authorized_keys",
                     self.fixture.local / "identity.key"):
            path.write_bytes(b"disposable guard-test bytes")
        metadata = self.fixture.remote / "s" / "7K3D" / "meta.json"
        metadata.parent.mkdir(parents=True)
        build = {"digest": image_digest(sys.executable), "stateVersion": 10}
        metadata.write_text(json.dumps({"pid": os.getpid(), "build": build}))
        self.host = {"id": "destination", "meshIdentity": "destination", "build": build}
        self.retained = RetainedCatalog(self.fixture, "7K3D", os.getpid(), os.getppid(), sys.executable)

    def observe(self):
        return self.retained.observe("guard-control", sys.executable, self.host)

    def test_read_only_known_good_preserves_catalog_and_credentials(self):
        before = self.database.read_bytes()
        receipt = self.observe()
        self.assertEqual(self.database.read_bytes(), before)
        self.assertTrue(receipt["existingCredentialsPreserved"])
        self.assertTrue(receipt["immutableActiveSessionPreserved"])
        self.assertNotIn("credentials", receipt)

    def test_lifecycle_fields_may_change(self):
        before = catalog_snapshot(self.fixture.remote, "7K3D")
        with sqlite3.connect(self.database) as database:
            database.execute("UPDATE sessions SET state = 'detached'")
        self.assertEqual(catalog_snapshot(self.fixture.remote, "7K3D"), before)
        self.observe()

    def test_exited_catalog_row_is_refused(self):
        with sqlite3.connect(self.database) as database:
            database.execute("UPDATE sessions SET state = 'exited'")
        with self.assertRaisesRegex(RuntimeError, "active session is absent"):
            self.observe()

    def test_immutable_launch_row_change_is_refused(self):
        with sqlite3.connect(self.database) as database:
            database.execute("UPDATE sessions SET command = ?", (json.dumps(["replacement"]),))
        with self.assertRaisesRegex(RuntimeError, "immutable session rows"):
            self.observe()

    def test_same_version_changed_schema_is_refused(self):
        with sqlite3.connect(self.database) as database:
            database.execute("ALTER TABLE hosts DROP COLUMN alias")
        with self.assertRaisesRegex(RuntimeError, "schema"):
            self.observe()

    def test_private_host_migration_preserves_catalog_and_worker(self):
        with sqlite3.connect(self.database) as database:
            database.execute("ALTER TABLE services ADD COLUMN private_host TEXT NOT NULL DEFAULT ''")
            database.execute("ALTER TABLE cached_services ADD COLUMN private_host TEXT NOT NULL DEFAULT ''")
            database.execute("INSERT INTO goose_db_version VALUES (11, 1)")
        self.host["build"] = self.host["build"] | {"stateVersion": 11}
        receipt = self.observe()
        self.assertEqual(receipt["catalogSchemaVersion"], 11)
        self.assertEqual(receipt["workerBuild"]["stateVersion"], 10)
        self.observe()

    def test_private_host_migration_refuses_unrelated_schema_change(self):
        with sqlite3.connect(self.database) as database:
            database.execute("ALTER TABLE services ADD COLUMN private_host TEXT NOT NULL DEFAULT ''")
            database.execute("ALTER TABLE cached_services ADD COLUMN private_host TEXT NOT NULL DEFAULT ''")
            database.execute("ALTER TABLE hosts DROP COLUMN alias")
            database.execute("INSERT INTO goose_db_version VALUES (11, 1)")
        self.host["build"] = self.host["build"] | {"stateVersion": 11}
        with self.assertRaisesRegex(RuntimeError, "schema"):
            self.observe()

    def test_equivalent_recreated_catalog_is_refused(self):
        replacement = self.database.with_suffix(".replacement")
        replacement.write_bytes(self.database.read_bytes())
        replacement.replace(self.database)
        with self.assertRaisesRegex(RuntimeError, "inode"):
            self.observe()

    def test_existing_grant_policy_change_is_refused(self):
        (self.fixture.remote / "authorized_keys").write_bytes(b"changed guard-test policy")
        with self.assertRaisesRegex(RuntimeError, "credentials"):
            self.observe()

    def test_different_executing_daemon_image_is_refused(self):
        self.host["build"] = self.host["build"] | {"digest": "0" * 64}
        with self.assertRaisesRegex(RuntimeError, "daemon image"):
            self.observe()


if __name__ == "__main__":
    unittest.main()
