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
        print("PASS: built CLI enrolls one-way pin through fixture SSH and native daemon; preview is read-only; repeated approval preserves grant")
    finally:
        daemon.terminate()
        daemon.wait(timeout=8)
        log.close()
PY
