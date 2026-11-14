#!/usr/bin/env python3
"""Summarize parallel AMD64 recurrence counters without a CI count gate."""
import argparse
import json
import os
from pathlib import Path


THRESHOLD = 2


def read_counter(path, run, attempt):
    value = json.loads(path.read_bytes())
    provenance = value["provenance"]
    if value.get("schema") != 1 or "cap" not in value or value["cap"] is not None:
        raise ValueError("normal CI counter must explicitly disable the count gate")
    if provenance["run"] != run or provenance["attempt"] != attempt or provenance["platform"] != "linux/amd64":
        raise ValueError("counter belongs to another run, attempt or platform")
    shard = provenance["shard"]
    if type(shard) is not int or shard not in range(3) or not provenance.get("job"):
        raise ValueError("counter has no physical AMD64 shard identity")
    rows = value["recurrences"]
    if type(value["events"]) is not int or value["events"] != len(rows):
        raise ValueError("counter total and retained recurrences differ")
    if not isinstance(rows, list) or any(type(row["index"]) is not int for row in rows):
        raise ValueError("counter recurrence indices are invalid")
    if [row["index"] for row in rows] != list(range(1, len(rows) + 1)):
        raise ValueError("counter recurrence indices are missing or duplicated")
    for row in rows:
        if not {"sourceId", "version", "digest", "session", "cohort"}.issubset(row):
            raise ValueError("counter recurrence source identities are incomplete")
    return shard, value


def aggregate(root, run, attempt):
    result = {"schema": 1, "run": run, "attempt": attempt, "platform": "linux/amd64", "cap": None,
              "investigationThreshold": THRESHOLD, "thresholdPurpose": "parent investigation only; never a CI count gate",
              "counters": [], "recurrences": [], "errors": []}
    counters = {}
    for path in sorted(root.rglob("eperm-counter.json")):
        try:
            shard, counter = read_counter(path, run, attempt)
            retain_counter(counters, shard, counter)
        except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
            result["errors"].append({"path": str(path), "error": type(error).__name__ + ": " + str(error)})
    for shard, counter in sorted(counters.items()):
        result["counters"].append(counter)
        result["recurrences"].extend(dict(row, run=run, runAttempt=attempt, shard=shard) for row in counter["recurrences"])
    result["missingShards"] = sorted(set(range(3)) - set(counters))
    result["complete"] = not result["errors"] and not result["missingShards"]
    result["total"] = len(result["recurrences"])
    result["investigationRequired"] = result["total"] >= THRESHOLD
    result["sourceIds"] = sorted({row["sourceId"] for row in result["recurrences"] if row.get("sourceId")})
    result["cohorts"] = sorted({row["cohort"] for row in result["recurrences"] if row.get("cohort")})
    result["workerIds"] = sorted({str(row.get("version")) + ":" + str(row.get("digest")) for row in result["recurrences"]})
    return result


def retain_counter(counters, shard, value):
    if shard in counters and counters[shard] != value:
        raise ValueError("conflicting counters for the same run, attempt and shard")
    counters[shard] = value


def render(value):
    lines = ["## AMD64 EPERM run summary", "",
             f"Retained EPERM events: {value['total']}. Investigation threshold: {THRESHOLD}.",
             "The threshold flags parent investigation and does not fail CI or stop later workers.",
             f"Investigation required: {value['investigationRequired']}. Complete counters: {value['complete']}.", ""]
    lines.extend(f"- Source {row['sourceId']}; worker {row['version']} {row['digest']}; shard {row['shard']}; event {row['index']}; session {row['session']}; cohort {row['cohort']}" for row in value["recurrences"])
    if not value["complete"]:
        lines.append("Counter evidence is incomplete or conflicting; see the JSON summary.")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--run", required=True)
    parser.add_argument("--attempt", required=True)
    args = parser.parse_args()
    result = aggregate(args.root, args.run, args.attempt)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    summary = render(result)
    print(summary, end="")
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a") as stream:
            stream.write(summary)
    if not result["complete"]:
        raise SystemExit("AMD64 recurrence counter evidence is incomplete or conflicting")


if __name__ == "__main__":
    main()
