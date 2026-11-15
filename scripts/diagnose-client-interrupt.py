#!/usr/bin/env python3
"""Prepare the original PC fixture and observe one hosted pc-interrupt."""
import argparse
import json
import os
import platform
import shutil
import signal
import subprocess
import time
from pathlib import Path

from fixtures import (
    installed_adoption,
    installed_clock,
    installed_native,
    installed_paths,
    installed_releases,
    interrupt_capture,
    interrupt_prepare,
)
from fixtures import installed_transport_lifecycle as lifecycle


def host_guard():
    if (os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted"
            or platform.system() != "Linux" or platform.machine() != "x86_64"):
        raise RuntimeError("interrupt diagnostic requires a disposable GitHub-hosted Linux AMD64 runner")
    result = subprocess.run(["/usr/bin/systemctl", "--user", "show", "mesh.service", "mesh-update-helper.service",
        "--property=Id,LoadState,MainPID,FragmentPath"], capture_output=True, text=True, timeout=installed_clock.timeout(120), check=True)
    loads = [line for line in result.stdout.splitlines() if line.startswith("LoadState=")]
    if loads != ["LoadState=not-found", "LoadState=not-found"]:
        raise RuntimeError("canonical native Mesh units must both be absent before the diagnostic")
    scope = Path("/proc/sys/kernel/yama/ptrace_scope")
    return {"units": result.stdout, "uid": os.getuid(), "platform": "linux/amd64", "kernel": platform.release(),
            "ptraceScope": scope.read_text().strip() if scope.exists() else None}


def interrupted(number, _frame):
    raise InterruptedError("client interrupt diagnostic received signal " + str(number))


class Run:
    def __init__(self, output):
        self.output = output
        self.root, self.releases, self.case, self.capture = None, None, None, None
        self.started = int(time.time())
        self.secondary = []
        self.cleanup_error = None
        self.record = {"schema": 1, "diagnosticOnly": True, "accepted": False, "outcomePassed": False,
            "cleanupVerified": False, "profile": "pc-interrupt", "baseline": host_guard(),
            "createdResources": ["owned short scratch root", "mesh.service", "mesh-update-helper.service",
                "private signer and preparation proof descendants", "original client/worker/shell/listener", "at most three attached tracers"],
            "teardownInstalled": "Run.finish in caller finally, with SIGINT and SIGTERM raising into finally"}
        self.save()

    def save(self):
        (self.output / "diagnostic-result.json").write_text(json.dumps(self.record, indent=2) + "\n")

    def prepare(self):
        source = interrupt_prepare.source_evidence()
        self.root = installed_paths.create()
        self.record["root"] = str(self.root)
        self.record["source"] = source
        self.save()
        releases = installed_releases.Releases(self.root, "linux/amd64", self.output)
        self.releases = releases
        origin, pair, previous, preparer = interrupt_prepare.prepare(releases, source)
        self.record["candidate"] = releases.builds["v0.1.9001"]
        self.save()
        os.environ[lifecycle.FLAG] = "1"
        self.case = installed_adoption.AdoptionCase(self.root, interrupt_prepare.HOST.copy(), pair,
            preparer, origin, releases, "interrupt", previous)

    def execute(self):
        case = self.case
        self.record["initial"] = case.prepare()
        case.seed_prior()
        self.record["genuinePrior"] = {"version": "v0.1.249", "phase": case.prior["phase"],
                                       "settings": case.prior["settings"]}
        case.seed()
        self.capture = interrupt_capture.Capture(case, self.output)
        self.capture.arm()
        self.capture.require_armed()
        self.capture.state("adoption.before")
        start = case.start_adoption("v0.1.9001")
        self.record["start"] = start
        self.record["interruption"] = interrupt_capture.interrupt(case, self.capture, "v0.1.9001")
        result = case.validate_result(case.await_result("v0.1.9001"), "v0.1.9001", start["pin"])
        if result["helper"]["pid"] == self.record["interruption"]["killedNativePID"]:
            raise RuntimeError("native manager did not replace the interrupted helper")
        self.capture.retained_identity()
        self.record.update(result=result, outcomePassed=True)
        self.observe("state-after-outcome", lambda: self.capture.state("outcome.passed"))
        self.observe("successor-coverage", lambda: lifecycle.prove(case,
            {"running": case.releases.builds["v0.1.9001"]}, "after interrupted adoption", maximum=120))

    def observe(self, name, action):
        try:
            return action()
        except BaseException as error:  # noqa: BLE001 - Secondary observation failures cannot replace the primary exception.
            self.secondary.append({"stage": name, "errorType": type(error).__name__})
            return None

    def cleanup_action(self, name, action):
        try:
            return action()
        except BaseException as error:  # noqa: BLE001 - Preserve the first cleanup exception while attempting remaining teardown.
            self.secondary.append({"stage": name, "errorType": type(error).__name__})
            if self.cleanup_error is None:
                self.cleanup_error = error
            return None

    def finish(self, primary):
        installed_clock.cleanup(final=True)
        if primary:
            self.record["primary"] = {"type": type(primary).__name__, "error": str(primary)[:8192]}
        if self.capture:
            self.observe("state-before-cleanup", lambda: self.capture.state("cleanup.before"))
        if self.case:
            self.close_case()
        if self.capture:
            self.record["observation"] = self.observe("external-close", lambda: self.capture.close(primary))
        if self.releases:
            self.cleanup_action("origin-close", self.releases.close)
        self.observe("journal-retention", lambda: interrupt_capture.retain_journal(self.output, self.started))
        if self.root:
            self.observe("producer-log-retention", lambda: installed_adoption.retain_producer_logs(self.root, self.output))
        self.record["after"] = self.cleanup_action("native-cleanup-check", host_guard)
        self.record["secondary"] = self.secondary
        self.remove_scratch()
        self.record["cleanupVerified"] = self.cleanup_error is None and self.record.get("scratchAbsent") is True
        self.save()

    def close_case(self):
        self.observe("service-snapshot", lambda: installed_adoption.collect_service_snapshot(self.case))
        self.cleanup_action("native-case-close", self.case.close)
        self.record["caseCleaned"] = self.case.cleaned
        self.observe("native-log-retention", lambda: installed_native.retain_logs(self.case.root, self.output, "pc-interrupt"))
        self.record["serverObservation"] = self.observe("server-trace-retention", lambda: lifecycle.retain(
            self.case, self.output, "pc-interrupt"))

    def remove_scratch(self):
        stopped = self.case is None or self.case.cleaned
        origin_stopped = self.releases is None or self.releases.closed and self.releases.transition_clean
        traces_stopped = self.capture is None or self.capture.closed()
        if self.cleanup_error is not None or not stopped or not origin_stopped or not traces_stopped:
            self.record["scratchAbsent"] = self.root is None
            return
        if self.root:
            self.observe("scratch-remove", lambda: shutil.rmtree(self.root))
        self.record["scratchAbsent"] = self.root is None or not self.root.exists()


