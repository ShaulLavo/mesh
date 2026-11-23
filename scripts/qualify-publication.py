#!/usr/bin/env python3
"""Acquire exact artifacts from a trusted main release for read-only replay."""
import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from fixtures import draft_intake, installed_artifacts, installed_storage, release_publication_platform as platforms
from fixtures import release_tool, release_worker_groups as groups

REPO = "ShaulLavo/mesh"
ACTIONS = f"repos/{REPO}/actions"
DRAFT = "release-signed-draft"
TRUST = "update-publication-trust"
INPUT = "update-publication-input"
POLICY = "update-policy"
NATIVES = {native: "update-native-" + platforms.platform_key(native) for native in platforms.PLATFORMS}
TOOLS = {native: "release-tool-source-" + platforms.platform_key(native) for native in platforms.PLATFORMS}
SIGN = "Sign complete release draft"
PRODUCERS = {DRAFT: (SIGN,), TRUST: (SIGN,), INPUT: (SIGN,), POLICY: ("policy",)}
PRODUCERS.update({name: (f"prepare ({native},",) for native, name in NATIVES.items()})
PRODUCERS.update({name: (SIGN, "policy" if native == "linux/amd64" else f"prepare ({native},")
                  for native, name in TOOLS.items()})


def load_spec(path):
    spec = json.loads(platforms.read(path))
    if set(spec) != {"mode", "run", "release", "artifacts"} or type(spec["run"]) is not int or spec["run"] <= 0:
        raise ValueError("qualification requires an exact writer run and release")
    release = spec["release"]
    if (set(release) != {"version", "source", "id"} or type(release["id"]) is not int or release["id"] <= 0
            or not re.fullmatch(r"v\d+\.\d+\.\d+", release["version"]) or not re.fullmatch(r"[0-9a-f]{40}", release["source"])):
        raise ValueError("qualification requires an exact release ID, version and source")
    pins = spec["artifacts"]
    expected = expected_pins(spec["mode"])
    if set(pins) != expected or len({pin["id"] for pin in pins.values()}) != len(expected):
        raise ValueError("qualification requires every distinct archive for its selected coverage")
    for pin in pins.values():
        if set(pin) != {"id", "size", "digest", "attempt"}:
            raise ValueError("qualification archive pin must contain its ID, size, SHA-256 and producer attempt")
        installed_artifacts.require_archive_pin(pin)
        platforms.attempt_number(pin["attempt"])
    if len({pins[name]["attempt"] for name in (DRAFT, INPUT, TRUST)}) != 1:
        raise ValueError("signed draft, trust and publication input must come from one signing attempt")
    return spec


def expected_pins(mode):
    if mode == "native":
        return set(PRODUCERS)
    if mode == "readers":
        return set(PRODUCERS) - set(NATIVES.values())
    raise ValueError("qualification mode must be native or readers")


def successful_producer(name, jobs, attempt, created):
    matching = [job for job in jobs if matches_producer(job["name"], PRODUCERS[name])
                and job["status"] == "completed" and job["conclusion"] == "success" and job["run_attempt"] == attempt
                and timestamp(job["started_at"]) <= timestamp(created) <= timestamp(job["completed_at"])]
    if len(matching) != 1:
        raise ValueError(f"{name} was not uploaded during its unique successful producer job in attempt {attempt}")


def matches_producer(name, expected):
    leaf = name.rsplit(" / ", 1)[-1]
    return any(leaf.startswith(value) if value.endswith(",") else leaf == value for value in expected)


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


ALLOWED_RUN_SOURCES = {
    (".github/workflows/ci.yml", "push"),
    (".github/workflows/release.yml", "workflow_dispatch"),
}


def check_run(spec):
    run, release = spec["run"], spec["release"]
    metadata = installed_artifacts.api(f"{ACTIONS}/runs/{run}")
    if (metadata["head_repository"]["full_name"] != REPO or metadata["head_branch"] != "main"
            or metadata["head_sha"] != release["source"]
            or (metadata["path"], metadata["event"]) not in ALLOWED_RUN_SOURCES):
        raise ValueError("qualification writer must be an exact main publication run in ShaulLavo/mesh")
    if int(os.environ["GITHUB_RUN_ID"]) == run:
        raise ValueError("qualification requires a separate read-only evidence run")


