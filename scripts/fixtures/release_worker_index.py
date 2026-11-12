"""Exact native proof matrix and signed-release worker evidence closure."""
import json
from pathlib import Path

from fixtures import release_worker_groups as groups, release_workers

PLATFORMS = ({"os": "linux", "arch": "amd64"}, {"os": "linux", "arch": "arm64"}, {"os": "darwin", "arch": "arm64"})


def key(platform, source, profile):
    return platform["os"] + "/" + platform["arch"] + ":" + source + ":" + profile


def matrix(selection):
    expected = {}
    for item in selection["baselines"]:
        identity = key(item["artifact"]["platform"], item["artifact"]["binarySha256"], "full" if item["rollback"] else "forward-only")
        if identity in expected:
            raise ValueError("frozen selection duplicates a native matrix entry")
        expected[identity] = item
    if not expected or {item["artifact"]["platform"]["os"] + "/" + item["artifact"]["platform"]["arch"] for item in expected.values()} != {"linux/amd64", "linux/arm64", "darwin/arm64"}:
        raise ValueError("frozen native matrix lacks a required platform")
    versions = {item["version"] for item in selection["baselines"]}
    if "sources" in selection and set(selection["sources"]) != versions:
        raise ValueError("frozen source selection and baseline matrix differ")
    if len(expected) != len(versions) * len(PLATFORMS):
        raise ValueError("frozen source selection lacks a platform/profile")
    return expected


def reference(path):
    data = path.read_bytes()
    return {"name": path.name, "sha256": groups.digest(data), "size": len(data)}


def authoritative_selection(selection, companion):
    context = companion["context"]
    selected = [groups.member(item) for item in selection["workers"] if item["artifact"]["platform"] == context["platform"]
                and release_workers.version(item["version"]) <= release_workers.version(context["sourceVersion"])]
    if not any(item["artifact"]["binarySha256"] == context["sourceDigest"] for item in selected):
        selected.append({"version": context["sourceVersion"], "protocol": context["sourceProtocol"],
                         "artifact": {"platform": context["platform"], "binarySha256": context["sourceDigest"]}})
    actual = [item for entry in companion["groups"] for item in entry["group"]["members"]]
    if [groups.member(item) for item in actual] != selected:
        raise ValueError("companion dropped, duplicated or changed an authoritative worker member")
    return [item | {"imageBytes": observed["imageBytes"]} for item, observed in zip(selected, actual, strict=True)]


def validate_sources(selection, references, resolver, expected_source):
    expected = matrix(selection)
    seen, verified = set(), []
    for ref in references:
        companion = groups.resolve_asset(ref, resolver, "worker-groups")
        context = companion["context"]
        identity = key(context["platform"], context["sourceDigest"], context["profile"])
        if identity not in expected or identity in seen or context["sourceVersion"] != expected[identity]["version"]:
            raise ValueError("unexpected, duplicate or wrong-version native source/profile")
        seen.add(identity)
        if context["harnessSource"] != expected_source or context["preparationSource"] != expected_source:
            raise ValueError("native worker evidence came from another harness or preparation source")
        selected = authoritative_selection(selection, companion)
        result = groups.validate_group_assets(context, selected, ref, resolver)
        verified.append((ref, result))
    if seen != set(expected):
        raise ValueError("native source/platform/profile matrix is incomplete")
    return verified


def validate_registry(selection, registries, contexts):
    expected_platforms = {json.dumps(platform, sort_keys=True) for platform in PLATFORMS}
    if set(registries) != expected_platforms:
        raise ValueError("native registry matrix is incomplete")
    for platform, recorded in registries.items():
        raw = recorded["bytes"].encode()
        if len(raw) > 8 << 20 or groups.digest(raw) != recorded["sha256"]:
            raise ValueError("native registry bytes changed")
        registry = json.loads(raw)
        selected = [groups.member(item) for item in selection["workers"] if json.dumps(item["artifact"]["platform"], sort_keys=True) == platform]
        if registry["schema"] != 1 or json.dumps(registry["platform"], sort_keys=True) != platform or [groups.member(item) for item in registry["workers"]] != selected:
            raise ValueError("native registry differs from frozen authoritative workers")
        if any(context["registrySha256"] != recorded["sha256"] for context in contexts if json.dumps(context["platform"], sort_keys=True) == platform):
            raise ValueError("source proof used another native registry")


def resolver(directory):
    def resolve(name):
        path = directory / name if name.endswith(".worker-groups.json") else directory / "groups" / "leaves" / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > groups.MAX_DOCUMENT:
            raise ValueError("required group asset bytes missing or oversized")
        return path.read_bytes()
    return resolve


