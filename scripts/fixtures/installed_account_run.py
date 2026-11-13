"""Durable ownership and bounded supervision of the privileged account owner."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import uuid
from fixtures import installed_clock


def save(path, value):
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    descriptor = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def identity(pid):
    try:
        directory = Path(f"/proc/{pid}")
        fields = (directory / "stat").read_text().rsplit(")", 1)[1].split()
        return {"pid": pid, "uid": directory.stat().st_uid, "birth": fields[19],
                "boot": Path("/proc/sys/kernel/random/boot_id").read_text().strip()}
    except (FileNotFoundError, ProcessLookupError):
        return None


def alive(owned):
    return owned is not None and identity(owned["pid"]) == owned


def signal_bound(owned, number):
    if owned is None:
        return
    current = identity(owned["pid"])
    if current is None:
        return
    if current != owned:
        raise RuntimeError("account owner PID lifetime changed; refusing signal")
    os.kill(owned["pid"], number)


def claim(root, output, operation):
    ticket = json.loads((root / "account-run.json").read_text())
    if ticket["operation"] != operation or ticket["root"] != str(root.resolve()) or ticket["output"] != str(output.resolve()):
        raise RuntimeError("privileged account owner does not match its durable launch ticket")
    if not alive(ticket["supervisor"]) or hashlib.sha256((root / "ordered-input.json").read_bytes()).hexdigest() != ticket["descriptor"]:
        raise RuntimeError("account-run supervisor lifetime or immutable input changed")
    ticket["owner"] = identity(os.getpid())
    save(root / "account-run.json", ticket)
    return operation


@installed_clock.timed("ordered-owner")
def launch(root, output, arguments, timeout=480):
    operation = uuid.uuid4().hex
    ticket_path = root / "account-run.json"
    if ticket_path.exists():
        raise RuntimeError("refusing an existing privileged account-run ticket")
    descriptor = json.loads((root / "ordered-input.json").read_text())
    ticket = {"schema": 1, "operation": operation, "root": str(root.resolve()), "output": str(output.resolve()),
              "deadline_ns": min(descriptor["fixtureDeadlineNs"], installed_clock.parent_deadline(timeout)),
              "cleanup_deadline_ns": descriptor["fixtureCleanupDeadlineNs"],
              "supervisor": identity(os.getpid()), "descriptor": hashlib.sha256((root / "ordered-input.json").read_bytes()).hexdigest(), "owner": None}
    save(ticket_path, ticket)
    process = subprocess.Popen([*arguments, operation], start_new_session=True)
    ticket["launcher"] = identity(process.pid)
    # The child alone writes owner identity; never overwrite its claim.
    save(output / "account-launcher.json", ticket)
    supervise(process, ticket_path, output, installed_clock.remaining_until(ticket["deadline_ns"], timeout), privileged_signal, recover)


def supervise(process, ticket_path, output, timeout, signal_owner, recover_owner):
    failure = None
    try:
        status = process.wait(timeout=timeout)
        if status:
            failure = RuntimeError("privileged account owner exited unsuccessfully: " + str(status))
    except BaseException as error:
        failure = error
        installed_clock.failure(error)
        record_failure(ticket_path, output, error)
        stop_owner(process, ticket_path, signal_owner)
    if failure is not None and not (output / "account-run-failure.json").exists():
        record_failure(ticket_path, output, failure)
    ticket = json.loads(ticket_path.read_text())
    if failure is not None:
        installed_clock.failure_deadline(failure, ticket["cleanup_deadline_ns"])
    if alive(ticket.get("owner")):
        raise RuntimeError("privileged account owner remains alive; retaining its scratch")
    try:
        recover_owner(ticket_path)
    except BaseException as error:
        save(output / "account-recovery-failure.json", {"originalError": str(failure), "cleanupError": str(error), "retainedRoot": str(ticket_path.parent)})
        raise RuntimeError(str(failure or "ordered cleanup failed") + "; recovery failed: " + str(error)) from error
    verify_cleanup(ticket_path.parent, output, parent_receipt=False)
    save(output / "account-run-result.json", {"schema": 1, "operation": ticket["operation"], "ownerStopped": True, "cleanupVerified": True})
    if failure is not None:
        raise failure


def owner_failure(root, error, emergency_ns):
    path = root / "account-run.json"
    ticket = json.loads(path.read_text())
    if ticket.get("owner") != identity(os.getpid()):
        raise RuntimeError("ordered failure owner identity changed")
    ticket.setdefault("failure_ns", time.monotonic_ns())
    ticket["cleanup_deadline_ns"] = min(ticket["cleanup_deadline_ns"], ticket["failure_ns"] + emergency_ns)
    ticket.setdefault("originalError", str(error))
    save(path, ticket)
    return ticket["cleanup_deadline_ns"]


def record_failure(ticket_path, output, error):
    ticket = json.loads(ticket_path.read_text())
    save(output / "account-run-failure.json", {"operation": ticket["operation"], "type": type(error).__name__, "error": str(error)})


def stop_owner(process, ticket_path, signal_owner):
    deadline_ns = min(json.loads(ticket_path.read_text())["cleanup_deadline_ns"], installed_clock.parent_deadline(20))
    signal_owner(ticket_path, signal.SIGTERM)
    try:
        process.wait(timeout=installed_clock.remaining_until(deadline_ns, 10))
    except subprocess.TimeoutExpired:
        signal_owner(ticket_path, signal.SIGKILL)
        process.wait(timeout=installed_clock.remaining_until(deadline_ns, 10))
    owner = json.loads(ticket_path.read_text()).get("owner")
    if alive(owner):
        signal_owner(ticket_path, signal.SIGKILL)
    while alive(owner):
        time.sleep(installed_clock.remaining_until(deadline_ns, 0.025))


def privileged_signal(ticket_path, number):
    deadline_ns = json.loads(ticket_path.read_text())["cleanup_deadline_ns"]
    subprocess.run(privileged_arguments("signal", ticket_path, str(int(number))), check=True, timeout=installed_clock.remaining_until(deadline_ns, 10))


def recover(ticket_path):
    deadline_ns = min(json.loads(ticket_path.read_text())["cleanup_deadline_ns"], installed_clock.parent_deadline(30))
    subprocess.run(privileged_arguments("recover", ticket_path, str(deadline_ns)), check=True,
                   timeout=installed_clock.remaining_until(deadline_ns, 30))


def privileged_arguments(operation, path, *arguments):
    return ["sudo", "--preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT", "env", "PYTHONPATH=" + str(Path(__file__).parents[1]),
            "PYTHONDONTWRITEBYTECODE=1", sys.executable, str(Path(__file__)), operation, str(path), *arguments]


def verify_cleanup(root, output, parent_receipt=True):
    ticket_path = root / "account-run.json"
    cleanup_path = output / "accounts-cleanup.json"
    if not ticket_path.exists() or not cleanup_path.exists():
        raise RuntimeError("required account-run ownership or cleanup report is missing; retaining scratch")
    ticket, result = json.loads(ticket_path.read_text()), json.loads(cleanup_path.read_text())
    if result.get("operation") != ticket["operation"] or not result.get("groupAbsent") or not result.get("resourcesStopped"):
        raise RuntimeError("account-run cleanup is not positively bound to this operation")
    if any(not account.get("absent") for account in result["accounts"] if account.get("created") or account.get("intent")):
        raise RuntimeError("an owned account remains unconfirmed after cleanup")
    if alive(ticket.get("owner")):
        raise RuntimeError("account-run owner still consumes its scratch")
    if parent_receipt:
        verify_parent_receipt(output, ticket)


def verify_parent_receipt(output, ticket):
    path = output / "account-run-result.json"
    if not path.exists():
        raise RuntimeError("account-run supervisor completion is missing; retaining scratch")
    result = json.loads(path.read_text())
    if result.get("operation") != ticket["operation"] or result.get("ownerStopped") is not True or result.get("cleanupVerified") is not True:
        raise RuntimeError("account-run supervisor did not verify owned cleanup")


def root_action(arguments):
    from fixtures import installed_accounts
    installed_accounts.require_hosted_root()
    operation, ticket_path, *values = arguments
    path = Path(ticket_path).resolve(strict=True)
    ticket = json.loads(path.read_text())
    owner = ticket.get("owner")
    if operation == "signal":
        signal_bound(owner, int(values[0]))
        return
    if operation != "recover" or alive(owner):
        raise RuntimeError("refusing recovery while the bound account owner may be running")
    deadline_ns = min(ticket["cleanup_deadline_ns"], int(values[0]))
    installed_clock.remaining_until(deadline_ns, 30)
    signal.signal(signal.SIGALRM, recovery_expired)
    signal.setitimer(signal.ITIMER_REAL, installed_clock.remaining_until(deadline_ns, 30))
    resources = Path(ticket["output"]) / "account-resources.json"
    if not resources.exists() or resources.stat().st_uid != 0:
        raise RuntimeError("account resource ownership is missing; retaining scratch")
    installed_accounts.Accounts.resume(resources, ticket).close()


def recovery_expired(_number, _frame):
    raise installed_clock.ClockExpired("ordered account recovery exhausted its finite reserve; retain owned resources")


if __name__ == "__main__":
    root_action(sys.argv[1:])
