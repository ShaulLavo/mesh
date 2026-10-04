"""Short disposable socket roots for the historical helper recovery proof."""
import os
import shutil
import sys
import tempfile
from pathlib import Path

# Darwin's 104-byte sun_path includes its terminating NUL.
MAX_SOCKET_BYTES = 103
HOSTS = ("repair", "rollback")


def selected_base():
    # Darwin's per-user TMPDIR can exceed sun_path before a state name is added.
    return Path(os.environ.get("MESH_SHORT_TMP", "/tmp"))


def socket_paths(proof_root):
    for host in HOSTS:
        state = proof_root / host / "state"
        yield state / "daemon.sock"
        yield state / ".d-4294967295"
        yield state / "s" / ("0" * 26) / "sock"


def validate_socket_root(proof_root):
    root = Path(proof_root).resolve()
    paths = [{"path": str(path), "bytes": len(os.fsencode(path))} for path in socket_paths(root)]
    longest = max(row["bytes"] for row in paths)
    if longest > MAX_SOCKET_BYTES:
        raise ValueError(f"helper fixture socket path requires {longest} bytes; maximum is {MAX_SOCKET_BYTES}")
    return {"root": str(root), "maximumSocketBytes": MAX_SOCKET_BYTES,
            "longestSocketBytes": longest, "socketPaths": paths}


def create_root():
    root = Path(tempfile.mkdtemp(prefix="mh-", dir=selected_base())).resolve()
    try:
        validate_socket_root(root / "proof")
    except ValueError:
        shutil.rmtree(root)
        raise
    return root


if __name__ == "__main__":
    if sys.argv[1:] != ["create"]:
        sys.exit("usage: helper_recovery_paths.py create")
    print(create_root())
