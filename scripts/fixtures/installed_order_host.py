"""One real installed host owned by one disposable native-manager account."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
from types import SimpleNamespace

from fixtures import installed_adoption, installed_origin, native_services, installed_chain, installed_listener_evidence, installed_clock
from fixtures import installed_order_trace as order_trace


class Origin(installed_origin.Origin):
    def __init__(self, root, diagnostics):
        super().__init__(root)
        self.diagnostics = diagnostics

    def inspect(self, state, output):
        self.read_host("inspect", state, output)

    def ledger(self, state, output):
        self.read_host("ledger", state, output)

    def read_host(self, kind, state, output):
        sidecar = state.parent / order_trace.READER
        if kind == "ledger":
            self.diagnostics.clear_reader(sidecar)
        environment = os.environ | {"MESH_INSTALLED_READ_ROOT": str(state.parent), "MESH_INSTALLED_READ_STATE": str(state),
            "MESH_INSTALLED_READ_OUTPUT": str(output.relative_to(state.parent)), "MESH_INSTALLED_READ_KIND": kind}
        if kind == "ledger":
            environment["MESH_INSTALLED_ORDER_TRACE_OUTPUT"] = order_trace.READER
        try:
            result = self.read_process(environment)
        finally:
            if kind == "ledger":
                self.diagnostics.load_reader(sidecar)
        if result.returncode:
            raise RuntimeError("native account product inspection failed: " + result.stdout[-4096:] + result.stderr[-4096:])

    def read_process(self, environment):
        with order_trace.span(self.diagnostics, "native.reader", operation=environment["MESH_INSTALLED_READ_KIND"]):
            return subprocess.run([str(self.root / "signer-test"), "-test.run=^TestInstalledHostRead$"],
                                env=environment, capture_output=True, text=True, timeout=installed_clock.timeout(15), check=False)


class Host:
    def __init__(self, value):
        self.diagnostics = order_trace.Trace(value["host"]["name"])
        releases = SimpleNamespace(**value["releases"])
        releases.directory = Path(releases.directory)
        origin = Origin(Path(value["origin"]), self.diagnostics)
        self.case = installed_adoption.AdoptionCase(Path(value["root"]), value["host"], value["pair"],
            Path(value["preparer"]), origin, releases, "ordered", value["previous"])
        self.closed = False

    @order_trace.dispatched
    def dispatch(self, operation, value):
        with installed_clock.handoff(value["fixtureDeadlineNs"], watchdog=True):
            return self.dispatch_operation(operation, value)

    def dispatch_operation(self, operation, value):
        if operation == "prepare":
            return self.prepare()
        if operation == "configure":
            return self.configure(value)
        if operation == "adopt":
            version = value["version"]
            started = self.case.start_adoption(version)
            return self.case.validate_result(self.case.await_result(version), version, started["pin"])
        if operation == "inspect":
            return self.inspect(value.get("active", True))
        if operation == "fault":
            self.case.fault.write_text(json.dumps({"daemon": value["versions"]}))
            return {"updated": True}
        if operation in ("stop", "start"):
            if value.get("expectedOffline") is not None:
                self.case.route_observer.offline(value["expectedOffline"])
            getattr(self.case.units, operation)("daemon")
            return {"completed": True}
        if operation == "close":
            self.close()
            return {"servicesAbsent": True, "processesAbsent": True}
        raise RuntimeError("unknown installed ordered-host operation")

    def prepare(self):
        initial = self.case.prepare()
        self.case.seed_prior()
        self.case.seed()
        host = self.case.control("host.info")["host"]
        return {"initial": initial, "id": host["id"], "endpoint": f"ws://127.0.0.1:{self.case.port}/mesh",
                "platform": host["build"]["platform"], "state": str(self.case.state)}

    def configure(self, value):
        case = self.case
        for peer in value["peers"]:
            subprocess.run([str(case.binary), "device", "approve", "--allow-root", "--", peer["id"]],
                           env=case.environment, capture_output=True, check=True, timeout=10)
        fleet = {"version": 1, "name": "installed-order", "revision": 1, "members": value["peers"]}
        (Path(case.environment["MESH_CONFIG_DIR"]) / "fleet.json").write_text(json.dumps(fleet))
        case.adoption_policy = {"schema": 1, "revision": 1, "mode": "automatic", "after": value["after"], "soak": value["soak"]}
        return {"configured": True, "baselineInitialization": case.initialize_preserved_baseline()}

    def inspect(self, active):
        case = self.case
        owner = order_trace.trace(self)
        order_trace.read(owner, "account.ledger", lambda: case.origin.ledger(case.state, case.root / "ordered-ledger.json"))
        result = installed_chain.normalize_ledger(json.loads((case.root / "ordered-ledger.json").read_text()))
        journal = case.state / "update/installation.json"
        result["journal"] = order_trace.read(owner, "account.journal", lambda: json.loads(journal.read_text()) if journal.exists() else None)
        updated = case.state / "updates/host-update.json"
        result["stateWrittenAt"] = order_trace.read(owner, "account.state-stat", lambda: updated.stat().st_mtime if updated.exists() else 0)
        result["peerFailures"] = order_trace.read(owner, "account.peer-failures", lambda: peer_failures(case.state, result["peers"]))
        if owner is not None:
            result["orderRead"] = {"native": owner.reader, "account": dict(owner.reads)}
        if not active:
            return result
        if not case.ready():
            return result | {"ready": False}
        result["ready"] = True
        result["build"] = case.control("host.info")["host"]["build"]
        case.inspect_retained()
        result["route"] = case.route_observer.snapshot()
        receipt = json.loads((case.state / "update/helper/installed.json").read_text())
        manager = case.units.managers["helper"]
        pid = manager.manager_pid()
        try:
            result["helper"] = native_services.mapped_image(pid, receipt["executable"])
            if result["helper"]["sha256"] != receipt["digest"]:
                raise RuntimeError("ordered native helper executes another image")
        except BaseException as error:
            order_trace.helper_inspection_failure(self, error, manager, pid, receipt, result)
            raise
        return result

    def close(self):
        if self.closed:
            return
        try:
            self.case.close()
        except BaseException as error:
            order_trace.failure(self, error, "host.close", "cleanup")
            raise
        finally:
            order_trace.persist_host(self)
        self.closed = True


def export_logs(root):
    result = {}
    for name in ("preserved-before.json", "preserved-after.json", "route-observation.json", "daemon.log", "daemon.err"):
        path = root / name
        if not path.exists():
            continue
        with path.open("rb") as stream:
            result[name] = read_log(stream, name).hex()
    if (root / installed_listener_evidence.FAILURE).exists():
        result[installed_listener_evidence.FAILURE] = installed_listener_evidence.failure_bytes(root).hex()
    order_trace.account_export(root, result)
    print(json.dumps(result), flush=True)


def read_log(stream, name):
    maximum = 65536 if name in ("daemon.log", "daemon.err") else 262144
    if name in ("daemon.log", "daemon.err"):
        stream.seek(max(0, os.fstat(stream.fileno()).st_size - maximum))
    data = stream.read(maximum + 1)
    if len(data) > maximum:
        raise RuntimeError("ordered evidence exceeds its file bound: " + name)
    return data


def interrupted(number, _frame):
    raise InterruptedError("installed ordered host interrupted by signal " + str(number))


def peer_failures(state, peers):
    result = {}
    for peer in peers:
        paths = sorted((state / "updates").glob("host-peer-failure-" + peer["id"] + "-*.json"))
        if len(paths) > 64:
            raise RuntimeError("native peer failure observation exceeds its bounded page")
        result[peer["id"]] = [json.loads(path.read_text()) for path in paths]
    return result


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted" or os.getuid() == 0:
        raise RuntimeError("ordered host requires its disposable hosted user account")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    if sys.argv[1] == "--logs":
        export_logs(Path(sys.argv[2]))
        return
    host = Host(json.loads(Path(sys.argv[1]).read_text()))
    try:
        serve(host)
    finally:
        host.close()


def serve(host):
    for line in sys.stdin:
        request = json.loads(line)
        result = host.dispatch(request["operation"], request)
        print(json.dumps(result), flush=True)
        if request["operation"] == "close":
            return


if __name__ == "__main__":
    main()
