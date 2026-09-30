#!/usr/bin/env python3
"""Keep integration processes away from the caller's Mesh installation."""

import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import tempfile

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
    if environment.get("HOME"):
        defaults.update(os.path.realpath(os.path.join(environment["HOME"], suffix))
                        for suffix in (".config/mesh", ".local/state/mesh"))
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
                    "MESH", "MESH_INTEGRATION_BINARY", "MESH_TEST_ZSH", "MESH_SHORT_TMP")
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
        })
        for name, suffix in (("XDG_CONFIG_HOME", "config-home"), ("XDG_STATE_HOME", "state-home"),
                             ("XDG_CACHE_HOME", "cache"), ("XDG_DATA_HOME", "data"), ("XDG_RUNTIME_DIR", "runtime")):
            directory = root / suffix
            directory.mkdir(mode=0o700)
            environment[name] = str(directory)
        # The bus is needed to prove scope ownership, but is never a Mesh path.
        if "DBUS_SESSION_BUS_ADDRESS" not in environment and os.environ.get("XDG_RUNTIME_DIR"):
            environment["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + os.path.join(os.environ["XDG_RUNTIME_DIR"], "bus")
        result = subprocess.run([bash, script, *arguments], env=environment)
        return result.returncode if result.returncode >= 0 else 128 - result.returncode


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
