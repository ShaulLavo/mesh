"""Exact installed profile partitions shared by admission, execution and closure."""
import json
from pathlib import Path

PROFILES = ("chain-none", "chain-first", "chain-final")
MAX_GROUPS = 4096


def indices(value):
    if not isinstance(value, list) or len(value) > MAX_GROUPS:
        raise ValueError("slice indices must be a bounded list")
    if any(type(index) is not int or not 0 <= index < MAX_GROUPS for index in value):
        raise ValueError("slice group index is outside its bound")
    if len(set(value)) != len(value):
        raise ValueError("slice repeats a group index")
    return sorted(value)


def parse_slice(value):
    if isinstance(value, (str, Path)):
        path = Path(value)
        if path.stat().st_size > 1 << 20:
            raise ValueError("installed slice exceeds its document bound")
        value = json.loads(path.read_bytes())
    if not isinstance(value, dict) or set(value) != {"schema", "select", "readers"} or type(value["schema"]) is not int or value["schema"] != 1:
        raise ValueError("installed slice schema differs")
    selected = value["select"]
    if not isinstance(selected, list) or len(selected) > len(PROFILES):
        raise ValueError("slice profiles must be a bounded list")
    profiles = {}
    for row in selected:
        profile, groups = parse_profile(row)
        if profile in profiles:
            raise ValueError("slice repeats a profile")
        profiles[profile] = groups
    readers = indices(value["readers"])
    if not profiles and not readers:
        raise ValueError("installed slice is empty")
    return {"schema": 1, "select": [{"profile": profile, "group_indices": profiles[profile]}
            for profile in PROFILES if profile in profiles], "readers": readers}


def parse_profile(row):
    if not isinstance(row, dict) or set(row) != {"profile", "group_indices"} or row["profile"] not in PROFILES:
        raise ValueError("slice profile differs")
    groups = indices(row["group_indices"])
    if not groups:
        raise ValueError("slice profile contains no groups")
    return row["profile"], groups


def validate_selection(selection, count, waypoint):
    if type(count) is not int or not 0 < count <= MAX_GROUPS:
        raise ValueError("invalid authoritative group count")
    actual = [index for row in selection["select"] for index in row["group_indices"]] + selection["readers"]
    if any(index >= count for index in actual) or selection["readers"] and not waypoint:
        raise ValueError("slice is outside the authoritative partition or reader mode")


def coverage(selection):
    return [(row["profile"], index) for row in selection["select"] for index in row["group_indices"]] + [
        ("readers", index) for index in selection["readers"]]


def validate_partition(selections, count, waypoint):
    expected = {(profile, index) for profile in PROFILES for index in range(count)}
    if waypoint:
        expected.update(("readers", index) for index in range(count))
    actual = []
    for selection in selections:
        validate_selection(selection, count, waypoint)
        actual.extend(coverage(selection))
    if len(set(actual)) != len(actual) or set(actual) != expected:
        raise ValueError("installed slices omit or duplicate authoritative profile/group or reader coverage")


def group_labels(profile):
    from fixtures import installed_releases
    intermediate, final = installed_releases.VERSIONS[1:]
    if profile == "chain-first":
        return ["before-install", "native-first-hop-rollback", "native-failed-edge-restart"]
    labels = ["before-install", "native-hop-" + intermediate,
              "native-final-" + ("rolled_back" if profile == "chain-final" else "committed")]
    if profile == "chain-final":
        labels.append("native-failed-edge-restart")
    return labels


def aggregate(bundle, outputs, preparation_output):
    """Recheck retained real evidence; diagnostic completion never grants acceptance."""
    from fixtures import installed_adoption, installed_prepared
    bundle = Path(bundle)
    descriptor, identity = installed_prepared.read_descriptor(bundle)
    selections, states = [], set()
    accepted = verify_preparation(Path(preparation_output), descriptor, identity)
    for output in outputs:
        selection, eligible = verify_output(Path(output), descriptor, identity, states, installed_adoption)
        selections.append(selection)
        accepted = accepted and eligible
    validate_partition(selections, len(descriptor["groups"]), descriptor["mode"] == "waypoint")
    return {"schema": 1, "preparedId": identity, "harnessSource": descriptor["harnessSource"],
            "measurement_completed": True, "accepted": accepted,
            "kind": "installed-acceptance" if accepted else "diagnostic-installed-measurement",
            "coverage": [list(item) for selection in selections for item in coverage(selection)]}


def verify_output(output, descriptor, identity, states, adoption):
    report = json.loads((output / "prepared-slice.json").read_bytes())
    clock = json.loads((output / "fixture-clock.json").read_bytes())
    if (report.get("preparedId") != identity or report.get("harnessSource") != descriptor["harnessSource"]
            or report.get("completed") is not True or not clock.get("measurement_completed") or clock.get("failures")):
        raise ValueError("prepared slice identity, completion or cleanup differs")
    selection = parse_slice(report["selection"])
    if (clock.get("provenance", {}).get("preparedId") != identity or clock["workload"].get("installedSlice") != selection
            or clock["workload"].get("preparedRole") != "consume"):
        raise ValueError("prepared slice clock belongs to another workload")
    validate_clock_context(clock, descriptor)
    for row in selection["select"]:
        verify_profile(output, descriptor, row, states, adoption)
    for index in selection["readers"]:
        reader = json.loads((output / ("original-format-readers-group-" + str(index) + ".json")).read_bytes())
        if "manifest schema 2 is unsupported" not in reader.get("oldRejectedManifestSchema2", ""):
            raise ValueError("prepared slice lacks its original negative reader proof")
    return selection, clock.get("accepted") is True and clock.get("mode") == "frozen"


def verify_preparation(output, descriptor, identity):
    report = json.loads((output / "prepared-export.json").read_bytes())
    clock = json.loads((output / "fixture-clock.json").read_bytes())
    if (report.get("preparedId") != identity or report.get("harnessSource") != descriptor["harnessSource"]
            or report.get("groups") != descriptor["groups"] or clock["workload"].get("preparedRole") != "prepare"
            or clock.get("provenance", {}).get("preparedId") != identity
            or not clock.get("measurement_completed") or clock.get("failures")):
        raise ValueError("prepared producer identity, full proofs or cleanup are incomplete")
    validate_clock_context(clock, descriptor)
    return clock.get("accepted") is True and clock.get("mode") == "frozen"


def validate_clock_context(clock, descriptor):
    expected = {"platform": descriptor["native"], "host": descriptor["host"]["name"], "mode": descriptor["mode"]}
    if any(clock["workload"].get(key) != value for key, value in expected.items()):
        raise ValueError("prepared clock platform, host or proof mode differs")


def verify_profile(output, descriptor, row, states, adoption):
    for index in row["group_indices"]:
        directory = output / "groups" / (descriptor["host"]["name"] + "-" + row["profile"]) / str(index)
        observed = adoption.validate_installed_group(directory, descriptor["groups"][index],
                    group_labels(row["profile"]), adoption.original_source(descriptor["pair"]["daemon"]))
        state = observed["observations"][0]["original"]["stateRun"]
        if state in states:
            raise ValueError("prepared slices reused an installed state identity")
        states.add(state)
