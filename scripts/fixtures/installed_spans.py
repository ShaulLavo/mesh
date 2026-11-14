"""Pure operation-span grammar and same-invocation measurement accounting."""

# These identities describe the retained baseline, not a discoverable fleet state.
BASELINES = {"linux/amd64": ("pc", "v0.1.249", "v0.1.196"),
             "linux/arm64": ("pi", "v0.1.255", "v0.1.230"),
             "darwin/arm64": ("mac", "v0.1.257", "v0.1.196")}
VERSIONS = ("v0.1.9001", "v0.1.9002", "v0.1.9003")
SCENARIOS = ("adopt", "rollback", "helper", "initial", "interrupt")


def require(condition, message):
    if not condition:
        raise ValueError(message)


def proof(source, target):
    owner = "proof:" + source + ":" + target
    return [owner, "native-transition:" + source + ":" + target, owner]


def build(version, epoch=1):
    return ["build:" + version + ":epoch=" + str(epoch)]


def acquisitions(daemon, helper):
    previous = "v0.1." + str(int(daemon.rsplit(".", 1)[1]) - 1)
    return [["acquire:" + version] for version in (daemon, helper, previous)]


def serving(public=True, head=VERSIONS[0]):
    result = [["serve:releases"]]
    if public:
        result.append(["serve:public"])
    if head is not None:
        result.append(["head:" + head])
    return result


def case(host, scenario):
    owner = "installed-case:" + host + ":" + scenario
    return [owner, "case-cleanup:" + host + "-" + scenario, owner]


def oldest_chunks(host, daemon, helper):
    chunks = [build(VERSIONS[0])] + acquisitions(daemon, helper)
    chunks += [proof(daemon, VERSIONS[0]), ["publish:" + VERSIONS[0]]]
    chunks += serving() + [["partition:" + daemon]]
    for scenario in ("adopt", "rollback"):
        chunks.extend(oldest_group(host, scenario, index) for index in range(3))
    return chunks


def oldest_group(host, scenario, index):
    owner = "installed-group:" + host + ":" + scenario + ":" + str(index)
    return [owner] + case(host, scenario) + [owner]


def fresh_chunks(platform, daemon):
    first, target = VERSIONS[:2]
    chunks = [["acquire:" + daemon], build(first), proof(daemon, first), ["publish:" + first],
              build(target), proof(platform.replace("/", "-"), target), ["publish:" + target]]
    chunks += serving(False)
    chunks.append(["fresh-case:", "head:" + target, "fresh-case:", "case-cleanup:fresh-managed", "fresh-case:"])
    return chunks


def adoption_chunks(platform, scenario):
    hosts = [BASELINES[platform]]
    if platform == "linux/amd64":
        hosts.append(("vps", "v0.1.255", "v0.1.196"))
    chunks = [build(VERSIONS[0])]
    for _host, daemon, helper in hosts:
        chunks += acquisitions(daemon, helper) + [proof(daemon, VERSIONS[0])]
    chunks += [["publish:" + VERSIONS[0]]] + serving()
    scenarios = SCENARIOS if scenario is None else (scenario,)
    for host, _daemon, _helper in hosts:
        chunks.extend(case(host, selected) for selected in scenarios)
    return chunks


def release_chunks(platform, daemon, helper, waypoint=False, order=False):
    chunks = acquisitions(daemon, helper)[:2 if order else 3]
    for index, version in enumerate(VERSIONS):
        if index == 2 and not order:
            chunks.append(["custom-source:" + VERSIONS[0]])
        source = daemon if index == 0 else platform.replace("/", "-")
        chunks += [build(version, 2 if waypoint and index else 1), proof(source, version), ["publish:" + version]]
    if order:
        return chunks + serving(False) + [["serve:public"], ["ordered-owner:"]]
    head = VERSIONS[2] if waypoint else VERSIONS[0]
    return chunks + serving(False, head) + [["partition:" + VERSIONS[0]], ["prepared-export:"]]


