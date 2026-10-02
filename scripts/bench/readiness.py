"""Control setup for an isolated daemon; call before starting a measurement."""
import json
import math
import socket
import time
import uuid

from run import control, read_frame


class SetupDeadline:
    def __init__(self, seconds=60, monotonic=time.monotonic):
        if not math.isfinite(seconds) or seconds <= 0:
            raise ValueError("setup deadline must be finite and positive")
        self.monotonic = monotonic
        self.ends = monotonic() + seconds

    def remaining(self):
        seconds = self.ends - self.monotonic()
        if seconds <= 0:
            raise TimeoutError("isolated daemon setup deadline expired")
        return seconds


def connect(endpoint, deadline, check=lambda: None):
    while True:
        check()
        remaining = deadline.remaining()
        connection = socket.socket(socket.AF_UNIX)
        try:
            connection.settimeout(remaining)
            connection.connect(str(endpoint))
            deadline.remaining()
            return connection
        except (FileNotFoundError, ConnectionRefusedError):
            connection.close()
            time.sleep(min(0.1, deadline.remaining()))
        except BaseException:
            connection.close()
            raise


class DeadlineConnection:
    # A fragmented frame shares the outer budget; each recv cannot renew it.
    def __init__(self, connection, deadline):
        self.connection = connection
        self.deadline = deadline

    def sendall(self, payload):
        self.connection.settimeout(self.deadline.remaining())
        self.connection.sendall(payload)
        self.deadline.remaining()

    def recv(self, size):
        self.connection.settimeout(self.deadline.remaining())
        result = self.connection.recv(size)
        self.deadline.remaining()
        return result


def exchange(connection, request, deadline):
    bounded = DeadlineConnection(connection, deadline)
    control(bounded, request)
    kind, payload = read_frame(bounded)
    if kind != 1:
        raise ValueError("setup expected a control response")
    result = json.loads(payload)
    deadline.remaining()
    if not isinstance(result, dict):
        raise TypeError("setup expected a control object")
    return result


def host_info(connection, deadline, expected_identity=None):
    request = {"type": "host.info", "requestId": uuid.uuid4().hex}
    result = exchange(connection, request, deadline)
    if result.get("type") != "host.info.result" or result.get("requestId") != request["requestId"]:
        raise ValueError("setup host identity response did not match the request")
    host = result.get("host")
    if not isinstance(host, dict):
        raise TypeError("setup host identity is incomplete")
    identity = host.get("id"), host.get("meshIdentity")
    if not all(isinstance(value, str) and value for value in identity):
        raise ValueError("setup host identity is incomplete")
    if expected_identity is not None and identity != expected_identity:
        raise ValueError("setup host identity changed")
    deadline.remaining()
    return identity


def subscribe(connection, deadline):
    request = {"type": "state.watch", "requestId": uuid.uuid4().hex,
               "watch": {"topics": ["sessions", "services", "metrics"], "metricsEvery": 2000}}
    result = exchange(connection, request, deadline)
    if result.get("type") != "state.snapshot" or result.get("requestId") != request["requestId"]:
        raise ValueError("setup watch snapshot did not match the request")
    snapshot = result.get("stateSnapshot")
    if not isinstance(snapshot, dict):
        raise TypeError("setup watch snapshot is missing")
    if type(snapshot.get("seq")) is not int or snapshot["seq"] < 0:
        raise ValueError("setup watch snapshot has no sequence")
    current = snapshot.get("current")
    if not isinstance(current, dict):
        raise TypeError("setup watch snapshot has no observations")
    for topic in ("sessions", "services"):
        catalog = snapshot.get(topic, [])
        if not isinstance(catalog, list) or catalog:
            raise ValueError("isolated idle daemon requires empty catalog lists")
        observation = current.get(topic)
        if not isinstance(observation, dict):
            raise TypeError("setup watch snapshot has an incomplete observation")
        age = observation.get("ageMillis")
        if type(age) is not int or not 0 <= age < 30000 or observation.get("failing", False) is not False:
            raise ValueError("setup watch snapshot observation is unavailable or stale")
    deadline.remaining()
    return snapshot
