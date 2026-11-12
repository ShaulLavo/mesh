#!/usr/bin/env python3
"""Record capabilities only from the actual native release executable."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile


def probe(binary, version, platform, environment=None):
    result = subprocess.run([str(binary), "update-reader", "-capabilities"],
                            env=environment, capture_output=True, check=True, timeout=30)
    if len(result.stdout) > 16 << 10:
        raise ValueError("candidate reader capabilities exceed their byte bound")
    value = json.loads(result.stdout)
    if type(value.get("schema")) is not int or value["schema"] != 1 or type(value.get("epoch")) is not int or value["epoch"] <= 0:
        raise ValueError("candidate did not report its implemented updater reader")
    build = value["build"]
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    expected_platform = dict(zip(("os", "arch"), platform.split("/"), strict=True))
    if build.get("version") != version or build.get("digest") != digest or build.get("platform") != expected_platform or build.get("modified") is not False:
        raise ValueError("candidate reader capabilities differ from the executing release artifact")
    return value


def main():
    signal.signal(signal.SIGTERM, terminate_run)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("version")
    parser.add_argument("platform")
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="mrc-", dir=os.environ.get("MESH_SHORT_TMP")) as temporary:
        root = Path(temporary)
        environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
        environment.update(HOME=str(root), MESH_STATE_DIR=str(root / "state"), MESH_CONFIG_DIR=str(root / "config"))
        value = probe(args.binary, args.version, args.platform, environment)
    args.output.write_text(json.dumps(value, sort_keys=True) + "\n")


def terminate_run(signum, _frame):
    raise SystemExit(128 + signum)


if __name__ == "__main__":
    main()
