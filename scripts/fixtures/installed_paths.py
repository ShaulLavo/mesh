"""Resolved socket budgets for every installed-catchup case, before setup."""
import os
import platform
import shutil
import tempfile
from pathlib import Path

CASES = ("pc", "pi", "vps", "mac", "published", "workers", "reader", "canary", "sleep", "burst") + tuple(
    host + "-" + scenario for host in ("pc", "pi", "vps", "mac") for scenario in ("adopt", "rollback", "helper", "initial"))


def socket_paths(root):
    for case in CASES:
        state = root / case / "state"
        yield state / "daemon.sock"
        yield state / ".d-4294967295"
        yield state / "s" / ("0" * 26) / "sock"
    yield root / "control.sock"
    yield root / "mesh-transition.AAAAAA" / "state" / "daemon.sock"
    yield root / "mesh-transition.AAAAAA" / "state" / "s" / ("0" * 26) / "sock"


def validate(root, system=None):
    root = Path(root).resolve()
    maximum = 103 if (system or platform.system()) == "Darwin" else 107
    paths = [{"path": str(path), "bytes": len(os.fsencode(path))} for path in socket_paths(root)]
    longest = max(row["bytes"] for row in paths)
    if longest > maximum:
        raise ValueError(f"installed fixture socket requires {longest} bytes; maximum is {maximum}")
    return {"root": str(root), "maximumSocketBytes": maximum, "longestSocketBytes": longest, "paths": paths}


def create():
    # TMPDIR on hosted macOS is already too long before Mesh adds a worker path.
    selected = Path(os.environ.get("MESH_SHORT_TMP", "/tmp")).absolute()
    require_mounted_drive(selected)
    base = selected.resolve(strict=True)
    require_mounted_drive(base)
    root = Path(tempfile.mkdtemp(prefix="mi-", dir=base)).resolve()
    try:
        validate(root)
    except ValueError:
        shutil.rmtree(root)
        raise
    return root


def require_mounted_drive(base):
    for drive in (Path("/work"), Path("/data")):
        if base.is_relative_to(drive) and not drive.is_mount():
            raise RuntimeError(f"fixture destination drive {drive} is not mounted")
