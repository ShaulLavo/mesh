"""Private, bounded observations of the installed fixture's listener startup."""
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
import threading
import time

FAILURE = "listener-failure.json"
MAXIMUM = 65536
SECONDS = 2


def read_private(path, maximum=16384):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError("not a regular fixture file")
    with path.open("rb") as stream:
        data = stream.read(maximum + 1)
    if len(data) > maximum:
        raise ValueError("fixture evidence exceeds its file bound")
    return data


def attempt(action):
    try:
        return action()
    except Exception as error:
        return {"status": "unavailable", "errorType": type(error).__name__}


def frames():
    frame = sys._current_frames().get(threading.main_thread().ident)
    result = []
    while frame is not None and len(result) < 32:
        result.append({"file": Path(frame.f_code.co_filename).name[:96],
                       "function": frame.f_code.co_name[:96], "line": frame.f_lineno})
        frame = frame.f_back
    return result


def request(endpoint, token):
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as connection:
        connection.settimeout(0.2)
        connection.connect(("127.0.0.1", endpoint))
        connection.send(token.encode())
        return json.loads(connection.recv(16384))


class Observer:
    def __init__(self, marker, token):
        self.path = Path(str(marker) + ".observation")
        self.token = token
        self.phase = "imports"
        self.connection = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.connection.bind(("127.0.0.1", 0))
        self.connection.settimeout(0.25)
        self.endpoint = self.connection.getsockname()[1]
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()
        try:
            control = request(self.endpoint, self.token)
            identity = {"pid": os.getpid(), "ppid": os.getppid(), "uid": os.getuid()}
            covered = any(frame["file"] == Path(__file__).name and frame["function"] == "request"
                          for frame in control.get("frames", []))
            if any(control.get(key) != value for key, value in identity.items()) or not covered:
                raise RuntimeError("interpreter observer positive control failed")
        except BaseException:
            self.connection.close()
            raise
        attempt(self.publish)

    def snapshot(self):
        return {"pid": os.getpid(), "ppid": os.getppid(), "uid": os.getuid(),
                "phase": self.phase, "frames": frames()}

    def serve(self):
        while True:
            try:
                token, address = self.connection.recvfrom(256)
                self.respond(token, address)
            except socket.timeout:
                continue
            except OSError:
                return

    def respond(self, token, address):
        if address[0] != "127.0.0.1" or token != self.token.encode():
            return
        self.connection.sendto(json.dumps(self.snapshot()).encode(), address)

    def publish(self):
        value = {"token": self.token, "pid": os.getpid(), "ppid": os.getppid(), "uid": os.getuid(),
                 "endpoint": self.endpoint, "phase": self.phase, "positiveControl": True}
        temporary = self.path.with_suffix(".observation-tmp")
        temporary.write_text(json.dumps(value))
        temporary.replace(self.path)

    def stage(self, phase):
        self.phase = phase
        attempt(self.publish)


def process(pid):
    fields = ("uid", "pid", "ppid", "pgid", "tpgid", "stat", "tty", "lstart")
    arguments = ["ps", "-p", str(pid)]
    for field in fields:
        arguments.extend(["-o", field + "="])
    result = subprocess.run(arguments, capture_output=True, text=True, timeout=0.25, check=False)
    if result.returncode == 1 and not result.stdout.strip():
        return {"status": "exited", "pid": pid}
    if result.returncode or len(result.stdout) > 2048:
        raise RuntimeError("process observation unavailable")
    values = result.stdout.strip().split(None, 7)
    if len(values) != len(fields):
        raise ValueError("process observation incomplete")
    row = dict(zip(fields, values))
    for field in ("uid", "pid", "ppid", "pgid", "tpgid"):
        row[field] = int(row[field])
    row["status"] = "stopped" if any(value in row["stat"] for value in "Tt") else "live"
    if "Z" in row["stat"]:
        row["status"] = "exited"
    row["wchan"] = attempt(lambda: wait_channel(pid))
    return row


def wait_channel(pid):
    result = subprocess.run(["ps", "-p", str(pid), "-o", "wchan="],
                            capture_output=True, text=True, timeout=0.1, check=False)
    if result.returncode or not result.stdout.strip() or len(result.stdout) > 256:
        raise RuntimeError("wait channel unavailable")
    return result.stdout.strip()


def owned(shell, child, expected):
    if shell.get("status") not in ("live", "stopped") or child.get("status") not in ("live", "stopped"):
        return False
    stamp = str(shell["uid"]) + " " + shell["lstart"]
    return (stamp.split() == expected["shellStamp"].split() and shell["uid"] == os.getuid()
            and child["uid"] == shell["uid"] and child["ppid"] == shell["pid"])


