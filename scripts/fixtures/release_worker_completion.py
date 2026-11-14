"""Post-sign installed evidence, bound to the already signed native index."""
import json
import os
from pathlib import Path
import tempfile

from fixtures import fleet_baselines, release_worker_groups as groups, release_workers


def route_contract(first, target, history):
    required = {"mesh-release.json", "mesh-update.json", "mesh-update.sig.json", "mesh-update-root.json"}
    updaters = [item for item in history if "mesh-update.json" in {asset["name"] for asset in item["assets"]}]
    if any(not required <= {asset["name"] for asset in item["assets"]} for item in updaters):
        raise ValueError("retained updater lacks its genuine signed discovery assets")
    updaters.sort(key=lambda item: release_workers.version(item["tag"]))
    if first is None and updaters or first is not None and (not updaters or updaters[0]["tag"] != first):
        raise ValueError("first published updater baseline must name the authentic first retained updater")
    if first is not None and release_workers.version(first) >= release_workers.version(target):
        raise ValueError("published target must follow the first updater")
    candidates = [item for item in updaters if first is not None
                  and release_workers.version(first) < release_workers.version(item["tag"]) < release_workers.version(target)]
    intermediate = None
    if candidates:
        item = candidates[0]
        assets = {asset["name"]: asset["sha256"] for asset in item["assets"]}
        intermediate = {"version": item["tag"], "metadata": assets["mesh-update.json"], "manifest": assets["mesh-release.json"]}
    return {"first": first, "target": target, "intermediate": intermediate,
            "multihop": "required-staged-heads" if intermediate else "unavailable-first-successor" if first else "unavailable-first-updater"}


def route_scenarios(contract):
    if contract["first"] is None:
        return []
    scenarios = [("published", False, None), ("published-rollback", True, None)]
    if contract["intermediate"] is not None:
        via = contract["intermediate"]["version"]
        scenarios.extend([("published-chain", False, via), ("published-chain-rb", True, via)])
    return scenarios


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
            "workerProtocol": manifest["compatibility"]["workerWrite"],
            "routeContract": route_contract(fleet_baselines.load()["firstPublishedUpdater"], manifest["version"],
                                           json.loads((directory / "mesh-retention.json").read_bytes()))}


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
            or draft.get("installedHarness") != harness or draft.get("routeContract") != expected["routeContract"]):
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
    expected = ["before-install"]
    for update in result["updates"]:
        expected.extend(hop_label(update, hop["build"]["version"]) for hop in update["hops"])
        expected.append(final_label(update))
    if result["rollback"]:
        expected.append("native-failed-edge-restart")
    return expected


def hop_label(update, version):
    return "real-published-" + update["authenticatedTarget"]["version"] + "-hop-" + version


def final_label(update):
    return "real-published-" + update["authenticatedTarget"]["version"] + "-final"


def route_companion(output, contract, name, rollback, via, selected):
    entries = []
    for group in selected:
        directory = output / "groups" / name / str(group["index"])
        results = list(directory.glob("*-native-pair.json"))
        observations = list(directory.glob("*-cohort.json"))
        if len(results) != 1 or len(observations) != 1:
            raise ValueError("published route group evidence missing")
        entries.append({"group": groups.public_group(group), "result": asset(output, results[0]), "observations": asset(output, observations[0])})
    value = {"routeContract": contract, "scenario": name, "rollback": rollback, "via": via, "groups": entries}
    path = output / (name + "-groups.json")
    path.write_bytes(groups.encode(value))
    return asset(output, path)


def record(root, output, native, version, source):
    path = root / "releases" / version / "mesh-worker-groups.json"
    context = release_context(path.parent)
    if (context["version"], context["source"]) != (version, source):
        raise ValueError("installed completion release inputs differ")
    record_context(output, native, context, json.loads(path.read_bytes())["selection"], os.environ["MESH_HARNESS_SOURCE"])


