"""Real systemd user and launchd GUI controls with owned-service teardown."""
import json
import hashlib
import os
import platform
import plistlib
import subprocess
import sys
import time
import uuid
from pathlib import Path


def command(arguments, environment=None, check=True):
    result = subprocess.run(arguments, env=environment, capture_output=True, text=True, timeout=10, check=False)
    if check and result.returncode:
        raise RuntimeError(f"native command {arguments!r} exited {result.returncode}: {result.stderr.strip()} {result.stdout.strip()}")
    return result


def wait_for(predicate, message):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.05)
    raise RuntimeError(message)


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


class Manager:
    def __init__(self, root):
        self.root = Path(root)
        self.system = platform.system()
        self.label = "mesh-catchup-" + uuid.uuid4().hex
        self.environment = dict(os.environ)
        self.started = False
        self.pid = None
        self.plist = self.root / "control.plist"
        if self.system == "Linux":
            runtime = f"/run/user/{os.getuid()}"
            self.environment.setdefault("XDG_RUNTIME_DIR", runtime)
            self.environment.setdefault("DBUS_SESSION_BUS_ADDRESS", "unix:path=" + runtime + "/bus")
            return
        if self.system != "Darwin":
            raise RuntimeError("installed proof requires Linux systemd or macOS launchd")
        self.domain = f"gui/{os.getuid()}"

    def systemctl(self, *arguments, check=True):
        return command(["/usr/bin/systemctl", "--user", *arguments], self.environment, check)

    def launchctl(self, *arguments, check=True):
        return command(["/bin/launchctl", *arguments], self.environment, check)

    def inventory(self):
        if self.system == "Linux":
            result = self.systemctl("list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
            return {"manager": "systemd-user", "uid": os.getuid(),
                    "units": [line.split()[:4] for line in result.stdout.splitlines()]}
        self.launchctl("print", self.domain)
        result = self.launchctl("list")
        return {"manager": "launchd-gui", "domain": self.domain, "units": result.stdout.splitlines()}

    def exists(self):
        if self.system == "Linux":
            result = self.systemctl("show", self.label + ".service", "--property=LoadState", "--value", check=False)
            return result.stdout.strip() not in ("", "not-found")
        return self.launchctl("print", self.domain + "/" + self.label, check=False).returncode == 0

    def start(self, marker):
        if self.exists():
            raise RuntimeError("refusing to replace a pre-existing native service")
        arguments = [str(Path(sys.executable).resolve()), str(Path(__file__).resolve()), "child", str(marker), self.label]
        # Register ownership before the manager can launch a process.
        self.started = True
        if self.system == "Linux":
            command(["/usr/bin/systemd-run", "--user", "--unit=" + self.label, "--collect",
                     "--service-type=exec", "--property=KillMode=control-group", *arguments], self.environment)
            return
        data = {"Label": self.label, "ProgramArguments": arguments, "RunAtLoad": True,
                "StandardOutPath": str(self.root / "control.out"), "StandardErrorPath": str(self.root / "control.err")}
        self.plist.write_bytes(plistlib.dumps(data))
        self.launchctl("bootstrap", self.domain, str(self.plist))

    def manager_pid(self):
        if self.system == "Linux":
            value = self.systemctl("show", self.label + ".service", "--property=MainPID", "--value").stdout.strip()
            return int(value or "0")
        result = self.launchctl("print", self.domain + "/" + self.label)
        rows = [line.strip() for line in result.stdout.splitlines()]
        value = next((line.removeprefix("pid = ") for line in rows if line.startswith("pid = ")), "0")
        return int(value)

    def close(self):
        if not self.started:
            return {"serviceAbsent": True, "processAbsent": True}
        if self.exists():
            self.pid = self.pid or self.manager_pid()
        if self.system == "Linux":
            self.systemctl("stop", self.label + ".service", check=False)
            self.systemctl("reset-failed", self.label + ".service", check=False)
        else:
            self.launchctl("bootout", self.domain + "/" + self.label, check=False)
        wait_for(lambda: not self.exists(), "test service remains registered after teardown")
        if self.pid:
            wait_for(lambda: not alive(self.pid), "test process remains alive after teardown")
        self.started = False
        return {"serviceAbsent": True, "processAbsent": True, "pid": self.pid, "label": self.label}


def mapped_image(pid, executable):
    expected = Path(executable).resolve().stat()
    image = Path(f"/proc/{pid}/exe")
    if platform.system() == "Darwin":
        image = darwin_image(pid, expected)
    loaded = image.stat()
    if (loaded.st_dev, loaded.st_ino) != (expected.st_dev, expected.st_ino):
        raise RuntimeError("native service mapped another executable inode")
    with image.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    return {"pid": pid, "inode": loaded.st_ino, "device": loaded.st_dev, "sha256": digest}


def darwin_image(pid, expected):
    output = command(["/usr/sbin/lsof", "-a", "-p", str(pid), "-d", "txt", "-F", "finD"]).stdout
    rows, row = [], {}
    for line in output.splitlines():
        if line.startswith("f"):
            rows.append(row)
            row = {}
        if line[:1] in ("i", "D", "n"):
            row[line[0]] = line[1:]
    rows.append(row)
    matches = [row for row in rows if row.get("i") == str(expected.st_ino)
               and row.get("D") and int(row["D"], 0) == expected.st_dev and row.get("n")]
    if len(matches) != 1:
        raise RuntimeError(f"mapped native service image inode {expected.st_ino} device {expected.st_dev} has {len(matches)} lsof matches")
    return Path(matches[0]["n"])


def positive_control(root, evidence):
    manager = Manager(root)
    record = {"schema": 1, "passed": False, "created": {"label": manager.label, "root": str(root)}}
    # The caller's finally is installed before inventory, file creation or launch.
    try:
        record["before"] = manager.inventory()
        Path(evidence).write_text(json.dumps(record, indent=2) + "\n")
        marker = Path(root) / "control.json"
        manager.start(marker)
        wait_for(marker.exists, "native manager did not execute positive control")
        child = json.loads(marker.read_text())
        manager.pid = manager.manager_pid()
        if child != {"pid": manager.pid, "token": manager.label} or not alive(manager.pid):
            raise RuntimeError("native manager PID does not match executing positive control")
        record["executed"] = child
        record["mappedImage"] = wait_for(lambda: control_image(manager.pid, record), "native control did not exec its expected /bin/sleep image")
        if manager.manager_pid() != manager.pid:
            raise RuntimeError("native service PID changed during mapped-image verification")
        record["passed"] = True
    except BaseException as error:
        record["error"] = str(error)
        raise
    finally:
        try:
            record["cleanup"] = manager.close()
        except BaseException as error:
            record["passed"] = False
            record["cleanupError"] = str(error)
            raise
        finally:
            Path(evidence).write_text(json.dumps(record, indent=2) + "\n")
    return record


def child(marker, token):
    temporary = Path(marker).with_suffix(".tmp")
    temporary.write_text(json.dumps({"pid": os.getpid(), "token": token}))
    temporary.replace(marker)
    # A fixed native executable supplies the image control on interpreter-based
    # macOS installations whose Python launcher execs a different framework file.
    os.execv("/bin/sleep", ["/bin/sleep", "60"])


def control_image(pid, record):
    try:
        return mapped_image(pid, "/bin/sleep")
    except RuntimeError as error:
        record["lastMappingError"] = str(error)
        return None


if __name__ == "__main__":
    if len(sys.argv) != 4 or sys.argv[1] != "child":
        sys.exit("usage: native_services.py child MARKER TOKEN")
    child(*sys.argv[2:])
