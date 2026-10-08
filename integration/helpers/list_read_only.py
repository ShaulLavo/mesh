import os
from pathlib import Path
import subprocess
import sys

binary, scratch = sys.argv[1], Path(sys.argv[2])
book_bytes = b'{"version":1,"hosts":[]}'


def listing(state, config, command="ls"):
    environment = dict(os.environ, MESH_STATE_DIR=str(state), MESH_CONFIG_DIR=str(config))
    return subprocess.run([binary, command], env=environment, capture_output=True, text=True, timeout=5)


def fixture(name):
    root = scratch / name
    root.mkdir(mode=0o700)
    config = root / "config"
    config.mkdir(mode=0o700)
    book = config / "hosts.json"
    book.write_bytes(book_bytes)
    book.chmod(0o644)
    config.chmod(0o500)
    return root, config, book


def empty_listing(name, parent_mode, existing_state):
    root, config, book = fixture(name)
    state = root / "state"
    if existing_state:
        state.mkdir(mode=0o500)
    before = book.stat()
    root.chmod(parent_mode)
    try:
        result = listing(state, config)
        assert result.returncode == 0, f"{name}: exit {result.returncode}: {result.stderr}"
        assert result.stdout.strip() == "no live sessions on this host", result.stdout
        alias = listing(state, config, "list")
        assert alias.returncode == 0, f"{name}: list alias exit {alias.returncode}: {alias.stderr}"
        assert alias.stdout == result.stdout, f"{name}: list alias output changed"
        assert state.exists() == existing_state, f"{name}: listing created absent state"
        assert not (state / "s").exists(), f"{name}: listing created sessions directory"
        after = book.stat()
        assert book.read_bytes() == book_bytes
        assert (before.st_ino, before.st_mode) == (after.st_ino, after.st_mode)
        assert not (config / ".hosts.lock").exists()
        assert not (config / "domains.json").exists(), f"{name}: listing published deployment policy"
    finally:
        root.chmod(0o700)
        config.chmod(0o700)
        if state.is_dir():
            state.chmod(0o700)
    print(f"PASS: {name}")


def refused_listing(name, prepare):
    root, config, book = fixture(name)
    state = root / "state"
    prepare(state)
    try:
        result = listing(state, config)
        assert result.returncode != 0, f"{name}: unsafe or unreadable state accepted"
        assert book.read_bytes() == book_bytes
        assert not (config / ".hosts.lock").exists()
    finally:
        config.chmod(0o700)
        if state.is_dir():
            state.chmod(0o700)
        sessions = state / "s"
        if sessions.is_dir():
            sessions.chmod(0o700)
    print(f"PASS: {name}")


def unreadable_sessions(state):
    state.mkdir(mode=0o700)
    (state / "s").mkdir(mode=0o000)


def sessions_file(state):
    state.mkdir(mode=0o700)
    (state / "s").write_bytes(b"fixture")


def dangling_sessions(state):
    state.mkdir(mode=0o700)
    (state / "s").symlink_to("missing-sessions")


def unsafe_identity(state):
    state.mkdir(mode=0o700)
    (state / "identity.key").symlink_to("missing-key")


empty_listing("absent state", 0o700, False)
if os.geteuid() != 0:
    empty_listing("absent state beneath unwritable parent", 0o500, False)
    empty_listing("existing read-only state without sessions", 0o500, True)
    refused_listing("unreadable sessions", unreadable_sessions)
else:
    print("SKIP: permission controls require a non-root user")
refused_listing("state is a file", lambda state: state.write_bytes(b"fixture"))
refused_listing("dangling state link", lambda state: state.symlink_to("missing-state"))
refused_listing("sessions is a file", sessions_file)
refused_listing("dangling sessions link", dangling_sessions)
refused_listing("unsafe identity", unsafe_identity)