def check_artifacts(spec):
    run, pins = spec["run"], spec["artifacts"]
    jobs = {attempt: installed_artifacts.pages(f"{ACTIONS}/runs/{run}/attempts/{attempt}/jobs", "jobs")
            for attempt in {pin["attempt"] for pin in pins.values()}}
    for name, pin in pins.items():
        artifact = installed_artifacts.api(f"{ACTIONS}/artifacts/{pin['id']}")
        check_artifact(name, pin, run, artifact)
        successful_producer(name, jobs[pin["attempt"]], pin["attempt"], artifact["created_at"])


def check_artifact(name, pin, run, artifact):
    if (artifact["id"], artifact["name"], artifact["size_in_bytes"], artifact["digest"], artifact["expired"],
            installed_artifacts.artifact_run(artifact)) != (pin["id"], name, pin["size"], pin["digest"], False, run):
        raise ValueError(f"{name} differs from its pinned live writer archive")


def fetch(spec, name, destination, *, native=False):
    import importlib
    extract = importlib.import_module("publication-workflow").extract
    pin = spec["artifacts"][name]
    if native:
        native_preflight(spec, name, destination, pin)
    with tempfile.TemporaryDirectory(prefix="qualification-zip-", dir=destination.parent) as temp:
        archive = Path(temp) / "payload.zip"
        installed_artifacts.fetch_archive(installed_storage.archive_url(REPO, pin["id"]), archive, pin)
        if native:
            installed_artifacts.extract_archive(archive, destination)
        else:
            extract(archive, destination)


def native_preflight(spec, name, destination, pin):
    key = name.removeprefix("update-native-")
    with tempfile.TemporaryDirectory(prefix="qualification-storage-", dir=destination.parent) as temp:
        path = Path(temp) / "pin.json"
        path.write_text(json.dumps({"run": spec["run"], "artifacts": {key: pin}}))
        installed_storage.preflight(REPO, path, key, destination, Path(temp) / "storage.json")
        print((Path(temp) / "storage.json").read_text())


def confirm_unpublished(version, run):
    jobs = installed_artifacts.pages(f"{ACTIONS}/runs/{run}/jobs?filter=all", "jobs")
    writer = installed_artifacts.api(f"{ACTIONS}/runs/{run}")
    if writer.get("status") != "completed":
        raise ValueError("qualification requires a completed draft writer")
    attempt = platforms.attempt_number(writer.get("run_attempt"))
    finalizers = [job for job in jobs if matches_producer(job["name"], ("Publish verified release",))]
    if any(job.get("status") != "completed" or job.get("conclusion") != "skipped" for job in finalizers):
        raise ValueError("the draft writer has a publication finalizer that may have started")
    if sum(job.get("run_attempt") == attempt for job in finalizers) != 1:
        raise ValueError("qualification requires one completed skipped finalizer for the current writer attempt")

    # Drafts are absent from this endpoint for an Actions token with contents: read.
    result = subprocess.run(["gh", "api", "--include", f"repos/{REPO}/releases/tags/{version}"],
                            capture_output=True, timeout=30, check=False)
    if result.returncode == 0:
        raise ValueError("qualification requires a release that has never been published")
    if not re.match(rb"\AHTTP/\d(?:\.\d)? 404(?:[ \t][^\r\n]*)?\r?\n", result.stdout):
        raise ValueError("could not establish that the draft release tag has never been published")


def check_snapshot(spec, snapshot):
    release = spec["release"]
    recorded = draft_intake.verify(snapshot, release["version"], release["source"], signed=True, run=spec["run"])
    if recorded["id"] != release["id"]:
        raise ValueError("the signed draft snapshot differs from the pinned release ID")
    confirm_unpublished(release["version"], spec["run"])


def prepare(spec, root, harness):
    check_run(spec)
    check_artifacts(spec)
    release, run = spec["release"], spec["run"]
    snapshot = root / "draft" / release["version"]
    logical = reader_inputs(spec, root, "linux/amd64")
    packets = root / "packets"
    run_packets(root, logical.parent, platforms.PLATFORMS)
    plan = platforms.prepare_plan(logical, snapshot, harness, packets, run, int(os.environ["GITHUB_RUN_ID"]))
    raw = groups.encode(plan)
    if len(raw) > groups.MAX_DOCUMENT:
        raise ValueError("qualification plan exceeds the document bound")
    (root / "plan.json").write_bytes(raw)
    with Path(os.environ["GITHUB_OUTPUT"]).open("a") as output:
        output.write(f"plan_sha={groups.digest(raw)}\ncoverage={spec['mode']}\n")


