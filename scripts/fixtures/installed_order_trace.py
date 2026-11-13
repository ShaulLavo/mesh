"""Bounded observations only. No diagnostic value participates in policy."""
from contextlib import contextmanager
from functools import wraps
import json
from pathlib import Path
import threading
import time
import traceback

from fixtures import installed_clock


TIMELINE = "order-timeline.json"
READER = "order-reader-timeline.json"
STATUS = "order-diagnostics-status.json"
ACCOUNT_FILES = {TIMELINE, READER, STATUS}
HISTORY_LIMIT = 64 << 10
HANDOFF_LIMIT = 262144


def stamp():
    return {"monotonicNs": time.monotonic_ns(), "wallNs": time.time_ns()}


def encoded(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=True).encode()


def bounded_text(value, limit, tail=False):
    value = value[-limit:] if tail else value[:limit]
    low, high = 0, len(value)
    while low < high:
        middle = (low + high + 1) // 2
        candidate = value[-middle:] if tail else value[:middle]
        if len(encoded(candidate)) <= limit:
            low = middle
            continue
        high = middle - 1
    return value[-low:] if tail and low else value[:low]


def error_record(error, phase, kind):
    message = str(error)
    stack = "".join(traceback.format_exception(type(error), error, error.__traceback__))
    retained_message, retained_stack = bounded_text(message, 512), bounded_text(stack, 2048, tail=True)
    return {"kind": bounded_text(kind, 64), "phase": bounded_text(phase, 128), "exceptionId": id(error),
        "type": bounded_text(type(error).__name__, 128), "message": retained_message, "traceback": retained_stack,
        "textTruncated": retained_message != message or retained_stack != stack, "at": stamp()}


def diagnostic(method):
    @wraps(method)
    def guarded(self, *args, **kwargs):
        try:
            return method(self, *args, **kwargs)
        except BaseException as error:
            notify_failure(error)
            self.complete = False
            self.diagnostic_errors.append({"type": type(error).__name__, "phase": method.__name__})
            del self.diagnostic_errors[:-8]
            if len(self.failures) < 8:
                self.failures.append(error_record(error, method.__name__, "export" if method.__name__ == "write" else "diagnostic"))
    return guarded


def notify_failure(error):
    from fixtures import installed_native
    installed_native.cleanup_failure(error, None)


class Trace:
    def __init__(self, owner):
        self.owner, self.complete = owner, True
        self.events, self.failures, self.reserved, self.diagnostic_errors = {}, [], {}, []
        self.handoff_bytes, self.reader, self.reads = None, None, {}

    @diagnostic
    def event(self, phase, start=None, **details):
        key = phase + ":" + str(details.get("operation", "")) + ":" + str(details.get("active", "")) + ":" + str(details.get("settled", ""))
        value = {"phase": phase, "thread": threading.get_ident(), "end": stamp(), "details": details}
        if start is not None:
            value["start"] = start
        value = json.loads(encoded(value))
        prior = self.events.get(key)
        self.events[key] = {"count": prior["count"] + 1 if prior else 1,
            "first": prior["first"] if prior else value, "last": value}
        if start is not None:
            previous = prior.get("slowest", value) if prior else value
            self.events[key]["slowest"] = max((previous, value), key=elapsed_ns)
        if len(encoded(self.events)) > 32768:
            self.complete = False
            self.events.pop(key)

    @diagnostic
    def reserve(self, phase, **details):
        value = {"at": stamp(), "details": details}
        value = json.loads(encoded(value))
        self.reserved[phase] = value
        if len(encoded(self.reserved)) > 8192:
            self.reserved.pop(phase)
            self.complete = False

    def retain_failure(self, phase, **details):
        value = json.loads(encoded({"at": stamp(), "details": details}))
        reserved = self.reserved | {phase: value}
        if len(encoded(reserved)) > 8192:
            raise ValueError("failure identity exceeds reservation bound")
        self.reserved = reserved

    @diagnostic
    def failure(self, error, phase, kind):
        if kind in ("diagnostic", "export"):
            notify_failure(error)
        if len(self.failures) == 8:
            self.complete = False
            return
        record = error_record(error, phase, kind)
        self.failures.append(record)
        if record["textTruncated"]:
            self.complete = False

    @diagnostic
    def handoff(self, value):
        payload = encoded({"observedAt": stamp(), "observation": value,
            "correlation": correlation(value), "precision": "integer nanoseconds from raw native strings"})
        if len(payload) > HANDOFF_LIMIT:
            self.complete = False
            self.reserve("handoff", retained=False, reason="full observation exceeds file bound")
            return
        self.handoff_bytes = payload
        self.reserve("handoff", retained=True, correlation=correlation(value))

    @diagnostic
    def eligibility(self, value):
        self.reserve("firstObservedEligible", correlation=correlation(value))

    @diagnostic
    def sample(self, value, settled, version, phase):
        if not settled:
            self.reserve("lastObservedIneligible", correlation=correlation(value), requestedVersion=version, requestedPhase=phase,
                targetLedgerObserved=any(entry.get("version") == version for entry in value.get("entries", [])))
        self.event("eligibility.sample", settled=settled, journalPhase=(value.get("journal") or {}).get("phase"), requestedVersion=version, requestedPhase=phase)

    @diagnostic
    def load_reader(self, path):
        with Path(path).open("rb") as stream:
            data = stream.read(HISTORY_LIMIT + 1)
        if len(data) > HISTORY_LIMIT:
            raise ValueError("native reader diagnostic bound exceeded")
        self.reader = json.loads(data)
        if self.reader.get("complete") is not True:
            raise ValueError("native reader diagnostic incomplete")

    @diagnostic
    def clear_reader(self, path):
        self.reader = None
        Path(path).unlink(missing_ok=True)

    def summary(self):
        value = {"schema": 1, "owner": bounded_text(self.owner, 512), "complete": self.complete,
            "sampling": "first and last boundary per phase/operation; counts include coalesced calls",
            "eligibilityDefinition": "committed journal/unowned state and one matching ledger request/generation/version",
            "writerBoundary": "unobserved; journal updatedAt precedes durable save completion",
            "reserved": self.reserved, "failures": self.failures, "diagnosticErrors": self.diagnostic_errors,
            "events": self.events}
        if len(encoded(value)) > HISTORY_LIMIT:
            self.complete = False
            value.update(complete=False, events={}, historyOmitted=True)
        if len(encoded(value)) > HISTORY_LIMIT:
            value.update(reserved={}, diagnosticErrors=[], reservationsOmitted=True)
        if len(encoded(value)) > HISTORY_LIMIT:
            raise ValueError("bounded failure summary exceeds file limit")
        return value

    @diagnostic
    def write(self, path):
        Path(path).write_bytes(encoded(self.summary()))