def consumer_chunks(workload, host):
    selection = workload.get("installedSlice")
    require(isinstance(selection, dict) and set(selection) == {"schema", "select", "readers"}
            and type(selection["schema"]) is int and selection["schema"] == 1, "invalid span consumer selection")
    selected, readers = selection["select"], selection["readers"]
    require(isinstance(selected, list) and len(selected) == 1 and isinstance(selected[0], dict), "span needs one complete profile")
    profile = selected[0]
    require(set(profile) == {"profile", "group_indices"} and profile["profile"] in ("chain-none", "chain-first", "chain-final")
            and profile["group_indices"] == list(range(5)) and all(type(i) is int for i in profile["group_indices"]),
            "span consumer group identities differ")
    expected_readers = list(range(5)) if workload["mode"] == "waypoint" and profile["profile"] == "chain-none" else []
    require(readers == expected_readers and all(type(i) is int for i in readers), "span consumer reader identities differ")
    chunks = [["prepared-load:", "partition:" + VERSIONS[0], "prepared-load:"], ["prepared-server-start"]]
    chunks += serving(head=None)
    chunks.extend(["readers:" + str(index)] for index in readers)
    chunks.extend(consumer_group(host, profile["profile"], index) for index in profile["group_indices"])
    return chunks


def consumer_group(host, profile, index):
    owner = "installed-group:" + host + ":" + profile + ":" + str(index)
    cleanup = {"chain-none": "chain", "chain-first": "chain-first-rollback", "chain-final": "chain-rollback"}[profile]
    return [owner, "head:" + VERSIONS[0], owner, "head:" + VERSIONS[2], owner, "case-cleanup:" + host + "-" + cleanup, owner]


def workload_chunks(workload):
    require(isinstance(workload, dict) and workload.get("platform") in BASELINES, "unknown span workload platform")
    platform, mode, role = workload["platform"], workload.get("mode"), workload.get("preparedRole")
    host, daemon, helper = BASELINES[platform]
    grouped = mode in ("oldest", "chain", "waypoint", "order")
    require(workload.get("grouped") is grouped and workload.get("host") == (host if grouped else None), "span workload host/group differs")
    require(workload.get("manifestSchemas") == ([1, 2] if mode == "waypoint" else [1]), "span manifest schemas differ")
    require(type(workload.get("catalogEvidence")) is bool and (not workload["catalogEvidence"] or mode == "oldest" and platform.startswith("linux/")),
            "span catalog identity differs")
    scenario = workload.get("scenario")
    require(scenario is None or mode == "adoption" and scenario in SCENARIOS, "unknown span scenario")
    recipe = "prepared-native/complete-installed-slice" if role == "consume" else "all-three-platforms/startup/full-transition"
    require(workload.get("buildRecipe") == "mesh_installed_fixture/" + recipe, "unknown span build recipe")
    require(role in (None, "prepare", "consume") and (role is None or mode in ("chain", "waypoint")), "unknown span prepared role")
    require(("installedSlice" in workload) == (role == "consume"), "span slice ownership differs")
    if role == "consume":
        return consumer_chunks(workload, host)
    signer = [["signer-build:"], ["signer-start"]]
    return signer + producer_chunks(workload, host, daemon, helper)


def producer_chunks(workload, host, daemon, helper):
    mode, role = workload.get("mode"), workload.get("preparedRole")
    if mode == "oldest" and role is None:
        return oldest_chunks(host, daemon, helper)
    if mode == "fresh" and role is None:
        return fresh_chunks(workload["platform"], daemon)
    if mode == "adoption" and role is None:
        return adoption_chunks(workload["platform"], workload.get("scenario"))
    if mode in ("chain", "waypoint") and role == "prepare":
        return release_chunks(workload["platform"], daemon, helper, mode == "waypoint")
    if mode == "order" and role is None and workload["platform"] == "linux/amd64":
        return release_chunks(workload["platform"], daemon, helper, order=True)
    raise ValueError("unknown span workload root sequence")


def canonical_chunks(workload):
    counts, result = {}, []
    chunks = workload_chunks(workload) + [["completion", "completion"]]
    for names in chunks:
        result.append(number_names(["setup"] + names, counts))
    return result


def number_names(names, counts):
    result = []
    for name in names:
        counts[name] = counts.get(name, 0) + 1
        result.append(name + "#" + str(counts[name]))
    return result


