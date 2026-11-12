"""Disposable hosted Linux accounts, each with its real systemd user manager."""
import json
import os
from pathlib import Path
import pwd
import shutil
import signal
import subprocess
import sys
import time
import uuid

from fixtures import installed_native, installed_paths, native_services


def require_hosted_root():
    if sys.platform != "linux" or os.geteuid() != 0 or os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise RuntimeError("test accounts require root on a disposable GitHub-hosted Linux runner")


def command(arguments, check=True):
    return native_services.command(arguments, check=check)


def account_exists(name):
    try:
        return pwd.getpwnam(name)
    except KeyError:
        return None


class Accounts:
    def __init__(self, root, output):
        require_hosted_root()
        self.root, self.output = Path(root), Path(output)
        self.accounts = []
        self.prefix = "m8" + uuid.uuid4().hex[:10]
        self.group = self.prefix
        self.group_created = False
        self.source = None
        self.before = command(["/usr/bin/systemctl", "list-units", "user@*.service", "--all", "--no-pager", "--plain"]).stdout

    def create(self):
        command(["/usr/sbin/groupadd", self.group])
        self.group_created = True
        command(["/usr/bin/chgrp", self.group, str(self.root)])
        self.root.chmod(0o2770)
        for role in ("canary", "sleep", "burst"):
            self.create_account(role)
        return self.accounts

    def create_account(self, role):
        name = self.prefix + role[0]
        if account_exists(name):
            raise RuntimeError("refusing an existing test account: " + name)
        home = self.root / role
        home.mkdir()
        record = {"name": name, "role": role, "home": str(home), "created": False, "managerStarted": False, "ownershipVerified": False}
        self.accounts.append(record)
        command(["/usr/sbin/useradd", "--no-create-home", "--home-dir", str(home), "--gid", self.group, "--shell", "/bin/bash", name])
        record["created"] = True
        entry = pwd.getpwnam(name)
        record["uid"] = entry.pw_uid
        if self.pids(entry.pw_uid) or Path(f"/run/user/{entry.pw_uid}").exists():
            raise RuntimeError("new test UID has pre-existing processes or runtime state")
        record["ownershipVerified"] = True
        os.chown(home, entry.pw_uid, entry.pw_gid)
        record["managerStarted"] = True
        command(["/usr/bin/systemctl", "start", f"user@{entry.pw_uid}.service"])
        environment = self.environment(record)
        command(["/usr/sbin/runuser", "-u", name, "--", "/usr/bin/env", *environment,
                 "/usr/bin/systemctl", "--user", "show-environment"])

    def environment(self, account):
        runtime = f"/run/user/{account['uid']}"
        return ["HOME=" + account["home"], "XDG_RUNTIME_DIR=" + runtime,
                "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtime + "/bus", "PYTHONDONTWRITEBYTECODE=1"]

    def run(self, account, arguments, **options):
        return subprocess.run(self.arguments(account, arguments), **options)

    def arguments(self, account, arguments):
        return ["/usr/sbin/runuser", "-u", account["name"], "--", "/usr/bin/env", *self.environment(account), *map(str, arguments)]

    def close(self):
        actions = [lambda account=account: self.remove_account(account) for account in reversed(self.accounts)]
        actions.append(self.remove_sources)
        actions.append(self.remove_group)
        try:
            installed_native.cleanup_all(actions)
        finally:
            record = {"beforeManagers": self.before, "accounts": self.accounts, "groupAbsent": not self.group_created}
            (self.output / "accounts-cleanup.json").write_text(json.dumps(record, indent=2) + "\n")

    def owned(self, account):
        entry = account_exists(account["name"])
        if not entry or entry.pw_uid != account["uid"] or entry.pw_dir != account["home"]:
            raise RuntimeError("test account identity changed; refusing destructive cleanup")

    def remove_account(self, account):
        if not account["created"]:
            return
        self.owned(account)
        uid = account["uid"]
        if not account["ownershipVerified"]:
            command(["/usr/sbin/userdel", account["name"]])
            account["absent"] = account_exists(account["name"]) is None
            return
        if account["managerStarted"]:
            command(["/usr/bin/systemctl", "stop", f"user@{uid}.service"])
        self.stop_processes(account)
        command(["/usr/bin/systemctl", "stop", f"user-runtime-dir@{uid}.service"])
        if Path(f"/run/user/{uid}").exists() or self.pids(uid):
            raise RuntimeError("owned account processes or runtime remain after teardown")
        command(["/usr/sbin/userdel", account["name"]])
        account["absent"] = account_exists(account["name"]) is None
        if not account["absent"]:
            raise RuntimeError("owned account remains after removal")
        shutil.rmtree(account["home"])
        account["homeAbsent"] = not Path(account["home"]).exists()

    def stop_processes(self, account):
        self.owned(account)
        for pid in self.pids(account["uid"]):
            signal_owned(pid, account["uid"], signal.SIGTERM)
        deadline = time.monotonic() + 5
        while self.pids(account["uid"]) and time.monotonic() < deadline:
            time.sleep(0.05)
        for pid in self.pids(account["uid"]):
            signal_owned(pid, account["uid"], signal.SIGKILL)
        native_services.wait_for(lambda: not self.pids(account["uid"]), "owned account process remains")

    def pids(self, uid):
        result = command(["/usr/bin/ps", "-u", str(uid), "-o", "pid="], check=False)
        if result.returncode not in (0, 1):
            raise RuntimeError("could not inventory test UID processes")
        return [int(value) for value in result.stdout.split()]

    def remove_group(self):
        if not self.group_created:
            return
        if any(account["created"] and not account.get("absent") for account in self.accounts):
            raise RuntimeError("retaining group for unconfirmed account cleanup")
        command(["/usr/sbin/groupdel", self.group])
        self.group_created = False

    def prepare_sources(self):
        source = Path(__file__).resolve().parents[2]
        self.source = self.root / "account-source"
        if self.source.exists():
            raise RuntimeError("refusing an existing native account fixture source directory")
        for relative in ("scripts/fixtures", "scripts/install/assets", "integration/helpers"):
            shutil.copytree(source / relative, self.source / relative, ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copy2(source / "scripts/prove-installed-catchup.py", self.source / "scripts/prove-installed-catchup.py")
        return self.source

    def remove_sources(self):
        if any(account["created"] and not account.get("absent") for account in self.accounts):
            raise RuntimeError("retaining source for unconfirmed native account cleanup")
        if self.source:
            shutil.rmtree(self.source)


def probe(output):
    require_hosted_root()
    root = installed_paths.create()
    output.mkdir(parents=True, exist_ok=True)
    accounts = Accounts(root, output)
    try:
        source = accounts.prepare_sources()
        for account in accounts.create():
            script = source / "scripts/prove-installed-catchup.py"
            accounts.run(account, [sys.executable, script, Path(account["home"]) / "control", "--native-preflight"],
                         check=True, timeout=30, env=os.environ | {"MESH_SHORT_TMP": "/tmp"})
    finally:
        try:
            retain_controls(root, output)
        finally:
            accounts.close()
        shutil.rmtree(root)
        (output / "accounts-scratch.json").write_text(json.dumps({"root": str(root), "absent": not root.exists()}) + "\n")


def retain_controls(root, output):
    for path in root.glob("*/control/*.json"):
        (output / (path.parent.parent.name + "-" + path.name)).write_bytes(path.read_bytes())


def signal_owned(pid, uid, number):
    try:
        if Path(f"/proc/{pid}").stat().st_uid == uid:
            os.kill(pid, number)
    except ProcessLookupError:
        pass
    except FileNotFoundError:
        pass


def interrupted(number, _frame):
    raise InterruptedError("account control interrupted by signal " + str(number))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    probe(Path(sys.argv[1]).resolve())
