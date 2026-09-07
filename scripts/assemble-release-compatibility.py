#!/usr/bin/env python3
import argparse
import hashlib
import json
import pathlib
import sys


def fail(message: str) -> None:
    raise SystemExit(f"release compatibility: {message}")


parser = argparse.ArgumentParser()
parser.add_argument("proofs", type=pathlib.Path)
parser.add_argument("output", type=pathlib.Path)
args = parser.parse_args()

transitions = []
receipts = []
for path in sorted(args.proofs.glob("*.json")):
    contents = path.read_bytes()
    digest = hashlib.sha256(contents).hexdigest()
    if path.name != f"{digest}.json":
        fail(f"{path.name} is not named for its SHA-256")
    receipt = json.loads(contents)
    if receipt.get("schema") != 1:
        fail(f"{path.name} has unsupported schema")
    if not all(receipt.get(field) is True for field in (
        "retainedOpenedCandidateState", "sessionsPreserved", "recoveryRecordsPreserved"
    )):
        fail(f"{path.name} did not pass every rollback check")
    for field in (
        "stateReadMin", "stateReadMax", "stateWrite", "workerMin",
        "workerMax", "workerWrite", "journalVersion",
    ):
        if not isinstance(receipt.get(field), int) or receipt[field] <= 0:
            fail(f"{path.name} has invalid {field}")
    if receipt["stateWrite"] != receipt["stateReadMax"]:
        fail(f"{path.name} did not observe the candidate's reported state schema")
    receipts.append(receipt)
    transitions.append({
        "fromDigest": receipt["fromDigest"],
        "toDigest": receipt["toDigest"],
        "platform": receipt["platform"],
        "proof": digest,
    })

if not transitions:
    fail("no transition receipts found")

candidate_fields = ("stateReadMax", "stateWrite", "workerMax", "workerWrite", "journalVersion")
for field in candidate_fields:
    values = {receipt[field] for receipt in receipts}
    if len(values) != 1:
        fail(f"transition receipts disagree on {field}: {sorted(values)}")

compatibility = {
    "stateReadMin": min(receipt["stateReadMin"] for receipt in receipts),
    "stateReadMax": receipts[0]["stateReadMax"],
    "stateWrite": receipts[0]["stateWrite"],
    "workerMin": min(receipt["workerMin"] for receipt in receipts),
    "workerMax": receipts[0]["workerMax"],
    "workerWrite": receipts[0]["workerWrite"],
    "journalVersion": receipts[0]["journalVersion"],
    "transitions": transitions,
}
args.output.parent.mkdir(parents=True, exist_ok=True)
temporary = args.output.with_name(f".{args.output.name}.tmp")
temporary.write_text(json.dumps(compatibility, indent=2, sort_keys=True) + "\n")
temporary.replace(args.output)
