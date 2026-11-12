"""Database checkpoints and daemon writes for the isolated native release proof."""

import hashlib
import json
import os
from pathlib import Path
import plistlib
import re
import shutil
import signal
import socket
import sqlite3
import struct
import subprocess
import sys


def database_snapshot(source, destination):
    with sqlite3.connect(source) as original, sqlite3.connect(destination) as copy:
        original.backup(copy)


def schema(path):
    with sqlite3.connect(path) as database:
        definitions = database.execute(
            "SELECT type,name,tbl_name,sql FROM sqlite_master "
            "WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name"
        ).fetchall()
        history = database.execute(
            "SELECT version_id,is_applied FROM goose_db_version ORDER BY id"
        ).fetchall()
    return {"definitions": definitions, "history": history}


def request(state, value):
    value["requestId"] = "rollback-proof-" + value["type"]
    payload = json.dumps(value).encode()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(10)
        connection.connect(str(state / "daemon.sock"))
        connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)
        with connection.makefile("rb") as stream:
            header = stream.read(5)
            assert len(header) == 5 and header[0] == 1, header
            length = struct.unpack(">I", header[1:])[0]
            assert 0 < length <= 4 << 20, length
            response = json.loads(stream.read(length))
    assert response["type"] != "error", response
    return response


def candidate_writes(state):
    site = state / "rollback-site"
    site.mkdir(exist_ok=True)
    (site / "index.html").write_text("rollback proof\n")
    service = {"name": "rollback-proof", "kind": "static", "target": str(site),
               "displayName": "Candidate rollback proof", "isolate": True}
    response = request(state, {"type": "service.upsert", "service": service})
    assert response["type"] == "service.upserted", response
    host = request(state, {"type": "host.info"})["host"]
    response = request(state, {"type": "host.rename", "rename": {
        "targetId": host["id"], "machineName": "rollback-proof",
        "expectedRevision": host["nameRevision"],
    }})
    assert response["type"] == "host.renamed", response
    (state / "rollback-name.before").write_bytes((state / "machine-name.json").read_bytes())
    verify_writes(state)


def verify_writes(state):
    with sqlite3.connect(state / "mesh.db") as database:
        service = database.execute(
            "SELECT kind,target,display_name,isolate FROM services WHERE name='rollback-proof'"
        ).fetchone()
        assert service == ("static", str((state / "rollback-site").resolve()), "Candidate rollback proof", 1), service
    assert (state / "machine-name.json").read_bytes() == (state / "rollback-name.before").read_bytes(), \
        "candidate-written machine name changed on rollback"
    response = request(state, {"type": "service.list"})
    assert any(row["name"] == "rollback-proof" for row in response["services"]), response
    host = request(state, {"type": "host.info"})["host"]
    assert host["machineName"] == "rollback-proof", host


def boundary_writes(path):
    # This uses the shared host cache. Final-state writes use the actual daemon.
    with sqlite3.connect(path) as database:
        changed = database.execute(
            "UPDATE hosts SET last_seen_at=last_seen_at+1"
        ).rowcount
        assert changed > 0, "missing retained session cache"


def candidate_manifest(candidate, environment):
    path = Path(os.environ.get("MESH_ROLLBACK_MANIFEST", ""))
    if not path.is_absolute() or not path.is_file():
        raise ValueError("full rollback proof requires MESH_ROLLBACK_MANIFEST with complete actual candidate metadata")
    if path.stat().st_size > 512 << 10:
        raise ValueError("rollback context manifest exceeds 512 KiB")
    manifest = json.loads(path.read_bytes())
    build = json.loads(subprocess.check_output([candidate, "version", "--json"], env=environment, timeout=10))
    if manifest.get("schema") != 1 or manifest.get("version") != build["version"] or manifest.get("commit") != build["commit"]:
        raise ValueError("rollback context names another release source or version")
    artifacts = manifest.get("artifacts", [])
    native = [artifact for artifact in artifacts if artifact["platform"] == build["platform"]]
    if len(artifacts) != 3 or len(native) != 1:
        raise ValueError("rollback context requires all three actual platform artifacts")
    with Path(candidate).open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    if native[0]["binarySha256"] != digest or build["digest"] != digest:
        raise ValueError("rollback context differs from the real release executable")
    compatibility = manifest["compatibility"]
    if compatibility["stateWrite"] != build["stateVersion"] or compatibility["workerWrite"] != build["workerProtocol"]:
        raise ValueError("rollback context differs from the actual candidate capabilities")
    return manifest, build


