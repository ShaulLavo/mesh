"""Post-sign installed evidence, bound to the already signed native index."""
import json
import os
from pathlib import Path

from fixtures import fleet_baselines, release_worker_groups as groups, release_workers


def expected_pairs(native):
    hosts = [host["name"] for host in fleet_baselines.load()["hosts"] if host["platform"] == native]
    return sorted(["fresh-managed-native-pair.json"] + [host + "-" + scenario + "-native-pair.json"
                  for host in hosts for scenario in ("publication", "rollback", "publication-interrupt")])


def release_context(directory):
    manifest_bytes = (directory / "mesh-release.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    return {"version": manifest["version"], "source": manifest["commit"], "manifest": groups.digest(manifest_bytes),
            "metadata": groups.digest((directory / "mesh-update.json").read_bytes()),
            "workerGroups": groups.digest((directory / "mesh-worker-groups.json").read_bytes()),
            "artifacts": manifest["artifacts"], "stateVersion": manifest["compatibility"]["stateWrite"],
            "workerProtocol": manifest["compatibility"]["workerWrite"]}


def release_binding(value):
    return {key: value[key] for key in ("version", "source", "manifest", "metadata", "workerGroups")}


def source_identity(build):
    return {"version": build["version"], "platform": build["platform"],
            "digest": build["digest"], "protocol": build["workerProtocol"]}


def publication_case(case, releases, scenario, original):
    verified = releases.verification
    return {"release": release_binding(verified), "candidate": verified["build"],
            "harness": verified["installedHarness"], "case": case.host["name"],
            "scenario": scenario, "original": original}


def validate_harness(observed, expected):
    if observed.get("expected") != expected or observed.get("actual") != expected:
        raise ValueError("installed completion lacks its actual expected harness observation")


def validate_build(build, expected):
    if (build.get("modified") is not False or type(build.get("workerProtocol")) is not int
            or any(build.get(key) != value for key, value in expected.items())):
        raise ValueError("installed case executes another authenticated image")


def validate_draft(draft, expected, platform, harness):
    if (release_binding(draft) != release_binding(expected) or draft.get("testTrustRoot") is not False
            or draft.get("installedHarness") != harness):
        raise ValueError("installed completion has another authenticated draft context")
    native = dict(zip(("os", "arch"), platform.split("/")))
    artifact = next(item for item in expected["artifacts"] if item["platform"] == native)
    validate_build(draft["build"], {"version": expected["version"], "commit": expected["source"],
        "platform": native, "digest": artifact["binarySha256"], "stateVersion": expected["stateVersion"],
        "workerProtocol": expected["workerProtocol"]})
    return draft


def baseline_source(selection, platform, version):
    native = dict(zip(("os", "arch"), platform.split("/")))
    matches = [item for item in selection["workers"] if item["version"] == version and item["artifact"]["platform"] == native]
    if len(matches) != 1:
        raise ValueError("installed scenario lacks its authoritative original source")
    return {"version": version, "platform": native, "digest": matches[0]["artifact"]["binarySha256"],
            "protocol": matches[0]["protocol"]}


def expected_case(name, platform, publication, selection):
    case = name.removesuffix("-native-pair.json")
    if name == "fresh-managed-native-pair.json":
        return publication | {"case": case, "scenario": "fresh", "original": source_identity(publication["candidate"])}
    matches = [(host, scenario) for host in fleet_baselines.load()["hosts"] for scenario in
               ("publication", "rollback", "publication-interrupt") if host["platform"] == platform and case == host["name"] + "-" + scenario]
    if len(matches) != 1:
        raise ValueError("installed completion contains another host or scenario")
    host, scenario = matches[0]
    return publication | {"case": case, "scenario": scenario, "original": baseline_source(selection, platform, host["daemon"])}


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
    context = release_context(path.parent)
    if (context["version"], context["source"]) != (version, source):
        raise ValueError("installed completion release inputs differ")
    draft_ref = asset(output, output / "real-draft.json")
    harness_ref = asset(output, output / "harness-source.json")
    harness = os.environ["MESH_HARNESS_SOURCE"]
    draft = validate_draft(resolve(output, draft_ref), context, native, harness)
    validate_harness(resolve(output, harness_ref), harness)
    publication = {"release": release_binding(context), "candidate": draft["build"], "harness": harness}
    selection = json.loads(path.read_bytes())["selection"]
    refs = []
    for name in expected_pairs(native):
        data = (output / name).read_bytes()
        validate_case(json.loads(data), expected_case(name, native, publication, selection))
        refs.append({"name": name, "sha256": groups.digest(data), "size": len(data)})
    first = fleet_baselines.load()["firstPublishedUpdater"]
    routes = [] if first is None else [asset(output, output / name) for name in ("published-groups.json", "published-rollback-groups.json")]
    value = {"schema": 1, "version": version, "releaseSource": source, "platform": native,
             "signedIndexSha256": groups.digest(path.read_bytes()), "firstPublishedUpdater": first,
             "installedHarness": harness, "draft": draft_ref, "harnessObservation": harness_ref, "cases": refs, "routes": routes}
    (output / "worker-completion.json").write_bytes(groups.encode(value))


