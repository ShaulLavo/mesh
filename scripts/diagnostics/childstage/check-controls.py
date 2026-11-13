"""Assert exact denied calls, owned lineage, mapped-image binding and teardown."""
import argparse
import hashlib
import json
import os
from pathlib import Path


def check(directory, worker):
    result = {}
    image = os.stat(worker)
    identity = f"{image.st_dev}:{image.st_ino}:{image.st_size}"
    digest = hashlib.sha256(worker.read_bytes()).hexdigest()
    for mode, number in (("parent", 56), ("child", 112), ("terminal", 16), ("clone3", 435)):
        result[mode] = check_mode(directory, mode, number, identity, digest)
    return result


def check_mode(directory, mode, number, identity, digest):
    rows = [json.loads(line) for line in (directory / f"{mode}.jsonl").read_text().splitlines()]
    control = json.loads((directory / f"{mode}.control.json").read_bytes())
    if control["go"] != "go1.27.0":
        raise RuntimeError("control compiler differs from actual failed image")
    if control["build"] != {"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "-compiler": "gc",
                            "github.com/charmbracelet/x/xpty": "v0.1.4", "github.com/creack/pty": "v1.1.24"}:
        raise RuntimeError("control build or PTY dependencies differ from the failed image attributes")
    armed = rows[0]
    if armed["event"] != "armed" or armed["root"] != control["parent"]:
        raise RuntimeError("observer was not armed on the Python launcher")
    matches = [row for row in rows if row.get("event") == "return" and row["result"] == -1
               and row["call"]["number"] == number and real_launch(row, mode)]
    if len(matches) != 1:
        raise RuntimeError(f"{mode}: missing or ambiguous exact denied call")
    denied = matches[0]
    check_identity(rows, control, denied, identity, mode)
    if armed["backend"] == "ptrace" and denied.get("mappedImage", {}).get("sha256") != digest:
        raise RuntimeError("observer did not retain the actual failed task's mapped-image bytes")
    if rows[-1] != {"event": "teardown", "remainingTasks": 0, "observerError": ""}:
        raise RuntimeError("observer did not finish with proven teardown")
    owned = {row["tid"] for row in rows if row.get("event") == "created"}
    owned.add(armed["root"])
    for pid in owned:
        if Path(f"/proc/{pid}").exists():
            raise RuntimeError(f"owned synthetic PID {pid} remains")
    return {"passed": True, "syscall": number, "tid": denied["tid"], "tgid": denied["tgid"],
            "worker": control["worker"], "sequence": denied["sequence"], "records": len(rows),
            "teardown": True, "compiler": control["go"], "image": identity, "sha256": digest}


def real_launch(row, mode):
    # Go's pidfd capability probe has no SIGCHLD exit signal. Retain it in the
    # trace, but assert the actual os/exec clone as the positive control.
    return mode != "parent" or row["call"]["args"][0] & 0xFF == 17


def check_identity(rows, control, denied, identity, mode):
    worker = control["worker"]
    execs = [row for row in rows if row.get("event") == "exec" and row["tid"] == worker]
    if len(execs) != 1 or execs[0]["image"] != identity or execs[0]["creatorTid"] != control["parent"]:
        raise RuntimeError("worker exec lacks direct launcher lineage or mapped identity")
    if mode in ("child", "terminal"):
        check_child(rows, denied, worker)
        if mode == "terminal" and denied["call"]["args"][1] != 0x540E:
            raise RuntimeError("terminal control did not reach TIOCSCTTY")
        return
    if denied["tgid"] != worker or denied["tid"] != control["tid"] or denied["preExec"]:
        raise RuntimeError("parent refusal belongs to another task")
    children = [row for row in rows if row.get("event") == "created"
                and row["creatorTgid"] == worker and row["preExec"]
                and row.get("monotonicNs", 0) >= denied["call"]["enteredNs"]]
    if children:
        raise RuntimeError("refused parent clone produced a child")


def check_child(rows, denied, worker):
    children = [row for row in rows if row.get("event") == "created" and row["tid"] == denied["tid"]]
    if len(children) != 1 or children[0]["creatorTgid"] != worker or not denied["preExec"]:
        raise RuntimeError("child-side EPERM lacks its worker parent")
    context = denied["context"]
    if "kernel" in context:
        if context["kernel"]["seccomp"] != 2:
            raise RuntimeError("kernel control lacks its synthetic security context")
    elif not context["terminals"] or context["status"].get("Seccomp") != "2":
        raise RuntimeError("child control did not retain its PTY and injected filter context")
    if context["image"] != denied["image"]:
        raise RuntimeError("pre-exec child no longer maps its worker image")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("worker", type=Path)
    args = parser.parse_args()
    print(json.dumps(check(args.directory, args.worker), indent=2))
