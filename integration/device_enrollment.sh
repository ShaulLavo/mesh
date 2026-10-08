#!/usr/bin/env bash
source "$(dirname -- "${BASH_SOURCE[0]}")/helpers/isolate.sh" || exit 1
set -euo pipefail
root=$(mktemp -d)
trap 'rm -rf -- "$root"' EXIT
if [[ -z ${MESH:-} ]]; then
  MESH="$root/mesh"
  go build -o "$MESH" ./cmd/mesh
fi
python3 - "$MESH" <<'PY'
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix="m175-") as directory:
    root = Path(directory).resolve()
    tools = root / "tools"
    tools.mkdir()
    source = root / "source"
    destination = root / "destination"
    config = root / "config"
    for path in (source, destination, config):
        path.mkdir(mode=0o700)
    ssh_log = root / "ssh.log"
    (tools / "ssh").write_text('''#!/bin/sh
[ "$1" = '-o' ] && [ "$2" = 'BatchMode=yes' ] && [ "$4" = 'StrictHostKeyChecking=yes' ] && [ "$7" = '--' ] && [ "$8" = 'fixture-admin' ] || exit 91
printf '%s\\n' "$9" >> "$MESH_175_SSH_LOG"
exec /bin/sh -c "$9"
''')
    (tools / "ssh").chmod(0o700)
    for name in ("tailscale", "systemctl", "launchctl"):
        (tools / name).write_text("#!/bin/sh\nexit 1\n")
        (tools / name).chmod(0o700)
    environment = os.environ | {
        "PATH": str(tools) + os.pathsep + os.environ["PATH"],
        "MESH_CONFIG_DIR": str(config), "MESH_STATE_DIR": str(source),
        "MESH_175_SSH_LOG": str(ssh_log),
    }

    def command(state, *args):
        return subprocess.run([binary, *args], env=environment | {"MESH_STATE_DIR": str(state)},
                              capture_output=True, text=True, timeout=10, check=True)

    source_id = json.loads(command(source, "device", "identity", "--json").stdout)["id"]
    target = json.loads(command(destination, "device", "identity", "--json").stdout)
    pin = target["id"]
    (config / "hosts.json").write_text(json.dumps({"version": 1, "hosts": [{
        "id": "fixture-host", "meshIdentity": pin, "endpoint": "ws://127.0.0.1:1/mesh",
    }]}))
    administration = root / "admin.json"
    administration.write_text(json.dumps({"fixture-host": {
        "target": "fixture-admin", "account": target["account"],
        "stateDir": str(destination), "binary": binary,
    }}))
    # The daemon checks releases asynchronously. Keep that unrelated writer and
    # its network request outside the enrollment fixture's byte-for-byte oracle.
    notice = destination / "update-notice"
    notice.mkdir(mode=0o700)
    (notice / "notice.json").write_text(json.dumps({
        "nextAttempt": (datetime.now(timezone.utc) + timedelta(days=1)).isoformat(),
    }))
    (notice / "refresh.lock").touch(mode=0o600)
    log = (root / "daemon.log").open("w")
    daemon = subprocess.Popen([binary, "daemon"], stdout=log, stderr=log,
                              env=environment | {"MESH_STATE_DIR": str(destination),
                                                 "MESH_CONFIG_DIR": str(root / "destination-config")})
    try:
        deadline = time.monotonic() + 8
        while not (destination / "daemon.sock").exists():
            if daemon.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError("native fixture daemon did not start")
            time.sleep(0.02)
        original_config = (config / "hosts.json").read_bytes()
        original_key = (source / "identity.key").read_bytes()

        def snapshot(*directories):
            result = {}
            paths = []
            for directory in directories:
                paths.extend([directory, *directory.rglob("*")])
            # The live daemon publishes wake state independently of enrollment.
            wake_state = destination / "wake"
            for path in paths:
                if path == wake_state or wake_state in path.parents:
                    continue
                if not path.exists():
                    result[str(path)] = None
                    continue
                result[str(path)] = (path.stat().st_mode, path.read_bytes() if path.is_file() else None)
            return result

        def assert_unchanged(before, operation):
            after = snapshot(config, source, destination, absent)
            changed = sorted(path for path in before.keys() | after.keys()
                             if before.get(path) != after.get(path))
            assert after == before, f"{operation} changed fixture files/directories: {changed}"

        original_administration = administration.read_bytes()
        absent = root / "absent-state"
        for legacy in (False, True):
            book = json.loads(original_config)
            if legacy:
                book["hosts"][0]["alias"] = "obsolete"
            (config / "hosts.json").write_text(json.dumps(book))
            for mode, refused in (("--check", False), ("--check", True), ("--yes", True)):
                account = target["account"] + "-wrong-account" if refused else target["account"]
                selected_state = absent if refused else destination
                before = snapshot(config, source, destination, absent)
                checked = subprocess.run([binary, "device", "approve-checked", "--account", account,
                                          "--state-dir", str(selected_state), "--destination", pin,
                                          mode, "--allow-root", "--", source_id],
                                         env=environment | {"MESH_STATE_DIR": str(selected_state)},
                                         capture_output=True, text=True, timeout=10)
                assert checked.returncode == (1 if refused else 0), checked.stderr
                if refused:
                    assert "local account does not match the expected daemon account" in checked.stderr
                assert_unchanged(before, "checked enrollment")
                mapping = json.loads(original_administration)
                mapping["fixture-host"]["account"] = account
                administration.write_text(json.dumps(mapping))
                before = snapshot(config, source, destination, absent)
                fleet = subprocess.run([binary, "device", "approve-fleet", "--admin-map", str(administration),
                                        mode, "--allow-root", "--", "fixture-host"],
                                       env=environment, capture_output=True, text=True, timeout=10)
                assert fleet.returncode == (0 if not refused and not legacy else 1), fleet.stderr
                if legacy:
                    assert 'unknown field "alias"' in fleet.stderr, fleet.stderr
                elif refused:
                    assert "local account does not match the expected daemon account" in fleet.stderr, fleet.stderr
                assert_unchanged(before, "fleet enrollment")
        (config / "hosts.json").write_bytes(original_config)
        administration.write_bytes(original_administration)
        ssh_log.write_text("")
        preview = command(source, "device", "approve-fleet", "--admin-map", str(administration),
                          "--check", "fixture-host")
        assert "fixture-host: approval available" in preview.stdout, preview.stdout
        assert not (destination / "authorized_keys").exists(), "preview wrote a grant"
        applied = command(source, "device", "approve-fleet", "--admin-map", str(administration),
                          "--yes", "--allow-root", "fixture-host")
        assert "fixture-host: approved" in applied.stdout, applied.stdout
        grant = (destination / "authorized_keys").read_bytes()
        command(source, "device", "approve-fleet", "--admin-map", str(administration),
                "--yes", "--allow-root", "fixture-host")
        assert (destination / "authorized_keys").read_bytes() == grant, "repeat changed grant incarnation"
        receipt = command(destination, "device", "approve-checked", "--account", target["account"],
                          "--state-dir", str(destination), "--destination", pin, "--check", "--", source_id)
        assert json.loads(receipt.stdout)["approved"], "Go parser did not read back source grant"
        assert not (source / "authorized_keys").exists(), "fleet direction wrote source grants"
        assert (config / "hosts.json").read_bytes() == original_config, "enrollment rewrote source pins"
        assert (source / "identity.key").read_bytes() == original_key, "enrollment replaced source key"
        assert ssh_log.read_text().count("'--yes'") == 2, "unexpected apply sequence"
        print("PASS: built CLI enrolls one-way pin through fixture SSH and native daemon; root checks and refusals preserve current/legacy files and directories; preview is read-only; repeated approval preserves grant")
    finally:
        daemon.terminate()
        daemon.wait(timeout=8)
        log.close()
PY