def reader_inputs(spec, root, native):
    release, run = spec["release"], spec["run"]
    snapshot = draft_inputs(spec, root)
    fetch(spec, INPUT, root / "input")
    fetch(spec, POLICY, root / "policy")
    tool = root / "source-tool"
    release_tool.download(REPO, run, release["source"], "source", platforms.platform_key(native), tool,
                          spec["artifacts"][TOOLS[native]]["id"])
    os.environ.update(MESH_RELEASE_TOOL=str(tool / "mesh-release-sign"), MESH_RELEASE_TOOL_SOURCE=release["source"],
                      MESH_RELEASE_TOOL_RUN=str(run), MESH_RELEASE_TOOL_ROLE="source", MESH_RELEASE_TRUST_CACHE=str(root / "trust"))
    documents = root / "policy" / "documents"
    logical = documents / release["version"]
    draft_intake.documents(snapshot, release["version"], release["source"], logical, run)
    return logical


def run_packets(root, documents, targets):
    packets = root / "packets"
    packets.mkdir()
    for native in targets:
        subprocess.run([str(root / "source-tool/mesh-release-routes"), "-operation", "packets", "-input",
                        str(root / "input/input.json"), "-documents", str(documents), "-platform", native,
                        "-output-dir", str(packets / platforms.platform_key(native))],
                       check=True, timeout=180)


def draft_inputs(spec, root):
    parent = root / "draft"
    parent.mkdir()
    snapshot = parent / spec["release"]["version"]
    fetch(spec, DRAFT, snapshot)
    check_snapshot(spec, snapshot)
    fetch(spec, TRUST, root / "trust")
    return snapshot


def fetch_evidence(root, handoff):
    if set(handoff) != set(platforms.PLATFORMS):
        raise ValueError("qualification requires exactly three evidence handoffs")
    run = int(os.environ["GITHUB_RUN_ID"])
    attempts, pins = {}, {}
    for native, value in handoff.items():
        ident, attempt = parse_handoff(value)
        name = platforms.artifact_name(native, attempt)
        artifact = installed_artifacts.api(f"{ACTIONS}/artifacts/{ident}")
        pin = {"id": ident, "size": artifact["size_in_bytes"], "digest": artifact["digest"]}
        check_artifact(name, pin, run, artifact)
        installed_artifacts.require_archive_pin(pin)
        pins[name], attempts[native] = pin, attempt
    if len({pin["id"] for pin in pins.values()}) != len(platforms.PLATFORMS):
        raise ValueError("qualification has duplicate evidence artifacts")
    for name in pins:
        fetch({"artifacts": pins}, name, root / name)
    print(json.dumps(attempts))


def parse_handoff(value):
    if not re.fullmatch(r"[1-9][0-9]*:[1-9][0-9]*", value):
        raise ValueError("qualification expects its producer's exact artifact ID and attempt")
    ident, attempt = map(int, value.split(":"))
    if attempt > int(os.environ["GITHUB_RUN_ATTEMPT"]):
        raise ValueError("qualification evidence cannot come from a newer attempt")
    return ident, attempt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("spec", type=Path)
    parser.add_argument("operation", choices=("prepare", "reader-inputs", "native-inputs", "verify-inputs", "evidence"))
    parser.add_argument("root", type=Path)
    parser.add_argument("target", nargs="?")
    args = parser.parse_args()
    platforms.require_qualification()
    spec, root = load_spec(args.spec), args.root.resolve(strict=True)
    if args.operation == "evidence":
        fetch_evidence(root, json.loads(args.target))
        return
    if args.operation == "prepare":
        prepare(spec, root, args.target)
        return
    if args.operation == "verify-inputs":
        draft_inputs(spec, root)
        return
    logical = reader_inputs(spec, root, args.target)
    run_packets(root, logical.parent, (args.target,))
    if args.operation == "native-inputs":
        fetch(spec, NATIVES[args.target], root / "native.noindex", native=True)


if __name__ == "__main__":
    main()
