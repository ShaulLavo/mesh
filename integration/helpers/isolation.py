#!/usr/bin/env python3
"""Keep integration processes away from the caller's Mesh installation."""

from contextlib import contextmanager
import os
from pathlib import Path
import pwd
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time

sys.dont_write_bytecode = True


def refuse_live_paths(paths, environment=None, *, parents=True):
    environment = os.environ if environment is None else environment
    homes = {environment.get("HOME", ""), pwd.getpwuid(os.getuid()).pw_dir}
    defaults = set()
    for home in homes:
        if not home:
            continue
        for suffix in (".config", ".local/state"):
            parent = os.path.join(home, suffix)
            defaults.update(os.path.join(resolve(parent), "mesh") for resolve in (os.path.abspath, os.path.realpath))
            defaults.add(os.path.realpath(os.path.join(parent, "mesh")))
    for name in ("XDG_CONFIG_HOME", "XDG_STATE_HOME"):
        if environment.get(name):
            defaults.update(resolve(os.path.join(environment[name], "mesh")) for resolve in (os.path.abspath, os.path.realpath))
    for name, value in paths.items():
        if not value:
            continue
        for resolve in (os.path.abspath, os.path.realpath):
            candidate = resolve(value)
            for default in defaults:
                common = os.path.commonpath((candidate, default))
                if common == default or parents and common == candidate:
                    raise RuntimeError(f"integration isolation refuses {name}={value}: overlaps the caller's Mesh default {default}")


def check_environment():
    paths = {name: os.environ.get(name) for name in ("MESH_STATE_DIR", "MESH_CONFIG_DIR")}
    refuse_live_paths(paths)
    refuse_live_paths({name: os.environ.get(name) for name in ("TMPDIR", "TEMP", "TMP", "MESH_SHORT_TMP")}, parents=False)
    if not all(paths.values()):
        raise RuntimeError("integration isolation requires explicit MESH_STATE_DIR and MESH_CONFIG_DIR")


# These entries need fixed ports before delayed commands and daemon restarts bind them.
SERVED_PORT_COUNTS = {"serve_kept.sh": 5, "serve_on_demand.sh": 9,
                      "serve_cli.sh": 2, "serving_survives_restart.sh": 1, "private_tls_distribution.sh": 1}


TERMINATION_SIGNALS = (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)


def defer_termination(signum, _):
    """A Python handler, not SIG_IGN, so children started during cleanup keep default dispositions."""


@contextmanager
def termination_cleanup():
    """Turn the first termination signal into SystemExit so finally blocks run their cleanup.

    Later signals cannot interrupt that cleanup: the first one replaces the
    handlers with one that ignores them. Cleanup that may already be running
    when the first signal arrives uses protected_cleanup().
    """
    def stop(signum, _):
        for watched in TERMINATION_SIGNALS:
            signal.signal(watched, defer_termination)
        raise SystemExit(128 + signum)

    previous = {signum: signal.getsignal(signum) for signum in TERMINATION_SIGNALS}
    try:
        for signum in previous:
            signal.signal(signum, stop)
        yield
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


@contextmanager
def protected_cleanup():
    """Run cleanup that termination signals must not interrupt or restart."""
    if threading.current_thread() is not threading.main_thread():
        # Python runs handlers only in the main thread, so they cannot interrupt this one.
        yield
        return
    previous = {signum: signal.getsignal(signum) for signum in TERMINATION_SIGNALS}
    try:
        for signum in previous:
            signal.signal(signum, defer_termination)
        yield
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def setup_failed(message):
    from owned_command import SetupFailed
    print(f"FAIL: {message}", file=sys.stderr, flush=True)
    return SetupFailed(1)


def run_setup(owner, argv, environment, output):
    try:
        return owner.run_setup(argv, environment, output)
    except FileNotFoundError:
        return 127


def owned_go_environment(owner, environment, tools):
    """The isolated Go paths, read by owned setup commands that cancellation can interrupt."""
    values = {}
    if not shutil.which("go", path=os.environ.get("PATH")):
        return values
    output = owner.root / "go-env.out"
    for name in ("GOROOT", "GOCACHE", "GOMODCACHE"):
        status = run_setup(owner, ["go", "env", name], environment, output)
        if status:
            raise setup_failed(f"go env {name} exited with status {status}")
        values[name] = output.read_text().strip()
    (tools / "go").symlink_to(Path(values.pop("GOROOT")) / "bin" / "go")
    return values


