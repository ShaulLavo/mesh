#!/usr/bin/env python3
"""Keep integration processes away from the caller's Mesh installation."""

from contextlib import contextmanager
import os
from pathlib import Path
import pwd
import shutil
import signal
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True

from process_diagnostics import process_snapshot, record_signal  # noqa: E402 - copied helpers stay bytecode-free


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


TERMINATION_SIGNALS = (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)


@contextmanager
def termination_cleanup():
    def stop(signum, _):
        raise SystemExit(128 + signum)

    previous = {signum: signal.getsignal(signum) for signum in TERMINATION_SIGNALS}
    try:
        for signum in previous:
            signal.signal(signum, stop)
        yield
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def run_process(command, environment):
    cancelled = None

    def pending(signum, _):
        nonlocal cancelled
        cancelled = signum

    def terminate(signum, _):
        for watched in TERMINATION_SIGNALS:
            signal.signal(watched, signal.SIG_IGN)
        try:
            record_signal(process.pid, signum, "integration-cancellation",
                          {"kind": "popen_process_group", "group_id": process.pid}, incarnation)
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass
        raise SystemExit(128 + signum)

    process = None
    incarnation = None
    previous = {signum: signal.getsignal(signum) for signum in TERMINATION_SIGNALS}
    try:
        # Queue cancellation until Popen has returned the PID we must reap.
        for signum in previous:
            signal.signal(signum, pending)
        process = subprocess.Popen(command, env=environment, start_new_session=True)
        incarnation = process_snapshot(process.pid)
        for signum in previous:
            signal.signal(signum, terminate)
        if cancelled is not None:
            terminate(cancelled, None)
        status = process.wait()
        return status if status >= 0 else 128 - status
    except SystemExit:
        # Wait outside the signal handler so Popen's wait lock has unwound.
        if process is not None:
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                pass
            try:
                record_signal(process.pid, signal.SIGKILL, "integration-cancellation-cleanup",
                              {"kind": "popen_process_group", "group_id": process.pid}, incarnation)
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        raise
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def run(bash, script, arguments):
    refuse_live_paths({name: os.environ.get(name) for name in ("MESH_STATE_DIR", "MESH_CONFIG_DIR")})
    refuse_live_paths({name: os.environ.get(name) for name in ("TMPDIR", "TEMP", "TMP", "MESH_SHORT_TMP")}, parents=False)
    with tempfile.TemporaryDirectory(prefix="m-isolate-", dir=os.environ.get("TMPDIR") or None) as temporary:
        root = Path(temporary)
        home = root / "home"
        tools = root / "bin"
        home.mkdir()
        tools.mkdir()
        for name, target in (("python3", str(Path(sys.executable).resolve())), ("bash", bash), ("sh", "/bin/sh")):
            (tools / name).symlink_to(target)
        selected = ("PATH", "TMPDIR", "TERM", "LANG", "DBUS_SESSION_BUS_ADDRESS", "GOCACHE", "GOMODCACHE",
                    "MESH", "MESH_INTEGRATION_BINARY", "MESH_TEST_ZSH", "MESH_SHORT_TMP", "MESH_WATCH_SECONDS")
        environment = {name: os.environ[name] for name in selected if name in os.environ}
        if shutil.which("go"):
            for name in ("GOROOT", "GOCACHE", "GOMODCACHE"):
                value = subprocess.check_output(["go", "env", name], text=True).strip()
                if name == "GOROOT":
                    (tools / "go").symlink_to(Path(value) / "bin" / "go")
                else:
                    environment[name] = value
        environment.update({
            "PATH": str(tools) + os.pathsep + environment.get("PATH", os.defpath),
            "HOME": str(home),
            "MESH_STATE_DIR": os.path.abspath(os.environ.get("MESH_STATE_DIR") or str(root / "state")),
            "MESH_CONFIG_DIR": os.path.abspath(os.environ.get("MESH_CONFIG_DIR") or str(root / "config")),
            "MESH_INTEGRATION_ENTRY": os.path.abspath(script),
            "PYTHONDONTWRITEBYTECODE": "1",
        })
        config = Path(environment["MESH_CONFIG_DIR"])
        config.mkdir(parents=True, exist_ok=True, mode=0o700)
        (config / "domains.json").write_text('{"primary":"mesh.test","aliases":["old.test"],"legacyCertificateDomain":"mesh.test"}')
        for name, suffix in (("XDG_CONFIG_HOME", "config-home"), ("XDG_STATE_HOME", "state-home"),
                             ("XDG_CACHE_HOME", "cache"), ("XDG_DATA_HOME", "data"), ("XDG_RUNTIME_DIR", "runtime")):
            directory = root / suffix
            directory.mkdir(mode=0o700)
            environment[name] = str(directory)
        # The bus is needed to prove scope ownership, but is never a Mesh path.
        if "DBUS_SESSION_BUS_ADDRESS" not in environment and os.environ.get("XDG_RUNTIME_DIR"):
            environment["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + os.path.join(os.environ["XDG_RUNTIME_DIR"], "bus")
        return run_process([bash, script, *arguments], environment)


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
    with termination_cleanup():
        sys.exit(main())
