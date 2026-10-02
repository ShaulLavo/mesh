import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import readiness
from run import read_frame, write_frame


@unittest.skipUnless(hasattr(socket, "socketpair"), "control setup tests need socketpair")
class ReadinessTests(unittest.TestCase):
    def exchange(self, action, response, delay=0):
        failures = []
        left, right = socket.socketpair()

        def server():
            try:
                with right:
                    _, payload = read_frame(right)
                    request = json.loads(payload)
                    time.sleep(delay)
                    value = response(request)
                    write_frame(right, 1, json.dumps(value).encode())
            except (BrokenPipeError, ConnectionResetError):
                pass
            except (OSError, ValueError, EOFError) as error:
                failures.append(error)

        thread = threading.Thread(target=server)
        thread.start()
        try:
            with left:
                return action(left)
        finally:
            thread.join(timeout=2)
            self.assertFalse(thread.is_alive())
            if failures:
                raise failures[0]

    @staticmethod
    def identity(request):
        return {"type": "host.info.result", "requestId": request["requestId"],
                "host": {"id": "fixture", "meshIdentity": "fixture-key"}}

    @staticmethod
    def snapshot(request):
        return {"type": "state.snapshot", "requestId": request["requestId"],
                "stateSnapshot": {"seq": 0, "current": {"sessions": {"ageMillis": 0}, "services": {"ageMillis": 0}}}}

    def test_known_good_identity(self):
        observed = self.exchange(lambda conn: readiness.host_info(conn, readiness.SetupDeadline(1)), self.identity)
        self.assertEqual(observed, ("fixture", "fixture-key"))

    def test_delayed_valid_identity_is_setup_not_measurement(self):
        started = time.monotonic()
        observed = self.exchange(lambda conn: readiness.host_info(conn, readiness.SetupDeadline(1)), self.identity, delay=0.35)
        setup_elapsed = time.monotonic() - started
        measurement_started = time.monotonic()
        self.assertEqual(observed, ("fixture", "fixture-key"))
        self.assertGreaterEqual(setup_elapsed, 0.35)
        self.assertGreaterEqual(measurement_started - started, setup_elapsed)

    def test_no_response_is_a_bounded_failure(self):
        with self.assertRaises(TimeoutError):
            self.exchange(lambda conn: readiness.host_info(conn, readiness.SetupDeadline(0.05)), self.identity, delay=0.15)

    def test_deadline_is_shared_with_snapshot(self):
        now = [0.0]
        deadline = readiness.SetupDeadline(1, monotonic=lambda: now[0])
        identity = self.exchange(lambda conn: readiness.host_info(conn, deadline), self.identity)
        self.assertEqual(identity, ("fixture", "fixture-key"))
        now[0] = 1.0
        left, right = socket.socketpair()
        with left, right, self.assertRaises(TimeoutError):
            readiness.subscribe(left, deadline)

    def test_fragmented_frame_cannot_renew_deadline(self):
        now = [0.0]
        deadline = readiness.SetupDeadline(1, monotonic=lambda: now[0])

        class Fragmented:
            def __init__(self):
                self.timeout = None
                self.wire = b'\x01\x00\x00\x00\x02{}'

            def settimeout(self, seconds):
                self.timeout = seconds

            def sendall(self, payload):
                pass

            def recv(self, size):
                now[0] += 0.3
                result, self.wire = self.wire[:1], self.wire[1:]
                return result

        with self.assertRaises(TimeoutError):
            readiness.exchange(Fragmented(), {"type": "host.info"}, deadline)
        self.assertLessEqual(now[0], 1.2)

    def test_protocol_kind_and_incomplete_frame_are_terminal(self):
        for kind, size, payload, failure in ((2, 2, b"{}", ValueError), (1, 10, b"{}", EOFError)):
            left, right = socket.socketpair()
            with left, right:
                right.sendall(bytes([kind]) + size.to_bytes(4, "big") + payload)
                right.shutdown(socket.SHUT_WR)
                with self.assertRaises(failure):
                    readiness.exchange(left, {"type": "host.info"}, readiness.SetupDeadline(1))

    def test_snapshot_error_is_terminal(self):
        valid = self.snapshot({"requestId": "fixture"})["stateSnapshot"]
        invalid = [None, [], {}, {**valid, "seq": True}, {**valid, "seq": -1}, {"seq": 0},
                   {**valid, "current": None}, {**valid, "current": {}}, {**valid, "current": []}]
        for topic in ("sessions", "services"):
            for catalog in (None, False, "", {}, [{"id": "occupied"}]):
                invalid.append({**valid, topic: catalog})
            for observation in (None, {}, {"ageMillis": True}, {"ageMillis": -1}, {"ageMillis": 30000},
                                {"ageMillis": 0, "failing": True}, {"ageMillis": 0, "failing": None}):
                invalid.append({**valid, "current": {**valid["current"], topic: observation}})
        for snapshot in invalid:
            with self.subTest(snapshot=snapshot), self.assertRaises((ValueError, TypeError)):
                self.exchange(lambda conn: readiness.subscribe(conn, readiness.SetupDeadline(1)),
                              lambda request, snapshot=snapshot: {"type": "state.snapshot", "requestId": request["requestId"], "stateSnapshot": snapshot})

    def test_identity_mismatch_and_error_are_terminal(self):
        for response in (
            lambda request: {**self.identity(request), "requestId": "wrong"},
            lambda request: {"type": "error", "requestId": request["requestId"]},
            lambda request: {**self.identity(request), "host": {"id": "fixture"}},
        ):
            with self.subTest(response=response), self.assertRaises(ValueError):
                self.exchange(lambda conn: readiness.host_info(conn, readiness.SetupDeadline(1)), response)
        with self.assertRaisesRegex(ValueError, "changed"):
            self.exchange(lambda conn: readiness.host_info(conn, readiness.SetupDeadline(1), ("other", "other-key")), self.identity)

    def test_snapshot_is_observed_before_setup_completes(self):
        result = self.exchange(lambda conn: readiness.subscribe(conn, readiness.SetupDeadline(1)),
                               self.snapshot, delay=0.35)
        self.assertEqual(result, self.snapshot({"requestId": "fixture"})["stateSnapshot"])
        result = self.exchange(lambda conn: readiness.subscribe(conn, readiness.SetupDeadline(1)),
                               lambda request: {**self.snapshot(request), "stateSnapshot": {
                                   **self.snapshot(request)["stateSnapshot"], "sessions": [], "services": []}})
        self.assertEqual(result["sessions"], [])
        self.assertEqual(result["services"], [])

    def test_decoding_cannot_cross_setup_deadline_into_success(self):
        original_loads = json.loads
        for action, response in ((readiness.host_info, self.identity), (readiness.subscribe, self.snapshot)):
            now = [0.0]

            class Connection:
                def settimeout(self, seconds):
                    pass

                def sendall(self, payload, response=response):
                    request = original_loads(payload[5:])
                    data = json.dumps(response(request)).encode()
                    self.wire = b"\x01" + len(data).to_bytes(4, "big") + data

                def recv(self, size):
                    value, self.wire = self.wire[:size], self.wire[size:]
                    return value

            deadline = readiness.SetupDeadline(1, monotonic=lambda now=now: now[0])
            self.assertIsNotNone(action(Connection(), deadline))

            def delayed_decode(payload, now=now):
                result = original_loads(payload)
                now[0] = 1.1
                return result

            with self.subTest(action=action.__name__), patch.object(readiness.json, "loads", delayed_decode), self.assertRaises(TimeoutError):
                action(Connection(), deadline)

    def test_invalid_deadline_is_rejected(self):
        for seconds in (0, -1, float("inf"), float("nan")):
            with self.subTest(seconds=seconds), self.assertRaises(ValueError):
                readiness.SetupDeadline(seconds)

    @unittest.skipUnless(os.environ.get("MESH") and hasattr(socket, "AF_UNIX"),
                         "real readiness smoke needs the integration Mesh binary and Unix sockets")
    def test_real_isolated_daemon_identity_and_two_subscribers(self):
        binary = Path(os.environ["MESH"]).resolve()
        with tempfile.TemporaryDirectory(prefix="mesh-ready-") as temporary:
            root = Path(temporary)
            environment = dict(os.environ)
            for name in ("home", "state", "config", "runtime", "apps"):
                (root / name).mkdir()
            environment.update(HOME=str(root / "home"), MESH_STATE_DIR=str(root / "state"),
                               MESH_CONFIG_DIR=str(root / "config"), XDG_RUNTIME_DIR=str(root / "runtime"))
            endpoint = root / "state" / "daemon.sock"
            with (root / "daemon.log").open("w+") as log:
                daemon = subprocess.Popen([str(binary), "daemon", "--tailnet-port", "0", "--ssh-port", "0",
                                           "--https-port", "0", "--app-data-root", str(root / "apps")],
                                          env=environment, cwd=root, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                connections = []
                try:
                    deadline = readiness.SetupDeadline(10)

                    def alive():
                        if daemon.poll() is not None:
                            log.seek(0)
                            self.fail(f"isolated daemon exited during setup: {log.read()}")

                    first = readiness.connect(endpoint, deadline, check=alive)
                    connections.append(first)
                    identity = readiness.host_info(first, deadline)
                    first.close()
                    for _ in range(2):
                        conn = readiness.connect(endpoint, deadline, check=alive)
                        connections.append(conn)
                        self.assertEqual(readiness.host_info(conn, deadline, identity), identity)
                        snapshot = readiness.subscribe(conn, deadline)
                        self.assertGreaterEqual(snapshot["seq"], 0)
                    self.assertIsNone(daemon.poll())
                finally:
                    for conn in connections:
                        conn.close()
                    daemon.terminate()
                    try:
                        daemon.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        daemon.kill()
                        daemon.wait(timeout=5)
                self.assertEqual(daemon.returncode, 0)
                self.assertFalse(endpoint.exists(), "owned daemon socket was not released")

    def test_missing_socket_expires_and_connect_failure_closes(self):
        class Refused:
            closed = False

            def settimeout(self, seconds):
                pass

            def connect(self, endpoint):
                raise ConnectionRefusedError()

            def close(self):
                self.closed = True

        conn = Refused()
        with patch.object(readiness.socket, "socket", return_value=conn), self.assertRaises(TimeoutError):
            readiness.connect("unused-fixture", readiness.SetupDeadline(0.01))
        self.assertTrue(conn.closed)


if __name__ == "__main__":
    unittest.main()