def run(args):
    os.environ.pop(lifecycle.FLAG, None)
    clock = installed_clock.Clock({"platform": "linux/amd64", "host": "pc", "mode": "adoption",
        "scenario": "interrupt", "grouped": False, "transportLifecycle": lifecycle.MARKER,
        "buildRecipe": "original-ungrouped-PC/v9001/genuine-single-source-preparation"})
    args.output.mkdir(parents=True, exist_ok=True)
    with clock.activate(watchdog=True):
        owned, primary = Run(args.output), None
        try:
            owned.prepare()
            owned.execute()
        except BaseException as error:  # noqa: BLE001 - Re-raise the same primary object after installed teardown.
            primary = error
            installed_clock.failure(error)
        finally:
            finish(owned, primary, clock)
        if primary is not None:
            raise primary
        if owned.cleanup_error is not None:
            raise owned.cleanup_error
        require_complete(owned.record)


def finish(owned, primary, clock):
    try:
        owned.finish(primary)
    except BaseException as error:
        if primary is None:
            raise
        primary.add_note("diagnostic cleanup also failed: " + type(error).__name__)
    def export():
        return clock.export(owned.output / "fixture-clock.json", owned.record["cleanupVerified"],
            {"harness": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
             "run": os.environ.get("GITHUB_RUN_ID"), "job": os.environ.get("GITHUB_JOB")})

    if primary is None and owned.cleanup_error is None:
        export()
        return
    try:
        export()
    except BaseException as error:  # noqa: BLE001 - Clock observation must preserve an existing product or cleanup error.
        (primary or owned.cleanup_error).add_note("diagnostic clock export also failed: " + type(error).__name__)


def require_complete(record):
    observation = record.get("observation") or {}
    traces = observation.get("traces", {})
    if not record["outcomePassed"] or not record["cleanupVerified"]:
        raise RuntimeError("diagnostic product outcome or cleanup failed")
    if record.get("secondary") or observation.get("errors") or observation.get("chronologyDropped") or set(traces) != {"client", "retiring-daemon", "successor-daemon"}:
        raise RuntimeError("external diagnostic coverage is incomplete")
    if any(value["dropped"] or value["errors"] or value["exitStatus"] != 0 for value in traces.values()):
        raise RuntimeError("external diagnostic trace ended incompletely")
    if (record.get("serverObservation") or {}).get("status") != "covered":
        raise RuntimeError("successor daemon lifecycle coverage is incomplete")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check-host", action="store_true")
    parser.add_argument("--verify-cleanup", action="store_true")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.check_host:
        print(json.dumps(host_guard()))
        return
    if args.verify_cleanup:
        if args.output is None:
            parser.error("--verify-cleanup requires --output")
        verify_cleanup(args.output)
        return
    if args.output is None:
        parser.error("diagnostic execution requires --output")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    run(args)


def verify_cleanup(output):
    host_guard()
    path = output / "diagnostic-result.json"
    if not path.exists():
        return
    value = json.loads(path.read_text())
    if not value.get("cleanupVerified") or Path(value.get("root", "/")).exists():
        raise RuntimeError("diagnostic cleanup is unconfirmed; preserve its owned scratch and evidence")


if __name__ == "__main__":
    main()
