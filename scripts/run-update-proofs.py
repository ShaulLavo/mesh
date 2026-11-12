#!/usr/bin/env python3
"""Run one native proof shard over the required, digest-verified baseline list."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import time


def run(command, environment, log):
    with log.open("wb") as stream, subprocess.Popen(command, env=environment, stdout=stream,
            stderr=subprocess.STDOUT, start_new_session=True) as process:
        try:
            status = process.wait(timeout=600)
        except BaseException:
            stop_group(process)
            raise
    if status:
        raise RuntimeError(f"native proof failed ({status}), see {log}")


def stop_group(process):
    if process.poll() is not None:
        return
    os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()


def terminate_run(signum, _frame):
    raise SystemExit(128 + signum)


def read_baselines(path):
    sources = []
    for line in path.read_text().splitlines():
        binary = Path(line)
        if not binary.is_absolute():
            binary = path.parent / binary
        source = binary.parent / "source.json"
        evidence = json.loads(source.read_bytes()) if source.exists() else {"version": binary.name, "rollback": True}
        sources.append((binary, evidence))
    return sources


def main():
    signal.signal(signal.SIGTERM, terminate_run)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tools", type=Path)
    parser.add_argument("baselines", type=Path)
    parser.add_argument("platform")
    parser.add_argument("output", type=Path)
    parser.add_argument("--shard", type=int, default=0)
    parser.add_argument("--shards", type=int, default=1)
    parser.add_argument("--manifest", type=Path, required=True)
    args = parser.parse_args()
    if not 0 <= args.shard < args.shards:
        parser.error("invalid shard position")
    sources = read_baselines(args.baselines)
    if sum(int(evidence["rollback"]) for _, evidence in sources) > args.shards:
        raise ValueError("too few shards for the required full sources")
    root = Path(__file__).resolve().parent.parent
    args.output.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(args.tools / "candidate-reader.json", args.output / "candidate-reader.json")
    environment = dict(os.environ, MESH_BOUNDARY_TOOL=str(args.tools / "boundary"),
                       MESH_STARTUP_FAULT_BINARY=str(args.tools / "startup-fault-candidate"),
                       MESH_WORKER_PROBE=str(args.tools / "worker-probe"),
                       MESH_ROLLBACK_MANIFEST=str(args.manifest.resolve()),
                       MESH_WORKER_REGISTRY=str(args.baselines.parent / "retained" / "workers.json"))
    if not Path(environment["MESH_WORKER_REGISTRY"]).is_file():
        raise ValueError("native proof requires the complete exact worker registry")
    results = []
    full_index, forward_index = 0, 0
    for binary, evidence in sources:
        assignment = full_index if evidence["rollback"] else forward_index
        full_index += int(evidence["rollback"])
        forward_index += int(not evidence["rollback"])
        # Diagnostic branch only: run one named source, whatever its shard.
        only = os.environ.get("MESH_DIAG_ONLY_SOURCE")
        if only and evidence["version"] != only:
            continue
        if not only and assignment % args.shards != args.shard:
            continue
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        expected = evidence.get("artifact", {}).get("binarySha256")
        if not expected:
            match = re.fullmatch(r"baseline_[a-z0-9_]+_([0-9a-f]{64})\.bin", binary.name)
            expected = match[1] if match else None
        if digest != expected:
            raise ValueError(f"required native source executable changed: {binary.name}")
        binary.chmod(0o755)
        label = evidence["version"]
        environment["MESH_LEGACY_BINARY"] = str(binary)
        probe = args.output / f"{label}.worker.log"
        run([str(args.tools / "worker-probe"), "-test.v", "-test.run=^TestLegacyWorkerProcessProtocolAndVersion$"], environment, probe)
        if "--- PASS: TestLegacyWorkerProcessProtocolAndVersion" not in probe.read_text():
            raise RuntimeError(f"original worker control did not run: {label}")
        environment["MESH_TRANSITION_PROOF_MODE"] = "full" if evidence["rollback"] else "forward-only"
        start = time.monotonic()
        run([str(root / "scripts/prove-release-transition.sh"), str(binary), str(args.tools / "candidate"),
             args.platform, str(args.output / "proofs")], environment, args.output / f"{label}.transition.log")
        elapsed = time.monotonic() - start
        results.append({"source": label, "sourceDigest": digest, "seconds": elapsed, "rollback": evidence["rollback"]})
        print(f"{label} {args.platform}: {elapsed:.2f} seconds; rollback={evidence['rollback']}", flush=True)
    (args.output / "timings.json").write_text(json.dumps(results, indent=2) + "\n")


if __name__ == "__main__":
    main()
