"""Read-only receipts for one retained catalog and its actual executing images."""
from contextlib import closing
import hashlib
import json
import os
import re
from pathlib import Path
import sqlite3
import subprocess

from terminal_window import require


def image_digest(binary):
    digest = hashlib.sha256()
    with Path(binary).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def catalog_snapshot(state, session_id):
    path = state / "mesh.db"
    stat = path.stat()
    with closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)) as database:
        schema = database.execute("SELECT type, name, tbl_name, sql FROM sqlite_master "
                                  "WHERE sql IS NOT NULL ORDER BY type, name").fetchall()
        version = database.execute("SELECT max(version_id) FROM goose_db_version WHERE is_applied = 1").fetchone()[0]
        columns = [row[1] for row in database.execute("PRAGMA table_info(hosts)")]
        rows = database.execute("SELECT id, host_id, command, cwd, created_at FROM sessions "
                                "WHERE id = ? AND state IN ('running', 'detached') ORDER BY host_id", (session_id,)).fetchall()
    require(isinstance(version, int) and version > 0, "retained catalog schema version is absent or invalid")
    require(len(rows) == 1, "retained active session is absent or duplicated in the catalog")
    return {"device": stat.st_dev, "inode": stat.st_ino, "schemaVersion": version,
            "schema": schema, "hostColumns": columns, "immutableSessionRows": rows}


def private_host_migration(previous, current):
    if current["schemaVersion"] <= previous["schemaVersion"]:
        return False
    normalized = []
    changed = set()
    for kind, name, table, sql in current["schema"]:
        if kind == "table" and name in ("services", "cached_services"):
            stripped, count = re.subn(r",\s*private_host TEXT NOT NULL DEFAULT ''", "", sql)
            if count != 1:
                return False
            sql = stripped
            changed.add(name)
        normalized.append((kind, name, table, sql))
    return changed == {"services", "cached_services"} and current | {
        "schemaVersion": previous["schemaVersion"], "schema": normalized} == previous


class RetainedCatalog:
    def __init__(self, fixture, session_id, shell_pid, worker_pid, worker_binary):
        self.fixture = fixture
        self.session_id = session_id
        self.shell_pid = shell_pid
        self.worker_pid = worker_pid
        self.initial = catalog_snapshot(fixture.remote, session_id)
        self.credentials = {path: path.read_bytes() for path in (
            fixture.remote / "identity.key", fixture.remote / "authorized_keys", fixture.local / "identity.key")}
        self.worker_build = json.loads(self.metadata_path().read_text())["build"]
        require(self.initial["schemaVersion"] == self.worker_build["stateVersion"],
                "retained catalog schema version differs from the source contract")
        require(self.worker_build["digest"] == image_digest(worker_binary),
                "retained worker was not created by the selected source image")

    def metadata_path(self):
        return self.fixture.remote / "s" / self.session_id / "meta.json"

    def observe(self, phase, daemon_binary, host):
        current = catalog_snapshot(self.fixture.remote, self.session_id)
        if private_host_migration(self.initial, current):
            self.initial = current
        require(current == self.initial, "source hop replaced catalog inode, schema or immutable session rows")
        require(all(path.read_bytes() == contents for path, contents in self.credentials.items()),
                "source hop changed existing device credentials or approved grant policy")
        metadata = json.loads(self.metadata_path().read_text())
        require(metadata["build"] == self.worker_build and metadata["pid"] == self.shell_pid,
                "source hop replaced the retained worker build or shell identity")
        os.kill(self.worker_pid, 0)
        parent = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(self.shell_pid)]))
        require(parent == self.worker_pid, "retained shell moved to another worker")
        digest = image_digest(daemon_binary)
        require(host["build"]["digest"] == digest and host["build"]["stateVersion"] in {self.worker_build["stateVersion"], self.initial["schemaVersion"]},
                "destination did not run the selected daemon image and state contract")
        require(host["id"] == host["meshIdentity"] == self.initial["immutableSessionRows"][0][1],
                "source hop changed the retained session's cryptographic destination")
        return {"phase": phase, "daemonBuild": host["build"], "workerBuild": self.worker_build,
                "catalogDevice": current["device"], "catalogInode": current["inode"],
                "catalogSchemaVersion": current["schemaVersion"], "catalogHostColumns": current["hostColumns"],
                "catalogSchemaDigest": hashlib.sha256(json.dumps(current["schema"]).encode()).hexdigest(),
                "immutableSessionFields": ["id", "host_id", "command", "cwd", "created_at"],
                "immutableActiveSessionPreserved": True, "existingCredentialsPreserved": True,
                "freshInputOutput": True}