def record_context(output, native, context, selection, harness):
    draft_ref = asset(output, output / "real-draft.json")
    harness_ref = asset(output, output / "harness-source.json")
    draft = validate_draft(resolve(output, draft_ref), context, native, harness)
    validate_harness(resolve(output, harness_ref), harness)
    publication = {"release": release_binding(context), "candidate": draft["build"], "harness": harness}
    refs = []
    for name in expected_pairs(native):
        data = (output / name).read_bytes()
        validate_case(json.loads(data), expected_case(name, native, publication, selection))
        refs.append({"name": name, "sha256": groups.digest(data), "size": len(data)})
    contract = context["routeContract"]
    routes = [asset(output, output / (name + "-groups.json")) for name, _rollback, _via in route_scenarios(contract)]
    observation = asset(output, output / "first-published-updater.json")
    value = {"schema": 2, "version": context["version"], "releaseSource": context["source"], "platform": native,
             "signedIndexSha256": context["workerGroups"], "firstPublishedUpdater": contract["first"],
             "routeContract": contract, "routeObservation": observation,
             "installedHarness": harness, "draft": draft_ref, "harnessObservation": harness_ref, "cases": refs, "routes": routes}
    validate_routes(output, value, selection, publication)
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
    if scenario in ("published", "published-rollback", "published-chain", "published-chain-rb"):
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
        expected = {"schema": 2, "version": release["version"], "releaseSource": release["source"], "platform": platform,
                    "signedIndexSha256": release["workerGroups"], "firstPublishedUpdater": fleet_baselines.load()["firstPublishedUpdater"],
                    "routeContract": release["routeContract"], "routeObservation": value["routeObservation"],
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
    contract = completion["routeContract"]
    if first != contract["first"] or resolve(root, completion["routeObservation"]) != contract:
        raise ValueError("published route availability differs from authenticated retained history")
    if first is None:
        if completion["routes"]:
            raise ValueError("unavailable first-published updater has invented route evidence")
        return
    expected = [groups.member(item) for item in selection["workers"] if item["artifact"]["platform"] == dict(zip(("os", "arch"), completion["platform"].split("/")))
                and release_workers.version(item["version"]) <= release_workers.version(first)]
    scenarios = route_scenarios(contract)
    if len(completion["routes"]) != len(scenarios):
        raise ValueError("published success, rollback and available signed intermediate grouped routes are required")
    for reference, (name, rollback, via) in zip(completion["routes"], scenarios, strict=True):
        value = resolve(root, reference)
        if (value["routeContract"], value["scenario"], value["rollback"], value["via"]) != (contract, name, rollback, via):
            raise ValueError("published grouped route has another source or outcome")
        validate_route_groups(root, value, expected, publication, selection)


def validate_route_groups(root, value, selected, publication, selection):
    first = value["routeContract"]["first"]
    observed = [item for entry in value["groups"] for item in entry["group"]["members"]]
    if [groups.member(item) for item in observed] != selected:
        raise ValueError("published grouped route has incomplete authoritative worker coverage")
    sources = [item for item in selected if item["version"] == first]
    if len(sources) != 1:
        raise ValueError("published route lacks its exact original source image")
    states = set()
    for entry in value["groups"]:
        original = validate_route_entry(root, value, entry, publication, selection, sources[0])
        if original["stateRun"] in states:
            raise ValueError("published groups reused a state identity")
        states.add(original["stateRun"])
    first = resolve(root, value["groups"][0]["observations"])["observations"][0]["original"]
    if [entry["group"] for entry in value["groups"]] != groups.partition(observed, first["imageBytes"]):
        raise ValueError("published route has another bounded deterministic partition")


def validate_route_entry(root, value, entry, publication, selection, source):
    first, target = value["routeContract"]["first"], value["routeContract"]["target"]
    result = resolve(root, entry["result"])
    name = value["scenario"]
    original_source = {"version": first, "platform": source["artifact"]["platform"],
                       "digest": source["artifact"]["binarySha256"], "protocol": source["protocol"]}
    validate_case(result, publication | {"case": name + "-managed", "scenario": name, "original": original_source})
    if (result["firstPublishedUpdater"], result["realDraft"], result["rollback"], result["routeContract"], result["via"]) != (first, target, value["rollback"], value["routeContract"], value["via"]):
        raise ValueError("installed group result has another published scenario")
    validate_route_updates(result, publication, selection, len(entry["group"]["members"]) + 1)
    record = resolve(root, entry["observations"])
    return groups.validate_observations(record, entry["group"], route_labels(result),
        {"digest": source["artifact"]["binarySha256"], "protocol": source["protocol"]})


def publication_groups(directory, native, context, index):
    first = context["routeContract"]["first"]
    if first is None:
        return []
    platform = dict(zip(("os", "arch"), native.split("/")))
    selected = [groups.member(item) for item in index["selection"]["workers"] if item["artifact"]["platform"] == platform
                and release_workers.version(item["version"]) <= release_workers.version(first)]
    sizes = {}
    for reference in index["sources"]:
        companion = groups.resolve_asset(reference, groups.file_resolver(directory), "worker-groups")
        collect_image_sizes(companion, platform, sizes)
    workers = [item | {"imageBytes": sizes[groups.encode(item)]} for item in selected]
    originals = [item for item in workers if item["version"] == first]
    if len(originals) != 1:
        raise ValueError("publication inventory lacks the authoritative first updater image")
    return groups.partition(workers, originals[0]["imageBytes"])


def collect_image_sizes(companion, platform, sizes):
    if companion["context"]["platform"] != platform:
        return
    for item in (item for entry in companion["groups"] for item in entry["group"]["members"]):
        key = groups.encode(groups.member(item))
        if key in sizes and sizes[key] != item["imageBytes"]:
            raise ValueError("publication inventory has conflicting signed image sizes")
        sizes[key] = item["imageBytes"]


def task_inventory(directory, native, harness):
    if native not in ("linux/amd64", "linux/arm64", "darwin/arm64"):
        raise ValueError("publication inventory has another native platform")
    context = release_context(directory)
    index = json.loads((directory / "mesh-worker-groups.json").read_bytes())
    tasks = [{"id": name.removesuffix("-native-pair.json"), "case": name} for name in expected_pairs(native)]
    selected = publication_groups(directory, native, context, index)
    for name, rollback, via in route_scenarios(context["routeContract"]):
        tasks.extend({"id": name + ":" + str(group["index"]), "scenario": name, "rollback": rollback,
                      "via": via, "group": group} for group in selected)
    return {"schema": 1, "release": release_binding(context), "platform": native, "harness": harness,
            "routeContract": context["routeContract"], "baselineSha256": groups.digest(groups.encode(fleet_baselines.load())),
            "retentionSha256": groups.digest((directory / "mesh-retention.json").read_bytes()),
            "signatureSha256": groups.digest((directory / "mesh-update.sig.json").read_bytes()), "tasks": tasks}


def select_task(inventory, task_id):
    tasks = [task for task in inventory["tasks"] if task["id"] == task_id]
    if len(tasks) != 1:
        raise ValueError("publication task is not in the complete deterministic inventory")
    return tasks[0]


def task_evidence(output, inventory, task):
    if "case" in task:
        return {"case": asset(output, output / task["case"])}
    directory = output / "groups" / task["scenario"] / str(task["group"]["index"])
    results, observations = list(directory.glob("*-native-pair.json")), list(directory.glob("*-cohort.json"))
    if len(results) != 1 or len(observations) != 1:
        raise ValueError("publication task lacks its complete group evidence")
    return {"route": {"routeContract": inventory["routeContract"], "scenario": task["scenario"],
            "rollback": task["rollback"], "via": task["via"], "groups": [{"group": task["group"],
            "result": asset(output, results[0]), "observations": asset(output, observations[0])}]}}


def record_task(output, inventory, task, context, selection):
    value = {"schema": 1, "inventorySha256": groups.digest(groups.encode(inventory)), "task": task,
             "draft": asset(output, output / "real-draft.json"),
             "harnessObservation": asset(output, output / "harness-source.json"),
             "routeObservation": asset(output, output / "first-published-updater.json"),
             **task_evidence(output, inventory, task)}
    validate_task(output, value, inventory, context, selection)
    (output / "publication-task.json").write_bytes(groups.encode(value))


def validate_task(root, value, inventory, context, selection):
    task = select_task(inventory, value["task"]["id"])
    keys = {"schema", "inventorySha256", "task", "draft", "harnessObservation", "routeObservation", "case" if "case" in task else "route"}
    if (set(value) != keys or value["schema"] != 1 or value["task"] != task
            or value["inventorySha256"] != groups.digest(groups.encode(inventory))):
        raise ValueError("publication task has another inventory, selection or diagnostic shape")
    native, harness = inventory["platform"], inventory["harness"]
    draft = validate_draft(resolve(root, value["draft"]), context, native, harness)
    validate_harness(resolve(root, value["harnessObservation"]), harness)
    if resolve(root, value["routeObservation"]) != inventory["routeContract"]:
        raise ValueError("publication task has another retained route observation")
    publication = {"release": release_binding(context), "candidate": draft["build"], "harness": harness}
    if "case" in task:
        if value["case"]["name"] != task["case"]:
            raise ValueError("publication task selected another case")
        validate_case(resolve(root, value["case"]), expected_case(task["case"], native, publication, selection))
        return
    route = value["route"]
    expected = {"routeContract": inventory["routeContract"], "scenario": task["scenario"], "rollback": task["rollback"],
                "via": task["via"], "groups": route["groups"]}
    if route != expected or len(route["groups"]) != 1 or route["groups"][0]["group"] != task["group"]:
        raise ValueError("publication task selected another route or bounded group")
    source = baseline_source(selection, native, inventory["routeContract"]["first"])
    validate_route_entry(root, route, route["groups"][0], publication, selection,
                         {"artifact": {"platform": source["platform"], "binarySha256": source["digest"]}, "protocol": source["protocol"]})


def task_references(value):
    refs = [value[key] for key in ("draft", "harnessObservation", "routeObservation")]
    if "case" in value:
        return refs + [value["case"]]
    entry = value["route"]["groups"][0]
    return refs + [entry["result"], entry["observations"]]


def copy_task(root, value, destination):
    for reference in task_references(value):
        resolve(root, reference)
        path = destination / reference["name"]
        path.parent.mkdir(parents=True, exist_ok=True)
        data = (root / reference["name"]).read_bytes()
        if path.exists() and path.read_bytes() != data:
            raise ValueError("publication tasks disagree on shared evidence")
        path.write_bytes(data)


def aggregate_tasks(directory, native, harness, inputs, output):
    inventory = task_inventory(directory, native, harness)
    context = release_context(directory)
    selection = json.loads((directory / "mesh-worker-groups.json").read_bytes())["selection"]
    if output.exists():
        raise ValueError("publication aggregation needs a new output directory")
    paths = sorted(inputs.rglob("publication-task.json"))
    if len(paths) != len(inventory["tasks"]):
        raise ValueError("publication task inventory is incomplete or extra")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="publication-aggregate-", dir=output.parent) as temporary:
        staging = Path(temporary)
        routes = collect_tasks(paths, staging, inventory, context, selection)
        for name, rollback, via in route_scenarios(context["routeContract"]):
            value = {"routeContract": context["routeContract"], "scenario": name, "rollback": rollback, "via": via,
                     "groups": sorted(routes[name], key=lambda entry: entry["group"]["index"])}
            (staging / (name + "-groups.json")).write_bytes(groups.encode(value))
        record_context(staging, native, context, selection, harness)
        staging.rename(output)


