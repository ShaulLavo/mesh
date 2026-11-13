"""Prove ptrace lifecycle accounting with bounded owned Go processes."""
import json
from pathlib import Path
import sys


HERE = Path(__file__).resolve().parent
MODES = ("lifecycle-thread-exit", "lifecycle-exit-group", "lifecycle-kill", "lifecycle-exec")


def check(output, name, worker):
    rows = [json.loads(line) for line in (output / f"{name}.jsonl").read_text().splitlines()]
    control = json.loads((output / f"{name}.control.json").read_bytes())
    if rows[-1] != {"event": "teardown", "remainingTasks": 0, "observerError": ""}:
        raise RuntimeError("lifecycle control has unproved coverage or cleanup")
    if rows[0]["root"] != control["parent"] or control["exit"] != control["expected"]:
        raise RuntimeError("lifecycle control lost its direct launcher or expected outcome")
    st = worker.stat()
    identity = f"{st.st_dev}:{st.st_ino}:{st.st_size}"
    executed = [row for row in rows if row["event"] == "exec" and row["tid"] == control["worker"] and row["image"] == identity]
    if len(executed) != 1 or executed[0]["creatorTid"] != control["parent"]:
        raise RuntimeError("lifecycle control has no exact worker exec")
    threads = [row for row in rows if row["event"] == "created" and row["tgid"] == control["worker"] and not row["preExec"]]
    if len(threads) < 8:
        raise RuntimeError("lifecycle control did not exercise Go thread churn")
    if control["mode"] == "lifecycle-exec" and not any(row["event"] == "exec" and row.get("formerTid") != row["tid"] for row in rows):
        raise RuntimeError("lifecycle exec did not replace a nonleader TID")
    races = {row["tid"] for row in rows if row["event"] == "terminal-race"}
    resolved = {row["tid"] for row in rows if row["event"] == "terminal-race-resolved"}
    if races != resolved or any(row.get("criticalReturnUnconfirmed") for row in rows):
        raise RuntimeError("lifecycle race lost a relevant return or terminal proof")
    return {"passed": True, "mode": control["mode"], "threads": len(threads), "terminalRaces": len(races),
            "records": len(rows), "worker": control["worker"]}


def exercise(output, worker, observe):
    output.mkdir()
    results = []
    for index in range(3):
        for mode in MODES:
            results.append(exercise_one(output, worker, observe, mode, index))
    (output / "results.json").write_text(json.dumps(results, indent=2) + "\n")
    return results


def exercise_one(output, worker, observe, mode, index):
    name = f"{index}-{mode}"
    argv = [sys.executable, str(HERE / "lifecycle-launcher.py"), str(worker), mode, str(output / f"{name}.control.json")]
    if observe(output, name, argv, 25):
        raise RuntimeError("synthetic lifecycle observer failed")
    return check(output, name, worker)
