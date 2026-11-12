"""One real installed host owned by one disposable native-manager account."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
from types import SimpleNamespace

from fixtures import installed_adoption, installed_native, installed_origin, native_services, installed_chain


class Origin(installed_origin.Origin):
    def inspect(self, state, output):
        self.read_host("inspect", state, output)

    def ledger(self, state, output):
        self.read_host("ledger", state, output)

    def read_host(self, kind, state, output):
        environment = os.environ | {"MESH_INSTALLED_READ_ROOT": str(state.parent), "MESH_INSTALLED_READ_STATE": str(state),
            "MESH_INSTALLED_READ_OUTPUT": str(output.relative_to(state.parent)), "MESH_INSTALLED_READ_KIND": kind}
        result = subprocess.run([str(self.root / "signer-test"), "-test.run=^TestInstalledHostRead$"],
                                env=environment, capture_output=True, text=True, timeout=15, check=False)
        if result.returncode:
            raise RuntimeError("native account product inspection failed: " + result.stdout[-4096:] + result.stderr[-4096:])


class Host:
    def __init__(self, value):
        releases = SimpleNamespace(**value["releases"])
        releases.directory = Path(releases.directory)
        origin = Origin(Path(value["origin"]))
        self.case = installed_adoption.AdoptionCase(Path(value["root"]), value["host"], value["pair"],
            Path(value["preparer"]), origin, releases, "ordered", value["previous"])
        self.output = Path(value["output"])
        self.closed = False

    def dispatch(self, operation, value):
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
        (case.root / "config/fleet.json").write_text(json.dumps(fleet))
        case.adoption_policy = {"schema": 1, "revision": 1, "mode": "automatic", "after": value["after"], "soak": value["soak"]}
        return {"configured": True}

    def inspect(self, active):
        case = self.case
        case.origin.ledger(case.state, case.root / "ordered-ledger.json")
        result = installed_chain.normalize_ledger(json.loads((case.root / "ordered-ledger.json").read_text()))
        journal = case.state / "update/installation.json"
        result["journal"] = json.loads(journal.read_text()) if journal.exists() else None
        updated = case.state / "updates/host-update.json"
        result["stateWrittenAt"] = updated.stat().st_mtime if updated.exists() else 0
        result["peerFailures"] = peer_failures(case.state, result["peers"])
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
        result["helper"] = native_services.mapped_image(manager.manager_pid(), receipt["executable"])
        if result["helper"]["sha256"] != receipt["digest"]:
            raise RuntimeError("ordered native helper executes another image")
        return result

    def close(self):
        if self.closed:
            return
        try:
            self.case.close()
            self.closed = True
        finally:
            installed_native.retain_logs(self.case.root, self.output, self.case.host["name"])


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
