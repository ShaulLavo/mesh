import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import struct
import tempfile
import sys
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

spec = importlib.util.spec_from_file_location("bench", Path(__file__).with_name("run.py"))
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


class FragmentedConnection:
    def __init__(self, wire, fragments):
        self.input = io.BytesIO(wire)
        self.fragments = fragments
        self.index = 0
        self.closed = False

    def recv(self, size):
        limit = self.fragments[self.index % len(self.fragments)]
        self.index += 1
        return self.input.read(min(size, limit))

    def sendall(self, _data):
        pass

    def close(self):
        self.closed = True


class HarnessTests(unittest.TestCase):
    def test_measurement_harness_identifies_executed_scripts(self):
        import hashlib
        observed = bench.measurement_harness()
        for name in ("run.py", "workload.py", "receipt.py", "process_diagnostics.py"):
            expected = hashlib.sha256((bench.HERE / name).read_bytes()).hexdigest()
            self.assertEqual(observed["files"][name], expected)
        expected = hashlib.sha256(json.dumps(observed["files"], sort_keys=True).encode()).hexdigest()
        self.assertEqual(observed["sha256"], expected)

    def test_fragmented_frame_and_control(self):
        left, right = socket.socketpair()
        with left, right:
            bench.control(left, {"type": "session.list"})
            kind, data = bench.read_frame(right)
            self.assertEqual(kind, 1)
            self.assertEqual(json.loads(data), {"type": "session.list"})
            left.sendall(struct.pack(">BI", 2, 9) + b"short")
            left.shutdown(socket.SHUT_WR)
            with self.assertRaises(EOFError):
                bench.read_frame(right)

    def test_rpc_uses_distinct_creation_ids(self):
        from unittest.mock import patch
        requests = []
        for _ in range(2):
            left, right = socket.socketpair()
            with left, right, patch.object(bench, "dial", return_value=left):
                bench.write_frame(right, 1, b'{"type":"session.created"}')
                bench.rpc(Path("unused"), {"type": "session.create"})
                _, payload = bench.read_frame(right)
                requests.append(json.loads(payload)["requestId"])
        self.assertNotEqual(*requests)

    def test_payload_limit(self):
        left, right = socket.socketpair()
        with left, right:
            left.sendall(struct.pack(">BI", 1, (4 << 20) + 1))
            with self.assertRaises(ValueError):
                bench.read_frame(right)

    def test_proc_sampling_observes_known_good_process(self):
        sample = bench.proc_sample(os.getpid())
        self.assertGreater(sample["rss_bytes"], 0)
        self.assertGreater(sample["start_ticks"], 0)
        self.assertEqual(sample["ppid"], os.getppid())

    def test_command_resources_and_failure(self):
        with tempfile.TemporaryDirectory() as root:
            result = bench.timed_command(Path(os.sys.executable), ["-c", "print('ok')"],
                                        dict(os.environ), root)
            self.assertGreater(result["wall_ms"], 0)
            self.assertGreater(result["cpu_ms"], 0)
            self.assertGreater(result["peak_rss_bytes"], 0)
            self.assertEqual(result["output_bytes"], 3)
            with self.assertRaises(RuntimeError):
                bench.timed_command(Path(os.sys.executable), ["-c", "raise SystemExit(2)"],
                                    dict(os.environ), root)

    def throughput_result(self, size, frames, fragments=(1, 3, 5, 19)):
        wire = b"".join(struct.pack(">BI", kind, len(payload)) + payload for kind, payload in frames)
        connection = FragmentedConnection(wire, fragments)
        samples = [{"ticks": 40, "start_ticks": 7}, {"ticks": 45, "start_ticks": 7}]
        try:
            with patch.object(bench, "measure_attach", return_value=(connection, {})), \
                    patch.object(bench, "own_worker", return_value=123), \
                    patch.object(bench, "proc_sample", side_effect=samples):
                return bench.throughput(Path("unused"), "7K3D", size)
        finally:
            self.assertTrue(connection.closed)

    def terminal_frame(self, sequence, data, sid=b"7K3D\0\0\0\0"):
        return 2, sid + struct.pack(">Q", sequence) + data

    def test_throughput_rejects_identical_length_corruption(self):
        data = bytearray(bench.workload.block(32))
        data[20] ^= 1
        frames = [self.terminal_frame(32, data + b"\r\nBENCH_DONE\r\n")]
        with self.assertRaisesRegex(RuntimeError, "content"):
            self.throughput_result(32, frames)

    def test_throughput_checks_fragmented_arbitrary_payloads(self):
        for size in (1, 73, (32 << 10) - 1, 32 << 10, (32 << 10) + 1, 70003):
            self.check_throughput_payload(size)

    def check_throughput_payload(self, size):
        blocks = [bench.workload.block(min(32 << 10, size - offset))
                  for offset in range(0, size, 32 << 10)]
        data = b"".join(blocks) + b"\r\nBENCH_DONE\r\n"
        # Frame boundaries split both ANSI sequences and the completion marker.
        frames = [self.terminal_frame(912 + offset, data[offset:offset + 11])
                  for offset in range(0, len(data), 11)]
        result = self.throughput_result(size, frames)
        self.assertEqual(result["received_bytes"], len(data))
        self.assertEqual(result["payload_sha256"], hashlib.sha256(data).hexdigest())
        self.assertEqual(result["worker_cpu"]["delta_ticks"], 5)
        self.assertEqual(result["worker_cpu"]["seconds"], 5 / os.sysconf("SC_CLK_TCK"))

    def test_throughput_checks_sequence_and_exact_bytes(self):
        marker = b"\r\nBENCH_DONE\r\n"
        cases = [
            ("dropped", [self.terminal_frame(0, b"x"), self.terminal_frame(2, marker)]),
            ("byte mismatch", [self.terminal_frame(0, b"xx" + marker)]),
            ("before completion", [(1, b'{}')]),
            ("invalid terminal frame", [(2, b"short")]),
            ("invalid terminal frame", [self.terminal_frame(0, b"x" + marker, b"OTHER\0\0\0")]),
            ("content", [self.terminal_frame(0, marker + b"x")]),
        ]
        for message, frames in cases:
            self.check_throughput_failure(message, frames)

    def check_throughput_failure(self, message, frames):
        with self.assertRaisesRegex(RuntimeError, message):
            self.throughput_result(1, frames)

    def test_throughput_rejects_missing_marker(self):
        with self.assertRaises(EOFError):
            self.throughput_result(1, [self.terminal_frame(0, b"x")])

    def test_cpu_delta_preserves_zero_and_rejects_identity_or_counter_changes(self):
        before = {"ticks": 42, "start_ticks": 7}
        self.assertEqual(bench.cpu_delta(before, before)["delta_ticks"], 0)
        with self.assertRaisesRegex(RuntimeError, "identity"):
            bench.cpu_delta(before, {"ticks": 45, "start_ticks": 8})
        with self.assertRaisesRegex(RuntimeError, "backwards"):
            bench.cpu_delta(before, {"ticks": 41, "start_ticks": 7})

    def test_cpu_delta_observes_real_process_work(self):
        before = bench.proc_sample(os.getpid())
        # CPU, not a wall-clock delay. Large enough to cross a /proc tick.
        value = 0
        for number in range(1000000):
            value ^= number * number
        self.assertIsInstance(value, int)
        observed = bench.cpu_delta(before, bench.proc_sample(os.getpid()))
        self.assertGreater(observed["delta_ticks"], 0)

    def test_wal_counts_commits_frames_and_resets(self):
        with tempfile.TemporaryDirectory() as root:
            database = Path(root) / "test.db"
            wal = Path(str(database) + "-wal")
            shm = Path(str(database) + "-shm")

            def write(salt, commits):
                header = bytearray(32)
                struct.pack_into(">I", header, 8, 512)
                header[16:24] = salt
                data = bytearray(header)
                for commit in commits:
                    frame = bytearray(24 + 512)
                    struct.pack_into(">II", frame, 0, 1, int(commit))
                    frame[8:16] = salt
                    data.extend(frame)
                wal.write_bytes(data)
                index = bytearray(48)
                struct.pack_into("=I", index, 16, len(commits))
                index[32:40] = salt
                shm.write_bytes(index)

            write(b"12345678", [True])
            counter = bench.WALCounter(database)
            write(b"12345678", [True, False, True, True])
            counter.poll()
            self.assertEqual((counter.frames, counter.commits), (3, 2))
            counter.poll()
            self.assertEqual((counter.frames, counter.commits), (3, 2))
            write(b"abcdefgh", [False, True])
            counter.poll()
            self.assertEqual((counter.frames, counter.commits, counter.resets), (5, 3, 1))

    def test_wal_matches_real_sqlite_writes(self):
        import sqlite3
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "real.db"
            with sqlite3.connect(path) as db:
                db.execute("PRAGMA journal_mode=WAL")
                db.execute("CREATE TABLE sample (n INTEGER)")
                db.commit()
                counter = bench.WALCounter(path)
                for n in range(3):
                    db.execute("INSERT INTO sample VALUES (?)", (n,))
                    db.commit()
                    counter.poll()
                self.assertEqual(counter.commits, 3)
                self.assertEqual(counter.frames, 3)
                db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
                db.execute("INSERT INTO sample VALUES (4)")
                db.commit()
                counter.poll()
                self.assertEqual(counter.commits, 4)
                self.assertEqual(counter.resets, 1)


if __name__ == "__main__":
    unittest.main()