def attach(driver):
    if not hasattr(driver, "diagnostics"):
        driver.diagnostics = Trace(driver.account["role"])
    return driver.diagnostics


def elapsed_ns(item):
    return item["end"]["monotonicNs"] - item["start"]["monotonicNs"]


def trace(driver):
    return getattr(driver, "diagnostics", None)


def event(driver, phase, **details):
    owner = trace(driver)
    if owner is not None:
        owner.event(phase, **details)


def failure(driver, error, phase, kind):
    owner = trace(driver)
    if owner is not None:
        owner.failure(error, phase, kind)


def limited_identity(value, keys):
    result = {key: bounded_text(str(value.get(key, "")), 768) for key in keys}
    truncated = [key for key in keys if result[key] != str(value.get(key, ""))]
    if truncated:
        result["truncatedFields"] = truncated
    return result


def mapping_identity(mapping):
    if mapping is None:
        return None
    return {kind: limited_identity(value, ("executable",)) | {"device": value["device"], "inode": value["inode"]}
            for kind, value in mapping.items()}


def helper_inspection_failure(host, primary, manager, pid, receipt, observed):
    owner = trace(host)
    if owner is None:
        return
    try:
        deadline_ns = installed_clock.HANDOFF.get()
        installed_clock.remaining_until(deadline_ns, 1)
        journal = observed.get("journal") or {}
        owner.retain_failure("helperInspectionFailure", service={"label": bounded_text(manager.label, 128),
            "manager": bounded_text(manager.system, 32), "pid": pid}, receipt=limited_identity(receipt, ("executable", "servicePath", "digest")),
            mapping=mapping_identity(getattr(primary, "mapped_image", None)), verifiedMapping=observed.get("helper"),
            installationJournal=limited_identity(journal, ("phase", "updatedAt")),
            hostUpdatePhase=bounded_text(observed.get("state", {}).get("phase", ""), 64),
            journalObservation="earlier in this inspection; not reread after failure", fixtureDeadlineNs=deadline_ns)
    except BaseException as error:
        primary.add_note("ordered helper failure evidence unavailable: " + bounded_text(str(error), 256))


def driver_exit(driver, primary, operation, returncode, deadline_ns):
    owner = trace(driver)
    if owner is None:
        return
    try:
        installed_clock.remaining_until(deadline_ns, 1)
        owner.retain_failure("driverExit", pid=driver.process.pid, returncode=returncode, signal=-returncode if returncode < 0 else None,
            operation=operation, stderrFile=driver.account["role"] + "-driver.log", fixtureDeadlineNs=deadline_ns)
    except BaseException as error:
        primary.add_note(f"ordered driver exit pid={driver.process.pid} returncode={returncode} signal={-returncode if returncode < 0 else None}; "
                         "evidence unavailable: " + bounded_text(str(error), 256))


@contextmanager
def span(owner, phase, **details):
    started = stamp()
    try:
        yield
    except BaseException as error:
        if owner is not None:
            owner.failure(error, phase, "operation")
        raise
    finally:
        if owner is not None:
            owner.event(phase, start=started, **details)


