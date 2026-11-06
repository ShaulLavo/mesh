from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import shutil
import signal
import sys
import tempfile
import time

from enrollment_snapshot import assert_unchanged, snapshot
from mesh_control import request_id, round_trip

from enrollment_command import OwnedCommand


class FixtureRoot:
    def __init__(self):
        self.root = Path(tempfile.mkdtemp(prefix="m175-")).resolve()
        self.commands = []
        self.cancelled = False
        self.handlers = {}

    def __enter__(self):
        for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            self.handlers[number] = signal.signal(number, self.cancel)
        return self

    def cancel(self, *_):
        self.cancelled = True

    def start(self, args, environment, timeout=0):
        if self.cancelled:
            raise InterruptedError("enrollment fixture cancelled")
        command = OwnedCommand(self.root, args, environment, timeout)
        self.commands.append(command)
        return command

    def run(self, args, environment, check=False):
        return self.start(args, environment, timeout=10).result(check, lambda: self.cancelled)

    def __exit__(self, *_):
        try:
            self._close_owned()
        finally:
            for number, handler in self.handlers.items():
                signal.signal(number, handler)
        if self.cancelled:
            raise InterruptedError("native enrollment fixture cancelled during cleanup")

    def _close_owned(self):
        failures = []
        for command in reversed(self.commands):
            try:
                command.close()
            except Exception as error:
                failures.append(str(error))
        if failures or any(not command.joined for command in self.commands):
            raise RuntimeError(f"native enrollment retained {self.root}: {failures}")
        shutil.rmtree(self.root)


def check_mode(fixture, binary, environment, config, source, destination, absent, target, pin,
               source_id, administration, original_administration, legacy, mode, refused):
    account = target["account"] + "-wrong-account" if refused else target["account"]
    selected_state = absent if refused else destination
    before = snapshot(config, source, destination, absent)
    checked = fixture.run([binary, "device", "approve-checked", "--account", account,
                              "--state-dir", str(selected_state), "--destination", pin,
                              mode, "--allow-root", "--", source_id],
                             environment | {"MESH_STATE_DIR": str(selected_state)})
    assert checked.returncode == (1 if refused else 0), checked.stderr
    if refused:
        assert "local account does not match the expected daemon account" in checked.stderr
    assert_unchanged(before, snapshot(config, source, destination, absent),
                     f"checked enrollment ({legacy=}, {mode=}, {refused=})")
    mapping = json.loads(original_administration)
    mapping["fixture-host"]["account"] = account
    administration.write_text(json.dumps(mapping))
    before = snapshot(config, source, destination, absent)
    fleet = fixture.run([binary, "device", "approve-fleet", "--admin-map", str(administration),
                            mode, "--allow-root", "--", "fixture-host"],
                           environment)
    assert fleet.returncode == (0 if not refused and not legacy else 1), fleet.stderr
    if legacy:
        assert 'unknown field "alias"' in fleet.stderr, fleet.stderr
    elif refused:
        assert "local account does not match the expected daemon account" in fleet.stderr, fleet.stderr
    assert_unchanged(before, snapshot(config, source, destination, absent),
                     f"fleet enrollment ({legacy=}, {mode=}, {refused=})")


def check_refusals(fixture, binary, environment, config, source, destination, absent, target, pin,
                   source_id, administration, original_config, original_administration):
    for legacy in (False, True):
        book = json.loads(original_config)
        if legacy:
            book["hosts"][0]["alias"] = "obsolete"
        (config / "hosts.json").write_text(json.dumps(book))
        for mode, refused in (("--check", False), ("--check", True), ("--yes", True)):
            check_mode(fixture, binary, environment, config, source, destination, absent, target, pin,
                       source_id, administration, original_administration, legacy, mode, refused)


def enroll(binary, fixture):
    root = fixture.root
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
    home = root / "home"
    temporary = root / "tmp"
    home.mkdir(mode=0o700)
    temporary.mkdir(mode=0o700)
    environment = {name: os.environ[name] for name in ("PATH", "LANG", "TERM") if name in os.environ} | {
        "HOME": str(home), "TMPDIR": str(temporary), "TMP": str(temporary), "TEMP": str(temporary),
        "PATH": str(tools) + os.pathsep + os.environ["PATH"],
        "MESH_CONFIG_DIR": str(config), "MESH_STATE_DIR": str(source),
        "MESH_175_SSH_LOG": str(ssh_log),
    }

    def command(state, *args):
        return fixture.run([binary, *args], environment | {"MESH_STATE_DIR": str(state)}, check=True)

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
    daemon = fixture.start([binary, "daemon"], environment | {"MESH_STATE_DIR": str(destination),
                                                            "MESH_CONFIG_DIR": str(root / "destination-config")})
    deadline = time.monotonic() + 8
    while not (destination / "daemon.sock").exists():
        if daemon.anchor.poll() is not None or fixture.cancelled or time.monotonic() >= deadline:
            raise RuntimeError("native fixture daemon did not start")
        time.sleep(0.02)
    # Socket publication precedes serving; a reply proves startup reached the
    # control handler before the filesystem baseline is collected.
    response = round_trip(str(destination / "daemon.sock"), {
        "type": "host.info", "requestId": request_id("enrollment-ready"),
    })
    assert response["type"] == "host.info.result", response
    assert response["host"]["meshIdentity"] == pin, response
    original_config = (config / "hosts.json").read_bytes()
    original_key = (source / "identity.key").read_bytes()

    original_administration = administration.read_bytes()
    absent = root / "absent-state"
    check_refusals(fixture, binary, environment, config, source, destination, absent, target, pin,
                   source_id, administration, original_config, original_administration)
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


if __name__ == "__main__":
    with FixtureRoot() as fixture:
        enroll(str(Path(sys.argv[1]).resolve()), fixture)