def collect_tasks(paths, staging, inventory, context, selection):
    seen, routes = set(), {name: [] for name, _rollback, _via in route_scenarios(context["routeContract"])}
    for path in paths:
        if path.is_symlink() or path.stat().st_size > groups.MAX_DOCUMENT:
            raise ValueError("publication task receipt is nonlocal or oversized")
        value = json.loads(path.read_bytes())
        validate_task(path.parent, value, inventory, context, selection)
        task = value["task"]
        if task["id"] in seen:
            raise ValueError("publication task inventory duplicates a task")
        seen.add(task["id"])
        copy_task(path.parent, value, staging)
        if "route" in value:
            routes[task["scenario"]].extend(value["route"]["groups"])
    if seen != {task["id"] for task in inventory["tasks"]}:
        raise ValueError("publication task inventory is incomplete")
    return routes


def identity(value):
    return {key: value[key] for key in ("version", "metadata", "manifest", "binary")}


def validate_observed_update(record, seen):
    expected, observed, plans = record["expectedHops"], record["hops"], record["routePlans"]
    target = identity(record["authenticatedTarget"])
    if not plans or any(identity(plan["target"]) != target for plan in plans):
        raise ValueError("published route lacks its stable authenticated target")
    planned = [plan["next"] for plan in plans if plan["next"]["version"] != target["version"]]
    kind = "multihop" if planned else "direct"
    if record["routeKind"] != kind or expected != planned or len(plans) != len(expected) + 1 or identity(plans[-1]["next"]) != target:
        raise ValueError("published route omitted or changed its authenticated planned route")
    if len(observed) != len(expected) or seen != {item["version"] for item in expected}:
        raise ValueError("published terminal state skipped its required signed intermediate/waypoint observations")
    for wanted, actual in zip(expected, observed, strict=True):
        if actual["expected"] != wanted or actual["build"]["digest"] != wanted["binary"] or actual["helper"]["sha256"] != wanted["binary"]:
            raise ValueError("published hop or helper does not match its authenticated expected image")
    for hop, plan in zip(observed, plans[1:], strict=True):
        if hop["build"]["digest"] != plan["currentDigest"]:
            raise ValueError("published route resumed from another authenticated intermediate image")
    if not expected and identity(plans[0]["next"]) != target:
        raise ValueError("direct published route does not select its authenticated final image")