def verify_source_range(manifest, source):
    compatibility = manifest["compatibility"]
    if not compatibility["stateReadMin"] <= source["stateVersion"] <= compatibility["stateReadMax"]:
        raise ValueError("rollback context does not cover the actual source database schema")
    if not compatibility["workerMin"] <= source["workerProtocol"] <= compatibility["workerMax"]:
        raise ValueError("rollback context does not cover the actual source worker protocol")


def prepare_rollback(state, old, candidate, label, failed_image=None):
    root = state.parent
    installed = root / "installed" / "mesh"
    installed.parent.mkdir(exist_ok=True)
    previous = root / "installed" / "previous"
    installed.unlink(missing_ok=True)
    previous.unlink(missing_ok=True)
    shutil.copyfile(old, previous)
    failed_image = failed_image or candidate
    shutil.copyfile(failed_image, installed)
    previous.chmod(0o700)
    installed.chmod(0o700)
    environment = dict(os.environ, MESH_STATE_DIR=str(state))
    source = json.loads(subprocess.check_output([old, "version", "--json"], env=environment, timeout=10))
    manifest, target = candidate_manifest(candidate, environment)
    verify_source_range(manifest, source)
    failed = json.loads(subprocess.check_output([failed_image, "version", "--json"], env=environment, timeout=10))
    if any(failed[field] != target[field] for field in ("version", "commit", "platform", "stateVersion", "workerProtocol")):
        raise ValueError("startup companion does not belong to the actual release candidate")
    (root / (label + ".image-context.json")).write_text(json.dumps({
        "release": target, "failedImage": failed, "manifest": str(Path(os.environ["MESH_ROLLBACK_MANIFEST"]).resolve()),
        "startupCompanion": failed["digest"] != target["digest"]}, sort_keys=True) + "\n")
    workers = retained_workers(state)
    with sqlite3.connect(state / "mesh.db") as database:
        host = database.execute("SELECT id FROM hosts ORDER BY id LIMIT 1").fetchone()[0]
    settings = {"stateDir": str(state), "executable": str(installed),
                "cacheDir": str(root / "cache"), "clientOnly": False,
                "healthTimeout": 6_000_000_000, "service": service_spec(root)}
    status = {
        "schema": 1, "phase": "rolling_back", "error": "isolated boundary " + label,
        "settings": settings, "candidate": str(root / "installed" / "candidate"),
        "previous": str(previous), "original": {"hostId": host, "build": source, "workers": workers},
        "request": {"id": "boundary-" + label, "targetId": host, "generation": 1,
                    "current": source, "manifest": manifest},
    }
    journal = state / "update" / "installation.json"
    journal.parent.mkdir(mode=0o700, exist_ok=True)
    journal.write_text(json.dumps(status))
    journal.chmod(0o600)
    service_tools(root, state, installed)


def retained_workers(state):
    result = []
    for path in (state / "s").glob("*/meta.json"):
        meta = json.loads(path.read_bytes())
        if not (path.parent / "sock").exists():
            continue
        shell = meta["pid"]
        worker = int(subprocess.check_output(["ps", "-o", "ppid=", "-p", str(shell)], timeout=5))
        result.append({"id": path.parent.name, "pid": worker, "shellPid": shell, "protocol": 1})
    assert result, "rollback helper baseline has no live workers"
    return result


def service_spec(root):
    if sys.platform != "darwin":
        return {"kind": "systemd", "name": "mesh-proof.service"}
    path = root / "mesh-proof.plist"
    path.write_bytes(plistlib.dumps({"Label": "mesh-proof", "AbandonProcessGroup": True}))
    return {"kind": "launchd", "name": "mesh-proof", "domain": "gui/" + str(os.getuid()),
            "configPath": str(path)}


