"""Deterministic post-sign platform work and evidence for the signed draft."""
import json
from pathlib import Path
import re

from fixtures import draft_intake, fleet_baselines, release_worker_completion as completion
from fixtures import release_worker_groups as groups
from fixtures import release_worker_index

PLATFORMS = ("linux/amd64", "linux/arm64", "darwin/arm64")
INSTALLED = "installed-platform.json"
RECEIPT = "publication-platform.json"
READERS = "readers/original-readers.json"
CLEANUP = ("scratch-cleanup.json", "publication-transport-cleanup.json")
CASE_SUFFIX = "-native-pair.json"
SHA = re.compile(r"[0-9a-f]{64}")


def read(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > groups.MAX_DOCUMENT:
        raise ValueError(f"publication evidence missing, nonlocal or oversized: {path}")
    return path.read_bytes()


def platform_key(native):
    return native.replace("/", "-")


def artifact_name(native, attempt):
    return f"publication-platform-{platform_key(native)}-attempt-{attempt}"


def draft_binding(snapshot, context):
    assets = draft_intake.inventory(snapshot, context["version"], context["source"], signed=True)
    fields = ("id", "name", "size", "state", "digest")
    selected = sorted(assets, key=lambda item: item["name"])
    return {"id": snapshot["id"], "run": snapshot["intake_run"],
            "assetsSha256": groups.digest(groups.encode([{key: item[key] for key in fields} for item in selected]))}


def legacy_tasks(native, selection):
    fresh = {"id": "fresh-managed", "case": "fresh-managed-native-pair.json"}
    tasks = [fresh]
    for host in fleet_baselines.load()["hosts"]:
        if host["platform"] != native:
            continue
        scenarios = ("publication", "rollback", "publication-interrupt") if host["name"] in ("pc", "mac") else ("publication",)
        prior = {role: completion.baseline_source(selection, native, host[role]) for role in ("daemon", "helper")}
        if any(host.get(role + "Sha256", prior[role]["digest"]) != prior[role]["digest"] for role in prior):
            raise ValueError("fleet baseline differs from the authenticated legacy image")
        for scenario in scenarios:
            name = host["name"] + "-" + scenario
            tasks.append({"id": name, "case": name + CASE_SUFFIX,
                          "prior": prior | {"helperRunning": host["helperRunning"]}})
    return sorted(tasks, key=lambda task: task["id"])


def route_tasks(directory, native, context, index):
    contract = context["routeContract"]
    if contract["first"] is None:
        return []
    partition = completion.publication_groups(directory, native, context, index)
    protocol = completion.baseline_source(index["selection"], native, contract["first"])["protocol"]
    group = next((group for group in partition if any(member["protocol"] < protocol for member in group["members"])), partition[0])
    return [
        {"id": name + ":" + str(group["index"]), "scenario": name, "rollback": rollback, "via": via, "group": group}
        for name, rollback, via in completion.route_scenarios(contract)
        if via is not None or contract["intermediate"] is None or native == "linux/amd64"
    ]


def inventory(directory, native, harness, context, index):
    return completion.inventory_context(directory, native, harness, context) | {
        "tasks": legacy_tasks(native, index["selection"]) + route_tasks(directory, native, context, index)}


def reader_index(path, native, context):
    raw = read(path)
    value = json.loads(raw)
    platform = dict(zip(("os", "arch"), native.split("/"), strict=True))
    if (value["schema"], value["target"], value["platform"]) != (1, context["version"], platform):
        raise ValueError("original reader index has another target or platform")
    candidate = next(item for item in context["artifacts"] if item["platform"] == platform)
    readers = []
    for invocation in value["invocations"]:
        artifact, packet = invocation["artifact"], invocation["sha256"]
        if (artifact["platform"] != platform or invocation["metadata"] != context["metadata"]
                or invocation["binary"] != candidate["binarySha256"] or not SHA.fullmatch(packet)
                or invocation["packet"] != packet + ".reader.json" or not SHA.fullmatch(artifact["binarySha256"])):
            raise ValueError("original reader invocation differs from its native candidate, source or packet")
        readers.append({"source": invocation["source"], "sourceDigest": artifact["binarySha256"], "packet": packet, "accepted": True})
    if len({item["source"] for item in readers}) != len(readers) or len({item["packet"] for item in readers}) != len(readers):
        raise ValueError("original reader index contains duplicate invocations")
    return {"indexSha256": groups.digest(raw), "outcome": {"target": context["version"], "platform": platform, "readers": readers}}


def plan(directory, harness, snapshot, reader_indexes, context, index):
    platforms = {}
    for native in PLATFORMS:
        readers = reader_index(reader_indexes / platform_key(native) / "index.json", native, context)
        platforms[native] = inventory(directory, native, harness, context, index) | {"readers": readers}
    return {"schema": 1, "draft": draft_binding(snapshot, context), "platforms": platforms}


def prepare_plan(directory, physical, harness, reader_indexes, run):
    context = completion.release_context(directory)
    snapshot = draft_intake.verify(physical, context["version"], context["source"], signed=True, run=run)
    manifest = json.loads(read(directory / "mesh-release.json"))
    index = release_worker_index.validate_index(directory / "mesh-worker-groups.json", manifest)
    return plan(directory, harness, snapshot, reader_indexes, context, index)


def load_plan(path, expected_sha256):
    raw = read(path)
    if groups.digest(raw) != expected_sha256:
        raise ValueError("publication plan differs from the signer's exact inventory digest")
    value = json.loads(raw)
    if value["schema"] != 1 or set(value["platforms"]) != set(PLATFORMS):
        raise ValueError("publication plan requires the exact platform set")
    return value


def validate_plan_platform(plan_value, directory, native, harness, snapshot, context, index):
    expected = inventory(directory, native, harness, context, index)
    actual = plan_value["platforms"][native]
    if actual != expected | {"readers": actual["readers"]} or plan_value["draft"] != draft_binding(snapshot, context):
        raise ValueError("publication plan differs from its authenticated draft, platform, harness or tasks")
    return actual


def references(output, inventory, binding):
    return {"schema": 1, "publication": binding,
            "draft": completion.asset(output, output / "real-draft.json"),
            "harnessObservation": completion.asset(output, output / "harness-source.json"),
            "routeObservation": completion.asset(output, output / "first-published-updater.json"),
            "tasks": [{"id": task["id"], **completion.task_evidence(output, inventory, task)} for task in inventory["tasks"]]}


def record_installed(output, tasks, binding, context, selection):
    value = references(output, tasks, binding)
    validate_installed(output, value, tasks, binding, context, selection)
    with (output / INSTALLED).open("xb") as stream:
        stream.write(groups.encode(value))


def validate_installed(output, value, inventory, binding, context, selection):
    tasks, receipts = inventory["tasks"], value["tasks"]
    if (set(value) != {"schema", "publication", "draft", "harnessObservation", "routeObservation", "tasks"}
            or value["schema"] != 1 or value["publication"] != binding
            or [entry["id"] for entry in receipts] != [task["id"] for task in tasks]):
        raise ValueError("installed platform evidence has another plan or an incomplete, extra or duplicate task set")
    native, harness = inventory["platform"], inventory["harness"]
    draft = completion.validate_draft(completion.resolve(output, value["draft"]), context, native, harness)
    completion.validate_harness(completion.resolve(output, value["harnessObservation"]), harness)
    if completion.resolve(output, value["routeObservation"]) != inventory["routeContract"]:
        raise ValueError("installed platform evidence has another retained route observation")
    publication = {"release": completion.release_binding(context), "candidate": draft["build"], "harness": harness}
    states, cases = set(), set()
    for task, entry in zip(tasks, receipts, strict=True):
        case, state = validate_task(output, inventory, task, entry, publication, selection)
        if case in cases or state is not None and state in states:
            raise ValueError("installed platform evidence duplicates a case or route state")
        cases.add(case)
        states.add(state)
    if cases != {str(path.relative_to(output)) for path in output.rglob("*" + CASE_SUFFIX)}:
        raise ValueError("installed platform evidence contains extra native cases")


def validate_task(output, inventory, task, entry, publication, selection):
    key = "case" if "case" in task else "route"
    if set(entry) != {"id", key}:
        raise ValueError("publication evidence has an invalid task shape")
    result, state = completion.validate_task_outcome(output, task, entry, inventory, publication, selection)
    if key == "case":
        if "prior" in task:
            validate_prior_case(result, task["prior"])
        return task["case"], None
    group = entry["route"]["groups"][0]
    directory = Path("groups") / task["scenario"] / str(task["group"]["index"])
    if any(Path(group[key]["name"]).parent != directory for key in ("result", "observations")):
        raise ValueError("installed route evidence belongs to another scenario or group")
    result = completion.resolve(output, group["result"])
    if task["via"] is None and result["updates"][0]["routeKind"] != "direct":
        raise ValueError("installed direct route traversed an intermediate")
    if task["via"] is not None and task["rollback"] and result["updates"][-1]["final"]["build"]["version"] != task["via"]:
        raise ValueError("installed final-hop rollback did not restore the selected intermediate")
    return group["result"]["name"], state


def validate_prior_case(result, prior):
    if result.get("priorPair") != prior:
        raise ValueError("installed legacy case lacks its exact prior daemon/helper pair and running mode")
    scenario = result["publication"]["scenario"]
    if scenario == "rollback" and result.get("activationFault") != {"bound": True, "released": True, "error": ""}:
        raise ValueError("installed legacy rollback lacks its actual activation fault")
    if scenario == "publication-interrupt":
        cut = result["interruption"]
        restarted = result["result"]["helper"]["pid"]
        if cut["confirmedBeforeInterruption"] is not False or min(cut["killedNativePID"], restarted) <= 0 or cut["killedNativePID"] == restarted:
            raise ValueError("installed interruption lacks a replaced native helper and unconfirmed adoption")


def attempt_number(attempt):
    if type(attempt) is not int or attempt <= 0:
        raise ValueError("publication platform evidence needs a positive producer attempt")
    return attempt


def job_binding(native, run, attempt, plan_sha256):
    return {"run": run, "attempt": attempt_number(attempt), "platform": native, "planSha256": plan_sha256}


def record_platform(output, plan_value, plan_sha256, native, run, attempt):
    value = {"schema": 1, **job_binding(native, run, attempt, plan_sha256),
             "installed": completion.asset(output, output / INSTALLED),
             "readers": completion.asset(output, output / READERS),
             "cleanup": [completion.asset(output, output / name) for name in CLEANUP]}
    validate_platform(output, value, plan_value, plan_sha256, native, run, attempt)
    with (output / RECEIPT).open("xb") as stream:
        stream.write(groups.encode(value))


def validate_platform(output, value, plan_value, plan_sha256, native, run, attempt):
    binding = job_binding(native, run, attempt, plan_sha256)
    if (type(value["run"]) is not int or type(value["attempt"]) is not int or run != plan_value["draft"]["run"]
            or value["run"] != run or value["attempt"] != attempt):
        raise ValueError("publication platform evidence has another run or producer attempt")
    if (value["schema"], value["planSha256"], value["platform"]) != (1, plan_sha256, native):
        raise ValueError("publication evidence has another plan or native platform")
    if (set(value) != {"schema", "planSha256", "platform", "run", "attempt", "installed", "readers", "cleanup"}
            or value["installed"]["name"] != INSTALLED or value["readers"]["name"] != READERS
            or [ref["name"] for ref in value["cleanup"]] != list(CLEANUP)):
        raise ValueError("publication platform evidence has another completion shape")
    readers = plan_value["platforms"][native]["readers"]["outcome"] | {"publication": binding}
    if completion.resolve(output, value["readers"]) != readers:
        raise ValueError("publication platform omitted, duplicated or changed an original reader result")
    scratch, transport = [completion.resolve(output, ref) for ref in value["cleanup"]]
    if (scratch["absent"] is not True or not Path(scratch["root"]).is_absolute()
            or transport["proxyStopped"] is not True or transport["testSignatureRootUsed"] is not False):
        raise ValueError("publication platform cleanup was not confirmed")
    installed = completion.resolve(output, value["installed"])
    if installed["publication"] != binding:
        raise ValueError("installed evidence has another plan, platform or producer attempt")
    return installed


def validate_platforms(directory, plan_value, plan_sha256, attempts, logical, snapshot, harness, context, index):
    if set(attempts) != set(PLATFORMS):
        raise ValueError("publication platform attempts require the exact platform set")
    expected = {artifact_name(native, attempt_number(attempt)): native for native, attempt in attempts.items()}
    paths = list(directory.rglob(RECEIPT))
    if (len(paths) != len(expected) or {path.parent.relative_to(directory).as_posix() for path in paths} != set(expected)
            or {path.name for path in directory.iterdir()} != set(expected) or any(path.parent.is_symlink() for path in paths)):
        raise ValueError("publication evidence has missing, duplicate or unexpected platform completion")
    for path in paths:
        native = expected[path.parent.name]
        inventory = validate_plan_platform(plan_value, logical, native, harness, snapshot, context, index)
        installed = validate_platform(path.parent, json.loads(read(path)), plan_value, plan_sha256,
                                      native, snapshot["intake_run"], attempts[native])
        binding = job_binding(native, snapshot["intake_run"], attempts[native], plan_sha256)
        validate_installed(path.parent, installed, inventory, binding, context, index["selection"])