def validate_route_updates(result, publication, selection, worker_count):
    contract, updates = result["routeContract"], result["updates"]
    versions = [contract["target"]] if result["via"] is None else [result["via"], contract["target"]]
    if len(updates) != len(versions):
        raise ValueError("published route omitted a required authentic head update")
    previous = result["publication"]["original"]
    for number, (update, version) in enumerate(zip(updates, versions, strict=True)):
        rollback = result["rollback"] and number == len(versions) - 1
        validate_route_update(update, version, rollback, previous, result, publication, selection, worker_count)
        previous = source_identity(update["final"]["build"])


def validate_route_update(update, version, rollback, previous, result, publication, selection, worker_count):
    target = update["authenticatedTarget"]
    if target["version"] != version or update["rollback"] is not rollback or source_identity(update["sourceBuild"]) != previous:
        raise ValueError("published route update has another source, head or outcome")
    validate_build(update["sourceBuild"], {"modified": False})
    if update["routePlans"][0]["currentDigest"] != previous["digest"]:
        raise ValueError("published authenticated route starts from another image")
    validate_observed_update(update, {hop["build"]["version"] for hop in update["hops"]})
    binding = publication["release"] if version == result["realDraft"] else result["routeContract"]["intermediate"]
    if any(target[key] != binding[key] for key in ("version", "metadata", "manifest")):
        raise ValueError("published update head differs from its signed retained or draft bytes")
    if version == result["realDraft"] and target["binary"] != publication["candidate"]["digest"]:
        raise ValueError("published update target differs from its signed candidate image")
    if version != result["realDraft"]:
        native = publication["candidate"]["platform"]
        original = baseline_source(selection, native["os"] + "/" + native["arch"], version)
        if target["binary"] != original["digest"]:
            raise ValueError("published intermediate differs from its authoritative signed native image")
    final = update["final"]
    restored = update["hops"][-1]["build"] if update["hops"] else update["sourceBuild"]
    expected = restored if rollback else {"version": version, "digest": target["binary"], "platform": publication["candidate"]["platform"]}
    if not rollback and version == result["realDraft"]:
        expected = publication["candidate"]
    validate_build(final["build"], expected)
    if final["helper"]["sha256"] != final["build"]["digest"] or final["ledger"]["state"].get("owned"):
        raise ValueError("published final helper or ownership differs from its authenticated outcome")
    validate_route_outcome(update, worker_count)