def interpreter(marker, token, child):
    entered = attempt(lambda: read_private(Path(str(marker) + ".entered")).decode().splitlines())
    result = {"entered": "unavailable", "hook": "unavailable"}
    if isinstance(entered, list) and entered == [token, str(child["pid"]), str(child["ppid"]), str(child["uid"])]:
        result["entered"] = "observed"
    observation = attempt(lambda: json.loads(read_private(Path(str(marker) + ".observation"))))
    if not observation.get("positiveControl") or observation.get("token") != token:
        return result
    if any(observation.get(key) != child[key] for key in ("pid", "ppid", "uid")):
        return result | {"hook": "ownership-refused"}
    result.update({"hook": "armed", "lastPhase": observation["phase"], "positiveControl": True})
    if child["status"] != "live":
        return result | {"current": {"status": "unavailable-" + child["status"]}}
    current = attempt(lambda: request(observation["endpoint"], token))
    if any(current.get(key) != child[key] for key in ("pid", "ppid", "uid")):
        return result | {"current": {"status": "unavailable-or-ownership-refused"}}
    return result | {"current": current}


def child_observation(expected, shell):
    marker = Path(expected["marker"])
    launched = attempt(lambda: int(read_private(Path(str(marker) + ".pid"), 32)))
    if not isinstance(launched, int) or launched <= 1:
        return {"status": "launch-pid-unavailable", "interpreter": {"status": "unavailable"}}
    child = attempt(lambda: process(launched))
    if child.get("status") == "unavailable":
        return child | {"pid": launched, "interpreter": {"status": "unavailable-process-state"}}
    if child.get("status") == "exited":
        return child | {"interpreter": {"status": "unavailable-exited"},
                        "lastObservation": last_observation(marker, expected["token"], launched)}
    if not owned(shell, child, expected):
        return {"status": "ownership-refused", "pid": launched, "interpreter": {"status": "unavailable"}}
    return child | {"ownership": "live child of original shell at capture; no signals",
                    "interpreter": interpreter(marker, expected["token"], child)}


def last_observation(marker, token, pid):
    value = attempt(lambda: json.loads(read_private(Path(str(marker) + ".observation"))))
    if value.get("token") != token or value.get("pid") != pid:
        return {"status": "unavailable"}
    return {"lastPhase": value["phase"], "positiveControl": value["positiveControl"],
            "status": "historical-only; process absent at capture"}


def collect(expected):
    shell = attempt(lambda: process(expected["shell"]))
    transcript = attempt(lambda: tail(Path(expected["workerLog"])))
    return {"schema": 1, "scope": "original listener readiness timeout", "shell": shell,
            "listener": attempt(lambda: child_observation(expected, shell)), "ptyTranscript": transcript,
            "markerPresentAtCapture": Path(expected["marker"]).exists(),
            "limits": {"seconds": SECONDS, "bytes": MAXIMUM, "signals": False}}


def tail(path):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError("not a regular fixture log")
    with path.open("rb") as stream:
        stream.seek(max(0, os.fstat(stream.fileno()).st_size - 8192))
        return stream.read(8192).decode(errors="replace")


def capture(case, marker, token):
    """Diagnostic failures never replace the original readiness exception."""
    record = {"schema": 1, "status": "unavailable"}
    started = time.monotonic()
    try:
        expected = {"marker": str(marker), "token": token, "shell": case.shell,
                    "shellStamp": case.process_stamps[case.shell],
                    "workerLog": str(case.state / "s" / case.session / "worker.log")}
        result = subprocess.run([sys.executable, str(Path(__file__).resolve())], input=json.dumps(expected),
                                capture_output=True, text=True, timeout=SECONDS, check=False)
        if result.returncode or len(result.stdout.encode()) > MAXIMUM:
            raise RuntimeError("diagnostic collector failed or exceeded its output bound")
        value = json.loads(result.stdout)
        if not isinstance(value, dict):
            raise ValueError("diagnostic output is not a record")
        record = value
    except Exception as error:
        record["errorType"] = type(error).__name__
    record["elapsedSeconds"] = time.monotonic() - started
    encoded = json.dumps(record) + "\n"
    if len(encoded.encode()) > MAXIMUM:
        encoded = json.dumps({"schema": 1, "status": "unavailable", "errorType": "OutputBound"}) + "\n"
    attempt(lambda: (case.root / FAILURE).write_text(encoded))


def wait(case, marker, token, wait_for):
    try:
        wait_for(marker.exists, "session listener did not start")
    except RuntimeError:
        attempt(lambda: capture(case, marker, token))
        raise


def failure_bytes(root):
    data = attempt(lambda: read_private(root / FAILURE, MAXIMUM))
    return json.dumps(data).encode() if isinstance(data, dict) else data


def retain(root, output, name):
    if (root / FAILURE).exists():
        attempt(lambda: (output / (name + "-" + FAILURE)).write_bytes(failure_bytes(root)))


if __name__ == "__main__":
    payload = sys.stdin.buffer.read(16385)
    if len(payload) > 16384:
        raise ValueError("diagnostic input exceeds its bound")
    encoded = json.dumps(collect(json.loads(payload)))
    if len(encoded.encode()) > MAXIMUM:
        raise ValueError("diagnostic output exceeds its bound")
    print(encoded)
