"""Disposable hosted Linux accounts, each with its real systemd user manager."""
import json
import os
from pathlib import Path
import pwd
import grp
import shutil
import signal
import subprocess
import sys
import time
import uuid

from fixtures import installed_native, installed_paths, native_services, installed_account_run


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
        ticket = self.root / "account-run.json"
        self.operation = json.loads(ticket.read_text())["operation"] if ticket.exists() else uuid.uuid4().hex
        self.group_gid = None
        self.group_intent = False
        self.before = command(["/usr/bin/systemctl", "list-units", "user@*.service", "--all", "--no-pager", "--plain"]).stdout
        self.persist()

    def persist(self):
        value = {"schema": 1, "operation": self.operation, "root": str(self.root.resolve()), "output": str(self.output.resolve()),
                 "accounts": self.accounts, "prefix": self.prefix, "group": self.group, "group_created": self.group_created,
                 "group_gid": self.group_gid, "group_intent": self.group_intent, "source": str(self.source) if self.source else None, "before": self.before}
        installed_account_run.save(self.output / "account-resources.json", value)

    @classmethod
    def resume(cls, path, ticket):
        value = json.loads(path.read_text())
        if value["operation"] != ticket["operation"] or value["root"] != ticket["root"] or value["output"] != ticket["output"]:
            raise RuntimeError("account resource journal differs from the owned run")
        instance = object.__new__(cls)
        instance.__dict__.update({key: value[key] for key in ("accounts", "prefix", "group", "group_created", "group_gid", "group_intent", "before", "operation")})
        instance.root, instance.output = Path(value["root"]), Path(value["output"])
        instance.source = Path(value["source"]) if value["source"] else None
        instance.validate_resources()
        return instance

    def validate_resources(self):
        if self.group != self.prefix or not self.prefix.startswith("m8"):
            raise RuntimeError("owned account group binding is invalid")
        for account in self.accounts:
            expected_home = self.root / account["role"]
            if account["name"] != self.prefix + account["role"][0] or Path(account["home"]) != expected_home:
                raise RuntimeError("owned account name/home binding changed")
        if self.source and self.source != self.root / "account-source":
            raise RuntimeError("owned account source location changed")

    def create(self):
        self.group_intent = True
        self.persist()
        command(["/usr/sbin/groupadd", self.group])
        self.group_created = True
        self.group_gid = grp.getgrnam(self.group).gr_gid
        self.persist()
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
        record = {"name": name, "role": role, "home": str(home), "created": False, "intent": True, "managerStarted": False, "ownershipVerified": False}
        self.accounts.append(record)
        self.persist()
        command(["/usr/sbin/useradd", "--no-create-home", "--home-dir", str(home), "--gid", self.group, "--shell", "/bin/bash", name])
        record["created"] = True
        entry = pwd.getpwnam(name)
        record["uid"] = entry.pw_uid
        record["gid"] = entry.pw_gid
        self.persist()
        if self.pids(entry.pw_uid) or Path(f"/run/user/{entry.pw_uid}").exists():
            raise RuntimeError("new test UID has pre-existing processes or runtime state")
        if self.manager_inventory(entry.pw_uid):
            raise RuntimeError("new test UID has pre-existing manager units")
        record["managerUnitsAbsentBefore"] = True
        record["ownershipVerified"] = True
        os.chown(home, entry.pw_uid, entry.pw_gid)
        record["managerStarted"] = True
        self.persist()
        command(["/usr/bin/systemctl", "start", f"user@{entry.pw_uid}.service"])
        environment = self.environment(record)
        command(["/usr/sbin/runuser", "-u", name, "--", "/usr/bin/env", *environment,
                 "/usr/bin/systemctl", "--user", "show-environment"])

    def environment(self, account):
        runtime = f"/run/user/{account['uid']}"
        return ["HOME=" + account["home"], "XDG_RUNTIME_DIR=" + runtime,
                "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtime + "/bus", "PYTHONDONTWRITEBYTECODE=1", "MESH_HARNESS_SOURCE="]

    def run(self, account, arguments, **options):
        return subprocess.run(self.arguments(account, arguments), **options)

    def arguments(self, account, arguments):
        return ["/usr/sbin/runuser", "-u", account["name"], "--", "/usr/bin/env", *self.environment(account), *map(str, arguments)]

    def close(self):
        actions = [lambda account=account: self.remove_account(account) for account in reversed(self.accounts)]
        actions.append(self.remove_sources)
        actions.append(self.remove_group)
        stopped = False
        try:
            installed_native.cleanup_all(actions)
            stopped = True
        finally:
            record = {"operation": self.operation, "beforeManagers": self.before, "accounts": self.accounts,
                      "groupAbsent": not self.group_created, "resourcesStopped": stopped}
            installed_account_run.save(self.output / "accounts-cleanup.json", record)

    def owned(self, account):
        entry = account_exists(account["name"])
        if not entry or entry.pw_uid != account["uid"] or entry.pw_dir != account["home"] or entry.pw_gid != account["gid"]:
            raise RuntimeError("test account identity changed; refusing destructive cleanup")

    def remove_account(self, account):
        if not account["created"]:
            if account_exists(account["name"]):
                raise RuntimeError("account creation outcome is unknown; retaining ownership evidence")
            account["absent"] = True
            self.persist()
            return
        if account_exists(account["name"]) is None:
            self.finish_absent_account(account)
            return
        self.owned(account)
        uid = account["uid"]
        if not account["ownershipVerified"]:
            command(["/usr/sbin/userdel", account["name"]])
            account["absent"] = account_exists(account["name"]) is None
            self.persist()
            return
        self.require_manager_ownership(account)
        if account["managerStarted"]:
            command(["/usr/bin/systemctl", "stop", f"user@{uid}.service"])
        self.stop_processes(account)
        command(["/usr/bin/systemctl", "stop", f"user-runtime-dir@{uid}.service"])
        if Path(f"/run/user/{uid}").exists() or self.pids(uid):
            raise RuntimeError("owned account processes or runtime remain after teardown")
        self.clear_failed_managers(account)
        account["processesAbsent"], account["runtimeAbsent"] = True, True
        self.persist()
        command(["/usr/sbin/userdel", account["name"]])
        account["absent"] = account_exists(account["name"]) is None
        if not account["absent"]:
            raise RuntimeError("owned account remains after removal")
        shutil.rmtree(account["home"])
        account["homeAbsent"] = not Path(account["home"]).exists()
        self.persist()

    def finish_absent_account(self, account):
        try:
            pwd.getpwuid(account["uid"])
        except KeyError:
            self.finish_unallocated_uid(account)
            return
        raise RuntimeError("owned account UID was reallocated; refusing recovery")

    def finish_unallocated_uid(self, account):
        uid = account["uid"]
        if self.pids(uid) or Path(f"/run/user/{uid}").exists():
            raise RuntimeError("absent account has unresolved processes/runtime; retaining scratch")
        if account["ownershipVerified"]:
            self.clear_failed_managers(account)
        account.update(absent=True, processesAbsent=True, runtimeAbsent=True)
        if Path(account["home"]).exists():
            shutil.rmtree(account["home"])
        account["homeAbsent"] = True
        self.persist()

    def manager_inventory(self, uid):
        units = [f"user@{uid}.service", f"user-runtime-dir@{uid}.service"]
        result = command(["/usr/bin/systemctl", "list-units", *units, "--all", "--no-pager", "--plain", "--no-legend"])
        rows = [line.split() for line in result.stdout.splitlines() if line.strip()]
        if any(len(row) < 4 or row[0] not in units for row in rows):
            raise RuntimeError("test user manager inventory is malformed")
        return rows

    def require_manager_ownership(self, account):
        if not account["created"] or not account["ownershipVerified"] or not account.get("managerUnitsAbsentBefore"):
            raise RuntimeError("test manager cleanup lacks verified ownership")

    def clear_failed_managers(self, account):
        self.require_manager_ownership(account)
        uid = account["uid"]
        if self.pids(uid) or Path(f"/run/user/{uid}").exists():
            raise RuntimeError("refusing failed-manager cleanup with remaining test processes/runtime")
        rows = self.manager_inventory(uid)
        if any(row[2] not in ("inactive", "failed") for row in rows):
            raise RuntimeError("owned user manager remains active after teardown")
        for row in rows:
            if row[2] == "failed":
                command(["/usr/bin/systemctl", "reset-failed", row[0]])
        native_services.wait_for(lambda: not self.manager_inventory(uid), "owned user manager unit remains after teardown")
        account["managerUnitsAbsent"] = True
        self.persist()

    def stop_processes(self, account):
        self.owned(account)
        for pid in self.pids(account["uid"]):
            signal_owned(pid, account["uid"], signal.SIGTERM, installed_account_run.identity(pid))
        deadline = time.monotonic() + 5
        while self.pids(account["uid"]) and time.monotonic() < deadline:
            time.sleep(0.05)
        for pid in self.pids(account["uid"]):
            signal_owned(pid, account["uid"], signal.SIGKILL, installed_account_run.identity(pid))
        native_services.wait_for(lambda: not self.pids(account["uid"]), "owned account process remains")

    def pids(self, uid):
        result = command(["/usr/bin/ps", "-u", str(uid), "-o", "pid="], check=False)
        if result.returncode not in (0, 1):
            raise RuntimeError("could not inventory test UID processes")
        return [int(value) for value in result.stdout.split()]

    def remove_group(self):
        if not self.group_created:
            if self.group_intent and group_exists(self.group):
                raise RuntimeError("group creation outcome is unknown; retaining evidence")
            return
        if grp.getgrnam(self.group).gr_gid != self.group_gid:
            raise RuntimeError("owned group lifetime binding changed; refusing removal")
        if any(account["created"] and not account.get("absent") for account in self.accounts):
            raise RuntimeError("retaining group for unconfirmed account cleanup")
        command(["/usr/sbin/groupdel", self.group])
        self.group_created = False
        self.persist()

    def prepare_sources(self):
        source = Path(__file__).resolve().parents[2]
        destination = self.root / "account-source"
        if destination.exists():
            raise RuntimeError("refusing an existing native account fixture source directory")
        self.source = destination
        self.persist()
        for relative in ("scripts/fixtures", "scripts/install/assets", "integration/helpers"):
            shutil.copytree(source / relative, self.source / relative, ignore=shutil.ignore_patterns("__pycache__"))
        shutil.copy2(source / "scripts/prove-installed-catchup.py", self.source / "scripts/prove-installed-catchup.py")
        return self.source

    def remove_sources(self):
        if any(account["created"] and not account.get("absent") for account in self.accounts):
            raise RuntimeError("retaining source for unconfirmed native account cleanup")
        if self.source:
            if self.source.exists():
                shutil.rmtree(self.source)
            self.source = None
            self.persist()


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


def signal_owned(pid, uid, number, expected):
    try:
        if expected and expected["uid"] == uid and installed_account_run.identity(pid) == expected:
            os.kill(pid, number)
    except ProcessLookupError:
        pass
    except FileNotFoundError:
        pass


def group_exists(name):
    try:
        return grp.getgrnam(name)
    except KeyError:
        return None


def interrupted(number, _frame):
    raise InterruptedError("account control interrupted by signal " + str(number))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    probe(Path(sys.argv[1]).resolve())