def validate_case_cleanup(record):
    if record.get("passed") is not True or record.get("cleanup") != {"servicesAbsent": True, "processesAbsent": True}:
        raise ValueError("installed completion contains a failed case or unconfirmed cleanup")


def validate_case(record, expected):
    validate_case_cleanup(record)
    if (record.get("publication") != expected or record.get("realDraft") != expected["release"]["version"]
            or record.get("testTrustRoot") is not False or "error" in record or "cleanupError" in record):
        raise ValueError("installed case has another release, trust, harness, host or scenario")
    scenario, candidate = expected["scenario"], expected["candidate"]
    if scenario in ("published", "published-rollback"):
        return
    if scenario == "fresh":
        validate_build(record["running"], candidate)
        images = (record["initial"]["helperBeforeDaemon"], record["initial"]["firstNativeStart"]["mappedImage"])
        if any(image["sha256"] != candidate["digest"] for image in images):
            raise ValueError("fresh installed native image differs from authenticated draft")
        return
    result = record["result"]
    running = candidate if scenario != "rollback" else {"version": expected["original"]["version"],
        "platform": expected["original"]["platform"], "digest": expected["original"]["digest"],
        "workerProtocol": expected["original"]["protocol"]}
    validate_build(result["running"], running)
    phase = "rolled-back" if scenario == "rollback" else "adopted"
    if result["phase"] != phase or result["helper"]["sha256"] != candidate["digest"]:
        raise ValueError("installed adoption or rollback has another native outcome")
    if scenario != "rollback" and result.get("confirmed") is not True:
        raise ValueError("installed draft adoption is not confirmed")
    if scenario == "publication-interrupt" and record["interruption"]["mappedImage"]["sha256"] != candidate["digest"]:
        raise ValueError("installed helper interruption belongs to another image")


def validate(directory, release, harness, selection):
    seen = set()
    for path in directory.rglob("worker-completion.json"):
        if path.stat().st_size > groups.MAX_DOCUMENT:
            raise ValueError("installed completion exceeds bound")
        value = json.loads(path.read_bytes())
        platform = value["platform"]
        if platform in seen or platform not in ("linux/amd64", "linux/arm64", "darwin/arm64"):
            raise ValueError("installed completion platform matrix differs")
        seen.add(platform)
        expected = {"schema": 1, "version": release["version"], "releaseSource": release["source"], "platform": platform,
                    "signedIndexSha256": release["workerGroups"], "firstPublishedUpdater": fleet_baselines.load()["firstPublishedUpdater"],
                    "installedHarness": harness, "draft": value["draft"], "harnessObservation": value["harnessObservation"],
                    "cases": value["cases"], "routes": value["routes"]}
        if value != expected or sorted(ref["name"] for ref in value["cases"]) != expected_pairs(platform):
            raise ValueError("installed completion has a wrong index, source, harness or scenario matrix")
        draft = validate_draft(resolve(path.parent, value["draft"]), release, platform, harness)
        validate_harness(resolve(path.parent, value["harnessObservation"]), harness)
        publication = {"release": release_binding(release), "candidate": draft["build"], "harness": harness}
        for ref in value["cases"]:
            validate_case(resolve(path.parent, ref), expected_case(ref["name"], platform, publication, selection))
        validate_routes(path.parent, value, selection, publication)
    if seen != {"linux/amd64", "linux/arm64", "darwin/arm64"}:
        raise ValueError("installed post-sign platform completion is missing")


def validate_routes(root, completion, selection, publication):
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
        validate_route_groups(root, value, expected, publication)


def validate_route_groups(root, value, selected, publication):
    observed = [item for entry in value["groups"] for item in entry["group"]["members"]]
    if [groups.member(item) for item in observed] != selected:
        raise ValueError("published grouped route has incomplete authoritative worker coverage")
    sources = [item for item in selected if item["version"] == value["first"]]
    if len(sources) != 1:
        raise ValueError("published route lacks its exact original source image")
    states = set()
    for entry in value["groups"]:
        result = resolve(root, entry["result"])
        name = "published-rollback" if value["rollback"] else "published"
        original_source = {"version": value["first"], "platform": sources[0]["artifact"]["platform"],
                           "digest": sources[0]["artifact"]["binarySha256"], "protocol": sources[0]["protocol"]}
        validate_case(result, publication | {"case": name + "-managed", "scenario": name, "original": original_source})
        if (result["firstPublishedUpdater"], result["realDraft"], result["rollback"]) != (value["first"], value["target"], value["rollback"]):
            raise ValueError("installed group result has another published scenario")
        record = resolve(root, entry["observations"])
        original = groups.validate_observations(record, entry["group"], route_labels(result),
            {"digest": sources[0]["artifact"]["binarySha256"], "protocol": sources[0]["protocol"]})
        if original["stateRun"] in states:
            raise ValueError("published groups reused a state identity")
        states.add(original["stateRun"])
    first = resolve(root, value["groups"][0]["observations"])["observations"][0]["original"]
    if [entry["group"] for entry in value["groups"]] != groups.partition(observed, first["imageBytes"]):
        raise ValueError("published route has another bounded deterministic partition")