def collect_validation(shards, proofs, selection, expected_source):
    refs = [reference(path) for path in sorted(proofs.glob("*.worker-groups.json"))]
    verified = validate_sources(selection, refs, resolver(proofs), expected_source)
    registries = {}
    for path in sorted(shards.glob("*/worker-registry.json")):
        raw = path.read_bytes()
        value = json.loads(raw)
        platform = json.dumps(value["platform"], sort_keys=True)
        record = {"bytes": raw.decode(), "sha256": groups.digest(raw)}
        if platform in registries and registries[platform] != record:
            raise ValueError("native shards disagree on their frozen registry")
        registries[platform] = record
    contexts = [result["companion"]["context"] for _, result in verified]
    validate_registry(selection, registries, contexts)
    validate_canonical(proofs, verified)
    return {"selection": selection, "sources": refs, "registries": registries}


def validate_canonical(proofs, verified, flat=False):
    forwards, rollbacks = set(), set()
    for _, result in verified:
        forward = result["forward"]
        name = groups.digest(forward) + ".json"
        forwards.add(name)
        if (proofs / name).read_bytes() != forward:
            raise ValueError("canonical forward is not an actual byte-identical group receipt")
        if result["rollback"] is None:
            continue
        raw = groups.encode(result["rollback"])
        name = groups.digest(raw) + ".rollback.json"
        rollbacks.add(name)
        path = (proofs if flat else proofs / "rollback") / name
        if path.read_bytes() != raw:
            raise ValueError("canonical rollback differs from validated union/conjunction")
    actual_forward = {path.name for path in proofs.glob("*.json") if groups.RECEIPT.fullmatch(path.name)}
    actual_rollback = {path.name for path in (proofs if flat else proofs / "rollback").glob("*.rollback.json")}
    if actual_forward != forwards or actual_rollback != rollbacks:
        raise ValueError("canonical proof matrix contains extra or missing assets")


def stage(dist, proofs, frozen, source):
    manifest_bytes = (dist / "mesh-release.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    if manifest["commit"] != source:
        raise ValueError("worker index release source differs from manifest")
    value = {"schema": 1, "context": {"version": manifest["version"], "releaseSource": source,
             "manifestSha256": groups.digest(manifest_bytes), "candidates": manifest["artifacts"],
             "preparationSource": source, "harnessSource": source,
             "registrySha256": {key: item["sha256"] for key, item in frozen["registries"].items()}}, **frozen}
    data = groups.encode(value)
    if len(data) > groups.MAX_DOCUMENT:
        raise ValueError("worker index exceeds document bound")
    (dist / "mesh-worker-groups.json").write_bytes(data)
    validate_index(dist / "mesh-worker-groups.json", manifest, proofs)
    return value


def validate_index(path, manifest, proofs=None):
    path = Path(path)
    if not path.is_file() or path.stat().st_size > groups.MAX_DOCUMENT:
        raise ValueError("required signed worker index missing or oversized")
    value = json.loads(path.read_bytes())
    context = {"version": manifest["version"], "releaseSource": manifest["commit"],
               "manifestSha256": groups.digest((path.parent / "mesh-release.json").read_bytes()), "candidates": manifest["artifacts"],
               "preparationSource": manifest["commit"], "harnessSource": manifest["commit"],
               "registrySha256": {key: item["sha256"] for key, item in value["registries"].items()}}
    if value.get("schema") != 1 or value["context"] != context:
        raise ValueError("worker index has a wrong release, manifest or candidate context")
    resolve = resolver(proofs) if proofs is not None else groups.file_resolver(path.parent)
    verified = validate_sources(value["selection"], value["sources"], resolve, manifest["commit"])
    contexts = [result["companion"]["context"] for _, result in verified]
    validate_registry(value["selection"], value["registries"], contexts)
    transitions = {(json.dumps(item["platform"], sort_keys=True), item["fromDigest"], item["toDigest"], item["proof"]) for item in manifest["compatibility"]["transitions"]}
    expected = set()
    for _, result in verified:
        forward = json.loads(result["forward"])
        expected.add((json.dumps(forward["platform"], sort_keys=True), forward["fromDigest"], forward["toDigest"], groups.digest(result["forward"])))
        candidate = next(item for item in manifest["artifacts"] if item["platform"] == forward["platform"])
        if candidate["binarySha256"] != forward["toDigest"]:
            raise ValueError("worker proof targets another native candidate")
    if transitions != expected:
        raise ValueError("worker index does not cover the exact manifest transition matrix")
    validate_canonical(path.parent if proofs is None else proofs, verified, proofs is None)
    return value


def closure(index, resolve):
    names = set()
    for reference in index["sources"]:
        names.add(reference["name"])
        companion = groups.resolve_asset(reference, resolve, "worker-groups")
        for entry in companion["groups"]:
            names.update(entry[key]["name"] for key in ("observations", "cleanup", "forward", "rollback", "startup") if entry.get(key))
    return sorted(names)
