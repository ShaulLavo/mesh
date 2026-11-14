#!/usr/bin/env python3
"""Diagnostic only: one original Linux AMD64 forward source, all its groups."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

from fixtures import installed_artifacts as intake, installed_storage, installed_paths
from fixtures import installed_clock, installed_releases, release_worker_groups as groups
from fixtures import linux_eperm_control


BASE = "7e96ed8e803067215450982a946a305a1ea9bb95"
REPO = Path(__file__).resolve().parents[1]
PIN = Path(__file__).with_name("fixtures") / "linux_eperm_vehicle.json"
ALLOWED = {".github/workflows/rollback-proof-investigation.yml", "scripts/linux-eperm-vehicle.py",
           "scripts/fixtures/linux_eperm_control.py", "scripts/fixtures/linux_eperm_vehicle.json",
           "scripts/test-linux-eperm-vehicle.py"}


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def load_runner():
    spec = importlib.util.spec_from_file_location("ordinary_update_proofs", REPO / "scripts/run-update-proofs.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def require_host():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise ValueError("runtime vehicle is hosted-only; use pure controls locally")
    if sys.platform != "linux" or os.uname().machine != "x86_64":
        raise ValueError("vehicle requires Linux AMD64")
    forbidden = ("MESH_WORKER_GROUP", "MESH_INSTALLED_CLOCK_HANDOFF", "MESH_WORKER_EPERM_LEDGER",
                 "MESH_TRANSITION_PROOF_MODE", "MESH_WORKER_REGISTRY")
    if any(name in os.environ for name in forbidden):
        raise ValueError("vehicle cannot inherit a selected group, profile, clock or ledger")


def verify_ref(expected, actual, changed):
    if not re.fullmatch(r"[0-9a-f]{40}", expected) or actual != expected:
        raise ValueError("actual driver checkout differs from its explicit source pin")
    if set(changed).difference(ALLOWED):
        raise ValueError("driver differs from corrected harness outside the reviewed vehicle")


def source_pin(expected, output):
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=REPO, text=True).strip()
    subprocess.run(["git", "merge-base", "--is-ancestor", BASE, actual], cwd=REPO, check=True, timeout=10)
    changed = subprocess.check_output(["git", "diff", "--name-only", BASE, actual], cwd=REPO, text=True).splitlines()
    verify_ref(expected, actual, changed)
    intake.verify_harness(expected, output / "harness-source.json")
    write(output / "composition.json", {"correctedHarnessBase": BASE, "actualDriverSource": actual, "changed": changed})


def live_pin(expected):
    value = intake.verify_set("ShaulLavo/mesh", expected["run"], expected["source"], "update-native")
    artifacts = intake.pages(f"repos/ShaulLavo/mesh/actions/runs/{expected['run']}/artifacts", "artifacts")
    matches = [item for item in artifacts if item["name"] == "update-rollback-manifest" and not item["expired"]]
    if len(matches) != 1:
        raise ValueError("original manifest artifact is missing or duplicated")
    item = matches[0]
    value["artifacts"]["rollback-manifest"] = {"id": item["id"], "name": item["name"],
        "size": item["size_in_bytes"], "digest": item.get("digest")}
    verify_live_pin(value, expected)
    return value


def verify_live_pin(value, expected):
    if any(value[key] != expected[key] for key in ("run", "source", "runHead", "artifacts")):
        raise ValueError("live complete producer set differs from the original pinned metadata")


def acquire(root, expected):
    write(root / "producer-pin.json", live_pin(expected))
    native, manifest = root / "native.noindex", root / "manifest.noindex"
    native.mkdir()
    manifest.mkdir()
    for key, destination in (("linux-amd64", native), ("rollback-manifest", manifest)):
        installed_storage.preflight("ShaulLavo/mesh", root / "producer-pin.json", key, destination, root / (key + ".space.json"))
        intake.download("ShaulLavo/mesh", root / "producer-pin.json", key, destination, root / (key + ".download.json"))
    shutil.copyfile(manifest / "mesh-release-context.json", native / "mesh-release-context.json")
    intake.verify_native(native, expected["actualPreparationSource"], "linux/amd64")
    verify_manifest(native, expected)
    for name in expected["preparation"]["sha256"]:
        (native / "tools" / name).chmod(0o755)
    for binary in (native / "retained").rglob("mesh"):
        binary.chmod(0o755)
    return native


def verify_manifest(native, expected):
    manifest = json.loads((native / "mesh-release-context.json").read_bytes())
    record = json.loads((native / "tools/rollback-artifact.json").read_bytes())
    if manifest["commit"] != expected["actualPreparationSource"] or manifest["version"] != expected["candidateReader"]["build"]["version"]:
        raise ValueError("original manifest source or version differs")
    if record["build"] != expected["candidateReader"]["build"]:
        raise ValueError("original native artifact metadata differs from reader")
    artifacts = manifest["artifacts"]
    if len(artifacts) != 3 or {item["platform"]["os"] + "/" + item["platform"]["arch"] for item in artifacts} != {"linux/amd64", "linux/arm64", "darwin/arm64"}:
        raise ValueError("original manifest platform coverage differs")
    native_artifacts = [item for item in artifacts if item["platform"] == {"os": "linux", "arch": "amd64"}]
    if native_artifacts != [record["artifact"]]:
        raise ValueError("original manifest artifact differs from actual native candidate")


def describe_groups(partition):
    return [{"index": group["index"], "members": len(group["members"]), "imageBytes": group["imageBytes"],
             "sha256": groups.digest(groups.encode(groups.public_group(group)))} for group in partition]


def verify_selection(evidence, source_digest, source_bytes, partition, expected):
    if evidence["version"] != expected["sourceVersion"] or evidence.get("rollback") is not False:
        raise ValueError("exact single source must retain its original forward-only profile")
    if source_digest != expected["sourceDigest"] or source_bytes != expected["sourceBytes"]:
        raise ValueError("original source executable changed")
    if describe_groups(partition) != expected["groups"]:
        raise ValueError("original complete worker partition changed")


def select(native, root, expected):
    registry = native / "retained/workers.json"
    if groups.digest(registry.read_bytes()) != expected["registrySha256"]:
        raise ValueError("original worker registry changed")
    for name, key in (("preparation.json", "preparation"), ("candidate-reader.json", "candidateReader")):
        if json.loads((native / "tools" / name).read_bytes()) != expected[key]:
            raise ValueError("original candidate reader or tool preparation changed")
    sources = [(binary, evidence) for binary, evidence in load_runner().read_baselines(native / "retained.list")
               if evidence["version"] == expected["sourceVersion"]]
    if len(sources) != 1:
        raise ValueError("original retained list must contain exactly one selected source")
    binary, evidence = sources[0]
    partition = groups.partition(groups.freeze(registry, evidence["version"], binary), binary.stat().st_size)
    verify_selection(evidence, groups.digest(binary.read_bytes()), binary.stat().st_size, partition, expected)
    listing = native / "eperm-selected.list"
    listing.write_text(str(binary.relative_to(native)) + "\n")
    write(root / "selection.json", {"source": evidence, "groups": [groups.public_group(group) for group in partition],
                                   "profile": "forward-only", "ordinaryMatrixExpanded": False})
    return listing


def proof_command(native, listing, output):
    return [sys.executable, str(REPO / "scripts/run-update-proofs.py"), str(native / "tools"), str(listing),
            "linux/amd64", str(output), "--shard", "0", "--shards", "1", "--manifest", str(native / "mesh-release-context.json")]


def run_proof(root, native, listing, state, expected_source):
    output = root / "proof"
    output.mkdir()
    environment = dict(os.environ, MESH_WORKER_EPERM_LEDGER=str(root / "eperm-ledger.json"),
                       MESH_HARNESS_SOURCE=expected_source, TMPDIR=state["scratch"], MESH_SHORT_TMP=state["scratch"])
    with (root / "eperm-ledger.json").open("x") as ledger:
        json.dump({"schema": 1, "cap": 2, "events": []}, ledger)
    command = proof_command(native, listing, output)
    state.update(launchAttempted=True, nativeCleanupConfirmed=False)
    write(root / "state.json", state)
    # The unchanged runner retains its separate 600-second probe and source-proof limits.
    deadline = time.monotonic_ns() + 1220 * installed_clock.SECOND
    ownership = None
    with (root / "proof.log").open("wb") as log:
        process = subprocess.Popen(command, cwd=REPO, env=environment, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            ownership = installed_clock.NativeOwnership(process, state["scratch"], output, deadline)
            installed_releases.wait_transition(process, command, deadline, ownership)
            state["nativeCleanupConfirmed"] = True
        except BaseException:
            installed_releases.stop_transition(process, ownership=ownership)
            state["nativeCleanupConfirmed"] = True
            raise
        finally:
            retain_ownership(ownership, root, state)


def retain_ownership(ownership, root, state):
    if ownership is not None:
        ownership.save()
    write(root / "state.json", state)


def counter(root):
    ledger = root / "eperm-ledger.json"
    value = json.loads(ledger.read_bytes()) if ledger.exists() else {"events": []}
    write(root / "counter.json", {"events": len(value["events"]), "cap": 2,
                                 "diagnosticOnly": True, "acceptanceEstablished": False})


def diagnostic_cleanup(root, state):
    path = root / "eperm-ledger.json"
    if not state.get("launchAttempted") and not path.exists():
        return {"confirmed": True, "events": 0}
    try:
        value = json.loads(path.read_bytes())
        if value.get("schema") != 1 or value.get("cap") != 2 or not isinstance(value.get("events"), list):
            raise ValueError("invalid diagnostic ownership ledger")
        snapshots = [snapshot_cleanup(event["failure"]["snapshot"]) for event in value["events"]]
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        return {"confirmed": False, "error": type(error).__name__ + ": " + str(error)}
    return {"confirmed": all(row["confirmed"] for row in snapshots), "events": len(snapshots), "snapshots": snapshots}


def snapshot_cleanup(snapshot):
    commands = [name for name in ("kernelJournal", "kernelDmesg", "hostedSudoKernelJournal") if name in snapshot]
    records = [{"command": name, "cleanup": snapshot[name].get("cleanup", {})} for name in commands]
    confirmed = snapshot.get("cleanupConfirmed") is True and "kernelJournal" in commands and all(
        row["cleanup"].get("reaped") is True and row["cleanup"].get("stdoutClosed") is True for row in records)
    return {"confirmed": confirmed, "commands": records}


def removable(state, control, diagnostic):
    return (not state.get("controlAttempted") or control.get("cleanupConfirmed") is True) and (
        not state.get("launchAttempted") or state.get("nativeCleanupConfirmed") is True) and diagnostic["confirmed"] is True


def cleanup(root):
    state_path, control_path = root / "state.json", root / "positive-control.json"
    try:
        state = json.loads(state_path.read_bytes()) if state_path.exists() else {}
        control = json.loads(control_path.read_bytes()) if control_path.exists() else {}
    except (OSError, ValueError) as error:
        write(root / "cleanup.json", {"payloadsMayBeRemoved": False, "reason": "unreadable ownership evidence; preserve payloads"})
        raise RuntimeError("ownership evidence unreadable; payloads preserved") from error
    diagnostic = diagnostic_cleanup(root, state)
    allowed = removable(state, control, diagnostic)
    paths = [root / "native.noindex", root / "manifest.noindex"]
    if not state and any(path.exists() for path in paths):
        allowed = False
    if any(path.is_symlink() for path in paths):
        allowed = False
    if state.get("scratch"):
        short = Path(state["scratch"])
        if short.parent != Path("/tmp") or not re.fullmatch(r"me-[a-z0-9_]+\.noindex", short.name) or short.is_symlink():
            raise RuntimeError("cleanup scratch ownership path differs; preserve payloads")
        if short.exists() and short.stat().st_uid != os.getuid():
            raise RuntimeError("cleanup scratch owner differs; preserve payloads")
        paths.append(short)
    write(root / "cleanup.json", {"payloadsMayBeRemoved": allowed, "paths": list(map(str, paths)), "diagnosticCleanup": diagnostic,
                                "reason": "owned resources absent or never launched" if allowed else "ownership unconfirmed; preserve payloads"})
    if not allowed:
        raise RuntimeError("ownership unconfirmed; payloads preserved")
    for path in paths:
        if path.exists():
            shutil.rmtree(path)
    write(root / "cleanup.json", {"payloadsMayBeRemoved": True, "payloadsAbsent": all(not path.exists() for path in paths),
                                "diagnosticCleanup": diagnostic,
                                "removed": list(map(str, paths))})


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


def execute(root, expected_source, sudo):
    require_host()
    source_pin(expected_source, root)
    expected = json.loads(PIN.read_bytes())
    state = {"controlAttempted": False, "launchAttempted": False, "nativeCleanupConfirmed": False}
    write(root / "state.json", state)
    try:
        state["controlAttempted"] = True
        write(root / "state.json", state)
        os.environ["MESH_WORKER_EPERM_SUDO"] = "1" if sudo else "0"
        linux_eperm_control.positive_control(root / "positive-control.json")
        short = Path(tempfile.mkdtemp(prefix="me-", suffix=".noindex", dir="/tmp"))
        state["scratch"] = str(short)
        write(root / "state.json", state)
        installed_paths.validate(short)
        os.environ["MESH_SHORT_TMP"] = str(short)
        native = acquire(root, expected)
        listing = select(native, root, expected)
        run_proof(root, native, listing, state, expected_source)
    except BaseException as error:
        write(root / "failure.json", {"type": type(error).__name__, "message": str(error), "diagnosticOnly": True})
        raise
    finally:
        try:
            counter(root)
        finally:
            cleanup(root)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("run", "cleanup"))
    parser.add_argument("root", type=Path)
    parser.add_argument("--driver-source")
    parser.add_argument("--hosted-sudo-kernel-journal", action="store_true")
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, terminate)
    signal.signal(signal.SIGINT, terminate)
    root = args.root.resolve(strict=True)
    if args.operation == "cleanup":
        cleanup(root)
        return
    execute(root, args.driver_source or "", args.hosted_sudo_kernel_journal)


if __name__ == "__main__":
    main()