def validate_route_outcome(update, worker_count):
    final, target = update["final"], update["authenticatedTarget"]
    phase = "rolled_back" if update["rollback"] else "committed"
    if final["journal"]["phase"] != phase or final["journal"]["request"]["manifest"]["version"] != target["version"]:
        raise ValueError("published update lacks its exact terminal journal")
    if update["rollback"]:
        current = final["journal"]["request"]["current"]
        edge = {"from": current["digest"], "to": target["binary"], "platform": current["platform"]}
        failures = [item for item in final["ledger"]["failed"] if item["edge"] == edge
                    and item["version"] == target["version"] and item["phase"] == "rolled_back"]
        if (current["digest"] != final["build"]["digest"] or current["platform"] != final["build"]["platform"]
                or not failures or update["failedRestart"]["failed"] != failures
                or update["failedRestart"]["helper"]["sha256"] != final["build"]["digest"]
                or update["activationFault"] != {"bound": True, "released": True, "error": ""}):
            raise ValueError("published rollback lacks its fault, durable failed edge or restored helper restart")
        return
    commits = [entry for entry in final["ledger"]["entries"] if entry["version"] == target["version"]
               and entry["metadata"] == target["metadata"] and entry["workersPreserved"] == worker_count]
    if not commits:
        raise ValueError("published update lacks its exact pinned preserved-worker commit")
