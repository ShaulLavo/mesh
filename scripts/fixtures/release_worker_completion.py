"""Post-sign installed evidence, bound to the already signed native index."""
import json
import os
from pathlib import Path

from fixtures import fleet_baselines, release_worker_groups as groups, release_workers


def expected_pairs(native):
    hosts = [host["name"] for host in fleet_baselines.load()["hosts"] if host["platform"] == native]
    return sorted(["fresh-managed-native-pair.json"] + [host + "-" + scenario + "-native-pair.json"
                  for host in hosts for scenario in ("publication", "rollback", "publication-interrupt")])


def asset(root, path):
    data = path.read_bytes()
    return {"name": str(path.relative_to(root)), "sha256": groups.digest(data), "size": len(data)}


def resolve(root, reference):
    path = Path(reference["name"])
    if path.is_absolute() or ".." in path.parts:
        raise ValueError("installed evidence path escapes its completion root")
    target = root / path
    if not target.resolve().is_relative_to(root.resolve()) or target.is_symlink() or target.stat().st_size > groups.MAX_DOCUMENT:
        raise ValueError("installed evidence is nonlocal or oversized")
    data = target.read_bytes()
    if len(data) != reference["size"] or groups.digest(data) != reference["sha256"]:
        raise ValueError("installed completion evidence changed")
    return json.loads(data)


def route_labels(result):
    expected = ["before-install"] + ["real-published-hop-" + hop["build"]["version"] for hop in result["hops"]]
    expected.append("real-published-final")
    if result["rollback"]:
        expected.append("native-failed-edge-restart")
    return expected


def route_companion(output, first, target, rollback, selected):
    entries = []
    for group in selected:
        directory = output / "groups" / ("published-rollback" if rollback else "published") / str(group["index"])
        results = list(directory.glob("*-native-pair.json"))
        observations = list(directory.glob("*-cohort.json"))
        if len(results) != 1 or len(observations) != 1:
            raise ValueError("published route group evidence missing")
        entries.append({"group": groups.public_group(group), "result": asset(output, results[0]), "observations": asset(output, observations[0])})
    value = {"first": first, "target": target, "rollback": rollback, "groups": entries}
    path = output / ("published-rollback-groups.json" if rollback else "published-groups.json")
    path.write_bytes(groups.encode(value))
    return asset(output, path)


def record(root, output, native, version, source):
    path = root / "releases" / version / "mesh-worker-groups.json"
    refs = []
    for name in expected_pairs(native):
        data = (output / name).read_bytes()
        validate_case(json.loads(data))
        refs.append({"name": name, "sha256": groups.digest(data), "size": len(data)})
    first = fleet_baselines.load()["firstPublishedUpdater"]
    routes = [] if first is None else [asset(output, output / name) for name in ("published-groups.json", "published-rollback-groups.json")]
    value = {"schema": 1, "version": version, "releaseSource": source, "platform": native,
             "signedIndexSha256": groups.digest(path.read_bytes()), "firstPublishedUpdater": first,
             "installedHarness": os.environ["MESH_HARNESS_SOURCE"], "cases": refs, "routes": routes}
    (output / "worker-completion.json").write_bytes(groups.encode(value))


def validate_case(record):
    if record.get("passed") is not True or record.get("cleanup") != {"servicesAbsent": True, "processesAbsent": True}:
        raise ValueError("installed completion contains a failed case or unconfirmed cleanup")


def validate(directory, version, source, index_digest, harness, selection):
    seen = set()
    for path in directory.rglob("worker-completion.json"):
        if path.stat().st_size > groups.MAX_DOCUMENT:
            raise ValueError("installed completion exceeds bound")
        value = json.loads(path.read_bytes())
        platform = value["platform"]
        if platform in seen or platform not in ("linux/amd64", "linux/arm64", "darwin/arm64"):
            raise ValueError("installed completion platform matrix differs")
        seen.add(platform)
        expected = {"schema": 1, "version": version, "releaseSource": source, "platform": platform,
                    "signedIndexSha256": index_digest, "firstPublishedUpdater": fleet_baselines.load()["firstPublishedUpdater"],
                    "installedHarness": harness, "cases": value["cases"], "routes": value["routes"]}
        if value != expected or sorted(ref["name"] for ref in value["cases"]) != expected_pairs(platform):
            raise ValueError("installed completion has a wrong index, source, harness or scenario matrix")
        for ref in value["cases"]:
            validate_case(resolve(path.parent, ref))
        validate_routes(path.parent, value, selection)
    if seen != {"linux/amd64", "linux/arm64", "darwin/arm64"}:
        raise ValueError("installed post-sign platform completion is missing")


def validate_routes(root, completion, selection):
    first = completion["firstPublishedUpdater"]
    if first is None:
        if completion["routes"]:
            raise ValueError("unavailable first-published updater has invented route evidence")
        return
    expected = [groups.member(item) for item in selection["workers"] if item["artifact"]["platform"] == dict(zip(("os", "arch"), completion["platform"].split("/")))
                and release_workers.version(item["version"]) <= release_workers.version(first)]
    if len(completion["routes"]) != 2:
        raise ValueError("published success and rollback grouped routes are required")
    for reference, rollback in zip(completion["routes"], (False, True), strict=True):
        value = resolve(root, reference)
        if (value["first"], value["target"], value["rollback"]) != (first, completion["version"], rollback):
            raise ValueError("published grouped route has another source or outcome")
        validate_route_groups(root, value, expected)


def validate_route_groups(root, value, selected):
    observed = [item for entry in value["groups"] for item in entry["group"]["members"]]
    if [groups.member(item) for item in observed] != selected:
        raise ValueError("published grouped route has incomplete authoritative worker coverage")
    sources = [item for item in selected if item["version"] == value["first"]]
    if len(sources) != 1:
        raise ValueError("published route lacks its exact original source image")
    states = set()
    for entry in value["groups"]:
        result = resolve(root, entry["result"])
        validate_case(result)
        if (result["firstPublishedUpdater"], result["realDraft"], result["rollback"]) != (value["first"], value["target"], value["rollback"]):
            raise ValueError("installed group result has another published scenario")
        record = resolve(root, entry["observations"])
        original = groups.validate_observations(record, entry["group"], route_labels(result))
        if original["digest"] != sources[0]["artifact"]["binarySha256"] or original["protocol"] != sources[0]["protocol"]:
            raise ValueError("published route retained another original source worker")
        if original["stateRun"] in states:
            raise ValueError("published groups reused a state identity")
        states.add(original["stateRun"])
    first = resolve(root, value["groups"][0]["observations"])["observations"][0]["original"]
    if [entry["group"] for entry in value["groups"]] != groups.partition(observed, first["imageBytes"]):
        raise ValueError("published route has another bounded deterministic partition")