def calls(method):
    @wraps(method)
    def observed(self, operation, timeout=30, **fields):
        with span(trace(self), "call", operation=operation, active=fields.get("active", True), fixtureDeadlineNs=fields.get("fixtureDeadlineNs")):
            return method(self, operation, timeout=timeout, **fields)
    return observed


def dispatched(method):
    @wraps(method)
    def observed(self, operation, value):
        try:
            with span(trace(self), "dispatch", operation=operation, active=value.get("active", True), fixtureDeadlineNs=value.get("fixtureDeadlineNs")):
                return method(self, operation, value)
        finally:
            persist_host(self)
    return observed


def persist_host(host):
    owner = trace(host)
    if owner is not None:
        owner.write(host.case.root / TIMELINE)


def read(owner, phase, operation):
    started = stamp()
    try:
        with span(owner, phase):
            return operation()
    finally:
        if owner is not None:
            owner.reads[phase] = {"start": started, "end": stamp()}


def correlation(value):
    request = (value.get("journal") or {}).get("request", {})
    journal = {key: request.get(key) for key in ("id", "generation")}
    journal["version"] = request.get("manifest", {}).get("version")
    journal["updatedAt"] = (value.get("journal") or {}).get("updatedAt")
    matches = [entry for entry in value.get("entries", []) if entry.get("request") == journal["id"]
        and entry.get("generation") == journal["generation"] and entry.get("version") == journal["version"]]
    owned = value.get("state", {}).get("owned")
    owned_matches = None if owned is None else owned.get("request") == journal["id"] and owned.get("generation") == journal["generation"]
    metadata_matches = None if owned is None else bool(matches) and all(entry.get("metadata") == owned.get("metadata") for entry in matches)
    return {"journal": journal, "matchingLedger": matches, "owned": owned,
        "ownedMatchesJournal": owned_matches, "ledgerMetadataMatchesOwned": metadata_matches,
        "journalMetadata": "not carried by installation journal", "matchingLedgerObserved": bool(matches)}


def sample(driver, value, settled, version, phase):
    owner = trace(driver)
    if owner is not None:
        owner.sample(value, settled, version, phase)


def publish(driver, future, value):
    owner = trace(driver)
    if owner is not None:
        owner.handoff(value)
        owner.eligibility(value)
        owner.event("future.publish")
    future.set_result(value)
    if owner is not None:
        owner.event("future.publish.end")


def cutoff(driver, predecessor, target, parsed):
    owner = trace(driver)
    if owner is not None:
        entry = next((item for item in correlation(predecessor)["matchingLedger"] if item.get("version") == target), None)
        owner.reserve("cutoff", rawPredecessor=entry, parsedCutoffNs=parsed, parserResolutionNs=1,
            preGuardWallNs=time.time_ns(), policySoakNs=5_000_000_000)


def summaries(drivers, record):
    record["orderDiagnostics"] = {driver.account["role"]: attach(driver).summary() for driver in drivers}


def retain(drivers, record, output):
    for driver in drivers:
        retain_owner(driver, output)
    summaries(drivers, record)
    if not all(value["complete"] for value in record["orderDiagnostics"].values()):
        return RuntimeError("ordered diagnostic coverage incomplete; no ordered acceptance")
    return None


def retain_owner(driver, output):
    owner = attach(driver)
    prefix = Path(output) / ("order-" + driver.account["role"])
    if owner.handoff_bytes is not None:
        write_diagnostic(owner.handoff_bytes, str(prefix) + "-handoff.json", owner)
    owner.write(str(prefix) + "-timeline.json")


def write_diagnostic(data, path, owner):
    try:
        Path(path).write_bytes(data)
    except BaseException as error:
        if owner is not None:
            owner.failure(error, "account.export", "export")
            owner.complete = False


def account_export(root, files):
    status = {"complete": True, "errors": []}
    for name in (TIMELINE, READER):
        export_one(root, files, name, status)
    files[STATUS] = encoded(status).hex()


def export_one(root, files, name, status):
    try:
        with (Path(root) / name).open("rb") as stream:
            data = stream.read(HISTORY_LIMIT + 1)
        if len(data) > HISTORY_LIMIT or len(encoded(files)) + len(data) * 2 > (2 << 20) - 4096:
            raise ValueError("diagnostic file or transfer bound exceeded")
        value = json.loads(data)
        files[name] = data.hex()
        if value.get("complete") is not True:
            raise ValueError("diagnostic sidecar incomplete")
    except BaseException as error:
        status["complete"] = False
        status["errors"].append({"file": name, "type": type(error).__name__, "message": str(error)[:256]})


def export_status(decoded, owner):
    if owner is None:
        return
    try:
        if json.loads(decoded.get(STATUS, b"{}")).get("complete") is not True:
            raise ValueError("account diagnostic export incomplete")
    except BaseException as error:
        owner.failure(error, "account.export", "export")
        owner.complete = False