def service_tools(root, state, installed):
    tools = root / "service-tools"
    tools.mkdir(exist_ok=True)
    for name in ("systemctl", "launchctl", "plutil"):
        tool = tools / name
        tool.write_text("#!" + sys.executable + "\nimport runpy,sys\nsys.argv.insert(1, 'service')\n"
                        + "runpy.run_path(" + repr(str(Path(__file__).resolve())) + ", run_name='__main__')\n")
        tool.chmod(0o700)
    (root / "service.json").write_text(json.dumps({"state": str(state), "executable": str(installed)}))


def service_action(arguments):
    root = Path(os.environ["MESH_STATE_DIR"]).parent
    spec = json.loads((root / "service.json").read_bytes())
    pidfile = root / "managed-daemon.pid"
    if arguments[:3] == ["-extract", "AbandonProcessGroup", "raw"]:
        assert arguments == ["-extract", "AbandonProcessGroup", "raw", "-o", "-", str(root / "mesh-proof.plist")]
        print("true")
        return
    action = arguments[1] if arguments[0] == "--user" else arguments[0]
    if arguments[0] == "--user":
        assert arguments == ["--user", action, "mesh-proof.service"] or arguments == [
            "--user", "show", "mesh-proof.service", "--property=KillMode", "--value"]
    if action == "show":
        print("process")
    elif action in ("start", "bootstrap"):
        start_managed_daemon(root, spec, pidfile)
    elif action in ("stop", "bootout"):
        stop_managed_daemon(pidfile)
    elif action == "print":
        if not pidfile.exists():
            print('Could not find service "mesh-proof" in domain', file=sys.stderr)
        raise SystemExit(0 if pidfile.exists() else 113)
    else:
        raise ValueError(arguments)


def start_managed_daemon(root, spec, pidfile):
    with (root / "managed-daemon.log").open("ab") as log:
        child = subprocess.Popen([spec["executable"], "daemon", "--tailnet-port=0", "--ssh-port=0"],
                                 stdin=subprocess.DEVNULL, stdout=log, stderr=log)
    pidfile.write_text(str(child.pid))


def stop_managed_daemon(pidfile):
    if not pidfile.exists():
        return
    try:
        os.kill(int(pidfile.read_text()), signal.SIGTERM)
    except ProcessLookupError:
        pass
    pidfile.unlink()


def verify_rollback(state):
    journal = json.loads((state / "update" / "installation.json").read_bytes())
    assert journal["phase"] == "rolled_back", journal
    assert journal["verified"]["build"]["digest"] == journal["request"]["current"]["digest"], journal
    original = {(row["id"], row["pid"], row["shellPid"]) for row in journal["original"]["workers"]}
    verified = {(row["id"], row["pid"], row["shellPid"]) for row in journal["verified"]["workers"]}
    assert original == verified, journal
    assert not (state / "update" / "activation.gate").exists(), "helper did not settle activation gate"


def persistent_snapshot(state):
    name = state / "machine-name.json"
    result = {"machineName": name.read_bytes().hex() if name.exists() else None}
    with sqlite3.connect(state / "mesh.db") as database:
        exists = database.execute("SELECT 1 FROM sqlite_master WHERE name='rollback_fixture'").fetchone()
        result["migrationWrites"] = database.execute("SELECT * FROM rollback_fixture").fetchall() if exists else None
    return json.dumps(result, sort_keys=True)


def startup_identity(paths):
    keys = ("sourceExecutable", "uninstrumentedCandidateInput", "instrumentedStartupFixture")
    identities = {key: hashlib.sha256(Path(path).read_bytes()).hexdigest() for key, path in zip(keys, paths, strict=True)}
    assert identities["instrumentedStartupFixture"] != identities["uninstrumentedCandidateInput"], identities
    print(json.dumps(identities, sort_keys=True), file=sys.stderr)


