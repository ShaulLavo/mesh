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
    parser.add_argument("--worker-registry", type=Path, help="authoritative Unit3 retained/workers.json with verified binaries")
    parser.add_argument("--worker-probe", type=Path, help="once-built Unit3 kernel/image observer")
    parser.add_argument("--source-host", help="one inventory host for a full exact-worker source shard")
    parser.add_argument("--signed-chain", action="store_true", help="run real signed intermediate/helper hops with the exact cohort")
    parser.add_argument("--format-waypoint", action="store_true", help="cross an actual incompatible manifest schema through an old-format waypoint")
    parser.add_argument("--ordered-hosts", action="store_true", help="three disposable native-manager hosts with real predecessor policy")
    parser.add_argument("--fresh-managed", action="store_true", help="actual fresh helper preparation and missing-policy automatic first start")
    parser.add_argument("--scenario", choices=("adopt", "rollback", "helper", "initial", "interrupt"), help="one adoption profile; the default requires all profiles")
    parser.add_argument("--publication-version", help="actual main-only signed draft version; never a supplemental source label")
    parser.add_argument("--publication-source", help="exact main release source bound by the real manifest and native image")
    arguments = parser.parse_args()
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if os.environ.get("MESH_HARNESS_SOURCE"):
        from fixtures import installed_artifacts
        installed_artifacts.verify_harness(os.environ["MESH_HARNESS_SOURCE"], output / "harness-source.json")
    native = platform.system().lower() + "/" + {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine(), platform.machine())
    required = os.environ.get("INSTALLED_CATCHUP_PLATFORM", native)
    if native != required:
        raise RuntimeError(f"native platform is {native}; required {required}")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    root = installed_paths.create()
    try:
        from fixtures import noindex_paths
        (output / "noindex-setup.json").write_text(json.dumps(noindex_paths.require(root) | {"before": noindex_paths.observation()}) + "\n")
        (output / "socket-paths.json").write_text(json.dumps(installed_paths.validate(root), indent=2) + "\n")
        native_services.positive_control(root, output / "native-manager.json")
        run_requested(arguments, root, output, native)
    finally:
        clean_scratch(root, output)
        (output / "noindex-after.json").write_text(json.dumps(noindex_paths.observation()) + "\n")


def run_requested(arguments, root, output, native):
    if arguments.publication_version is not None or arguments.publication_source is not None:
        if not arguments.publication_version or not arguments.publication_source or not arguments.native_helper:
            raise RuntimeError("real draft mode needs its exact release version, source and production helper preparer")
        from fixtures import installed_publication
        installed_publication.prove(root, output, native, arguments.publication_version, arguments.publication_source,
            arguments.native_helper.resolve(strict=True), arguments.worker_registry, arguments.worker_probe)
        return
    if arguments.fresh_managed:
        from fixtures import installed_fresh
        installed_fresh.prove(root, output, native)
        return
    if arguments.installed_adoption:
        if arguments.native_helper is None and not arguments.prepare_only:
            raise RuntimeError("installed adoption requires the production helper preparation fixture")
        from fixtures import installed_adoption
        preparer = arguments.native_helper.resolve(strict=True) if arguments.native_helper else None
        installed_adoption.run(root, output, native, preparer, arguments.prepare_only,
            arguments.worker_registry, arguments.worker_probe, arguments.source_host, arguments.signed_chain or arguments.format_waypoint,
            arguments.format_waypoint, arguments.ordered_hosts, arguments.scenario)
        return
    if arguments.acquire_baselines:
        acquire_baselines(root, output, native, arguments.native_helper)


def clean_scratch(root, output):
    record = output / "native-manager.json"
    cleanup = json.loads(record.read_text()).get("cleanup", {}) if record.exists() else {}
    if not cleanup.get("serviceAbsent") or not cleanup.get("processAbsent"):
        raise RuntimeError(f"native cleanup unconfirmed; retained {root} for the test owner")
    for path in output.rglob("*-native-pair.json"):
        pair = json.loads(path.read_text()).get("cleanup", {})
        if not pair.get("servicesAbsent") or not pair.get("processesAbsent"):
            raise RuntimeError(f"installed cleanup unconfirmed; retained {root} for the test owner")
    accounts = output / "accounts-cleanup.json"
    if (root / "ordered-input.json").exists() or (root / "account-run.json").exists():
        from fixtures import installed_account_run
        installed_account_run.verify_cleanup(root, output)
    if accounts.exists():
        result = json.loads(accounts.read_text())
        if not result.get("groupAbsent") or any(not item.get("absent") for item in result["accounts"] if item["created"]):
            raise RuntimeError(f"native account cleanup unconfirmed; retained {root} for the test owner")
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