def operation_leaf_keys(keys, workload):
    """Validate exact operation identities/ownership, then fuse release serving."""
    keys = list(keys)
    chunks = canonical_chunks(workload)
    require(keys == [key for chunk in chunks for key in chunk], "span leaf sequence differs from strict workload grammar")
    result, serving_phase = [], False
    for chunk in chunks:
        kind = chunk[1].split(":", 1)[0].split("#", 1)[0]
        if kind in ("serve", "head") and serving_phase:
            result[-1].extend(chunk)
            continue
        require(kind != "head", "top-level head needs a declared serving phase")
        result.append(list(chunk))
        serving_phase = kind == "serve"
    return result


def bootstrap_parts(parts, workload):
    """The existing startup prefix ends at its last original-image acquisition."""
    if workload.get("preparedRole") == "consume":
        return []
    last = max(index for index, part in enumerate(parts) if part[1].startswith("acquire:"))
    return parts[:last + 1]


def partition_leaf_keys(keys, workload):
    parts = operation_leaf_keys(keys, workload)
    bootstrap = bootstrap_parts(parts, workload)
    if not bootstrap:
        return parts
    return [[key for part in bootstrap for key in part]] + parts[len(bootstrap):]


def completed_leaves(record):
    """No failed, instrumented, partial, or cross-invocation source can size spans."""
    require(record.get("measurement_completed") is True and not record.get("failures")
            and not record.get("missing_sample_keys") and not record.get("unresolved_key_sets"), "span source invocation is incomplete")
    require(not record["workload"].get("transportLifecycle") and not record["provenance"].get("transportLifecycle"), "instrumented span source")
    leaves = record["leaves"]
    require(bool(leaves), "span source has no leaves")
    cursor = record["origin_ns"]
    for row in leaves:
        cursor = completed_leaf(row, cursor)
    require(cursor == record["end_ns"] and cursor - record["origin_ns"] == record["command_ns"] == record["disjoint_ns"],
            "span command endpoints differ")
    partition_leaf_keys([row["key"] for row in leaves], record["workload"])
    validate_operations(record, leaves)
    return leaves


def completed_leaf(row, cursor):
    require(all(row.get(field) is True for field in ("completed", "eligible", "cleanup_verified")) and not row.get("censored"),
            "ineligible span leaf")
    require(all(type(row.get(field)) is int for field in ("start_ns", "end_ns", "elapsed_ns"))
            and row["start_ns"] == cursor and row["end_ns"] - cursor == row["elapsed_ns"] > 0, "span leaf gap/overlap or invalid endpoints")
    return row["end_ns"]


def validate_operations(record, leaves):
    samples = {row["key"]: row for row in record["samples"]}
    require(len(samples) == len(record["samples"]), "duplicate span source operation samples")
    by_key = {row["key"]: row for row in leaves}
    for chunk in canonical_chunks(record["workload"])[:-1]:
        sample = samples.get(chunk[1], {})
        require(all(sample.get(field) is True for field in ("completed", "eligible", "cleanup_verified")) and not sample.get("censored"),
                "span source lacks its completed outer operation: " + chunk[1])
        start, end = sample.get("start_ns"), sample.get("end_ns")
        require(type(start) is int and type(end) is int and end - start == sample.get("elapsed_ns") and start < end
                and by_key[chunk[0]]["start_ns"] <= start <= by_key[chunk[1]]["end_ns"]
                and by_key[chunk[-1]]["start_ns"] <= end <= by_key[chunk[-1]]["end_ns"], "span operation lies outside its owning chunk")


def span_intervals(record, *, telemetry=False):
    leaves = completed_leaves(record)
    partition = operation_leaf_keys if telemetry else partition_leaf_keys
    parts = partition([row["key"] for row in leaves], record["workload"])
    by_key = {row["key"]: row for row in leaves}
    result = []
    for part in parts:
        start, end = by_key[part[0]]["start_ns"], by_key[part[-1]]["end_ns"]
        elapsed = sum(by_key[key]["elapsed_ns"] for key in part)
        require(end - start == elapsed, "span endpoints differ from member leaves")
        result.append({"key": "span:" + part[0], "leaves": part, "start_ns": start, "end_ns": end, "elapsed_ns": elapsed})
    return result
