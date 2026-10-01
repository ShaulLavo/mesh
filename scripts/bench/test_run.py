import importlib.util
import json
import os
from pathlib import Path
import socket
import struct
import tempfile
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))

spec = importlib.util.spec_from_file_location("bench", Path(__file__).with_name("run.py"))
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


class HarnessTests(unittest.TestCase):
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

    def test_throughput_checks_sequence_and_exact_bytes(self):
        from unittest.mock import patch
        left, right = socket.socketpair()
        with left, right:
            def attached(_state, _sid):
                data = b"x" * 32 + b"\r\nBENCH_DONE\r\n"
                frame = b"7K3D\0\0\0\0" + struct.pack(">Q", 32) + data
                bench.write_frame(right, 2, frame)
                return left, {}
            with patch.object(bench, "measure_attach", attached):
                result = bench.throughput(Path("unused"), "7K3D", 32)
                self.assertEqual(result["received_bytes"], 46)
        left, right = socket.socketpair()
        with left, right:
            def dropped(_state, _sid):
                for sequence, data in [(0, b"x"), (2, b"\r\nBENCH_DONE\r\n")]:
                    bench.write_frame(right, 2, b"7K3D\0\0\0\0" + struct.pack(">Q", sequence) + data)
                return left, {}
            with patch.object(bench, "measure_attach", dropped), self.assertRaisesRegex(RuntimeError, "dropped"):
                bench.throughput(Path("unused"), "7K3D", 1)

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
