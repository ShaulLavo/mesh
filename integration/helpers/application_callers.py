"""Two application callers on one account cannot reach each other's work."""
import base64
import json
import os
from pathlib import Path
import subprocess
import sys

from durable_create import drop_reply
from durable_output import bound
from pipe_execution import PipeFixture, exchange
from terminal_window import eventually, print_diagnostics


JOB = 'cat; printf "%s " "$GREETING"; pwd -P'


def mesh(fixture, *args, ok=True):
    args = list(args)
    split = args.index("--") if "--" in args else len(args)
    args.insert(split, "--json")
    result = subprocess.run([fixture.binary, *args], env=fixture.environment,
                            stdin=subprocess.DEVNULL, capture_output=True, text=True,
                            check=False, timeout=30)
    assert result.stdout, (args, result.returncode, result.stderr)
    envelope = json.loads(result.stdout)
    if ok:
        assert result.returncode == 0 and envelope["status"] == "ok", (args, result.stdout, result.stderr)
        return envelope["data"]
    assert result.returncode != 0, (args, result.stdout)
    return envelope["errors"][0]["code"]


def credential(fixture, name):
    return json.loads((fixture.config / "mcp" / "identities" / f"{name}.json").read_text())["credential"]


def credential_never_printed(fixture, *outputs):
    identities = fixture.config / "mcp" / "identities"
    for path in identities.glob("*.json"):
        secret = json.loads(path.read_text())["credential"]
        for text in outputs:
            assert secret not in text, f"credential {path.name} leaked"


def scenario(fixture):
    host = fixture.ready()["id"]
    created = mesh(fixture, "mcp", "identity", "create", "alice")
    mesh(fixture, "mcp", "identity", "create", "bob")
    assert mesh(fixture, "mcp", "identity", "create", "alice", ok=False) == "usage.invalid"
    assert {i["name"] for i in mesh(fixture, "mcp", "identity", "ls")["identities"]} == {"alice", "bob"}
    assert oct(Path(created["path"]).stat().st_mode & 0o777) == "0o600"

    def ctx(identity, *args, ok=True):
        return mesh(fixture, "context", *args, "--host", host, "--identity", identity, ok=ok)

    work = fixture.root / "work"
    (work / "sub").mkdir(parents=True)
    alice = ctx("alice", "open", "--request-key", "ctx", "--cwd", str(work), "--shell", "sh", "--env", "GREETING=hi")
    bob = ctx("bob", "open", "--request-key", "ctx", "--cwd", str(work))
    assert alice["contextId"] != bob["contextId"] and alice["envNames"] == ["GREETING"]
    assert "hi" not in json.dumps(alice)
    replay = ctx("alice", "open", "--request-key", "ctx", "--cwd", str(work), "--shell", "sh", "--env", "GREETING=hi")
    assert replay["contextId"] == alice["contextId"]
    assert ctx("alice", "open", "--request-key", "ctx", "--cwd", str(work / "sub"), ok=False) == "request.key_conflict"
    assert ctx("bob", "get", alice["contextId"], ok=False) == "context.not_found"
    assert ctx("bob", "close", alice["contextId"], ok=False) == "context.not_found"
    assert [c["contextId"] for c in ctx("bob", "ls")["contexts"]] == [bob["contextId"]]

    def job(identity, context, key, *argv, shell=None, ok=True):
        args = ["session", "create", "--host", host, "--identity", identity, "--context", context,
                "--kind", "pipe", "--stdin", "open", "--request-key", key]
        args += ["--shell-command", shell] if shell else ["--", *argv]
        return mesh(fixture, *args, ok=ok)

    running = job("alice", alice["handle"], "job", shell=JOB)
    assert running["contextId"] == alice["contextId"] and running["contextRevision"] == "1"
    assert job("bob", alice["contextId"], "job", shell="true", ok=False) == "context.not_found"
    bob_lookup = ["session", "request", "--host", host, "--identity", "bob", "--context", alice["contextId"], "--request-key", "job"]
    assert mesh(fixture, *bob_lookup, ok=False) == "request.not_found"

    target = ("--host", host, running["sessionId"], "--instance", running["instance"])
    for identity, context in (("bob", None), ("bob", bob["contextId"]), ("alice", bob["contextId"])):
        scope = ["--identity", identity] + (["--context", context] if context else [])
        for command in (["logs", "--stream", "stdout"], ["wait", "--timeout", "200ms"],
                        ["input", "--write-key", "x", "--offset", "0", "--text", "no"],
                        ["signal", "--signal", "term"], ["kill"], ["rm"]):
            code = mesh(fixture, "session", command[0], *target, *command[1:], *scope, ok=False)
            assert code == "access.denied", (identity, context, command, code)

    # The raw controls refuse the other caller's credential too.
    bob_caller = credential(fixture, "bob")
    for kind, extra in (("inspect", {}), ("logs", {"stream": "stdout", "maxBytes": 64}), ("kill", {})):
        swapped = exchange(fixture, bound(kind, running, caller=bob_caller, **extra))
        assert swapped["errorCode"] == "access.denied" and not swapped.get("executionReceipt"), swapped
    # A frontend that forgets its credential gets no account administration.
    anonymous = exchange(fixture, bound("logs", running, stream="stdout", maxBytes=64))
    assert anonymous["errorCode"] == "access.denied", anonymous
    stray = exchange(fixture, {"type": "session.list", "requestId": "stray", "caller": "mac1_x"})
    assert stray["errorCode"] == "usage.invalid", stray

    # Explicit whole-account CLI administration keeps working.
    assert mesh(fixture, "session", "inspect", *target)["phase"] == "live"
    mesh(fixture, "session", "logs", *target, "--stream", "stdout")

    # Edits after reservation never change the job; revisions conflict.
    updated = ctx("alice", "update", alice["handle"], "--expected-revision", "1", "--cwd", str(work / "sub"))
    assert updated["revision"] == "2"
    assert ctx("alice", "update", alice["handle"], "--expected-revision", "1", "--label", "x", ok=False) == "context.conflict"

    owned = ("--identity", "alice", "--context", alice["contextId"])
    mesh(fixture, "session", "input", *target, *owned, "--write-key", "eof", "--offset", "0", "--eof")
    done = eventually(lambda: finished(fixture, target, owned), "alice's job did not finish")
    assert done["receipt"]["result"]["termination"]["code"] == 0
    output = mesh(fixture, "session", "logs", *target, *owned, "--stream", "stdout")
    # The job kept the revision it was reserved against: the first directory.
    text = base64.b64decode(output["bytesBase64"]).decode()
    assert text == f"hi {os.path.realpath(work)}\n", text

    # Equal input in a distinct context is a distinct job; closure keeps receipts.
    second = ctx("alice", "open", "--request-key", "ctx-2", "--cwd", str(work))
    again = job("alice", second["contextId"], "job", shell=JOB)
    assert again["sessionId"] != running["sessionId"]
    closed = ctx("alice", "close", alice["handle"])
    assert closed["state"] == "closed"
    lookup = mesh(fixture, "session", "request", "--host", host, *owned, "--request-key", "job")
    assert lookup["instance"] == running["instance"] and lookup["contextRevision"] == "1"
    assert job("alice", alice["contextId"], "new", "/bin/true", ok=False) == "context.closed"
    assert ctx("alice", "receipt", "--request-key", "ctx")["contextId"] == alice["contextId"]

    lost_replies(fixture, host, work)

    fixture.crash_daemon()
    fixture.start_local_daemon()
    assert ctx("alice", "get", alice["handle"])["state"] == "closed"
    assert ctx("bob", "get", alice["handle"], ok=False) == "context.not_found"
    mesh(fixture, "session", "input", "--host", host, again["sessionId"], "--instance", again["instance"],
         "--identity", "alice", "--context", second["contextId"], "--write-key", "eof", "--offset", "0", "--eof")
    credential_never_printed(fixture, (fixture.root / "daemon.log").read_text(errors="replace"))


