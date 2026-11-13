"""Fixture-only monotonic clocks; calibration never supplies acceptance bounds."""
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass
import functools
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time

SECOND = 1_000_000_000
MILLISECOND = 1_000_000
ACTIVE = ContextVar("installed_fixture_clock", default=None)
HANDOFF = ContextVar("installed_fixture_parent_deadline", default=None)


def positive(value):
    if type(value) is not int or value <= 0:
        raise ValueError("clock duration must be positive integer nanoseconds")
    return value


def bound(measured_ns):
    return ((9 * positive(measured_ns) + 8 * MILLISECOND - 1) // (8 * MILLISECOND)) * MILLISECOND


@dataclass(frozen=True)
class SampleRef:
    provenance: dict
    workload: dict
    elapsed_ns: int
    completed: bool
    cleanup_verified: bool

    def eligible(self, workload):
        return self.completed is True and self.cleanup_verified is True and self.workload == workload and bool(self.provenance) and type(self.elapsed_ns) is int and self.elapsed_ns > 0


@dataclass(frozen=True)
class PhaseBudget:
    key: str
    limit_ns: int
    workload: dict
    samples: tuple

    def validate(self):
        if not self.samples or any(not sample.eligible(self.workload) for sample in self.samples):
            raise ValueError("missing, censored or ineligible clock sample: " + self.key)
        if positive(self.limit_ns) != bound(max(sample.elapsed_ns for sample in self.samples)):
            raise ValueError("clock bound differs from reviewed 1.125 derivation: " + self.key)


@dataclass(frozen=True)
class ClockPlan:
    mode: str = "unbound"
    workload: dict = None
    phases: tuple = ()
    guards: tuple = ()
    command_ns: int = 0
    external_ns: int = 0
    emergency_ns: int = 0
    eligibility_review: str = ""
    external_budget: PhaseBudget = None

    def validate(self):
        if self.mode == "unbound":
            raise ValueError("unbound fixture clock cannot execute")
        if self.mode not in ("diagnostic", "frozen") or not self.workload:
            raise ValueError("fixture needs an explicit diagnostic or frozen clock plan")
        total = positive(self.command_ns) + positive(self.external_ns) + positive(self.emergency_ns)
        if total > 900 * SECOND or self.command_ns >= 3600 * SECOND:
            raise ValueError("fixture admission exceeds 900 seconds or material validity")
        if self.mode == "diagnostic":
            self.validate_diagnostic()
            return
        self.validate_frozen()

    def validate_diagnostic(self):
        if self.command_ns > 600 * SECOND or self.phases or self.guards:
            raise ValueError("diagnostic keeps the current overall bound and cannot size phases")

    def validate_frozen(self):
        budgets = self.phases + self.guards
        if not self.phases or not self.eligibility_review or len({row.key for row in budgets}) != len(budgets):
            raise ValueError("frozen clock needs reviewed eligibility and exact unique keys")
        for row in budgets:
            row.validate()
        if self.external_budget is None or self.external_budget.limit_ns != self.external_ns:
            raise ValueError("frozen external overhead needs its independently measured budget")
        self.external_budget.validate()
        if sum(row.limit_ns for row in self.phases) != self.command_ns:
            raise ValueError("command clock must equal disjoint leaf budgets")


def load_plan(path):
    path = Path(path)
    if path.stat().st_size > 1 << 20:
        raise ValueError("clock plan exceeds document bound")
    value = json.loads(path.read_text())
    for name in ("phases", "guards"):
        value[name] = tuple(load_budget(row) for row in value.get(name, []))
    if value.get("external_budget") is not None:
        value["external_budget"] = load_budget(value["external_budget"])
    plan = ClockPlan(**value)
    plan.validate()
    return plan


def load_budget(value):
    return PhaseBudget(**(value | {"samples": tuple(SampleRef(**row) for row in value["samples"])}))


class ClockExpired(TimeoutError):
    pass


class Clock:
    def __init__(self, plan, *, origin_ns=None, job_origin_ns=None, now=time.monotonic_ns):
        plan.validate()
        self.plan, self.now = plan, now
        self.origin_ns = now() if origin_ns is None else origin_ns
        self.job_origin_ns = self.origin_ns if job_origin_ns is None else job_origin_ns
        self.job_deadline_ns = self.job_origin_ns + 900 * SECOND
        self.deadline_ns = self.origin_ns + plan.command_ns
        if self.origin_ns - self.job_origin_ns > plan.external_ns or self.deadline_ns + plan.emergency_ns > self.job_deadline_ns:
            raise ClockExpired("insufficient live job admission for command and emergency reserve")
        self.rows, self.samples, self.failures, self.stack = [], [], [], []
        self.expected_samples, self.provenance = set(), {}
        self.unresolved = {"installed-partition-and-scenarios"} if plan.workload.get("grouped") and plan.workload.get("mode") != "order" else set()
        if plan.workload.get("mode") == "order":
            self.expected_samples.add("ordered-owner:#1")
        self.counts, self.cleanup_deadline_ns, self.clean_keys = {}, None, set()
        self.active_key, self.started_ns = "setup#1", self.origin_ns
        self.counts["setup"] = 1
        self.watchdog, self.previous_handler = False, None
        self.budgets = {row.key: row.limit_ns for row in plan.phases + plan.guards}

    def effective_deadline(self):
        if self.cleanup_deadline_ns is not None:
            return self.cleanup_deadline_ns
        ends = [self.deadline_ns]
        ends.extend(row["deadline_ns"] for row in self.stack)
        if self.plan.mode == "frozen":
            ends.append(self.started_ns + self.limit(self.active_key))
        return min(ends)

    def limit(self, key):
        if key not in self.budgets:
            raise ValueError("missing frozen clock key: " + key)
        return self.budgets[key]

    def remaining(self, maximum=None):
        self.check()
        seconds = (self.effective_deadline() - self.now()) / SECOND
        return seconds if maximum is None else min(maximum, seconds)

    def check(self):
        if self.now() >= self.effective_deadline():
            error = ClockExpired("installed fixture clock expired at " + self.active_key)
            self.fail(error)
            raise error

    def fail(self, error):
        self.failures.append({"type": type(error).__name__, "error": str(error)})
        if self.cleanup_deadline_ns is None:
            self.cleanup_deadline_ns = min(self.now() + self.plan.emergency_ns, self.job_deadline_ns)
        self.disarm()
        self.arm_cleanup()

    def arm_cleanup(self):
        if self.watchdog and self.cleanup_deadline_ns is not None:
            remaining = (self.cleanup_deadline_ns - self.now()) / SECOND
            if remaining > 0:
                signal.setitimer(signal.ITIMER_REAL, remaining)

    def begin_cleanup(self):
        error = sys.exc_info()[1]
        if error is None and self.now() >= self.effective_deadline():
            error = ClockExpired("fixture work deadline ended before cleanup")
        if error is not None:
            self.fail(error)
        self.arm_cleanup()

    def next_key(self, name):
        self.counts[name] = self.counts.get(name, 0) + 1
        return name + "#" + str(self.counts[name])

    def switch(self, name):
        self.check()
        now = self.now()
        self.close_leaf(now)
        self.active_key, self.started_ns = self.next_key(name), now
        self.arm()
        return self.active_key

    def close_leaf(self, now):
        self.rows.append({"key": self.active_key, "start_ns": self.started_ns, "end_ns": now,
                          "elapsed_ns": now - self.started_ns, "completed": not self.failures})

    @contextmanager
    def operation(self, name):
        if self.failures and not name.startswith(("case-cleanup", "completion")):
            raise ClockExpired("new fixture work refused after first failure")
        previous = self.active_key.rsplit("#", 1)[0]
        key = self.switch(name)
        start = self.now()
        guard_key = "guard:" + key
        deadline = min(self.deadline_ns, start + self.limit(guard_key)) if guard_key in self.budgets else self.deadline_ns
        self.stack.append({"key": key, "deadline_ns": deadline})
        try:
            yield key
            self.check()
        except BaseException as error:
            self.fail(error)
            raise
        finally:
            self.finish_operation(key, start, previous)

    def finish_operation(self, key, start, previous):
        self.samples.append({"key": key, "start_ns": start, "end_ns": self.now(),
                             "elapsed_ns": self.now() - start, "completed": not self.failures,
                             "cleanup_verified": False})
        self.stack.pop()
        self.switch(previous)

    def arm(self):
        if self.watchdog and not self.failures:
            signal.setitimer(signal.ITIMER_REAL, max(0.000001, (self.effective_deadline() - self.now()) / SECOND))

    def disarm(self):
        if self.watchdog:
            signal.setitimer(signal.ITIMER_REAL, 0)

    def alarm(self, _number, _frame):
        self.check()
        self.arm()

    @contextmanager
    def activate(self, watchdog=False):
        token = ACTIVE.set(self)
        self.watchdog = watchdog
        if watchdog:
            self.previous_handler = signal.signal(signal.SIGALRM, self.alarm)
        try:
            self.arm()
            yield self
        finally:
            self.disarm()
            self.restore(token)

    def restore(self, token):
        if self.watchdog:
            signal.signal(signal.SIGALRM, self.previous_handler)
        ACTIVE.reset(token)

    def export(self, path, cleanup_verified, provenance):
        self.disarm()
        now = self.now()
        if now >= self.effective_deadline():
            self.fail(ClockExpired("fixture clock expired before terminal cleanup/export"))
        self.close_leaf(now)
        keys = [row["key"] for row in self.rows]
        complete = not self.failures and cleanup_verified
        if self.plan.mode == "frozen" and keys != [row.key for row in self.plan.phases]:
            self.fail(ValueError("frozen clock sequence differs from actual disjoint leaves"))
            complete = False
        samples = [qualify(row, cleanup_verified or row["key"] in self.clean_keys) for row in self.samples]
        leaves = [qualify(row, cleanup_verified) for row in self.rows]
        missing = sorted(self.expected_samples.difference(row["key"] for row in samples if row["eligible"]))
        if missing:
            self.fail(ValueError("fixture sample matrix remains incomplete: " + ", ".join(missing)))
            complete = False
        if self.unresolved:
            self.fail(ValueError("fixture key sets remain unresolved: " + ", ".join(sorted(self.unresolved))))
            complete = False
        value = {"mode": self.plan.mode, "accepted": complete and self.plan.mode == "frozen",
                 "measurement_completed": complete, "origin_ns": self.origin_ns, "end_ns": now,
                 "command_ns": now - self.origin_ns, "disjoint_ns": sum(row["elapsed_ns"] for row in self.rows),
                 "external_before_ns": self.origin_ns - self.job_origin_ns, "external_budget_ns": self.plan.external_ns,
                 "emergency_reserve_ns": self.plan.emergency_ns, "deadline_ns": self.deadline_ns,
                 "cleanup_deadline_ns": self.cleanup_deadline_ns, "workload": self.plan.workload,
                 "provenance": self.provenance | provenance, "leaves": leaves, "samples": samples, "failures": self.failures,
                 "expected_sample_keys": sorted(self.expected_samples),
                 "missing_sample_keys": missing, "unresolved_key_sets": sorted(self.unresolved)}
        self.persist(path, value)
        return value

    def persist(self, path, value):
        path = Path(path)
        accepted = value["accepted"]
        value["accepted"] = False
        last = value["leaves"][-1]
        completed, eligible = last["completed"], last["eligible"]
        last.update(completed=False, eligible=False)
        path.write_text(json.dumps(value, indent=2) + "\n")
        now = self.now()
        last.update(end_ns=now, elapsed_ns=now - last["start_ns"], completed=completed, eligible=eligible)
        value.update(end_ns=now, command_ns=now - self.origin_ns,
                     disjoint_ns=sum(row["elapsed_ns"] for row in value["leaves"]))
        value["measurement_write_epilogue"] = {"start_ns": now, "charged_to": "completion", "final_absolute_guard": True}
        value["accepted"] = accepted
        path.write_text(json.dumps(value, indent=2) + "\n")
        self.check_persisted(path, value)

    def check_persisted(self, path, value):
        try:
            self.check()
        except ClockExpired:
            now = self.now()
            last = value["leaves"][-1]
            last.update(completed=False, eligible=False, end_ns=now, elapsed_ns=now - last["start_ns"], censored=True)
            value.update(accepted=False, measurement_completed=False, failures=self.failures)
            value.update(end_ns=now, command_ns=now - self.origin_ns,
                         disjoint_ns=sum(row["elapsed_ns"] for row in value["leaves"]))
            path.write_text(json.dumps(value, indent=2) + "\n")


def qualify(row, cleaned):
    cleaned = cleaned or row.get("cleanup_verified") is True
    return row | {"cleanup_verified": cleaned, "eligible": row["completed"] and cleaned}


def current():
    return ACTIVE.get()


def timeout(maximum):
    clock = current()
    result = maximum if clock is None else clock.remaining(maximum)
    return result if HANDOFF.get() is None else remaining_until(HANDOFF.get(), result)


def parent_deadline(maximum):
    end = time.monotonic_ns() + int(maximum * SECOND)
    if current():
        end = min(end, current().effective_deadline())
    return min(end, HANDOFF.get()) if HANDOFF.get() is not None else end


@contextmanager
def handoff(deadline_ns, *, watchdog=False):
    positive(deadline_ns)
    remaining_until(deadline_ns, 900)
    token = HANDOFF.set(min(deadline_ns, HANDOFF.get() or deadline_ns))
    previous = start_handoff_alarm(HANDOFF.get()) if watchdog else None
    try:
        yield
    finally:
        if previous is not None:
            restore_handoff_alarm(previous)
        HANDOFF.reset(token)


def start_handoff_alarm(deadline_ns):
    previous = (signal.getsignal(signal.SIGALRM), signal.getitimer(signal.ITIMER_REAL)[0], time.monotonic_ns())
    signal.signal(signal.SIGALRM, handoff_expired)
    signal.setitimer(signal.ITIMER_REAL, remaining_until(deadline_ns, 900))
    return previous


def restore_handoff_alarm(previous):
    signal.setitimer(signal.ITIMER_REAL, 0)
    signal.signal(signal.SIGALRM, previous[0])
    if previous[1]:
        remaining = previous[1] - (time.monotonic_ns() - previous[2]) / SECOND
        signal.setitimer(signal.ITIMER_REAL, max(0.000001, remaining))


def handoff_expired(_number, _frame):
    raise ClockExpired("absolute fixture parent deadline expired in child dispatch")


def failure_cleanup_handoff(deadline_ns):
    HANDOFF.set(positive(deadline_ns))
    signal.setitimer(signal.ITIMER_REAL, remaining_until(deadline_ns, 900))


def remaining_until(deadline_ns, maximum):
    remaining_ns = deadline_ns - time.monotonic_ns()
    if remaining_ns <= 0:
        raise ClockExpired("parent fixture deadline expired during handoff")
    return min(maximum, remaining_ns / SECOND)


def failure(error):
    if current():
        current().fail(error)


def failure_deadline(error, deadline_ns):
    if current():
        current().fail(error)
        current().cleanup_deadline_ns = min(current().cleanup_deadline_ns, deadline_ns)


def cleanup():
    if current():
        current().begin_cleanup()


def clean_sample(key):
    if current() and key:
        current().clean_keys.add(key)


def expect_samples(keys):
    if current():
        current().expected_samples.update(keys)


def installed_partition_resolved():
    if current():
        current().unresolved.discard("installed-partition-and-scenarios")


def validate_group(key, group):
    clock = current()
    if clock is None:
        return
    workload = group_workload(group)
    clock.provenance.setdefault("resolvedGroups", {})[key] = group
    if clock.plan.mode == "frozen":
        guard = next((row for row in clock.plan.guards if row.key == "guard:" + key), None)
        if guard is None or guard.workload != workload:
            raise ValueError("installed group differs from its reviewed fixed clock guard")


def group_workload(group):
    return {"index": group["index"], "members": [member_workload(row) for row in group["members"]]}


def member_workload(row):
    value = {"version": row["version"], "protocol": row["protocol"], "platform": row["artifact"]["platform"]}
    return value | ({"customRole": row["version"]} if row["version"] in ("v0.1.9001", "v0.1.9002", "v0.1.9003") else {"binarySha256": row["artifact"]["binarySha256"]})


def arguments(parser):
    parser.add_argument("--clock-mode", choices=("unbound", "diagnostic", "frozen"), default="unbound")
    parser.add_argument("--clock-plan", type=Path, help="independently reviewed frozen fixture budget")
    parser.add_argument("--clock-job-start", type=Path, help="same-host monotonic job-origin record")
    parser.add_argument("--clock-command-seconds", type=int, default=600)
    parser.add_argument("--clock-external-seconds", type=int, default=240)
    parser.add_argument("--clock-emergency-seconds", type=int, default=60)


def from_arguments(args, native):
    workload = {"platform": native, "host": args.source_host, "mode": scenario(args),
                "scenario": args.scenario, "grouped": args.worker_registry is not None,
                "publishedRegistrySha256": hashlib.sha256(args.worker_registry.read_bytes()).hexdigest() if args.worker_registry else None,
                "catalogEvidence": os.environ.get("MESH_CATALOG_EVIDENCE") == "1",
                "buildRecipe": "mesh_installed_fixture/all-three-platforms/startup/full-transition",
                "manifestSchemas": [1, 2] if args.format_waypoint else [1]}
    mode = args.clock_mode
    if mode == "frozen":
        plan = load_plan(args.clock_plan)
        if plan.mode != mode or plan.workload != workload:
            raise ValueError("frozen clock workload differs from actual invocation")
    else:
        plan = ClockPlan(mode=mode, workload=workload, command_ns=args.clock_command_seconds * SECOND,
                         external_ns=args.clock_external_seconds * SECOND, emergency_ns=args.clock_emergency_seconds * SECOND)
    plan.validate()
    if args.clock_job_start is None:
        raise ValueError("executable fixture needs its original job clock for admission")
    job = json.loads(args.clock_job_start.read_text())
    if job["run"] != os.environ.get("GITHUB_RUN_ID") or job["job"] != os.environ.get("GITHUB_JOB"):
        raise ValueError("fixture admission clock belongs to another job")
    return Clock(plan, job_origin_ns=positive(job["origin_ns"]))


def scenario(args):
    for flag, label in (("fresh_managed", "fresh"), ("ordered_hosts", "order"), ("format_waypoint", "waypoint"), ("signed_chain", "chain")):
        if getattr(args, flag):
            return label
    return "oldest" if args.worker_registry else "adoption"


@contextmanager
def operation(name):
    clock = current()
    if clock is None:
        yield None
        return
    with clock.operation(name) as key:
        yield key


def timed(kind, identity=lambda *_args, **_kwargs: ""):
    def decorate(function):
        @functools.wraps(function)
        def call(*args, **kwargs):
            name = kind + ":" + identity(*args, **kwargs)
            if kind == "case-cleanup":
                cleanup()
            with operation(name):
                return function(*args, **kwargs)
        return call
    return decorate


@contextmanager
def native_group(output, group):
    path = os.environ.get("MESH_INSTALLED_CLOCK_HANDOFF")
    if not path:
        yield None
        return
    value = json.loads(Path(path).read_text())
    key = "native-group:" + value["prefix"] + ":" + str(group["index"])
    start = time.monotonic_ns()
    end = positive(value["deadline_ns"])
    if value["mode"] == "frozen":
        guard = value["guards"][key]
        if guard["workload"] != group_workload(group):
            raise ValueError("native group differs from reviewed budget workload")
        end = min(end, start + positive(guard["limit_ns"]))
    sample = {"key": key, "start_ns": start, "completed": False, "cleanup_verified": False}
    primary = None
    try:
        with handoff(end):
            yield sample
            remaining_until(end, 1)
        sample.update(completed=True, cleanup_verified=True)
    except BaseException as error:
        primary = error
        raise
    finally:
        evidence = Path(path).parent / "fixture-group-clock.json"
        retain_native_sample(evidence, sample, start, end, primary)


def retain_native_sample(path, sample, start, deadline_ns, primary):
    try:
        previous = json.loads(path.read_text()) if path.exists() else {"samples": []}
        previous["samples"].append(sample)
        persist_native_sample(path, previous, sample, start, deadline_ns, primary)
    except BaseException as error:
        sample["completed"] = False
        if primary is None:
            raise
        primary.add_note("native measurement export unavailable: " + str(error))


def persist_native_sample(path, value, sample, start, deadline_ns, primary):
    completed, sample["completed"] = sample["completed"], False
    sample.update(end_ns=time.monotonic_ns(), elapsed_ns=time.monotonic_ns() - start)
    path.write_text(json.dumps(value, indent=2) + "\n")
    measured = time.monotonic_ns()
    sample.update(end_ns=measured, elapsed_ns=measured - start,
                  measurement_write_epilogue={"start_ns": measured, "charged_to": "enclosing native transition", "final_absolute_guard": True})
    try:
        remaining_until(deadline_ns, 1)
        sample["completed"] = completed
        path.write_text(json.dumps(value, indent=2) + "\n")
        remaining_until(deadline_ns, 1)
    except ClockExpired as error:
        sample.update(completed=False, censored=True, end_ns=time.monotonic_ns(), elapsed_ns=time.monotonic_ns() - start)
        path.write_text(json.dumps(value, indent=2) + "\n")
        if primary is None:
            raise
        primary.add_note("native measurement export exceeded its absolute group deadline: " + str(error))


def native_selection(groups):
    path = os.environ.get("MESH_INSTALLED_CLOCK_HANDOFF")
    if not path:
        return
    value = json.loads(Path(path).read_text())
    keys = ["native-group:" + value["prefix"] + ":" + str(group["index"]) for group in groups]
    result = {"expected_sample_keys": keys, "workloads": [group_workload(group) for group in groups], "samples": []}
    (Path(path).parent / "fixture-group-clock.json").write_text(json.dumps(result, indent=2) + "\n")


def job_record(action, path):
    path = Path(path)
    now = time.monotonic_ns()
    if action == "job-start":
        if path.exists():
            raise ValueError("job clock cannot be reset")
        path.write_text(json.dumps({"origin_ns": now, "run": os.environ["GITHUB_RUN_ID"], "job": os.environ["GITHUB_JOB"]}) + "\n")
        return
    value = json.loads(path.read_text())
    value.update(end_ns=now, elapsed_ns=now - value["origin_ns"], admission_ns=900 * SECOND,
                 external_complete=False, unmeasured_tail="action post-job cleanup and cache-save work")
    fixture = path.parent / "cohort-proof" / "fixture-clock.json"
    if fixture.exists():
        command = json.loads(fixture.read_text())
        value.update(external_before_ns=command["origin_ns"] - value["origin_ns"], external_after_ns=now - command["end_ns"],
                     external_budget_ns=command["external_budget_ns"])
        value["external_observed_ns"] = value["external_before_ns"] + value["external_after_ns"]
    print("CLOCK_JOB_INTERVAL " + json.dumps(value), flush=True)
    if value["elapsed_ns"] >= 900 * SECOND:
        raise ClockExpired("job clock expired during external overhead or cleanup")
    if fixture.exists() and value["external_observed_ns"] > value["external_budget_ns"]:
        raise ClockExpired("external job overhead exceeded its declared diagnostic/frozen budget")


def cleanup_inputs(root):
    root = Path(root)
    control, proof = root / "cohort-control", root / "cohort-proof"
    control.mkdir(exist_ok=True)
    launch, ownership, clock = control / "fixture-launch.json", proof / "fixture-ownership.json", proof / "fixture-clock.json"
    prelaunch = not any(path.exists() for path in (launch, ownership, clock))
    value = {"inputsMayBeRemoved": prelaunch, "reason": "no fixture launch attempted" if prelaunch else "fixture ownership unconfirmed"}
    try:
        owned = json.loads(ownership.read_text()) if ownership.exists() else {}
        final = json.loads(clock.read_text()) if clock.exists() else {}
        absent = owned.get("signerAbsent") is True and owned.get("transitionsAbsent") is True
        root_removed = "retained_root" in final.get("provenance", {}) and final["provenance"]["retained_root"] is None
        if absent and root_removed:
            value.update(inputsMayBeRemoved=True, reason="fixture owners and root confirmed absent")
    except (OSError, ValueError, TypeError):
        value.update(inputsMayBeRemoved=False, reason="fixture ownership evidence unreadable")
    value["retained"] = [] if value["inputsMayBeRemoved"] else [str(root / name) for name in ("native-worker-input.noindex", "native-tools.noindex")]
    (control / "tools-cleanup-decision.json").write_text(json.dumps(value, indent=2) + "\n")
    if not value["inputsMayBeRemoved"]:
        raise RuntimeError("fixture ownership unconfirmed; retain native inputs and helper")


class NativeOwnership:
    def __init__(self, process, root, output, deadline_ns):
        self.process, self.root, self.output = process, Path(root), Path(output)
        self.owned, self.expected, self.pending, self.correlated = {}, set(), set(), set()
        self.unresolved, self.scans, self.observation_ns, self.verification_ns = [], 0, 0, 0
        self.launch_birth = None
        self.verified = False
        self.observe(deadline_ns, launching=True)

    def observe(self, deadline_ns, launching=False):
        started = time.monotonic_ns()
        try:
            rows = process_snapshot(deadline_ns)
            self.capture(rows, launching)
            self.read_native_records(rows)
            self.scans += 1
        finally:
            self.observation_ns += time.monotonic_ns() - started

    def capture(self, rows, launching):
        leader = rows.get(self.process.pid)
        if launching:
            if leader is None or leader["uid"] != os.getuid():
                self.unresolved.append("launcher identity unavailable at launch")
                return
            self.launch_birth = leader["birth"]
            self.owned[(self.process.pid, self.launch_birth)] = leader
        known = {pid for pid, birth in self.owned if pid in rows and rows[pid]["birth"] == birth}
        if self.launch_birth is not None and (leader is None or leader["birth"] == self.launch_birth):
            known.update(pid for pid, row in rows.items() if row["group"] == self.process.pid and row["uid"] == os.getuid())
        while True:
            children = {pid for pid, row in rows.items() if row["parent"] in known and row["uid"] == os.getuid()}
            if children.issubset(known):
                break
            known.update(children)
        for pid in known:
            self.owned[(pid, rows[pid]["birth"])] = rows[pid]

    def read_native_records(self, rows):
        paths = list(self.output.rglob("*.cohort.json"))
        for directory in self.root.glob("mesh-transition.*"):
            paths.extend(directory.glob("cohort.json"))
            paths.extend(directory.glob("state/s/*/meta.json"))
        for path in paths:
            value = json.loads(path.read_text())
            if path.name == "meta.json":
                self.note_shell(value.get("pid"), rows)
                continue
            self.note_cohort(value, rows)

    def note_shell(self, pid, rows):
        if type(pid) is not int or pid <= 0:
            return
        self.note_pid(pid, rows)
        if pid not in rows or (pid, rows[pid]["birth"]) not in self.owned:
            return
        parent = rows[pid]["parent"]
        if parent in rows and (parent, rows[parent]["birth"]) in self.owned:
            self.note_pid(parent, rows)
            self.pending.discard(parent)

    def note_cohort(self, value, rows):
        records = list(value.get("workers", []))
        if value.get("original"):
            records.append(value["original"])
        for record in records:
            self.note_worker(record, rows)

    def note_worker(self, record, rows):
        pid, shell = record.get("pid"), record.get("shellPid")
        if type(pid) is not int or pid <= 0:
            return
        self.note_pid(pid, rows)
        if type(shell) is int and shell > 0:
            self.note_pid(shell, rows)
            self.pending.discard(pid)
        else:
            self.pending.add(pid)

    def note_pid(self, pid, rows):
        self.expected.add(pid)
        if pid in rows and (pid, rows[pid]["birth"]) in self.owned:
            self.correlated.add(pid)

    def verify(self, deadline_ns):
        started = time.monotonic_ns()
        self.verified = False
        try:
            self.verify_absence(deadline_ns)
            self.verified = True
        finally:
            self.verification_ns += time.monotonic_ns() - started

    def verify_absence(self, deadline_ns):
        rows = process_snapshot(deadline_ns)
        self.read_native_records(rows)
        missing = sorted(self.expected.difference(self.correlated))
        changed = [pid for pid in self.expected if pid in rows and (pid, rows[pid]["birth"]) not in self.owned]
        live = [row for (pid, birth), row in self.owned.items() if pid in rows and rows[pid]["birth"] == birth]
        if self.launch_birth is None or not self.expected or missing or changed or self.pending or self.unresolved or live:
            raise RuntimeError("native ownership coverage/absence unconfirmed; retain root: " + str({
                "missing": missing, "changedLifetimes": changed, "pendingWorkers": sorted(self.pending), "live": [row["pid"] for row in live], "unresolved": self.unresolved}))

    def live(self, deadline_ns):
        rows = process_snapshot(deadline_ns)
        return {pid: birth for pid, birth in self.owned if pid in rows and rows[pid]["birth"] == birth}

    def save(self):
        value = {"launcherPid": self.process.pid, "launcherBirth": self.launch_birth,
                 "owned": list(self.owned.values()), "expectedPids": sorted(self.expected),
                 "pendingWorkers": sorted(self.pending), "unresolved": self.unresolved,
                 "correlatedPids": sorted(self.correlated),
                 "scans": self.scans, "observation_ns": self.observation_ns, "verification_ns": self.verification_ns,
                 "absenceVerified": self.verified,
                 "coverage": "launch ancestry reconciled with native worker/shell records"}
        (self.output / "fixture-native-ownership.json").write_text(json.dumps(value, indent=2) + "\n")


def process_snapshot(deadline_ns):
    result = subprocess.run(["ps", "-axww", "-o", "pid=,ppid=,pgid=,uid=,lstart=,stat="], capture_output=True, text=True,
                            check=True, timeout=remaining_until(deadline_ns, 2))
    rows = [line.split() for line in result.stdout.splitlines()]
    return {int(row[0]): {"pid": int(row[0]), "parent": int(row[1]), "group": int(row[2]), "uid": int(row[3]),
                         "birth": " ".join(row[3:-1])} for row in rows if len(row) == 10 and not row[-1].startswith("Z")}


def stop_group(process, deadline_ns, roots=(), *, ownership=None):
    group = process.pid
    owned = ownership.live(deadline_ns) if ownership is not None else {}
    send_group(group, signal.SIGTERM)
    signal_descendants(owned, signal.SIGTERM, deadline_ns)
    wait_group(process, group, deadline_ns, 2)
    send_group(group, signal.SIGKILL)
    signal_descendants(owned, signal.SIGKILL, deadline_ns)
    process.wait(timeout=remaining_until(deadline_ns, 10))
    wait_group(process, group, deadline_ns, 10, require_absent=True)
    if ownership is None:
        raise RuntimeError("native launch-time ownership unavailable; retain owned root")
    ownership.verify(deadline_ns)


def process_birth(pid, deadline_ns):
    result = subprocess.run(["ps", "-p", str(pid), "-o", "uid=,lstart=,stat="], capture_output=True, text=True,
                            check=False, timeout=remaining_until(deadline_ns, 2))
    fields = result.stdout.split()
    if not fields or fields[-1].startswith("Z"):
        return None
    if fields[0] != str(os.getuid()):
        raise RuntimeError("native descendant ownership changed; refusing signal")
    return " ".join(fields[:-1])


def signal_descendants(owned, number, deadline_ns):
    for pid, birth in owned.items():
        if process_birth(pid, deadline_ns) == birth:
            signal_process(pid, number)


def signal_process(pid, number):
    try:
        os.kill(pid, number)
    except ProcessLookupError:
        pass


def send_group(group, number):
    try:
        os.killpg(group, number)
    except ProcessLookupError:
        pass


def wait_group(process, group, deadline_ns, maximum, require_absent=False):
    end = min(deadline_ns, time.monotonic_ns() + maximum * SECOND)
    while time.monotonic_ns() < end:
        process.poll()
        if not group_alive(group):
            return
        time.sleep(min(0.025, remaining_until(end, 0.025)))
    if require_absent and group_alive(group):
        raise RuntimeError("native process-group cleanup unconfirmed; retain its root")


def group_alive(group):
    result = subprocess.run(["ps", "-ax", "-o", "pgid=,stat="], capture_output=True, text=True, check=True, timeout=2)
    return any(line.split()[0] == str(group) and not line.split()[1].startswith("Z") for line in result.stdout.splitlines())


if __name__ == "__main__":
    if sys.argv[1] == "cleanup-inputs":
        cleanup_inputs(sys.argv[2])
    else:
        job_record(*sys.argv[1:])
