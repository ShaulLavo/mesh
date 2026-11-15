"""Same-case controls and bounded chronology for one diagnostic interruption."""
import json
import os
import signal
import subprocess
import threading
import time
import uuid
from pathlib import Path

from fixtures import installed_clock, installed_native
from fixtures import interrupt_trace as external

ORIGINAL = "940d638ea8ea5be29f17c1ab311aedd4039be2f6a221bd1f28a4ae2f1aee9eda"


class Capture:
    def __init__(self, case, output):
        self.case, self.output = case, Path(output)
        self.directory = self.output / "transport-close"
        self.directory.mkdir(mode=0o700)
        self.traces, self.errors = {}, []
        self.stop = threading.Event()
        self.thread = None
        self.rows, self.dropped = 0, 0
        self.written = 0
        self.stream = (self.directory / "chronology.jsonl").open("x")
        self.lock = threading.Lock()
        self.original = {}
        self.control = None
        self.parent_deadline = installed_clock.parent_deadline()

    def event(self, stage, **fields):
        data = json.dumps({"stage": stage, "unixNano": time.time_ns(),
                           "monotonicNano": time.monotonic_ns(), **fields}) + "\n"
        size = len(data.encode())
        with self.lock:
            if self.rows >= 2048 or size > 65536 or self.written + size > 2 << 20:
                self.dropped += 1
                return
            self.stream.write(data)
            self.stream.flush()
            self.rows += 1
            self.written += size

    def attach(self, role, pid, digest):
        actor = external.identity(pid)
        if actor["uid"] != os.getuid() or actor["sha256"] != digest:
            raise RuntimeError("external observation refused another actor owner or image")
        trace = external.Trace(self.directory, role, actor, parent=self.parent_deadline)
        self.traces[role] = trace
        self.event("tracer.before-attach", role=role, identity=actor, sockets=external.sockets(pid))
        trace.start()
        external.wait(trace.armed, "external tracer did not cover all actor threads", parent=self.parent_deadline, stop=self.stop)
        trace.require_identity()
        self.event("tracer.armed", role=role, tasks=external.tasks(pid), sockets=external.sockets(pid))
        return trace

    def arm(self):
        case = self.case
        self.original = {"client": external.identity(case.terminal.pid),
            "worker": external.identity(case.worker), "shell": external.identity(case.shell),
            "listener": external.identity(case.listener["pid"]), "session": case.session}
        daemon = installed_native.peer_pid(case.state / "daemon.sock")
        self.attach("client", case.terminal.pid, ORIGINAL)
        self.attach("retiring-daemon", daemon, ORIGINAL)
        self.positive_control()
        self.thread = threading.Thread(target=self.watch_successor, daemon=True)
        self.thread.start()

    def positive_control(self):
        client, daemon = self.traces["client"], self.traces["retiring-daemon"]
        pair = matched_socket(client.expected["pid"], daemon.expected["pid"], self.case.port)
        before = traffic(client, pair[0]["fd"]), traffic(daemon, pair[1]["fd"])
        token = uuid.uuid4().hex
        marker = "__OBSERVED__" + token + "__" + str(self.case.shell) + "__"
        start = len(self.case.terminal.drain())
        self.event("control.before", session=self.case.session, sockets=pair, token=token)
        self.case.terminal.send("printf '__OBSERVED__%s__%s__\\n' " + token + " \"$$\"\n")
        self.case.terminal.expect(marker, since=start)
        external.wait(lambda: progressed(client, pair[0]["fd"], before[0]) and progressed(daemon, pair[1]["fd"], before[1]),
                      "same original client and daemon socket traffic was not observed", parent=self.parent_deadline)
        for trace in (client, daemon):
            trace.require_identity()
            if not trace.armed():
                raise RuntimeError("original process instrumentation became unarmed")
            if token in trace.path.read_text():
                raise RuntimeError("external observer rendered the synthetic control payload")
        self.control = {"passed": True, "session": self.case.session, "sockets": pair,
            "client": client.expected, "retiringDaemon": daemon.expected, "shell": self.original["shell"],
            "before": before, "after": [traffic(client, pair[0]["fd"]), traffic(daemon, pair[1]["fd"])]}
        self.event("control.passed", control=self.control)

    def require_armed(self):
        if self.control is None or self.errors or self.dropped:
            raise RuntimeError("interrupt diagnostic control is unarmed")
        for role in ("client", "retiring-daemon"):
            trace = self.traces[role]
            trace.require_identity()
            if not trace.armed():
                raise RuntimeError("original actor tracer lost coverage before interruption")

    def watch_successor(self):
        marker = self.case.root / ("start." + self.case.releases.builds["v0.1.9001"]["version"] + ".json")
        try:
            self.await_successor(marker)
        except Exception as error:  # noqa: BLE001 - A watcher failure must not alter the native transaction.
            self.errors.append({"stage": "successor-observation", "type": type(error).__name__})

    def await_successor(self, marker):
        while not self.stop.wait(0.02):
            started = read_start(marker)
            if started is None:
                continue
            pid = started["pid"]
            self.attach("successor-daemon", pid, self.case.releases.builds["v0.1.9001"]["digest"])
            return

    def retained_identity(self):
        case = self.case
        if case.terminal.pid != self.original["client"]["pid"] or case.terminal.poll() is not None:
            raise RuntimeError("original diagnostic client exited or changed")
        for role, pid in (("client", case.terminal.pid), ("worker", case.worker),
                          ("shell", case.shell), ("listener", case.listener["pid"])):
            if external.identity(pid) != self.original[role]:
                raise RuntimeError("original diagnostic actor identity changed: " + role)
        self.event("outcome.original-identities", session=case.session, identities=self.original)

    def state(self, stage):
        self.event(stage, clientExitStatus=self.case.terminal.poll(), session=self.case.session,
                   states=selected_state(self.case), managers=manager_actors(self.case), actorSockets=actor_sockets(self.traces))

    def close(self, primary=None):
        self.stop.set()
        if self.thread:
            self.thread.join(timeout=external.budget())
            if self.thread.is_alive():
                self.errors.append({"stage": "successor-collector", "type": "JoinTimeout"})
        for role, trace in self.traces.copy().items():
            try:
                trace.close()
            except Exception as error:  # noqa: BLE001 - Attempt every owned tracer cleanup.
                self.errors.append({"stage": "tracer-close", "role": role, "type": type(error).__name__})
        self.event("observer.end", primaryType=type(primary).__name__ if primary else None)
        self.stream.close()
        value = {"diagnosticOnly": True, "accepted": False, "control": self.control, "original": self.original,
            "traces": {role: trace.summary() for role, trace in self.traces.items()}, "errors": self.errors,
            "chronologyDropped": self.dropped, "successorExternalAttachMayBeLate": True,
            "chronologyBytes": self.written, "chronologyLimits": {"rows": 2048, "bytes": 2 << 20, "recordBytes": 65536},
            "originalGoErrorChain": "unavailable in the unmodified published client",
            "effects": "ptrace stops threads at selected syscalls; no claim of timing equivalence or ordinary acceptance"}
        (self.directory / "summary.json").write_text(json.dumps(value, indent=2) + "\n")
        return value

    def closed(self):
        if self.thread and self.thread.is_alive():
            return False
        return all(trace.process is None or trace.process.poll() is not None for trace in self.traces.values())


