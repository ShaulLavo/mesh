#!/usr/bin/env python3
"""Exercise real systemd updates and a Granted-phase reboot in a disposable VM.

Provide an SSH-accessible Linux guest with passwordless sudo, a lingering user,
Mesh installed as that user's mesh.service, a live shell session, and two exact
release manifests/artifacts served on guest-local trusted HTTPS github.com URLs.
The helper is installed/upgraded by the normal updater. No host trust or services
are changed. See --help for the explicit disposable-guest opt-in.
"""

import argparse
import json
import pathlib
import re
import shlex
import subprocess
import time
import uuid


def arguments():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True, help="SSH alias for a disposable Linux VM")
    parser.add_argument("--ssh-config", help="optional SSH config; keys stay outside this script")
    parser.add_argument("--state-dir", required=True, help="absolute guest Mesh state directory")
    parser.add_argument("--cli", required=True, help="absolute guest path to the new CLI")
    parser.add_argument("--installed", required=True, help="absolute guest path used by mesh.service")
    parser.add_argument("--session", required=True, help="live shell session to preserve and interrupt")
    parser.add_argument("--update-version", required=True, help="first fixture release, containing the current helper")
    parser.add_argument("--reboot-version", required=True, help="second fixture release")
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--recover", action="store_true", help="also recover the interrupted session into a new shell")
    parser.add_argument("--disposable-vm", action="store_true", required=True,
                        help="authorize helper suspension and actual guest reboot; never use a production host")
    args = parser.parse_args()
    for field in ("state_dir", "cli", "installed"):
        if not pathlib.PurePosixPath(getattr(args, field)).is_absolute():
            parser.error(field + " must be an absolute guest path")
    if not re.fullmatch(r"[A-Z0-9]{4}", args.session):
        parser.error("session must be a four-character Mesh ID")
    for version in (args.update_version, args.reboot_version):
        if not re.fullmatch(r"v\d+\.\d+\.\d+", version):
            parser.error("fixture versions must be exact stable tags")
    return args


class Guest:
    def __init__(self, args):
        self.args = args
        self.ssh = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3"]
        if args.ssh_config:
            self.ssh += ["-F", args.ssh_config]
        args.output.mkdir(parents=True, exist_ok=True)

    def command(self, command, timeout=30):
        result = subprocess.run(self.ssh + [self.args.target, command], capture_output=True,
                                text=True, timeout=timeout, check=True)
        return result.stdout

    def python(self, source):
        return self.command("python3 -c " + shlex.quote(source))

    def mesh(self, *args, installed=False):
        binary = self.args.installed if installed else self.args.cli
        return shlex.join(["env", "MESH_STATE_DIR=" + self.args.state_dir, binary, *args])

    def journal(self):
        path = str(pathlib.PurePosixPath(self.args.state_dir) / "update/installation.json")
        return json.loads(self.python("import pathlib; print(pathlib.Path(" + repr(path) + ").read_text())"))

    def save(self, name, data):
        contents = data if isinstance(data, str) else json.dumps(data, indent=2) + "\n"
        (self.args.output / name).write_text(contents)

    def snapshot(self):
        path = str(pathlib.PurePosixPath(self.args.state_dir) / "s" / self.args.session / "meta.json")
        source = """import pathlib,json,hashlib
meta=json.loads(pathlib.Path(META).read_text())
pid=meta['pid']
files=pathlib.Path.home()/'.config/systemd/user'
units={str(p.relative_to(files)):hashlib.sha256(p.read_bytes()).hexdigest() for p in files.glob('mesh.service*') if p.is_file()}
units.update({str(p.relative_to(files)):hashlib.sha256(p.read_bytes()).hexdigest() for p in (files/'mesh.service.d').glob('*') if p.is_file()})
def process(pid):
 stat=pathlib.Path('/proc')/str(pid)/'stat'
 if not stat.exists():return {'pid':pid,'start':None,'parent':None}
 fields=stat.read_text().rsplit(')',1)[1].split()
 return {'pid':pid,'start':fields[19],'parent':int(fields[1])}
shell=process(pid)
worker=process(shell['parent']) if shell['parent'] else None
print(json.dumps({'meta':meta,'processes':{'shell':shell,'worker':worker},'units':units,'boot':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}))
""".replace("META", repr(path))
        return json.loads(self.python(source))

    def terminal_io(self, recover=False):
        token = "VM_IO_" + uuid.uuid4().hex
        action = ["recover", self.args.session, "--shell"] if recover else ["attach", self.args.session]
        with (self.args.output / ("recovery-io.log" if recover else "preserved-io.log")).open("wb") as log:
            process = subprocess.Popen(self.ssh + ["-tt", self.args.target, self.mesh(*action, installed=True)],
                                       stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT)
            try:
                time.sleep(2)
                process.stdin.write(("printf '%s\\n' " + shlex.quote(token) + "\n").encode())
                process.stdin.flush()
                time.sleep(2)
                log.flush()
                contents = pathlib.Path(log.name).read_text(errors="replace")
                contents = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", contents).replace("\r", "")
                if not re.search(r"(?m)^" + token + r"$", contents):
                    raise RuntimeError("interactive shell did not execute the I/O marker")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


