#!/usr/bin/env python3
"""Verify permitted edges with their original, digest-identified source executables."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile

from fixtures import release_publication_platform, release_tool

SPEC = importlib.util.spec_from_file_location("baselines", Path(__file__).with_name("prepare-release-baselines.py"))
BASELINES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BASELINES)
PROBE_SPEC = importlib.util.spec_from_file_location("reader_probe", Path(__file__).with_name("probe-update-reader.py"))
READER = importlib.util.module_from_spec(PROBE_SPEC)
PROBE_SPEC.loader.exec_module(READER)


def terminate_run(signum, _frame):
    raise SystemExit(128 + signum)


def verify_invocation(item, args, root, environment):
    if item["packet"] != item["sha256"] + ".reader.json":
        raise ValueError("invalid original reader packet filename")
    packet = args.packets / item["packet"]
    if hashlib.sha256(packet.read_bytes()).hexdigest() != item["sha256"]:
        raise ValueError("original reader packet bytes changed")
    artifact = item["artifact"]
    archive = BASELINES.acquire(item["source"], artifact["archive"], artifact["sha256"], args.cache)
    binary = root / item["source"]
    BASELINES.unpack(archive, artifact["binarySha256"], binary)
    capabilities = READER.probe(binary, item["source"], args.platform, environment)
    if capabilities["epoch"] != item["epoch"] or capabilities["build"]["commit"] != item["commit"]:
        raise ValueError("original source reader differs from its signed capability marker")
    log = args.output / (item["source"] + ".reader.log")
    try:
        result = subprocess.run([str(binary), "update-reader", "-input", str(packet), "-trust-cache", str(args.trust_cache)],
                                env=environment, capture_output=True, timeout=30, check=False)
    except subprocess.TimeoutExpired as error:
        log.write_bytes((error.stdout or b"") + (error.stderr or b""))
        raise
    log.write_bytes(result.stdout + result.stderr)
    result.check_returncode()
    verified = json.loads(result.stdout)
    if verified.get("schema") != 1 or verified.get("source") != capabilities["build"] or verified.get("epoch") != item["epoch"] or verified.get("position") != item["position"]:
        raise ValueError("original verifier returned another source or reader position")
    if verified.get("target") != args.target or verified.get("metadata") != item["metadata"] or verified.get("binary") != item["binary"]:
        raise ValueError("original verifier returned another target or metadata digest")
    return {"source": item["source"], "sourceDigest": artifact["binarySha256"], "packet": item["sha256"], "accepted": True}


def platform_publication(args, raw):
    if not args.publication_platform_plan:
        return None
    target = release_tool.native_target()
    if target != dict(zip(("os", "arch"), args.platform.split("/"), strict=True)):
        raise ValueError("original readers must execute on their planned native platform")
    plan = release_publication_platform.load_plan(args.publication_platform_plan, args.publication_plan_sha256)
    release_publication_platform.require_execution(plan["draft"]["run"], plan["evidenceRun"])
    readers = plan["platforms"][args.platform]["readers"]
    run, attempt = int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"])
    if hashlib.sha256(raw).hexdigest() != readers["indexSha256"]:
        raise ValueError("original reader index differs from the exact publication plan")
    return release_publication_platform.job_binding(args.platform, run, attempt, args.publication_plan_sha256)


def main():
    signal.signal(signal.SIGTERM, terminate_run)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("packets", type=Path)
    parser.add_argument("platform")
    parser.add_argument("cache", type=Path)
    parser.add_argument("trust_cache", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--publication-platform-plan", type=Path)
    parser.add_argument("--publication-plan-sha256")
    args = parser.parse_args()
    if bool(args.publication_platform_plan) != bool(args.publication_plan_sha256):
        parser.error("publication reader qualification needs a plan and its signer digest")
    raw = (args.packets / "index.json").read_bytes()
    binding = platform_publication(args, raw)
    index = json.loads(raw)
    expected = dict(zip(("os", "arch"), args.platform.split("/"), strict=True))
    if index["schema"] != 1 or index["platform"] != expected:
        raise ValueError("original reader index names another platform")
    args.target = index["target"]
    args.cache.mkdir(parents=True, exist_ok=True)
    args.output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="mur-", dir=os.environ.get("MESH_SHORT_TMP")) as temporary:
        root = Path(temporary)
        environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG") if key in os.environ}
        environment.update(HOME=str(root), MESH_STATE_DIR=str(root / "state"), MESH_CONFIG_DIR=str(root / "config"))
        results = [verify_invocation(item, args, root, environment) for item in index["invocations"]]
    outcome = {"target": index["target"], "platform": expected, "readers": results}
    if binding is not None:
        outcome["publication"] = binding
    (args.output / "original-readers.json").write_text(json.dumps(outcome, indent=2) + "\n")
    print(f"{len(results)} original {args.platform} source readers accepted their signed edges")


if __name__ == "__main__":
    main()