def traffic(trace, fd):
    with trace.lock:
        return [sum(value for (call, number), value in trace.progress.items() if number == fd and call in calls)
                for calls in (external.READS, external.WRITES)]


def read_start(marker):
    try:
        if marker.stat().st_size > 4096:
            raise ValueError("candidate startup marker exceeds its bound")
        return json.loads(marker.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return None


def progressed(trace, fd, before):
    return all(after > previous for after, previous in zip(traffic(trace, fd), before))


def matched_socket(client, daemon, port):
    left = [row for row in external.sockets(client) if row["state"] == "01" and row["remote"].endswith(":" + str(port))]
    right = external.sockets(daemon)
    pairs = [(a, b) for a in left for b in right if (a["local"], a["remote"]) == (b["remote"], b["local"])]
    if len(pairs) != 1:
        raise RuntimeError("original terminal client and daemon lack one matching TCP connection")
    return pairs[0]


def actor_sockets(traces):
    return {role: attempt(lambda trace=trace: external.sockets(trace.expected["pid"])) for role, trace in traces.copy().items()}


def manager_actors(case):
    return {role: attempt(lambda manager=manager: external.identity(manager.manager_pid()))
            for role, manager in case.units.managers.items()}


def attempt(action):
    try:
        return action()
    except Exception as error:  # noqa: BLE001 - Evidence collection cannot replace a product failure.
        return {"errorType": type(error).__name__}


def selected_state(case):
    paths = {"installation": case.state / "update/installation.json", "adoption": case.state / "updates/host-adoption.json",
             "receipt": case.state / "update/helper/installed.json", "worker": case.state / "s" / case.session / "meta.json"}
    return {name: attempt(lambda name=name, path=path: state_document(name, path)) for name, path in paths.items()}


def state_document(name, path):
    if path.stat().st_size > 1 << 20:
        raise ValueError("state evidence exceeded its bound")
    value = json.loads(path.read_bytes())
    fields = {"installation": ("phase", "request"), "adoption": ("schema", "confirmed", "settled", "details"),
              "receipt": ("executable", "servicePath", "digest"),
              "worker": ("id", "pid", "state", "lastAttachedAt", "detachedAt", "exitedAt", "exitCode")}
    result = {key: value[key] for key in fields[name] if key in value}
    if name == "installation":
        request = result.pop("request", {})
        result.update(requestId=request.get("id"), version=request.get("manifest", {}).get("version"))
    if name == "adoption":
        details = result.pop("details", {})
        result["phase"] = details.get("phase")
    return result


def interrupt(case, capture, version):
    marker = case.root / ("start.helper." + version + ".json")
    installed_native.native_services.wait_for(marker.exists, "replacement helper startup boundary is absent")
    started = json.loads(marker.read_text())
    pid = case.units.managers["helper"].manager_pid()
    receipt = json.loads((case.state / "update/helper/installed.json").read_text())
    image = installed_native.native_services.mapped_image(pid, receipt["executable"])
    expected = case.releases.builds[version]["digest"]
    if pid != started["pid"] or image["sha256"] != expected or receipt["digest"] != expected:
        raise RuntimeError("replacement helper boundary belongs to another native process or image")
    capture.require_armed()
    capture.event("helper.kill.before", identity=external.identity(pid), image=image, states=selected_state(case))
    os.kill(pid, signal.SIGKILL)
    capture.event("helper.kill.sent", pid=pid, signal=signal.SIGKILL)
    case.write_fault({})
    return {"killedNativePID": pid, "mappedImage": image,
            "boundary": "real helper launched after durable receipt/launcher replacement"}


def retain_journal(output, since):
    fields = ("__REALTIME_TIMESTAMP", "__MONOTONIC_TIMESTAMP", "_PID", "_COMM", "_SYSTEMD_USER_UNIT",
              "USER_UNIT", "MESSAGE_ID", "EXIT_CODE", "EXIT_STATUS", "JOB_TYPE", "JOB_RESULT")
    command = ["/usr/bin/journalctl", "--user", "-u", "mesh.service", "-u", "mesh-update-helper.service",
               "--since=@" + str(since), "--output=json", "--output-fields=" + ",".join(fields), "--no-pager", "-n", "256"]
    result = subprocess.run(command, capture_output=True, timeout=external.budget(), check=False)
    if len(result.stdout) > 1 << 20:
        raise ValueError("native journal exceeded its bound")
    rows = [{key: value[key] for key in fields if key in value} for value in
            (json.loads(line) for line in result.stdout.splitlines())]
    (Path(output) / "native-journal.json").write_text(json.dumps({"exitStatus": result.returncode,
        "boundedNewestRecords": 256, "messagesOmitted": True, "records": rows}) + "\n")