def startup_overlay(repo, output, migration_directory):
    output.mkdir(parents=True, exist_ok=True)
    source = repo / "internal" / "storage" / "store.go"
    original = source.read_text()
    instrumented = original.replace('"modernc.org/sqlite"', '"modernc.org/sqlite"\n'
                                    '\t"github.com/shaul/mesh/internal/storage/cmd/boundary/startupfault"')
    instrumented = instrumented.replace('sqliteDriver               = "sqlite"',
                                        'sqliteDriver               = startupfault.DriverName')
    assert instrumented != original and 'sqliteDriver               = startupfault.DriverName' in instrumented
    overlay_source = output / "store.go"
    overlay_source.write_text(instrumented)
    replacements = {str(source): str(overlay_source)}
    if migration_directory:
        migration_overlay(repo, output, Path(migration_directory), replacements)
    contents = json.dumps({"Replace": replacements}, sort_keys=True).encode()
    (output / "overlay.json").write_bytes(contents)
    identity = {"source": hashlib.sha256(original.encode()).hexdigest(),
                "instrumentedSource": hashlib.sha256(instrumented.encode()).hexdigest(),
                "overlay": hashlib.sha256(contents).hexdigest(), "fixtureOnly": True}
    (output / "identity.json").write_text(json.dumps(identity, sort_keys=True))


def migration_overlay(repo, output, directory, replacements):
    # Explicit embed paths let Go build include fixture files outside the checkout.
    names = sorted(directory.glob("*.sql"))
    version = max(int(path.name.split("_", 1)[0]) for path in names)
    for path in names:
        replacements[str(repo / "db" / "migrations" / path.name)] = str(path)
    embed = output / "embed.go"
    embed.write_text('package migrations\nimport "embed"\n//go:embed '
                     + " ".join(path.name for path in names) + '\nvar Files embed.FS\n')
    replacements[str(repo / "db" / "migrations" / "embed.go")] = str(embed)
    release_path = repo / "internal" / "release" / "release.go"
    release_source = release_path.read_text()
    modified, count = re.subn(r"CurrentStateVersion\s+= \d+", "CurrentStateVersion = " + str(version), release_source)
    assert count == 1
    release_overlay = output / "release.go"
    release_overlay.write_text(modified)
    replacements[str(release_path)] = str(release_overlay)


def main(args):
    mode, *values = args
    if mode == "snapshot":
        database_snapshot(*values)
    elif mode == "schema":
        Path(values[1]).write_text(json.dumps(schema(values[0]), sort_keys=True))
    elif mode == "boundary-writes":
        boundary_writes(values[0])
    elif mode == "candidate-writes":
        candidate_writes(Path(values[0]))
    elif mode == "verify-writes":
        verify_writes(Path(values[0]))
    elif mode == "verify-cache":
        with sqlite3.connect(values[0]) as database:
            facts = database.execute("SELECT COUNT(*) FROM cached_session_facts").fetchone()[0]
            assert facts > 0, "candidate storage code did not cache retained session facts"
    elif mode == "prepare-rollback":
        prepare_rollback(Path(values[0]), *values[1:])
    elif mode == "verify-manifest":
        environment = dict(os.environ, MESH_STATE_DIR=values[2], MESH_CONFIG_DIR=values[3])
        manifest, _ = candidate_manifest(values[0], environment)
        source = json.loads(subprocess.check_output([values[1], "version", "--json"], env=environment, timeout=10))
        verify_source_range(manifest, source)
    elif mode == "verify-rollback":
        verify_rollback(Path(values[0]))
    elif mode == "rollback-phase":
        state = Path(values[0])
        phase = json.loads((state / "update" / "installation.json").read_bytes())["phase"]
        print("settling" if phase == "rolled_back" and (state / "update" / "activation.gate").exists() else phase)
    elif mode == "service":
        service_action(values)
    elif mode == "startup-overlay":
        startup_overlay(Path(values[0]).resolve(), Path(values[1]).resolve(), values[2] if len(values) > 2 else "")
    elif mode == "startup-identity":
        startup_identity(values)
    elif mode == "persistent-snapshot":
        Path(values[1]).write_text(persistent_snapshot(Path(values[0])))
    elif mode == "verify-persistent":
        assert Path(values[1]).read_text() == persistent_snapshot(Path(values[0])), "candidate persistent writes changed during rollback"
    else:
        raise ValueError(mode)


if __name__ == "__main__":
    main(sys.argv[1:])
