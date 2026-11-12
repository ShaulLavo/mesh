#!/usr/bin/env python3
"""Installed catch-up proof, gated first on an actual native service manager."""
import argparse
import json
import shutil
import signal
import platform
import os
import subprocess
from pathlib import Path

from fixtures import fleet_baselines, installed_paths, native_services


def interrupted(number, _frame):
    raise InterruptedError(f"installed proof interrupted by signal {number}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("--native-preflight", action="store_true", required=True)
    parser.add_argument("--acquire-baselines", action="store_true", help="verify actual native legacy pairs after the manager control")
    parser.add_argument("--native-helper", type=Path, help="run the native installed-pair restart control with this production API fixture")
    parser.add_argument("--installed-adoption", action="store_true", help="run actual native product adoption and failed activation")
    parser.add_argument("--prepare-only", action="store_true", help="prepare real signed proof inputs without native installation")
    arguments = parser.parse_args()
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    native = platform.system().lower() + "/" + {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine(), platform.machine())
    required = os.environ.get("INSTALLED_CATCHUP_PLATFORM", native)
    if native != required:
        raise RuntimeError(f"native platform is {native}; required {required}")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    root = installed_paths.create()
    try:
        (output / "socket-paths.json").write_text(json.dumps(installed_paths.validate(root), indent=2) + "\n")
        native_services.positive_control(root, output / "native-manager.json")
        run_requested(arguments, root, output, native)
    finally:
        clean_scratch(root, output)


def run_requested(arguments, root, output, native):
    if arguments.installed_adoption:
        if arguments.native_helper is None and not arguments.prepare_only:
            raise RuntimeError("installed adoption requires the production helper preparation fixture")
        from fixtures import installed_adoption
        preparer = arguments.native_helper.resolve(strict=True) if arguments.native_helper else None
        installed_adoption.run(root, output, native, preparer, arguments.prepare_only)
        return
    if arguments.acquire_baselines:
        acquire_baselines(root, output, native, arguments.native_helper)


def clean_scratch(root, output):
    record = output / "native-manager.json"
    cleanup = json.loads(record.read_text()).get("cleanup", {}) if record.exists() else {}
    if not cleanup.get("serviceAbsent") or not cleanup.get("processAbsent"):
        raise RuntimeError(f"native cleanup unconfirmed; retained {root} for the test owner")
    for path in output.glob("*-native-pair.json"):
        pair = json.loads(path.read_text()).get("cleanup", {})
        if not pair.get("servicesAbsent") or not pair.get("processesAbsent"):
            raise RuntimeError(f"installed cleanup unconfirmed; retained {root} for the test owner")
    shutil.rmtree(root)
    (output / "scratch-cleanup.json").write_text(json.dumps({"root": str(root), "absent": not root.exists()}) + "\n")


def acquire_baselines(root, output, native, preparer=None):
    space = shutil.disk_usage(root)
    storage = {"root": str(root.resolve()), "device": root.stat().st_dev, "freeBytes": space.free,
               "requiredFreeBytes": 1 << 30}
    (output / "storage-preflight.json").write_text(json.dumps(storage, indent=2) + "\n")
    if space.free < storage["requiredFreeBytes"]:
        raise RuntimeError("installed fixture needs 1 GiB free before acquiring native baselines")
    fixture = fleet_baselines.load()
    evidence = {"schema": 1, "platform": native, "firstPublishedUpdater": fixture["firstPublishedUpdater"], "pairs": []}
    hosts = [host for host in fixture["hosts"] if host["platform"] == native]
    if not hosts:
        raise RuntimeError("no exact fleet baseline for this native platform")
    for host in hosts:
        pair = fleet_baselines.acquire_pair(root / "archives", host)
        evidence["pairs"].append(inspect_pair(host, pair, root))
        if preparer:
            from fixtures import installed_native
            installed_native.prove(root, output, host, pair, preparer.resolve(strict=True))
    (output / "published-baselines.json").write_text(json.dumps(evidence, indent=2) + "\n")


def inspect_pair(host, pair, root):
    builds = {}
    environment = os.environ | {"HOME": str(root), "MESH_STATE_DIR": str(root / "unused-state"),
                               "MESH_CONFIG_DIR": str(root / "unused-config")}
    for role, release in pair.items():
        result = subprocess.run([str(release["executable"]), "version", "--json"], env=environment,
                                capture_output=True, text=True, timeout=10, check=True)
        build = json.loads(result.stdout)
        expected = release["artifact"]["binarySha256"]
        if (build["version"] != host[role] or build["digest"] != expected or build["modified"]
                or build["platform"] != release["artifact"]["platform"]):
            raise RuntimeError("actual published executable reports a different baseline identity")
        builds[role] = {"build": build, "manifestSha256": release["manifestSha256"], "artifact": release["artifact"]}
    return {"host": host["name"], "helperRunning": host["helperRunning"], "releases": builds,
            "acquisition": "system-trusted public HTTPS; actual published archives, executed natively"}


if __name__ == "__main__":
    main()
