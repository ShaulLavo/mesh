#!/usr/bin/env python3
"""Re-reduce retained installed plans into local, non-executable span drafts."""
import argparse
from dataclasses import asdict, replace
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import types

from fixtures import installed_clock as current

BASE = "fa3c1fe723825012e61755abeb3ddd9bd05fe33d"
DIRECTORY = ".internal/fixtures/installed-clock/"
PENDING = "Fresh independent numerical and code review required before parent activation."


def require(condition, message):
    if not condition:
        raise ValueError(message)


def source_bytes(tree, source, path):
    return subprocess.check_output(["git", "-C", str(tree), "show", source + ":" + path])


def historical_api(tree, source):
    raw = source_bytes(tree, source, "scripts/fixtures/installed_clock.py")
    module = types.ModuleType("historical_clock_" + source)
    sys.modules[module.__name__] = module
    exec(compile(raw, source + ":installed_clock.py", "exec"), module.__dict__)
    return module


def plan_shape(value, api):
    converted = value | {"phases": tuple(api.load_budget(row) for row in value["phases"]),
                         "guards": tuple(api.load_budget(row) for row in value["guards"]),
                         "external_budget": api.load_budget(value["external_budget"])}
    return api.ClockPlan(**converted)


def observations(plan, tree):
    refs, result = {}, []
    for sample in plan["phases"][0]["samples"]:
        ref = sample["provenance"]["fixture_record"]
        identity = (ref["path"], ref["sha256"])
        require(identity not in refs, "duplicate retained invocation")
        raw = Path(ref["path"]).read_bytes()
        require(hashlib.sha256(raw).hexdigest() == ref["sha256"], "retained fixture hash differs")
        record = json.loads(raw)
        api = historical_api(tree, record["provenance"]["harness"])
        validate_original(plan, record, api, ref)
        refs[identity] = True
        result.append({"fixture": record, "source": ref, "provenance": sample["provenance"]})
    return result


def validate_original(plan, record, api, ref):
    require(plan["workload"] == record["workload"], "original workload differs")
    require([row["key"] for row in plan["phases"]] == [row["key"] for row in record["leaves"]], "original leaf sequence differs")
    current.span_intervals(record)
    by_key = {row["key"]: row for row in record["leaves"]}
    for phase in plan["phases"]:
        matching = [sample for sample in phase["samples"] if sample["provenance"]["fixture_record"] == ref]
        require(len(matching) == 1, "original phase lacks exact invocation")
        sample = api.SampleRef(**matching[0])
        require(sample.eligible(record["workload"]) and sample.elapsed_ns == by_key[phase["key"]]["elapsed_ns"],
                "historical sample eligibility/interval differs")


def migrate(plan, tree):
    original = plan_shape(plan, historical_api(tree, BASE))
    original.validate()
    observed = observations(plan, tree)
    phases = current.reduce_span_budgets(observed)
    candidate = plan | {"phases": [asdict(row) for row in phases], "command_ns": sum(row.limit_ns for row in phases),
                        "eligibility_review": ""}
    require(candidate["command_ns"] <= plan["command_ns"], "span migration increases command allowance")
    require(candidate["guards"] == plan["guards"], "guard objects changed")
    unchanged = set(plan) - {"phases", "command_ns", "eligibility_review"}
    require(all(candidate[key] == plan[key] for key in unchanged), "external/reserve/admission/workload changed")
    shape = plan_shape(candidate, current)
    replace(shape, eligibility_review="TEST ONLY: span shape; no eligibility approval").validate()
    try:
        shape.validate()
    except ValueError as error:
        require("reviewed eligibility" in str(error), "unexpected draft rejection")
        return candidate, observed
    raise ValueError("draft became executable before review")


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--write-drafts", action="store_true", help="replace this exclusive tree's numeric files with non-executable drafts")
    args = parser.parse_args()
    tree = Path(__file__).resolve().parents[1]
    index_raw = source_bytes(tree, BASE, DIRECTORY + "index.json")
    require((tree / DIRECTORY / "index.json").read_bytes() == index_raw, "index selections changed")
    index = json.loads(index_raw)
    args.output.mkdir(parents=True, exist_ok=True)
    rows, sources = [], set()
    for name, entry in index["plans"].items():
        raw = source_bytes(tree, BASE, DIRECTORY + entry["path"])
        old = json.loads(raw)
        candidate, observed = migrate(old, tree)
        identities = {(row["source"]["path"], row["source"]["sha256"]) for row in observed}
        require(not sources.intersection(identities), "source invocation reused by different plans")
        sources.update(identities)
        destination = args.output / (name + ".candidate-plan.json")
        write_json(destination, candidate)
        if args.write_drafts:
            write_json(tree / DIRECTORY / entry["path"], candidate)
        rows.append({"name": name, "candidate": destination.name, "candidate_sha256": hashlib.sha256(destination.read_bytes()).hexdigest(),
                     "original_plan_sha256": hashlib.sha256(raw).hexdigest(), "original_eligibility_review": old["eligibility_review"],
                     "sources": [row["source"] for row in observed], "leaves": len(old["phases"]), "spans": len(candidate["phases"]),
                     "guards_preserved": len(old["guards"]), "previous_command_ns": old["command_ns"], "command_ns": candidate["command_ns"],
                     "measured_ns": [row["fixture"]["command_ns"] for row in observed]})
    write_json(args.output / "PACKET.json", {"kind": "operation-span-candidates", "review_pending": PENDING, "runnable": False,
        "accepted": False, "baseline": BASE, "index_sha256": hashlib.sha256(index_raw).hexdigest(), "plans": rows,
        "missing_consumers": ["linux-amd64-chain-consume-final", "linux-arm64-chain-consume-none",
                              "linux-arm64-waypoint-consume-final", "darwin-arm64-chain-consume-none"],
        "historical_reviews_transfer_to_span_semantics": False})
    print(f"{len(rows)} drafts, {sum(row['leaves'] for row in rows)} leaves, {sum(row['spans'] for row in rows)} spans, "
          f"{sum(row['guards_preserved'] for row in rows)} unchanged guards. {PENDING}")


if __name__ == "__main__":
    main()