def wait_for(check, seconds=90):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (subprocess.SubprocessError, json.JSONDecodeError, OSError) as error:
            last = error
        time.sleep(1)
    raise RuntimeError("acceptance deadline exceeded: " + str(last))


def phase(guest, version, wanted):
    journal = guest.journal()
    if journal["request"]["manifest"]["version"] != version:
        return None
    if journal["phase"] in ("failed", "rolled_back", "rollback_failed", "cancelled"):
        raise RuntimeError("installation stopped: " + json.dumps(journal))
    return journal if journal["phase"] == wanted else None


def process_identity(snapshot):
    return tuple((snapshot["processes"][role]["pid"], snapshot["processes"][role]["start"]) for role in ("shell", "worker"))


def run(args):
    guest = Guest(args)
    guest.python("import socket; assert socket.gethostbyname('github.com').startswith('127.'), 'require guest-local release fixture'")
    platform = guest.command("uname -s; uname -m").strip().splitlines()
    if platform[0] != "Linux" or platform[1] not in ("x86_64", "aarch64"):
        raise RuntimeError("this acceptance driver requires a supported Linux guest")
    platform = "linux/" + {"x86_64": "amd64", "aarch64": "arm64"}[platform[1]]
    if guest.command("systemctl --user show mesh.service -p KillMode --value").strip() != "process":
        raise RuntimeError("guest daemon must preserve workers with KillMode=process")
    before = guest.snapshot()
    guest.save("before.json", before)
    guest.save("update.log", guest.command(guest.mesh("update", "--local", "--version", args.update_version, "--yes", "--json"), timeout=180))
    wait_for(lambda: phase(guest, args.update_version, "committed"))
    after = guest.snapshot()
    guest.save("after-update.json", after)
    if process_identity(before) != process_identity(after) or before["units"] != after["units"]:
        raise RuntimeError("live worker identity or custom daemon units changed")
    guest.terminal_io()
    time.sleep(2)
    helper_pid = int(guest.command("systemctl --user show mesh-update-helper.service -p MainPID --value").strip())
    if helper_pid <= 1:
        raise RuntimeError("no running helper to suspend")
    helper_start = guest.python("import pathlib; print(pathlib.Path('/proc/" + str(helper_pid) + "/stat').read_text().rsplit(')',1)[1].split()[19])").strip()
    guest.command("kill -STOP " + str(helper_pid))
    rebooted = False
    with (args.output / "initiating-cli.log").open("wb") as log:
        command = guest.mesh("update", "--local", "--version", args.reboot_version, "--yes", "--json", installed=True)
        client = subprocess.Popen(guest.ssh + [args.target, command], stdout=log, stderr=subprocess.STDOUT)
        try:
            granted = wait_for(lambda: phase(guest, args.reboot_version, "granted"))
            guest.save("granted.json", granted)
            workers = granted["original"]["workers"]
            expected = before["processes"]
            if not any(w["id"] == args.session and w["pid"] == expected["worker"]["pid"] and w["shellPid"] == expected["shell"]["pid"] for w in workers):
                raise RuntimeError("granted receipt does not identify the preserved worker and shell")
            try:
                guest.command("sync; sudo -n systemctl reboot --force")
            except subprocess.CalledProcessError as error:
                if error.returncode != 255:
                    raise
            rebooted = True
            wait_for(lambda: guest.snapshot()["boot"] != before["boot"])
            committed = wait_for(lambda: phase(guest, args.reboot_version, "committed"))
            guest.save("committed.json", committed)
        finally:
            if client.poll() is None:
                client.terminate()
            client.wait(timeout=10)
            if not rebooted:
                guest.python("import pathlib,os,signal; p=pathlib.Path('/proc/" + str(helper_pid) + "/stat'); "
                             "same=p.exists() and p.read_text().rsplit(')',1)[1].split()[19]==" + repr(helper_start) + "; "
                             "os.kill(" + str(helper_pid) + ",signal.SIGCONT) if same else None")
    interrupted = committed.get("verified", {}).get("interruptedWorkers", [])
    if not any(worker["id"] == args.session for worker in interrupted):
        raise RuntimeError("reboot receipt omitted the interrupted session")
    if guest.snapshot()["units"] != before["units"]:
        raise RuntimeError("reboot update changed custom daemon units")
    run_id = committed["request"]["id"]
    guest.save("fleet-status.json", wait_for(lambda: guest.command(guest.mesh("update", "status", run_id, "--json", installed=True))))
    if args.recover:
        guest.terminal_io(recover=True)
    guest.save("result.json", {"passed": True, "phase": "granted", "run": run_id,
                               "recoveryTested": args.recover, "platform": platform})
    print("Passed: worker preservation, real Granted-phase reboot, and interrupted-session receipt.")


if __name__ == "__main__":
    run(arguments())
