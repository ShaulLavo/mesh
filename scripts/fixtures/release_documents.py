"""Logical publication selection and the supported flat legacy projection.

The trusted Go release tool owns container parsing and signature verification.
"""
import hashlib
import json
from pathlib import Path
import re
import shutil

from fixtures import fleet_baselines, release_worker_groups, release_worker_index

ROOT = Path(__file__).resolve().parents[2]
BUNDLE = "mesh-documents.tar.gz"
ENVELOPE = "mesh-update.sig.json"
FIXED = ("mesh-release.json", "mesh-update.json", "mesh-update-root.json", "mesh-retention.json",
         "mesh-workers.json", "mesh-worker-groups.json")
PHYSICAL = (BUNDLE, ENVELOPE, "mesh-update-root.json", "mesh-release.json")
LEGACY_BRIDGES = ("v0.1.255",)
RECEIPT = re.compile(r"[0-9a-f]{64}\.json\Z")


def read_document(directory, name, expected=None):
    path = directory / name
    if Path(name).name != name or "\\" in name or path.is_symlink() or not path.is_file():
        raise ValueError("publication requires a regular logical document basename")
    raw = path.read_bytes()
    if expected is not None and hashlib.sha256(raw).hexdigest() != expected:
        raise ValueError("logical document differs from its authenticated reference")
    return raw


def current_pages(directory, metadata):
    pending, seen = [metadata["page"]], set()
    while pending:
        ref = pending.pop()
        if ref["tag"] != metadata["version"] or ref["name"] in seen:
            continue
        raw = read_document(directory, ref["name"], ref["sha256"])
        seen.add(ref["name"])
        page = json.loads(raw)
        pending.extend(item["ref"] for item in page["back"])
    return seen


def members(directory):
    """Select explicit publication inputs after the existing semantic closure check."""
    manifest = json.loads(read_document(directory, "mesh-release.json"))
    metadata = json.loads(read_document(directory, "mesh-update.json"))
    index = release_worker_index.validate_index(directory / "mesh-worker-groups.json", manifest)
    selected = set(FIXED)
    selected.update(release_worker_index.closure(index, release_worker_groups.file_resolver(directory)))
    selected.update(item["proof"] + ".json" for item in manifest["compatibility"]["transitions"])
    selected.update(item["rollback"] + ".rollback.json" for item in metadata["rollback"])
    selected.update(current_pages(directory, metadata))
    for name in selected:
        read_document(directory, name)
    return sorted(selected)


def stage(dist, proofs, destination):
    """Build the logical writer input without admitting unrelated staging files."""
    destination.mkdir(exist_ok=False)
    for name in FIXED:
        shutil.copyfile(dist / name, destination / name)
    index = json.loads((dist / "mesh-worker-groups.json").read_bytes())
    copy_proofs(index, proofs, destination)
    manifest = json.loads((dist / "mesh-release.json").read_bytes())
    for item in manifest["compatibility"]["transitions"]:
        name = item["proof"] + ".json"
        shutil.copyfile(proofs / name, destination / name)
    metadata = json.loads((dist / "mesh-update.json").read_bytes())
    for item in metadata["rollback"]:
        name = item["rollback"] + ".rollback.json"
        shutil.copyfile(proofs / "rollback" / name, destination / name)
    stage_pages(dist, destination, metadata)
    return members(destination)


def copy_proofs(index, proofs, destination):
    for name in release_worker_index.closure(index, release_worker_index.resolver(proofs)):
        source = proofs / name if name.endswith(".worker-groups.json") else proofs / "groups" / "leaves" / name
        shutil.copyfile(source, destination / name)


def stage_pages(dist, destination, metadata):
    for name in current_pages(dist, metadata):
        shutil.copyfile(dist / name, destination / name)


def legacy_versions(fleet=None, policy=None, bridges=LEGACY_BRIDGES):
    fleet = fleet_baselines.load() if fleet is None else fleet
    policy = json.loads((ROOT / "scripts/upgrade-policy.json").read_bytes()) if policy is None else policy
    return sorted({host["daemon"] for host in fleet["hosts"]} | set(policy["requiredRollback"]) | set(bridges))


def historical_manifest(history, documents, version):
    matches = [item for item in history if item["tag"] == version]
    if len(matches) != 1:
        raise ValueError("legacy projection requires one authenticated historical release")
    refs = [item for item in matches[0]["assets"] if item["name"] == "mesh-release.json"]
    if len(refs) != 1:
        raise ValueError("legacy projection requires an authenticated historical manifest hash")
    raw = read_document(documents / version, "mesh-release.json", refs[0]["sha256"])
    manifest = json.loads(raw)
    if manifest["version"] != version or manifest["schema"] != 1:
        raise ValueError("legacy projection historical manifest has another identity")
    return manifest


def legacy_projection(manifest, directory, documents, history, versions=None):
    versions = legacy_versions() if versions is None else versions
    names = set()
    for version in versions:
        original = historical_manifest(history, documents, version)
        for platform in release_worker_index.PLATFORMS:
            names.add(legacy_receipt(directory, manifest, original, platform))
    return sorted(names)


def legacy_receipt(directory, manifest, original, platform):
    sources = [item for item in original["artifacts"] if item["platform"] == platform]
    targets = [item for item in manifest["artifacts"] if item["platform"] == platform]
    if len(sources) != 1 or len(targets) != 1:
        raise ValueError("legacy projection requires exactly one source and candidate artifact per platform")
    source, target = sources[0]["binarySha256"], targets[0]["binarySha256"]
    transitions = [item for item in manifest["compatibility"]["transitions"]
                   if item["platform"] == platform and item["fromDigest"] == source and item["toDigest"] == target]
    if len(transitions) != 1:
        raise ValueError("legacy projection requires one exact platform/source/candidate transition")
    name = transitions[0]["proof"] + ".json"
    raw = read_document(directory, name, transitions[0]["proof"])
    receipt = json.loads(raw)
    if receipt.get("schema") != 1 or any(receipt.get(key) != transitions[0][key] for key in ("platform", "fromDigest", "toDigest")):
        raise ValueError("legacy projection receipt differs from the canonical transition")
    return name


def selection_projection(directory, index):
    """Resolve the same union from the authenticated native selection on intake."""
    manifest = json.loads(read_document(directory, "mesh-release.json"))
    names = set()
    for version in legacy_versions():
        original = {"artifacts": [item["artifact"] for item in index["selection"]["baselines"] if item["version"] == version]}
        for platform in release_worker_index.PLATFORMS:
            names.add(legacy_receipt(directory, manifest, original, platform))
    return sorted(names)


def logical_inventory(directory):
    """Use only an already authenticated materialized directory, never API members."""
    names = members(directory)
    names.append(ENVELOPE)
    return [{"name": name, "sha256": hashlib.sha256(read_document(directory, name)).hexdigest()} for name in sorted(names)]