def lost_replies(fixture, host, work):
    """A fresh frontend recovers an open and a create whose replies were lost."""
    alice = credential(fixture, "alice")
    drop_reply(fixture, {"type": "context.open", "requestId": "lost-open", "requestKey": "lost",
                         "caller": alice, "executionContext": {"open": {"cwd": str(work)}}})
    opened = eventually(lambda: exchange(fixture, {"type": "context.receipt", "requestId": "find",
                                                   "requestKey": "lost", "caller": alice}).get("contextInfo"),
                        "lost context open was not recorded")
    scope = {"caller": alice, "executionContext": {"contextId": opened["contextId"]}}
    create = dict(type="execution.create", requestId="lost-create", requestKey="lost-job", kind="pipe",
                  command=["/bin/true"], **scope)
    drop_reply(fixture, create)
    lookup = dict(type="execution.request", requestId="find-job", requestKey="lost-job", **scope)
    found = eventually(lambda: exchange(fixture, lookup).get("executionReceipt"), "lost create was not recorded")
    assert exchange(fixture, create)["executionReceipt"]["instance"] == found["instance"]
    changed = exchange(fixture, dict(create, command=["/bin/false"]))
    assert changed["errorCode"] == "request.key_conflict", changed
    other = exchange(fixture, dict(lookup, caller=credential(fixture, "bob")))
    assert other["errorCode"] == "request.not_found", other


def finished(fixture, target, owned):
    observed = mesh(fixture, "session", "wait", *target, *owned, "--timeout", "2s")
    return observed if observed.get("wait") == "completed" else None


def main():
    root = Path(os.environ["HOME"]).parent / "application-callers"
    root.mkdir()
    fixture = PipeFixture(sys.argv[1], root)
    try:
        fixture.start_local_daemon()
        scenario(fixture)
    except BaseException:
        print_diagnostics(fixture)
        raise
    finally:
        fixture.close()
    print("PASS: application callers are isolated on one account; contexts replay, conflict, close and survive restart")


if __name__ == "__main__":
    main()
