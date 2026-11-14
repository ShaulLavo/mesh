#!/usr/bin/env python3
"""Job-scoped EPERM control and conservative payload cleanup for each parallel AMD64 shard."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import sys

from fixtures import linux_eperm_control as control, release_worker_eperm as eperm


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def require_host():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
        raise ValueError("EPERM integration is hosted-only; use mocked controls locally")
    if sys.platform != "linux" or os.uname().machine != "x86_64":
        raise ValueError("EPERM integration requires Linux AMD64")


def initialize(root, shard):
    require_host()
    if type(shard) is not int or shard not in range(3):
        raise ValueError("normal AMD64 proof requires shard 0, 1 or 2")
    provenance = {"run": os.environ["GITHUB_RUN_ID"], "attempt": os.environ["GITHUB_RUN_ATTEMPT"],
                  "job": os.environ["GITHUB_JOB"], "platform": "linux/amd64", "shard": shard}
    root.mkdir(parents=True, exist_ok=True)
    # Exclusive creation refuses a second initialization, including an interrupted one.
    with (root / "eperm-state.json").open("x") as stream:
        json.dump({"controlAttempted": True, "proofAttempted": False}, stream)
    with (root / "eperm-ledger.json").open("x") as stream:
        json.dump({"schema": 1, "cap": None, "provenance": provenance, "events": []}, stream)
    control.positive_control(root / "positive-control.json")


def read_state(root):
    value = json.loads((root / "eperm-state.json").read_bytes())
    if not isinstance(value, dict) or value.get("controlAttempted") is not True:
        raise ValueError("missing job ownership state")
    return value


def scratch_path(value):
    path = Path(value)
    if path.parent != Path("/tmp") or not re.fullmatch(r"ms-[a-zA-Z0-9_]+\.noindex", path.name) or path.is_symlink():
        raise ValueError("scratch ownership path differs; preserve payloads")
    if path.exists() and path.stat().st_uid != os.getuid():
        raise ValueError("scratch owner differs; preserve payloads")
    return path


def begin(root, scratch):
    state = read_state(root)
    positive = json.loads((root / "positive-control.json").read_bytes())
    if positive.get("passed") is not True or positive.get("cleanupConfirmed") is not True:
        raise ValueError("healthy control did not pass")
    if state.get("proofAttempted"):
        raise ValueError("proof unit already attempted; ledger cannot reset")
    expected = root / "eperm-ledger.json"
    if Path(os.environ.get("MESH_WORKER_EPERM_LEDGER", "")) != expected:
        raise ValueError("proof must inherit the single output-root ledger")
    value = json.loads(expected.read_bytes())
    eperm.validate_ledger(value)
    if value["cap"] is not None:
        raise ValueError("normal CI requires an explicit null count gate")
    audit = eperm.diagnostic_cleanup(expected, required=True)
    if not audit["confirmed"] or audit["events"] != 0:
        raise ValueError("initial shared ledger is missing, corrupt or already used")
    state.update(proofAttempted=True, scratch=str(scratch_path(scratch)), proofReturned=False)
    write(root / "eperm-state.json", state)


def returned(root, status):
    state = read_state(root)
    state.update(proofReturned=True, proofStatus=status)
    write(root / "eperm-state.json", state)


def control_cleanup(root):
    value = json.loads((root / "positive-control.json").read_bytes())
    audit = eperm.snapshot_cleanup(value.get("snapshot", {}))
    confirmed = value.get("cleanupConfirmed") is True and value.get("childReturncode") is not None and audit["confirmed"]
    return {"confirmed": confirmed, "knownChildPid": value.get("knownChildPid"),
            "childReturncode": value.get("childReturncode"), "cleanupError": value.get("cleanupError"), "snapshot": audit}


def native_cleanup(root, state):
    if not state.get("proofAttempted"):
        return {"confirmed": True, "reason": "native proof never attempted"}
    # The original shell traps own native processes. Do not signal or reconcile them here.
    selected = sorted((root / "proofs/groups").glob("*/*/*/selected.json"))
    records = [{"path": str(path.parent / "cleanup.json"),
                "cleanup": json.loads((path.parent / "cleanup.json").read_bytes())} for path in selected]
    groups_clean = all(row["cleanup"].get("processesAbsent") is True and row["cleanup"].get("stateAbsent") is True for row in records)
    short = scratch_path(state["scratch"])
    scratch_empty = not short.exists() or short.is_dir() and not any(short.iterdir())
    confirmed = state.get("proofReturned") is True and state.get("proofStatus") == 0 and bool(records) and groups_clean and scratch_empty
    return {"confirmed": confirmed, "groups": records, "scratchEmpty": scratch_empty,
            "reason": "successful runner and original group traps" if confirmed else "native ownership unconfirmed; original shell traps own teardown"}


def decision(root, native):
    result = {"payloadsMayBeRemoved": False, "paths": [str(native)], "scope": "recorded control, diagnostics and native proof only"}
    # Audit independently so missing native/control records cannot hide raw diagnostic PIDs.
    result["diagnosticCleanup"] = eperm.diagnostic_cleanup(root / "eperm-ledger.json", required=True)
    for name, operation in (("controlCleanup", lambda: control_cleanup(root)),
                            ("nativeCleanup", lambda: native_cleanup(root, read_state(root)))):
        try:
            result[name] = operation()
        except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
            result[name] = {"confirmed": False, "error": type(error).__name__ + ": " + str(error)}
    result["payloadsMayBeRemoved"] = all(result[name]["confirmed"] for name in ("diagnosticCleanup", "controlCleanup", "nativeCleanup"))
    return result


def cleanup(root, native):
    root.mkdir(parents=True, exist_ok=True)
    result = decision(root, native)
    if native.is_symlink() or native != root.parent / "mesh-native.noindex":
        result.update(payloadsMayBeRemoved=False, pathError="native payload path differs")
    write(root / "eperm-cleanup.json", result)
    audit = result["diagnosticCleanup"]
    counter = recurrence_counter(root, audit)
    write(root / "eperm-counter.json", counter)
    for recurrence in counter.get("recurrences", []):
        print("EPERM retained recurrence: " + json.dumps(recurrence), flush=True)
    print(json.dumps({"EPERM": audit.get("events"), "cap": counter.get("cap"), "payloadsMayBeRemoved": result["payloadsMayBeRemoved"]}), flush=True)
    if not result["payloadsMayBeRemoved"]:
        raise RuntimeError("cleanup ownership unconfirmed; payloads and proof evidence preserved")
    state = read_state(root)
    paths = [native]
    if state.get("scratch"):
        paths.append(scratch_path(state["scratch"]))
    for path in paths:
        if path.exists():
            shutil.rmtree(path)
    result.update(payloadsAbsent=all(not path.exists() for path in paths), removed=list(map(str, paths)))
    write(root / "eperm-cleanup.json", result)


def recurrence_counter(root, audit):
    result = {"schema": 1, "events": audit.get("events"), "investigationThreshold": 2,
              "ledgerConfirmed": audit["confirmed"], "ledgerError": audit.get("error")}
    try:
        ledger = json.loads((root / "eperm-ledger.json").read_bytes())
        eperm.validate_ledger(ledger)
        result.update(cap=ledger["cap"], provenance=ledger.get("provenance"), events=len(ledger["events"]))
        result["recurrences"] = [recurrence_summary(root, index, event) for index, event in enumerate(ledger["events"], 1)]
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        result["counterError"] = type(error).__name__ + ": " + str(error)
    return result


def recurrence_summary(root, index, event):
    failure = event.get("failure", {})
    return {"index": index, "session": event.get("session"), "version": event.get("version"), "digest": event.get("digest"),
            "originalPid": failure.get("pid"), "originalOutcome": failure.get("outcome"),
            "originalException": failure.get("exception"), "retry": event.get("retry"), "cohort": failure.get("cohort"),
            "sourceId": source_id(root, failure.get("cohort"))}


def source_id(root, cohort):
    if not isinstance(cohort, str):
        return None
    try:
        parts = Path(cohort).relative_to(root / "proofs/groups").parts
    except ValueError:
        return None
    return parts[0] if len(parts) >= 4 and re.fullmatch(r"[0-9a-f]{64}", parts[0]) else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("initialize", "begin", "returned", "cleanup"))
    parser.add_argument("root", type=Path)
    parser.add_argument("value", nargs="?")
    parser.add_argument("--shard", type=int)
    args = parser.parse_args()
    root = args.root.resolve()
    if args.operation == "initialize":
        initialize(root, args.shard)
    if args.operation == "begin":
        begin(root, args.value)
    if args.operation == "returned":
        returned(root, int(args.value))
    if args.operation == "cleanup":
        cleanup(root, Path(args.value))


if __name__ == "__main__":
    main()
