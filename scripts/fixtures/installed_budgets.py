"""Select independently approved fixture plans without deriving or activating bounds."""
import argparse
import json
import os
from pathlib import Path

from fixtures import installed_clock

ROOT = Path(__file__).resolve().parents[2]
INDEX = ROOT / ".internal/fixtures/installed-clock/index.json"
PLATFORMS = {"linux/amd64": ("linux-amd64", "pc"), "linux/arm64": ("linux-arm64", "pi"),
             "darwin/arm64": ("darwin-arm64", "mac")}
COHORT_MODES = ("oldest", "chain", "waypoint", "order")
SCENARIOS = ("adopt", "rollback", "helper", "initial", "interrupt")


def selection(platform, mode, scenario=None, catalog=False):
    key, host = PLATFORMS[platform]
    if mode not in COHORT_MODES + ("adoption", "fresh"):
        raise ValueError("unsupported frozen fixture mode: " + mode)
    if scenario is not None and (mode != "adoption" or scenario not in SCENARIOS):
        raise ValueError("unsupported frozen fixture scenario")
    if catalog and (not platform.startswith("linux/") or mode != "oldest"):
        raise ValueError("catalog evidence needs its reviewed Linux oldest profile")
    profile = mode + ("-" + (scenario or "all") if mode == "adoption" else "")
    profile += "-catalog" if catalog else ""
    context = {"platform": platform, "mode": mode, "scenario": scenario, "catalogEvidence": catalog,
               "host": host if mode in COHORT_MODES else None, "grouped": mode in COHORT_MODES}
    return key + "-" + profile, context


def read_index(path):
    try:
        value = json.loads(path.read_text())
    except FileNotFoundError as error:
        raise ValueError("frozen budgets inactive: independently approved plan index is absent: " + str(path)
                         + "; reviewed plans must be activated explicitly") from error
    if value.get("schema") != 1 or not isinstance(value.get("plans"), dict):
        raise ValueError("invalid frozen budget index schema")
    return value["plans"]


def select_plan(index, key, context):
    index = Path(index)
    entry = read_index(index).get(key)
    if not isinstance(entry, dict) or not isinstance(entry.get("path"), str):
        raise ValueError("no independently approved frozen plan for " + key + "; no approved source is activated for this selection")
    path = (index.parent / entry["path"]).resolve()
    if not path.is_relative_to(index.parent.resolve()) or path.suffix != ".json" or path == index.resolve():
        raise ValueError("frozen plan must be a JSON file within the fixture budget directory")
    try:
        plan = installed_clock.load_plan(path)
    except FileNotFoundError as error:
        raise ValueError("approved frozen plan source is absent for " + key + ": " + str(path)) from error
    if plan.mode != "frozen" or any(plan.workload.get(name) != value for name, value in context.items()):
        raise ValueError("frozen plan selection differs from platform/mode/profile: " + key)
    return path, plan


def workflow_minutes(plan):
    minutes = (plan.admission_ns + 60 * installed_clock.SECOND - 1) // (60 * installed_clock.SECOND)
    if not 1 <= minutes <= 30:
        raise ValueError("frozen workflow admission must fit the 30-minute ceiling")
    return minutes


def bind(arguments, native):
    if arguments.clock_mode != "frozen":
        return
    role = "prepare" if getattr(arguments, "export_prepared", None) else "consume" if getattr(arguments, "prepared_releases", None) else None
    if role:
        if arguments.clock_plan is None:
            raise ValueError("prepared " + role + " needs an independently approved explicit --clock-plan; physical slice index mapping is pending")
        if installed_clock.load_plan(arguments.clock_plan).workload.get("preparedRole") != role:
            raise ValueError("frozen prepared role differs from the explicit plan")
        return
    if arguments.clock_plan is not None:
        return
    mode = installed_clock.scenario(arguments)
    key, context = selection(native, mode, arguments.scenario, os.environ.get("MESH_CATALOG_EVIDENCE") == "1")
    path, _plan = select_plan(arguments.clock_plan_index, key, context)
    arguments.clock_plan = path


def required_profiles(cohort, support, adoption_scenario, catalog):
    profiles = []
    for platform in PLATFORMS:
        if support:
            profiles.extend((selection(platform, "adoption", adoption_scenario), selection(platform, "fresh")))
        if cohort:
            profiles.extend(selection(platform, mode, catalog=catalog and platform.startswith("linux/") and mode == "oldest")
                            for mode in COHORT_MODES if mode != "order" or platform == "linux/amd64")
    return profiles


def workflow_budgets(args):
    profiles = required_profiles(args.cohort, args.support, args.adoption_scenario, args.catalog_evidence)
    values = {}
    for key, context in profiles:
        _path, plan = select_plan(args.index, key, context)
        values[key] = workflow_minutes(plan)
    return values


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    budgets = commands.add_parser("workflow-budgets")
    budgets.add_argument("--index", type=Path, default=INDEX)
    budgets.add_argument("--cohort", action="store_true")
    budgets.add_argument("--support", action="store_true")
    budgets.add_argument("--adoption-scenario", choices=SCENARIOS)
    budgets.add_argument("--catalog-evidence", action="store_true")
    args = parser.parse_args()
    try:
        print(json.dumps(workflow_budgets(args), separators=(",", ":")))
    except (OSError, ValueError, TypeError, KeyError) as error:
        parser.exit(2, str(error) + "\n")


if __name__ == "__main__":
    main()