def prepare_isolation(owner):
    """Owner-side setup for one integration entry: private HOME, XDG, config and tools.

    It runs in the job's owner after the owner has accepted ownership, so the
    entry, its setup commands and everything they start are joined before the
    owner's scratch root, which holds HOME and the XDG directories, is deleted.
    """
    spec = owner.admission["isolation"]
    caller = dict(os.environ)
    try:
        refuse_live_paths({name: caller.get(name) for name in ("MESH_STATE_DIR", "MESH_CONFIG_DIR")})
        refuse_live_paths({name: caller.get(name) for name in ("TMPDIR", "TEMP", "TMP", "MESH_SHORT_TMP")}, parents=False)
    except RuntimeError as error:
        raise setup_failed(str(error)) from error
    # Losing an available bus would turn the scope assertions into a false pass.
    if spec.get("user_bus") and run_setup(owner, ["busctl", "--user", "status"], caller, owner.root / "bus.out"):
        raise setup_failed(f"{owner.name} lost the caller's user bus")
    root = owner.root
    home = root / "home"
    tools = root / "bin"
    home.mkdir()
    tools.mkdir()
    for name, target in (("python3", str(Path(sys.executable).resolve())), ("bash", spec["bash"]), ("sh", "/bin/sh")):
        (tools / name).symlink_to(target)
    selected = ("PATH", "TMPDIR", "TERM", "LANG", "DBUS_SESSION_BUS_ADDRESS", "GOCACHE", "GOMODCACHE",
                "MESH", "MESH_INTEGRATION_BINARY", "MESH_TEST_ZSH", "MESH_SHORT_TMP", "MESH_WATCH_SECONDS")
    environment = {name: caller[name] for name in selected if name in caller}
    environment.update(owned_go_environment(owner, caller, tools))
    environment.update({
        "PATH": str(tools) + os.pathsep + environment.get("PATH", os.defpath),
        "HOME": str(home),
        "MESH_STATE_DIR": os.path.abspath(caller.get("MESH_STATE_DIR") or str(root / "state")),
        "MESH_CONFIG_DIR": os.path.abspath(caller.get("MESH_CONFIG_DIR") or str(root / "config")),
        "MESH_INTEGRATION_ENTRY": os.path.abspath(spec["script"]),
        "PYTHONDONTWRITEBYTECODE": "1",
    })
    config = Path(environment["MESH_CONFIG_DIR"])
    config.mkdir(parents=True, exist_ok=True, mode=0o700)
    policy = config / "domains.json"
    policy.write_text('{"primary":"mesh.test","aliases":["old.test"],"legacyCertificateDomain":"mesh.test"}')
    policy.chmod(0o600)
    for name, suffix in (("XDG_CONFIG_HOME", "config-home"), ("XDG_STATE_HOME", "state-home"),
                         ("XDG_CACHE_HOME", "cache"), ("XDG_DATA_HOME", "data"), ("XDG_RUNTIME_DIR", "runtime")):
        directory = root / suffix
        directory.mkdir(mode=0o700)
        environment[name] = str(directory)
    # The bus is needed to prove scope ownership, but is never a Mesh path.
    if "DBUS_SESSION_BUS_ADDRESS" not in environment and caller.get("XDG_RUNTIME_DIR"):
        environment["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + os.path.join(caller["XDG_RUNTIME_DIR"], "bus")
    owner.port_reservations = []
    ports = []
    for _ in range(SERVED_PORT_COUNTS.get(Path(spec["script"]).name, 0)):
        reservation = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
        owner.port_reservations.append(reservation)
        # Keep automatic allocation off both families without accepting traffic.
        # Explicit SO_REUSEADDR listeners can start and restart until this owner exits.
        reservation.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        reservation.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
        reservation.bind(("::", 0))
        ports.append(str(reservation.getsockname()[1]))
    if ports:
        environment["MESH_INTEGRATION_PORTS"] = " ".join(ports)
    return [spec["bash"], spec["script"], *spec.get("arguments", ())], environment


class LogForward:
    """Forward the job's regular log to this process's stderr without ever waiting for a reader.

    Each tick reads at most one chunk, and only after the previous one went out,
    so work and memory per tick stay bounded and cancellation is never stuck
    behind a full pipe. The log remains the full record: when forwarding cannot
    finish, the caller keeps it instead of deleting it.
    """

    def __init__(self, path):
        from owned_command import Delivery
        self.path = path
        self.offset = 0
        self.delivery = Delivery(sys.stderr.fileno())

    def __call__(self, deadline=float("inf"), clock=time.monotonic):
        if not self.delivery.pump(deadline, clock) or self.delivery.stopped:
            return
        try:
            with open(self.path, "rb") as source:
                source.seek(self.offset)
                data = source.read(self.delivery.CHUNK)
        except OSError:
            return
        self.offset += len(data)
        self.delivery.queue(data)
        self.delivery.pump(deadline, clock)

    def delivered(self):
        try:
            size = os.path.getsize(self.path)
        except OSError:
            return False
        return self.delivery.complete() and self.offset >= size

    def finish(self, deadline, wait, clock):
        """Forward the rest by deadline. True when the whole log reached stderr."""
        while clock() < deadline:
            self(deadline, clock)
            if clock() >= deadline:
                return False
            if self.delivered():
                return True
            if self.delivery.stopped or clock() >= deadline:
                return False
            wait()
        return False


def run(bash, script, arguments):
    """Standalone entry: this process is the controller of one owned job.

    Its contract is narrower than verify's. The shell prelude resolves paths
    before this process exists, there is no budget, and the job's stderr is
    forwarded here from a private log without blocking; a log that cannot be
    forwarded in full is kept beside the record directory. Ownership itself is
    the same: the job's owner joins every descendant before deleting the
    isolated root, and this process removes its record directory only after
    that owner and its launcher have exited.
    """
    refuse_live_paths({name: os.environ.get(name) for name in ("MESH_STATE_DIR", "MESH_CONFIG_DIR")})
    refuse_live_paths({name: os.environ.get(name) for name in ("TMPDIR", "TEMP", "TMP", "MESH_SHORT_TMP")}, parents=False)
    from owned_command import Controller, Triggers, keep_log
    from process_tree import Kernel
    # Refuse before creating anything where children cannot be joined.
    Kernel().require()
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    triggers = Triggers()
    root = Path(tempfile.mkdtemp(prefix="m-entry-", dir=os.environ.get("TMPDIR") or None))
    controller = Controller(root, triggers)
    log = root / "entry.log"
    with open(log, "w") as diagnostics:
        job = controller.start(Path(script).name, budget=float("inf"), cwd=os.getcwd(), inherit_env=True,
                               scratch_parent=os.environ.get("TMPDIR") or None,
                               isolation={"bash": bash, "script": script, "arguments": arguments},
                               popen={"stderr": diagnostics})
    forward = LogForward(log)
    controller.wait([job], each_tick=forward)
    settings = job.settings
    if not forward.finish(controller.clock() + settings["allowance"],
                          lambda: triggers.wait([], settings["tick"]), controller.clock):
        kept = keep_log(log, root.parent, "m-entry-diagnostics-")
        forward.delivery.notice(f"\nFAIL: diagnostics were not fully delivered; the full log is kept at {kept}\n")
    status = job.outcome.get("returncode")
    proved = controller.proved()
    if proved:
        shutil.rmtree(root)
    else:
        forward.delivery.notice(f"FAIL: kept {root}: {job.name} is {job.state()}: {job.outcome.get('reasons', [])}\n")
    # The first signal decides the status, even one recorded after the job finished.
    first = triggers.check()
    if first is not None:
        return first.status
    if not proved:
        return status or 1
    return 1 if status is None else status


def main():
    try:
        if sys.argv[1:] == ["--check"]:
            check_environment()
            return 0
        return run(*sys.argv[1:3], sys.argv[3:])
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
