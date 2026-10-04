#!/usr/bin/env python3
import argparse
import json
import socket
import struct
import time

from mesh_control import receive, request_id, round_trip


def send(connection, request):
    payload = json.dumps(request, separators=(",", ":")).encode()
    connection.sendall(b"\x01" + struct.pack(">I", len(payload)) + payload)


def read(connection):
    header = receive(connection, 5)
    assert header[0] == 1, "watch carried terminal data"
    length = struct.unpack(">I", header[1:])[0]
    assert length <= 4 << 20, "watch frame exceeded transport bound"
    return json.loads(receive(connection, length)), length + 5


def connect(path):
    deadline = time.monotonic() + 5
    while True:
        connection = socket.socket(socket.AF_UNIX)
        connection.settimeout(12)
        try:
            connection.connect(path)
            return connection
        except (FileNotFoundError, ConnectionRefusedError):
            connection.close()
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.05)


def watch(path, topics):
    connection = connect(path)
    send(connection, {"type": "host.info", "requestId": request_id("identity")})
    info, _ = read(connection)
    assert info["type"] == "host.info.result"
    send(connection, {"type": "state.watch", "requestId": request_id("watch"), "watch": {"topics": topics, "metricsEvery": 2000}})
    return connection, read(connection)[0]


def observe_idle(connection, initial, seconds):
    snapshot = initial["stateSnapshot"]
    assert set(snapshot["current"]) == {"sessions", "services"}, snapshot
    seq = snapshot["seq"]
    started = time.monotonic()
    idle_bytes = 0
    observed = False
    while time.monotonic() - started < seconds:
        message, size = read(connection)
        idle_bytes += size
        assert message["type"] in ("state.event", "state.current"), message
        content = message.get("stateEvent", message.get("stateCurrent"))
        assert content["seq"] == seq + 1, (seq, content)
        seq = content["seq"]
        current = message.get("stateCurrent")
        if current is None:
            continue
        # A memory event can arrive before the heartbeat at the observation deadline.
        assert current["seq"] == seq, (seq, current)
        assert set(current["sections"]) == set(snapshot["current"]), current
        for section in current["sections"].values():
            assert not section.get("failing", False), section
            assert 0 <= section["ageMillis"] < 30000, section
        observed = True
        if time.monotonic() - started >= seconds - 0.25:
            break
    elapsed = time.monotonic() - started
    connection.close()
    assert observed, "unchanged catalog received no current confirmation"
    assert idle_bytes / elapsed < 1000, (idle_bytes, elapsed)
    return idle_bytes, elapsed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("socket")
    parser.add_argument("site")
    parser.add_argument("--seconds", type=float, default=10)
    args = parser.parse_args()
    connection, initial = watch(args.socket, ["sessions", "services", "metrics"])
    assert initial["type"] == "state.snapshot", initial
    seq = initial["stateSnapshot"]["seq"]
    refused, result = watch(args.socket, ["sessions"])
    refused.close()
    assert result["errorCode"] == "state.subscriber_limit", result
    assert round_trip(args.socket, {"type": "session.list", "requestId": request_id("list"), "lean": True})["type"] == "session.listed"
    service = {"name": "watch-fixture", "kind": "static", "target": args.site}
    result = round_trip(args.socket, {"type": "service.upsert", "requestId": request_id("upsert"), "service": service})
    assert result["type"] == "service.upserted", result
    result = round_trip(args.socket, {"type": "service.delete", "requestId": request_id("delete"), "serviceName": service["name"]})
    assert result["type"] == "service.deleted", result
    started = time.monotonic()
    result = round_trip(args.socket, {"type": "session.create", "requestId": request_id("create"), "command": ["/bin/sh", "-c", "sleep 1"], "term": "xterm", "cols": 80, "rows": 24})
    assert result["type"] == "session.created", result
    session_id = result["sessionId"]
    seen_session = False
    seen_service_removed = False
    seen_metrics = False
    deadline = time.monotonic() + 6
    latency = None
    while time.monotonic() < deadline and not (seen_session and seen_service_removed and seen_metrics):
        message, _ = read(connection)
        assert message["type"] in ("state.event", "state.current"), message
        content = message.get("stateEvent", message.get("stateCurrent"))
        assert content["seq"] == seq + 1, (seq, content)
        seq = content["seq"]
        if message["type"] != "state.event":
            continue
        payload = content["payload"]
        if content["kind"] in ("session.added", "session.changed") and payload["session"]["id"] == session_id:
            record = payload["session"].get("recovery", {})
            assert not record.get("lines") and not record.get("command") and not record.get("agentResume"), "watch sent recovery execution details"
            if not seen_session:
                latency = time.monotonic() - started
            seen_session = True
        seen_service_removed |= content["kind"] == "service.removed"
        if content["kind"] == "metrics":
            metric = payload["metrics"]
            ram = metric["ram"]
            assert ram["availability"] == "available", ram
            assert ram["value"]["totalBytes"] >= ram["value"]["availableBytes"]
            assert ram["value"]["estimate"], ram
            seen_metrics = True
    assert seen_session and seen_service_removed and seen_metrics
    assert latency <= 1.5, latency
    metrics_started = time.monotonic()
    metrics_bytes = 0
    while time.monotonic() - metrics_started < args.seconds:
        message, size = read(connection)
        metrics_bytes += size
        content = message.get("stateEvent", message.get("stateCurrent"))
        assert content["seq"] == seq + 1, (seq, content)
        seq = content["seq"]
    metrics_elapsed = time.monotonic() - metrics_started
    # Per-host metrics payload: measured 1090 B/s with integer cores; retain 15% headroom.
    assert metrics_bytes / metrics_elapsed < 1250, (metrics_bytes, metrics_elapsed)
    connection.close()
    for _ in range(30):
        connection, initial = watch(args.socket, ["sessions", "services"])
        if initial["type"] == "state.snapshot":
            break
        connection.close()
        time.sleep(0.05)
    assert initial["type"] == "state.snapshot", "subscriber reservation leaked"
    assert all(row["name"] != service["name"] for row in initial["stateSnapshot"].get("services", []))
    idle_bytes, elapsed = observe_idle(connection, initial, args.seconds)
    print(json.dumps({"metricsIdleBytes": metrics_bytes, "metricsIdleSeconds": round(metrics_elapsed, 3), "metricsIdleBytesPerSecond": round(metrics_bytes / metrics_elapsed, 3), "idleBytes": idle_bytes, "idleSeconds": round(elapsed, 3), "idleBytesPerSecond": round(idle_bytes / elapsed, 3), "sessionDeliverySeconds": round(latency, 3), "subscriberRelease": True, "adapterRAM": ram}, sort_keys=True))


if __name__ == "__main__":
    main()
