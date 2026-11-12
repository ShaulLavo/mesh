"""Deterministic independent worker runs and their content-bound evidence closure."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

from fixtures import release_workers, noindex_paths

MAX_MEMBERS = 31
MAX_IMAGES = 1 << 30
MAX_DOCUMENT = 1 << 20
ASSET = re.compile(r"([0-9a-f]{64})\.(worker-groups|worker-group)\.json\Z")
RECEIPT = re.compile(r"[0-9a-f]{64}\.json\Z")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def member(item):
    return {key: item[key] for key in ("version", "protocol", "artifact", "commit") if key in item}


def freeze(registry, version, binary):
    selected = release_workers.applicable(registry, version, binary)
    return [item | {"imageBytes": Path(item["binary"]).stat().st_size} for item in selected]


def partition(selected, original_bytes):
    if not selected or len(selected) > 4096 or not 0 < original_bytes <= 128 << 20:
        raise ValueError("invalid bounded worker selection or original image")
    groups, current, size = [], [], original_bytes
    for item in selected:
        image = item["imageBytes"]
        if type(image) is not int or not 0 < image <= 128 << 20 or original_bytes + image > MAX_IMAGES:
            raise ValueError("worker and attached source exceed image budget")
        if len(current) == MAX_MEMBERS or size + image > MAX_IMAGES:
            groups.append({"index": len(groups), "members": current, "imageBytes": size})
            current, size = [], original_bytes
        current.append(item)
        size += image
    groups.append({"index": len(groups), "members": current, "imageBytes": size})
    return groups


def public_group(group):
    return group | {"members": [member(item) | {"imageBytes": item["imageBytes"]} for item in group["members"]]}


def selected_group(descriptor):
    value = json.loads(Path(descriptor).read_bytes())
    group = value["group"]
    checked = partition(group["members"], value["originalBytes"])
    if len(checked) != 1 or checked[0]["imageBytes"] != group["imageBytes"]:
        raise ValueError("group descriptor exceeds count or image bound")
    for item in group["members"]:
        data = Path(item["binary"]).read_bytes()
        if len(data) != item["imageBytes"] or digest(data) != item["artifact"]["binarySha256"]:
            raise ValueError("selected group executable changed")
    return group["members"]


def labels(profile, first, last):
    result = ["candidate", "restart"]
    if profile == "forward-only":
        return result
    if profile != "full" or type(first) is not int or type(last) is not int or not 0 <= first <= last <= 4096:
        raise ValueError("invalid proof profile or state range")
    for state in range(first, last + 1):
        sides = ("before", "after-commit", "failure", "interrupt") if state < last else ("before", "after-commit")
        result.extend("rollback-" + str(state) + "-" + side for side in sides)
    for state in range(first + 1, last + 1):
        result.extend(f"rollback-startup-{state}-{side}-{mode}" for side in ("before", "after") for mode in ("failure", "interrupt"))
    result.append(f"rollback-{last}-candidate-writes")
    return result


def leaf(directory, value, kind):
    data = encode({"kind": kind, "value": value})
    if len(data) > MAX_DOCUMENT:
        raise ValueError("worker group leaf exceeds document bound")
    name = digest(data) + ".worker-group.json"
    directory.mkdir(parents=True, exist_ok=True)
    (directory / name).write_bytes(data)
    return {"name": name, "sha256": digest(data), "size": len(data)}


def resolve_asset(reference, resolver, suffix=None):
    name = reference["name"]
    match = ASSET.fullmatch(name)
    if not match or suffix and match[2] != suffix:
        raise ValueError("worker evidence reference is not a flat content-addressed asset")
    size = reference["size"]
    if type(size) is not int or not 0 < size <= MAX_DOCUMENT or reference["sha256"] != match[1]:
        raise ValueError("invalid worker evidence asset identity")
    data = resolver(name)
    if len(data) != size or digest(data) != match[1]:
        raise ValueError("worker evidence asset missing, changed or swapped")
    return json.loads(data)


def typed_leaf(reference, kind, resolver):
    value = resolve_asset(reference, resolver, "worker-group")
    if set(value) != {"kind", "value"} or value["kind"] != kind:
        raise ValueError("worker evidence leaf has another type")
    return value["value"]


def validate_observations(record, group, expected_labels):
    workers = record["workers"]
    expected = [(item["version"], item["protocol"], item["artifact"]["binarySha256"]) for item in group["members"]]
    actual = [(row["version"], row["protocol"], row["digest"]) for row in workers]
    if actual != expected or any(row.get("startupFailure") for row in workers):
        raise ValueError("group worker identities differ from its exact selection")
    identities = [{key: row[key] for key in ("session", "pid", "shellPid", "digest", "protocol", "version")} for row in workers]
    if len({row["pid"] for row in workers}) != len(workers) or any(row["pid"] <= 0 or row["shellPid"] <= 0 for row in workers):
        raise ValueError("group worker process ownership is incomplete")
    observations = record["observations"]
    if sorted(row["label"] for row in observations) != sorted(expected_labels):
        raise ValueError("group required observation labels differ")
    original = None
    for row in observations:
        if row["identities"] != identities or row["workers"] != len(workers) or row["processes"] != 2 * len(workers):
            raise ValueError("worker identity changed within an independent group state")
        attached = row["original"]
        if not attached or attached["pid"] <= 0 or attached["shellPid"] <= 0 or attached["pid"] in {worker["pid"] for worker in workers}:
            raise ValueError("original attached worker identity missing")
        if original is not None and attached != original:
            raise ValueError("original attached worker changed within group")
        original = attached
    return original


def validate_group_assets(expected_context, selection, reference, resolver):
    """Validate exact coverage from authoritative inputs, never from a pass marker."""
    value = resolve_asset(reference, resolver, "worker-groups")
    if value.get("schema") != 1 or value["context"] != expected_context:
        raise ValueError("worker groups have another source, harness, registry or profile context")
    if value["originalBytes"] != expected_context["sourceImageBytes"]:
        raise ValueError("original attached image byte budget differs")
    expected_groups = [public_group(group) for group in partition(selection, value["originalBytes"])]
    groups = value["groups"]
    if len(groups) != len(expected_groups):
        raise ValueError("worker group coverage incomplete or extra")
    forward, rollback, states = None, [], set()
    for entry, expected in zip(groups, expected_groups, strict=True):
        actual_forward, actual_rollback = validate_entry(entry, expected, expected_context, resolver)
        if entry["stateRun"] in states:
            raise ValueError("independent groups reused a state identity")
        states.add(entry["stateRun"])
        if forward is not None and actual_forward != forward:
            raise ValueError("independent groups produced differing forward receipt bytes")
        forward = actual_forward
        if actual_rollback is not None:
            rollback.append(actual_rollback)
    return {"forward": forward, "rollback": aggregate_rollback(rollback), "companion": value}


def validate_entry(entry, expected, context, resolver):
    if entry["group"] != expected:
        raise ValueError("worker group partition or exact member coverage differs")
    record = typed_leaf(entry["observations"], "observations", resolver)
    original = validate_observations(record, expected, labels(context["profile"], context["firstState"], context["lastState"]))
    if original["digest"] != context["sourceDigest"] or original["stateRun"] != entry["stateRun"]:
        raise ValueError("original attached worker belongs to another group or source")
    cleanup = typed_leaf(entry["cleanup"], "cleanup", resolver)
    if cleanup != {"stateRun": entry["stateRun"], "processesAbsent": True, "stateAbsent": True}:
        raise ValueError("worker group cleanup unconfirmed")
    forward = typed_leaf(entry["forward"], "forward", resolver)
    raw = forward["bytes"].encode()
    receipt = json.loads(raw)
    if digest(raw) != forward["sha256"] or receipt["fromDigest"] != context["sourceDigest"] or receipt["toDigest"] != context["candidateDigest"] or receipt["platform"] != context["platform"]:
        raise ValueError("group forward receipt context differs")
    if receipt["stateReadMin"] != context["firstState"] or receipt["stateReadMax"] != context["lastState"]:
        raise ValueError("group forward receipt state range differs")
    expected_receipt = {"schema": 1, "platform": context["platform"], "fromDigest": context["sourceDigest"],
                        "toDigest": context["candidateDigest"], "stateReadMin": context["firstState"],
                        "stateReadMax": context["lastState"], "stateWrite": context["lastState"],
                        "workerMin": context["sourceProtocol"], "workerMax": context["candidateProtocol"],
                        "workerWrite": context["candidateProtocol"], "journalVersion": 1,
                        "candidateOpenedRetainedState": True, "sessionsPreserved": True, "recoveryRecordsPreserved": True}
    if receipt != expected_receipt or any(receipt[key] is not True for key in ("candidateOpenedRetainedState", "sessionsPreserved", "recoveryRecordsPreserved")):
        raise ValueError("forward receipt does not certify its exact schema 1 transition")
    rollback = validate_rollback(entry, context, expected, forward["sha256"], resolver)
    return raw, rollback


def validate_rollback(entry, context, group, forward, resolver):
    if context["profile"] == "forward-only":
        if entry.get("rollback") is not None:
            raise ValueError("unexpected rollback in forward profile")
        return None
    rollback = typed_leaf(entry["rollback"], "rollback", resolver)
    workers = sorted({context["sourceDigest"]} | {item["artifact"]["binarySha256"] for item in group["members"]})
    boundaries = [{"state": state, "candidateWrote": True, "sourceStarted": True, "sessionIO": True, "recoveryPreserved": True}
                  for state in range(context["firstState"], context["lastState"] + 1)]
    if rollback != {"schema": 1, "platform": context["platform"], "fromDigest": context["sourceDigest"],
                    "toDigest": context["candidateDigest"], "forward": forward, "workers": workers, "boundaries": boundaries}:
        raise ValueError("rollback receipt differs from fully observed group boundaries")
    startup = typed_leaf(entry["startup"], "startup", resolver)
    required = {label.removeprefix("rollback-") for label in labels("full", context["firstState"], context["lastState"])
                if label.startswith("rollback-startup-")}
    if set(startup) != required:
        raise ValueError("actual startup image contexts are incomplete")
    for label, observed in startup.items():
        if observed["source"]["digest"] != context["sourceDigest"] or observed["release"]["digest"] != context["candidateDigest"]:
            raise ValueError("startup fault image context differs")
        failed, target = observed["failedImage"], observed["release"]
        if failed["digest"] != context["startupDigest"] or not observed["startupCompanion"]:
            raise ValueError("actual startup companion digest differs")
        if any(failed[key] != target[key] for key in ("version", "commit", "platform", "stateVersion", "workerProtocol")):
            raise ValueError("startup companion does not belong to the candidate")
        if observed["manifestSha256"] != context["rollbackManifestSha256"]:
            raise ValueError("startup image used another rollback context manifest")
    return rollback


def aggregate_rollback(proofs):
    if not proofs:
        return None
    first = proofs[0]
    common = {key: value for key, value in first.items() if key not in ("workers", "boundaries")}
    if any({key: value for key, value in proof.items() if key not in ("workers", "boundaries")} != common for proof in proofs):
        raise ValueError("group rollback receipt context disagreement")
    boundaries = []
    for position, boundary in enumerate(first["boundaries"]):
        flags = {key: all(proof["boundaries"][position][key] is True for proof in proofs) for key in boundary if key != "state"}
        boundaries.append({"state": boundary["state"]} | flags)
    return common | {"workers": sorted({worker for proof in proofs for worker in proof["workers"]}), "boundaries": boundaries}


def file_resolver(directory):
    def resolve(name):
        path = directory / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size > MAX_DOCUMENT:
            raise ValueError("missing, nonregular or oversized worker evidence")
        return path.read_bytes()
    return resolve


def context(source, candidate, platform, registry, profile, environment):
    build = json.loads(subprocess.check_output([str(source), "version", "--json"], env=environment, timeout=10))
    target = json.loads(subprocess.check_output([str(candidate), "version", "--json"], env=environment, timeout=10))
    checkout = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    startup = environment.get("MESH_STARTUP_FAULT_BINARY")
    return {"harnessSource": environment.get("MESH_HARNESS_SOURCE", checkout),
            "preparationSource": environment.get("MESH_PREPARATION_SOURCE", checkout),
            "registrySha256": digest(Path(registry).read_bytes()), "sourceDigest": digest(Path(source).read_bytes()),
            "sourceVersion": build["version"], "sourceProtocol": build["workerProtocol"],
            "candidateProtocol": target["workerProtocol"],
            "candidateDigest": digest(Path(candidate).read_bytes()), "platform": dict(zip(("os", "arch"), platform.split("/"))),
            "sourceImageBytes": Path(source).stat().st_size,
            "profile": profile, "firstState": build["stateVersion"], "lastState": target["stateVersion"],
            "startupDigest": digest(Path(startup).read_bytes()) if startup else None,
            "rollbackManifestSha256": digest(Path(environment["MESH_ROLLBACK_MANIFEST"]).read_bytes()) if profile == "full" else None}


def run(source, candidate, platform, output):
    source, candidate, output = Path(source), Path(candidate), Path(output)
    environment = dict(os.environ)
    registry = environment["MESH_WORKER_REGISTRY"]
    profile = environment.get("MESH_TRANSITION_PROOF_MODE", "full")
    before = noindex_paths.observation()
    ctx = context(source, candidate, platform, registry, profile, environment)
    build = json.loads(subprocess.check_output([str(source), "version", "--json"], env=environment, timeout=10))
    selected = freeze(registry, build["version"], source)
    original = source.stat().st_size
    groups = partition(selected, original)
    root = output / "groups" / ctx["sourceDigest"] / profile
    root.mkdir(parents=True)
    leaves = output / "groups" / "leaves"
    entries, started = [], time.monotonic()
    for group in groups:
        entries.append(run_group(source, candidate, platform, root, leaves, group, original, environment))
    if digest(Path(registry).read_bytes()) != ctx["registrySha256"]:
        raise ValueError("authoritative worker registry changed during independent group runs")
    peaks = [peak_rss(entry, leaves) for entry in entries]
    value = {"schema": 1, "context": ctx, "originalBytes": original, "groups": entries,
             "measurements": {"before": before, "after": noindex_paths.observation(), "peakRetainedRssKiB": max(peaks)}}
    data = encode(value)
    if len(data) > MAX_DOCUMENT:
        raise ValueError("group companion exceeds document bound")
    reference = {"name": digest(data) + ".worker-groups.json", "sha256": digest(data), "size": len(data)}
    # A private candidate is validated before any canonical proof becomes visible.
    private = root / reference["name"]
    private.write_bytes(data)
    def resolver(name):
        return private.read_bytes() if name == private.name else file_resolver(leaves)(name)
    verified = validate_group_assets(ctx, selected, reference, resolver)
    publish(output, verified, reference, data)
    (root / "timing.json").write_bytes(encode({"seconds": time.monotonic() - started, "groups": len(groups)}))
    print(digest(verified["forward"]))


def peak_rss(entry, leaves):
    record = typed_leaf(entry["observations"], "observations", file_resolver(leaves))
    return max(row["rssKiB"] + row["originalRssKiB"] for row in record["observations"])


def run_group(source, candidate, platform, root, leaves, group, original, environment):
    directory = root / str(group["index"])
    directory.mkdir()
    descriptor = directory / "selected.json"
    descriptor.write_bytes(encode({"group": group, "originalBytes": original}))
    child = environment | {"MESH_WORKER_GROUP": str(descriptor.resolve())}
    script = Path(__file__).parents[1] / "prove-release-transition.sh"
    subprocess.run(["bash", str(script), str(source), str(candidate), platform, str(directory)], env=child, check=True)
    receipts = list(directory.glob("*.json"))
    receipts = [path for path in receipts if RECEIPT.fullmatch(path.name)]
    if len(receipts) != 1:
        raise ValueError("group did not produce exactly one actual forward receipt")
    raw = receipts[0].read_bytes()
    observations = json.loads((directory / "observations" / (receipts[0].stem + ".cohort.json")).read_bytes())
    cleanup = json.loads((directory / "cleanup.json").read_bytes())
    entry = {"group": public_group(group), "stateRun": cleanup["stateRun"],
             "observations": leaf(leaves, observations, "observations"), "cleanup": leaf(leaves, cleanup, "cleanup"),
             "forward": leaf(leaves, {"bytes": raw.decode(), "sha256": digest(raw)}, "forward"), "rollback": None}
    if child.get("MESH_TRANSITION_PROOF_MODE", "full") == "full":
        rollbacks = list((directory / "rollback").glob("*.rollback.json"))
        if len(rollbacks) != 1:
            raise ValueError("group did not produce its actual rollback receipt")
        entry["rollback"] = leaf(leaves, json.loads(rollbacks[0].read_bytes()), "rollback")
        startup = {path.name.removesuffix(".image-context.json"): json.loads(path.read_bytes()) for path in (directory / "startup").glob("*.image-context.json")}
        entry["startup"] = leaf(leaves, startup, "startup")
    return entry


def publish(output, verified, reference, data):
    forward = verified["forward"]
    (output / (digest(forward) + ".json")).write_bytes(forward)
    rollback = verified["rollback"]
    if rollback is not None:
        directory = output / "rollback"
        directory.mkdir(exist_ok=True)
        raw = encode(rollback)
        (directory / (digest(raw) + ".rollback.json")).write_bytes(raw)
    (output / reference["name"]).write_bytes(data)


if __name__ == "__main__":
    run(*sys.argv[1:])
