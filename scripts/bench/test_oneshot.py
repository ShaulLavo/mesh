#!/usr/bin/env python3
"""Unit tests for scripts/bench/oneshot.py pure logic, markers, and cleanup."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
import oneshot  # noqa: E402 - adjacent benchmark module
import ssh_command  # noqa: E402 - adjacent benchmark module


def make_pairs(mesh_seconds, ssh_seconds):
    return [
        {"order": ["mesh", "ssh"], "mesh": {"seconds": m}, "ssh": {"seconds": s}}
        for m, s in zip(mesh_seconds, ssh_seconds)
    ]


class OneshotPureLogicTests(unittest.TestCase):
    def test_parse_hosts_normalizes_pc_and_deduplicates(self):
        self.assertEqual(oneshot.parse_hosts("pc, vps, pi, mac, local"), ["local", "vps", "pi", "mac"])
        self.assertEqual(oneshot.parse_hosts("VPS,mac"), ["vps", "mac"])

    def test_parse_hosts_rejects_unknown_or_empty(self):
        for bad in ("", " , ", "local,bogus"):
            with self.assertRaises(ValueError):
                oneshot.parse_hosts(bad)

    def test_required_platforms_always_includes_pc_client(self):
        self.assertEqual(oneshot.required_platforms(["mac"]), ["darwin/arm64", "linux/amd64"])
        self.assertEqual(oneshot.required_platforms(["pi"]), ["linux/amd64", "linux/arm64"])
        self.assertEqual(oneshot.required_platforms(["local", "vps"]), ["linux/amd64"])
        self.assertEqual(
            oneshot.required_platforms(["local", "vps", "pi", "mac"]),
            ["darwin/arm64", "linux/amd64", "linux/arm64"],
        )

    def test_parse_tailscale_ping_handles_all_units(self):
        stdout = "\n".join([
            "pong from vps (100.84.25.68) via 37.27.17.186:41641 in 80ms",
            "pong from vps (100.84.25.68) via 37.27.17.186:41641 in 79.5ms",
            "pong from pi (100.94.222.118) via 192.168.0.179:41641 in 500µs",
            "pong from slow (100.94.222.119) via derp in 1.2s",
        ])
        parsed = oneshot.parse_tailscale_ping(stdout)
        self.assertTrue(parsed["available"])
        self.assertEqual(parsed["samples_ms"], [80.0, 79.5, 0.5, 1200.0])
        self.assertEqual(parsed["summary_ms"]["min"], 0.5)
        self.assertEqual(parsed["summary_ms"]["p50"], 79.75)
        self.assertEqual(parsed["summary_ms"]["max"], 1200.0)

    def test_parse_tailscale_ping_unavailable_on_empty(self):
        parsed = oneshot.parse_tailscale_ping("timeout waiting for pong\n")
        self.assertFalse(parsed["available"])
        self.assertEqual(parsed["samples_ms"], [])

    def test_parse_tailscale_peer_ipv4(self):
        status = {
            "Peer": {
                "a": {"HostName": "Shaul's Mac", "DNSName": "shauls-macbook-air.tail.ts.net.", "TailscaleIPs": ["fd7a::1", "100.92.135.44"]},
                "b": {"HostName": "pi", "DNSName": "pi.tail.ts.net.", "TailscaleIPs": ["100.94.222.118"]},
            }
        }
        self.assertEqual(oneshot.parse_tailscale_peer_ipv4(status, "shauls-macbook-air"), "100.92.135.44")
        self.assertEqual(oneshot.parse_tailscale_peer_ipv4(status, "pi"), "100.94.222.118")
        self.assertIsNone(oneshot.parse_tailscale_peer_ipv4(status, "missing"))

    def test_parse_master_check_pid_and_ssh_connection(self):
        self.assertEqual(oneshot.parse_master_check_pid("Master running (pid=643652)\n"), 643652)
        self.assertIsNone(oneshot.parse_master_check_pid("Control socket connect failed"))
        self.assertEqual(
            oneshot.parse_ssh_connection("100.77.94.94 44224 100.92.135.44 22\n"),
            {"client_ip": "100.77.94.94", "client_port": "44224", "server_ip": "100.92.135.44", "server_port": "22"},
        )
        self.assertIsNone(oneshot.parse_ssh_connection(""))

    def test_build_ssh_exec_command_pins_cwd_and_absolute_true(self):
        cmd = oneshot.build_ssh_exec_command("/Users/shaul/mesh-oneshot-bench-1/work", "/usr/bin/true")
        self.assertEqual(cmd, "cd /Users/shaul/mesh-oneshot-bench-1/work && exec /usr/bin/true")

    def test_marker_stream_reads_coalesced_markers(self):
        read_fd, write_fd = os.pipe()
        with os.fdopen(read_fd, "r", encoding="utf-8") as reader:
            stream = oneshot.MarkerStream(reader)
            coalesced = (
                "noise before\n"
                'ONESHOT_SERVER_BENCH {"samples":[{"total_seconds":0.1}]}\n'
                'ONESHOT_STOPPED {"clean":true,"remaining_pids":[]}\n'
            ).encode("utf-8")
            os.write(write_fd, coalesced)
            os.close(write_fd)
            bench_payload = stream.read_marker(oneshot.SERVER_BENCH_PREFIX, timeout=2.0)
            stop_payload = stream.read_marker(oneshot.STOPPED_PREFIX, timeout=2.0)
        self.assertEqual(bench_payload, {"samples": [{"total_seconds": 0.1}]})
        self.assertEqual(stop_payload, {"clean": True, "remaining_pids": []})

    def test_drop_warmups(self):
        warm, measured = oneshot.drop_warmups([1, 2, 3, 4, 5], 3)
        self.assertEqual(warm, [1, 2, 3])
        self.assertEqual(measured, [4, 5])
        with self.assertRaises(ValueError):
            oneshot.drop_warmups([1], -1)

    def test_validate_mcp_exec_reply_accepts_completed_zero(self):
        good = {
            "jsonrpc": "2.0",
            "id": 1,
            "result": {
                "isError": False,
                "structuredContent": {
                    "data": {
                        "state": "exited",
                        "wait": "completed",
                        "result": {"termination": {"kind": "exit", "code": 0}},
                    }
                },
            },
        }
        self.assertEqual(oneshot.validate_mcp_exec_reply(good)["state"], "exited")

    def test_validate_mcp_exec_reply_rejects_failures(self):
        bad_cases = [
            {"error": {"code": -32603, "message": "boom"}},
            {"result": {"isError": True, "structuredContent": {}}},
            {"result": {"isError": False, "structuredContent": {"data": {"state": "running", "wait": "timed_out"}}}},
            {"result": {"isError": False, "structuredContent": {
                "data": {"state": "exited", "wait": "completed", "result": {"termination": {"kind": "exit", "code": 1}}}
            }}},
        ]
        for case in bad_cases:
            with self.assertRaises(RuntimeError):
                oneshot.validate_mcp_exec_reply(case)

    def test_validate_server_wait_reply(self):
        good = {
            "type": "execution.receipt",
            "executionWait": "completed",
            "executionReceipt": {"result": {"termination": {"kind": "exit", "code": 0}}},
        }
        self.assertEqual(oneshot.validate_server_wait_reply(good)["result"]["termination"]["code"], 0)
        for bad in (
            {"type": "error", "error": "boom"},
            {"type": "execution.receipt", "executionWait": "timed_out", "executionReceipt": {}},
            {"type": "execution.receipt", "executionWait": "completed",
             "executionReceipt": {"result": {"termination": {"kind": "exit", "code": 2}}}},
        ):
            with self.assertRaises(RuntimeError):
                oneshot.validate_server_wait_reply(bad)

    def test_discover_owned_processes_ignores_unrelated_and_sibling_and_catches_detached_child(self):
        scratch = "/home/pi/mesh-oneshot-bench-abcd"
        rows = [
            # Owned daemon (by recorded pid + start)
            {"pid": 100, "ppid": 50, "pgid": 50, "start": "1000", "exe": f"{scratch}/mesh",
             "argv": [f"{scratch}/mesh", "daemon"], "owner_token": "tok-1"},
            # Owned worker (exact binary + session dir under state/s)
            {"pid": 101, "ppid": 1, "pgid": 101, "start": "1010", "exe": f"{scratch}/mesh",
             "argv": [f"{scratch}/mesh", "session-worker", "--dir", f"{scratch}/state/s/ABCD"], "owner_token": "tok-1"},
            # Detached child started in its own session with NO scratch path in argv (caught via ppid + meta.json)
            {"pid": 102, "ppid": 101, "pgid": 102, "start": "1020", "exe": "/bin/sleep",
             "argv": ["/bin/sleep", "60"], "owner_token": None},
            # Orphaned detached child whose worker already exited (caught via meta.json + pgid == pid)
            {"pid": 103, "ppid": 1, "pgid": 103, "start": "1030", "exe": "/bin/sleep",
             "argv": ["/bin/sleep", "60"], "owner_token": None},
            # Unrelated process reading the benchmark log (MUST NOT be matched)
            {"pid": 200, "ppid": 1, "pgid": 200, "start": "2000", "exe": "/usr/bin/tail",
             "argv": ["tail", "-f", f"{scratch}/daemon.log"], "owner_token": None},
            # Sibling scratch directory with shared prefix (MUST NOT be matched)
            {"pid": 201, "ppid": 1, "pgid": 201, "start": "2010", "exe": f"{scratch}2/mesh",
             "argv": [f"{scratch}2/mesh", "session-worker", "--dir", f"{scratch}2/state/s/EFGH"], "owner_token": "tok-2"},
            # Real canonical daemon worker (MUST NOT be matched)
            {"pid": 202, "ppid": 1, "pgid": 202, "start": "2020", "exe": "/home/pi/.local/bin/mesh",
             "argv": ["/home/pi/.local/bin/mesh", "session-worker", "--dir", "/home/pi/.local/state/mesh/s/REAL"], "owner_token": None},
            # Excluded ancestor process
            {"pid": 50, "ppid": 1, "pgid": 50, "start": "500", "exe": "/usr/bin/python3",
             "argv": ["python3", f"{scratch}/oneshot.py"], "owner_token": "tok-1"},
        ]
        found = oneshot.discover_owned_processes(
            rows=rows,
            owned_roots=[(100, "1000")],
            exact_binary=f"{scratch}/mesh",
            state_sessions_dir=f"{scratch}/state/s",
            meta_pids=[102, 103],
            owner_token="tok-1",
            exclude_pids={50},
        )
        self.assertEqual(found, [(100, "1000"), (101, "1010"), (102, "1020"), (103, "1030")])

    @unittest.skipUnless(sys.platform == "linux", "real process cleanup test uses Linux /proc + pidfd")
    def test_cleanup_owned_tree_preserves_unrelated_and_joins_detached_child(self):
        with tempfile.TemporaryDirectory(prefix="mesh-oneshot-bench-test-", dir="/work/tmp") as directory:
            scratch = Path(directory)
            log_file = scratch / "daemon.log"
            log_file.write_text("log\n")
            state_s = scratch / "state" / "s" / "TEST"
            state_s.mkdir(parents=True)
            # Unrelated process referencing the scratch path in argv (like tailing a log).
            unrelated = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)", str(log_file)],
                start_new_session=True,
            )
            # Detached child in its own session with NO scratch path in argv, recorded in meta.json.
            detached = subprocess.Popen(
                ["/bin/sleep", "60"],
                start_new_session=True,
            )
            (state_s / "meta.json").write_text(json.dumps({"pid": detached.pid}))
            try:
                remaining = oneshot.cleanup_owned_tree(
                    scratch,
                    state_dir=scratch / "state",
                    owner_token="unused-token-for-meta-test",
                )
                self.assertEqual(remaining, [])
                detached.wait(timeout=5)
                self.assertIsNotNone(detached.poll())
                # Unrelated process must still be alive!
                self.assertIsNone(unrelated.poll())
            finally:
                oneshot.stop_popen(unrelated, grace=1.0)
                oneshot.stop_popen(detached, grace=1.0)

    def test_summarize_host_verdicts_slowdown_no_clear_slowdown_and_inconclusive(self):
        slow = oneshot.summarize_host(
            make_pairs([0.35] * 30, [0.05] * 30),
            [{"create_seconds": 0.1, "wait_seconds": 0.05, "total_seconds": 0.15}] * 30,
            True,
            ssh_command.summarize,
        )
        self.assertEqual(slow["promise_verdict"], "slowdown")
        self.assertFalse(slow["at_or_under_ssh_p50"])
        self.assertAlmostEqual(slow["comparison"]["p50_ratio"], 7.0)
        self.assertAlmostEqual(slow["comparison"]["p50_delta_ms"], 300.0)
        self.assertAlmostEqual(slow["server_ms"]["total_ms"]["p50"], 150.0)

        fast = oneshot.summarize_host(
            make_pairs([0.04] * 30, [0.05] * 30),
            [{"create_seconds": 0.01, "wait_seconds": 0.01, "total_seconds": 0.02}] * 30,
            True,
            ssh_command.summarize,
        )
        self.assertEqual(fast["promise_verdict"], "no clear slowdown")
        self.assertTrue(fast["at_or_under_ssh_p50"])

        # Only 5 pairs: 1 / 2^5 = 0.03125 >= 0.01 -> cannot reach p < 0.01 -> inconclusive!
        small = oneshot.summarize_host(
            make_pairs([0.35] * 5, [0.05] * 5),
            [],
            True,
            ssh_command.summarize,
        )
        self.assertEqual(small["promise_verdict"], "inconclusive")
        self.assertFalse(small["at_or_under_ssh_p50"])

    def test_summarize_host_when_ssh_unavailable(self):
        summary = oneshot.summarize_host(
            [{"order": ["mesh"], "mesh": {"seconds": 0.12}} for _ in range(5)],
            [{"create_seconds": 0.05, "wait_seconds": 0.03, "total_seconds": 0.08}] * 5,
            False,
            ssh_command.summarize,
        )
        self.assertIsNone(summary["warm_ssh_ms"])
        self.assertIsNone(summary["comparison"])
        self.assertEqual(summary["promise_verdict"], "n/a")
        self.assertIsNone(summary["at_or_under_ssh_p50"])
        self.assertAlmostEqual(summary["mesh_mcp_ms"]["p50"], 120.0)

    def test_failed_cleanup_forces_nonzero_exit_while_preserving_report(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-gate-test-", dir="/work/tmp") as directory:
            output = Path(directory) / "report.json"
            args = SimpleNamespace(
                hosts=["vps"],
                pairs=1,
                warmups=0,
                seed=435,
                output=output,
                markdown=None,
                scratch_parent=Path(directory),
            )

            def fake_fleet(_args, _helpers, _run_id, _tok, _scratch, _reg, report):
                report["hosts"]["vps"] = {"status": "ok", "summary": {"promise_verdict": "no clear slowdown"}}
                report["cleanup"]["vps"] = {"clean": False, "remaining_pids": [99999], "scratch_removed": False}
                report["status"] = "ok"

            import contextlib
            import io
            with patch.object(oneshot, "execute_fleet_run", side_effect=fake_fleet), \
                 contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(io.StringIO()):
                rc = oneshot.run_coordinator(args)
            self.assertEqual(rc, 1)
            written = json.loads(output.read_text())
            self.assertEqual(written["status"], "error")
            self.assertIn("cleanup failed", written["error"])
            self.assertEqual(written["hosts"]["vps"]["status"], "ok")

    def test_server_bench_checks_cancellation_between_samples(self):
        stop_event = threading.Event()
        calls = 0

        def fake_sample(_sock, _cwd, _true_path, _key):
            nonlocal calls
            calls += 1
            stop_event.set()
            return {"create_seconds": 0.01, "wait_seconds": 0.01, "total_seconds": 0.02, "exit": 0}

        with patch.object(oneshot, "run_one_server_sample", side_effect=fake_sample):
            with self.assertRaisesRegex(RuntimeError, "cancelled"):
                oneshot.run_server_bench("/fake/sock", "/tmp", "/bin/true", 0, 5, stop_event=stop_event)
        self.assertEqual(calls, 1)

    def test_build_mac_queue_command_uses_fifo_runner(self):
        cmd = oneshot.build_mac_queue_command(
            ["/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1/oneshot.py", "--remote-agent"],
            lane="mesh-oneshot-bench",
            thread="t-1",
            expected_seconds=300,
            wait_seconds=600,
            timeout_seconds=600,
        )
        self.assertIn("mac-queue.ts run", cmd)
        self.assertIn("--lane mesh-oneshot-bench", cmd)
        self.assertIn("-- /usr/bin/python3 /Users/shaul/mesh-oneshot-bench-1/oneshot.py --remote-agent", cmd)

    def test_isolated_env_sets_private_permissions(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-env-test-") as directory:
            root = Path(directory)
            old_umask = os.umask(0o002)
            try:
                _env, state, config = oneshot.isolated_env(root)
            finally:
                os.umask(old_umask)
            self.assertEqual(state.stat().st_mode & 0o777, 0o700)
            self.assertEqual(config.stat().st_mode & 0o777, 0o700)

    def test_format_markdown_table_renders_ok_and_error_rows(self):
        summary = oneshot.summarize_host(
            make_pairs([0.20] * 30, [0.05] * 30),
            [{"create_seconds": 0.08, "wait_seconds": 0.04, "total_seconds": 0.12}] * 30,
            True,
            ssh_command.summarize,
        )
        report = {
            "hosts": {
                "vps": {
                    "status": "ok",
                    "rtt": {"available": True, "summary_ms": {"p50": 80.0}},
                    "ssh": {"available": True, "alias": "vps"},
                    "summary": summary,
                },
                "pi": {"status": "error", "error": "unreachable\nsecond line | pipe"},
            }
        }
        table = oneshot.format_markdown_table(report)
        self.assertIn("| vps | 80.0 | 50.0 / 50.0 (50.0–50.0) | 200.0 / 200.0 (200.0–200.0) | 4.00x | +150.0 ms | 80.0 / 40.0 / 120.0 | slowdown | no |", table)
        self.assertIn("| pi | error | error | error | n/a | n/a | n/a | error (unreachable) | n/a |", table)


if __name__ == "__main__":
    unittest.main()
