"""Synthetic span plans for pure controls. Never used by fixture dispatch."""
from dataclasses import asdict, replace
import json
from pathlib import Path

from fixtures import installed_clock as clocks, installed_spans as spans

ROOT = Path(__file__).resolve().parents[2] / ".internal/fixtures/installed-clock"


def reviewed_for_test(name="linux-amd64-fresh"):
    value = json.loads((ROOT / (name + ".json")).read_text())
    value["eligibility_review"] = "TEST ONLY: control; no independent eligibility approval"
    value["phases"] = tuple(clocks.load_budget(row) for row in value["phases"])
    value["guards"] = tuple(clocks.load_budget(row) for row in value["guards"])
    value["external_budget"] = clocks.load_budget(value["external_budget"])
    return clocks.ClockPlan(**value)


def record(workload, durations=None):
    keys = [key for chunk in spans.canonical_chunks(workload) for key in chunk]
    durations = durations or [clocks.MILLISECOND] * len(keys)
    leaves, cursor = [], clocks.SECOND
    for key, duration in zip(keys, durations):
        leaves.append({"key": key, "start_ns": cursor, "end_ns": cursor + duration, "elapsed_ns": duration,
                       "completed": True, "eligible": True, "cleanup_verified": True})
        cursor += duration
    by_key = {row["key"]: row for row in leaves}
    samples = []
    for chunk in spans.canonical_chunks(workload)[:-1]:
        start, end = by_key[chunk[1]]["start_ns"], by_key[chunk[-1]]["end_ns"]
        samples.append(by_key[chunk[1]] | {"end_ns": end, "elapsed_ns": end - start})
    return {"workload": workload, "provenance": {"testOnly": True}, "leaves": leaves, "samples": samples,
            "measurement_completed": True, "failures": [], "missing_sample_keys": [], "unresolved_key_sets": [],
            "origin_ns": clocks.SECOND, "end_ns": cursor, "command_ns": cursor - clocks.SECOND, "disjoint_ns": cursor - clocks.SECOND}


def observed(value, identity="a"):
    return {"fixture": value, "source": {"path": "/test-only/" + identity, "sha256": identity * 64}}


def synthetic_plan(workload, external):
    phases = clocks.reduce_span_budgets([observed(record(workload))])
    return clocks.ClockPlan(mode="frozen", workload=workload, phases=phases, command_ns=sum(row.limit_ns for row in phases),
                            external_ns=external.limit_ns, external_budget=external, emergency_ns=clocks.SECOND,
                            eligibility_review="TEST ONLY: control; no eligibility approval", admission_ns=1175 * clocks.SECOND)


def leaf_keys(plan):
    return [key for row in plan.phases for key in row.workload["clockSpan"]["leaves"]]


def with_parts(plan, parts):
    phases = []
    for phase, part in zip(plan.phases, parts):
        workload = plan.workload | {"clockSpan": {"leaves": part}}
        samples = tuple(replace(sample, workload=workload) for sample in phase.samples)
        phases.append(replace(phase, key="span:" + part[0] if part else "span:empty", workload=workload, samples=samples))
    return replace(plan, phases=tuple(phases))


def write_plan(path, plan):
    Path(path).write_text(json.dumps(asdict(plan)))
