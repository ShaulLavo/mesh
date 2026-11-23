#!/usr/bin/env python3
"""Unit tests for scripts/bench/oneshot.py pure logic, markers, and cleanup."""

import contextlib
import io
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
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

    def test_parse_proc_net_tcp_inodes_selects_only_matching_established_connection(self):
        # 100.77.94.94 -> 5E5E4D64 (little-endian), port 41270 -> A136
        # 100.84.25.68 -> 44195464 (little-endian), port 22 -> 0016
        proc_net = "\n".join([
            "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
            "   0: 5E5E4D64:A136 44195464:0016 01 00000000:00000000 00:00000000 00000000  1000        0 468755272 1",
            "   1: 5E5E4D64:A136 44195464:0016 06 00000000:00000000 00:00000000 00000000  1000        0 999999999 1",
            "   2: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111111111 1",
        ])
        conn = {"client_ip": "100.77.94.94", "client_port": "41270", "server_ip": "100.84.25.68", "server_port": "22"}
        self.assertEqual(oneshot.parse_proc_net_tcp_inodes(proc_net, conn), ["468755272"])

    def test_ssh_hardening_options_prevent_known_hosts_write_and_forwarding(self):
        argv = oneshot.remote_ssh_base_argv("100.84.25.68")
        for expected in (
            "BatchMode=yes",
            "UpdateHostKeys=no",
            "StrictHostKeyChecking=yes",
            "ClearAllForwardings=yes",
            "ForwardAgent=no",
            "ForwardX11=no",
            "HostName=100.84.25.68",
        ):
            self.assertIn(expected, argv)

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

    def test_discover_owned_processes_rejects_stale_recycled_pid_and_unrelated_processes(self):
        scratch = "/home/pi/mesh-oneshot-bench-abcd"
        rows = [
            # Owned daemon (exact recorded pid + start)
            {"pid": 100, "ppid": 50, "pgid": 50, "start": "1000", "exe": f"{scratch}/mesh",
             "argv": [f"{scratch}/mesh", "daemon"], "owner_token": "tok-1"},
            # Owned worker (exact binary + session dir under state/s)
            {"pid": 101, "ppid": 1, "pgid": 101, "start": "1010", "exe": f"{scratch}/mesh",
             "argv": [f"{scratch}/mesh", "session-worker", "--dir", f"{scratch}/state/s/ABCD"], "owner_token": "tok-1"},
            # Live detached child of live worker (caught via ppid in owned)
            {"pid": 102, "ppid": 101, "pgid": 102, "start": "1020", "exe": "/bin/sleep",
             "argv": ["/bin/sleep", "60"], "owner_token": None},
            # Previously recorded detached child whose worker exited (exact pid + start matches owned_roots)
            {"pid": 103, "ppid": 1, "pgid": 103, "start": "1030", "exe": "/bin/sleep",
             "argv": ["/bin/sleep", "60"], "owner_token": None},
            # Recycled PID: old recorded (104, "1040") exited, new unrelated group leader has (104, "9999") -> MUST NOT match!
            {"pid": 104, "ppid": 1, "pgid": 104, "start": "9999", "exe": "/usr/bin/vim",
             "argv": ["vim"], "owner_token": None},
            # Unrelated process reading the benchmark log (MUST NOT be matched)
            {"pid": 200, "ppid": 1, "pgid": 200, "start": "2000", "exe": "/usr/bin/tail",
             "argv": ["tail", "-f", f"{scratch}/daemon.log"], "owner_token": None},
            # Sibling scratch directory with shared prefix (MUST NOT be matched)
            {"pid": 201, "ppid": 1, "pgid": 201, "start": "2010", "exe": f"{scratch}2/mesh",
             "argv": [f"{scratch}2/mesh", "session-worker", "--dir", f"{scratch}2/state/s/EFGH"], "owner_token": "tok-2"},
            # Excluded ancestor process
            {"pid": 50, "ppid": 1, "pgid": 50, "start": "500", "exe": "/usr/bin/python3",
             "argv": ["python3", f"{scratch}/oneshot.py"], "owner_token": "tok-1"},
        ]
        found = oneshot.discover_owned_processes(
            rows=rows,
            owned_roots=[(100, "1000"), (103, "1030"), (104, "1040")],
            exact_binary=f"{scratch}/mesh",
            state_sessions_dir=f"{scratch}/state/s",
            owner_token="tok-1",
            exclude_pids={50},
        )
        self.assertEqual(found, [(100, "1000"), (101, "1010"), (102, "1020"), (103, "1030")])

    @unittest.skipUnless(sys.platform == "linux", "real process cleanup test uses Linux /proc + pidfd")
    def test_cleanup_owned_tree_preserves_unrelated_and_joins_recorded_detached_child(self):
        with tempfile.TemporaryDirectory(prefix="mesh-oneshot-bench-test-", dir="/work/tmp") as directory:
            scratch = Path(directory)
            log_file = scratch / "daemon.log"
            log_file.write_text("log\n")
            registry = oneshot.OwnedRegistry(scratch / "owned-pids.jsonl")
            # Unrelated process referencing the scratch path in argv (like tailing a log).
            unrelated = subprocess.Popen(
                [sys.executable, "-c", "import time; time.sleep(60)", str(log_file)],
                start_new_session=True,
            )
            # Detached child in its own session with NO scratch path in argv, recorded with exact (pid, start).
            detached = subprocess.Popen(["/bin/sleep", "60"], start_new_session=True)
            registry.record(detached.pid)
            try:
                remaining = oneshot.cleanup_owned_tree(
                    scratch,
                    registry=registry,
                    state_dir=scratch / "state",
                    owner_token="unused-token-for-registry-test",
                )
                self.assertEqual(remaining, [])
                detached.wait(timeout=5)
                self.assertIsNotNone(detached.poll())
                self.assertIsNone(unrelated.poll())
            finally:
                oneshot.stop_popen(unrelated, grace=1.0)
                oneshot.stop_popen(detached, grace=1.0)

    def test_list_posix_process_rows_raises_on_ps_failure(self):
        failed = subprocess.CompletedProcess(["ps"], 1, stdout="", stderr="ps: permission denied")
        with patch.object(oneshot, "run_command_with_deadline", return_value=failed):
            with self.assertRaisesRegex(RuntimeError, "ps process listing failed"):
                oneshot.list_posix_process_rows()

    def test_worker_socket_eof_does_not_abort_daemon_stop(self):
        with tempfile.TemporaryDirectory(prefix="mesh-oneshot-bench-eof-", dir="/work/tmp") as directory:
            scratch = Path(directory)
            sock = scratch / "state" / "s" / "ABCD" / "sock"
            sock.parent.mkdir(parents=True)
            sock.write_text("")
            stopped = []
            with patch.object(oneshot, "daemon_rpc", side_effect=EOFError("worker closed")), \
                 patch.object(oneshot, "stop_popen", side_effect=lambda p, **_kw: stopped.append(p)), \
                 patch.object(oneshot, "list_process_rows", return_value=[]):
                remaining = oneshot.cleanup_owned_tree(scratch, daemon_proc="fake-daemon")
            self.assertEqual(remaining, [])
            self.assertEqual(stopped, ["fake-daemon"])

    def test_cleanup_scratch_refuses_missing_or_mismatched_owner_token(self):
        with tempfile.TemporaryDirectory(prefix="mesh-oneshot-bench-tok-", dir="/work/tmp") as directory:
            scratch = Path(directory)
            args = SimpleNamespace(scratch=scratch, owner_token="expected-token")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                rc = oneshot.run_cleanup_scratch(args)
            self.assertEqual(rc, 1)
            self.assertTrue(scratch.exists())
            self.assertFalse(json.loads(out.getvalue())["clean"])

            oneshot.write_owner_token(scratch, "different-token")
            out2 = io.StringIO()
            with contextlib.redirect_stdout(out2):
                rc2 = oneshot.run_cleanup_scratch(args)
            self.assertEqual(rc2, 1)
            self.assertTrue(scratch.exists())
            self.assertIn("mismatch", json.loads(out2.getvalue())["error"])

    def test_daemon_rpc_aborts_promptly_on_stop_event(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-rpc-cancel-", dir="/work/tmp") as directory:
            sock_path = Path(directory) / "daemon.sock"
            elapsed = self._measure_cancelled_rpc(sock_path)
            self.assertLess(elapsed, 1.5)

    def _measure_cancelled_rpc(self, sock_path):
        with socket.socket(socket.AF_UNIX) as server:
            server.bind(str(sock_path))
            server.listen(1)
            stop_event = threading.Event()
            threading.Timer(0.1, stop_event.set).start()
            started = time.monotonic()
            with self.assertRaisesRegex(RuntimeError, "cancelled"):
                oneshot.daemon_rpc(sock_path, {"type": "host.info"}, timeout=30.0, stop_event=stop_event)
            return time.monotonic() - started

    def test_decode_darwin_bsdinfo_and_linux_boot_id(self):
        import struct
        buf = bytearray(136)
        struct.pack_into("<I", buf, 4, 2)  # live status
        struct.pack_into("<I", buf, 12, 4242)
        struct.pack_into("<Q", buf, 120, 1795417082)
        struct.pack_into("<Q", buf, 128, 537564)
        self.assertEqual(oneshot.decode_darwin_bsdinfo(4242, bytes(buf)), "1795417082:537564")
        self.assertIsNone(oneshot.decode_darwin_bsdinfo(9999, bytes(buf)))
        struct.pack_into("<I", buf, 4, 5)  # zombie
        self.assertIsNone(oneshot.decode_darwin_bsdinfo(4242, bytes(buf)))
        if sys.platform == "linux":
            start = oneshot.linux_start_id(os.getpid())
            boot_id = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
            self.assertTrue(start.startswith(boot_id + ":"))

    def test_shutdown_workers_respects_single_overall_deadline(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-deadline-", dir="/work/tmp") as directory:
            state = Path(directory) / "state"
            for idx in range(10):
                sock = state / "s" / f"S{idx:03d}" / "sock"
                sock.parent.mkdir(parents=True)
                sock.write_text("")
            calls = []

            def slow_rpc(_sock, _req, timeout=0.25):
                calls.append(timeout)
                time.sleep(min(timeout, 0.12))
                raise TimeoutError("unresponsive worker")

            deadline = oneshot.CleanupDeadline(1.25)
            started = time.monotonic()
            with patch.object(oneshot, "daemon_rpc", side_effect=slow_rpc):
                oneshot.shutdown_workers_gracefully(state, deadline)
            elapsed = time.monotonic() - started
            # Must reserve >= 0.85s for daemon stop + kills, so graceful loop takes <= ~0.45s even for 10 sockets.
            self.assertLess(elapsed, 0.65)
            self.assertLess(len(calls), 10)

    def test_cleanup_deadline_clamp_shortens_remaining(self):
        deadline = oneshot.CleanupDeadline(10.0)
        self.assertGreater(deadline.remaining(), 8.0)
        deadline.clamp(0.5)
        self.assertLessEqual(deadline.remaining(), 0.55)

    def test_token_guarded_command_blocks_missing_or_mismatched_token(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-guard-", dir="/work/tmp") as directory:
            scratch = Path(directory)
            cmd = oneshot.token_guarded_command(scratch, "tok-good", 'touch "$scratch/stop"')
            res_missing = subprocess.run(["/bin/sh", "-c", cmd], capture_output=True)
            self.assertEqual(res_missing.returncode, 42)
            self.assertFalse((scratch / "stop").exists())

            oneshot.write_owner_token(scratch, "tok-other")
            res_wrong = subprocess.run(["/bin/sh", "-c", cmd], capture_output=True)
            self.assertEqual(res_wrong.returncode, 42)
            self.assertFalse((scratch / "stop").exists())

            oneshot.write_owner_token(scratch, "tok-good")
            res_ok = subprocess.run(["/bin/sh", "-c", cmd], capture_output=True)
            self.assertEqual(res_ok.returncode, 0)
            self.assertTrue((scratch / "stop").exists())

    def test_bracket_darwin_ps_rows_rejects_reused_pid(self):
        ps_lines = [
            "100 1 100 S /Users/shaul/mesh-oneshot-bench-1/mesh daemon",
            "200 1 200 S /Users/shaul/mesh-oneshot-bench-1/mesh session-worker --dir /Users/shaul/mesh-oneshot-bench-1/state/s/A",
            "300 1 300 S /bin/sleep 60",
        ]
        starts_before = {100: "1795417000:100", 200: "1795417000:200"}
        # PID 200 changed hands mid-ps (new birth time), PID 300 wasn't in starts_before -> both rejected!
        starts_after = {100: "1795417000:100", 200: "1795417005:999", 300: "1795417006:111"}
        rows = oneshot.bracket_darwin_ps_rows(ps_lines, starts_before, starts_after.get)
        self.assertEqual([row["pid"] for row in rows], [100])
        self.assertEqual(rows[0]["start"], "1795417000:100")

    def test_darwin_all_pids_rejects_zero_on_second_call(self):
        fake_lib = SimpleNamespace(proc_listallpids=lambda buf, size: 10 if buf is None else 0)
        with patch.object(oneshot, "darwin_libproc", return_value=fake_lib):
            with self.assertRaisesRegex(RuntimeError, "proc_listallpids read failed"):
                oneshot.darwin_all_pids()

    def test_darwin_process_scan_bounds_ps_by_live_deadline(self):
        deadline = oneshot.CleanupDeadline(8.0)
        threading.Timer(0.08, lambda: deadline.clamp(0.15)).start()
        started = time.monotonic()
        with self.assertRaisesRegex(RuntimeError, "cleanup deadline expired"):
            oneshot.run_command_with_deadline(
                [sys.executable, "-c", "import time; time.sleep(10)"], deadline=deadline
            )
        # The clamp, not the original 8 s deadline, ended the run; this bound
        # is a generous hang cap, not a speed check.
        self.assertLess(time.monotonic() - started, 5.0)

        expired = oneshot.CleanupDeadline(0.1)
        expired._at = time.monotonic() - 0.1
        with patch.object(oneshot, "darwin_all_pids", return_value=[100]), \
             patch.object(oneshot, "darwin_start_id", return_value="1:1"), \
             patch.object(oneshot.subprocess, "Popen") as popen_mock:
            with self.assertRaisesRegex(RuntimeError, "cleanup deadline expired"):
                oneshot.list_darwin_process_rows(deadline=expired)
        popen_mock.assert_not_called()

        with patch.object(oneshot.sys, "platform", "darwin"), \
             patch.object(oneshot.subprocess, "Popen") as popen_mock2:
            with self.assertRaisesRegex(RuntimeError, "cleanup deadline expired"):
                oneshot.parent_pid(123, deadline=expired)
        popen_mock2.assert_not_called()

    def test_children_found_while_waiting_are_escalated_after_scans_fail(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-wait-child-", dir="/work/tmp") as directory:
            reg = oneshot.OwnedRegistry(Path(directory) / "owned.jsonl")
            parent, child = (100, "1:100"), (200, "1:200")
            scans = iter([([parent], None), ([parent, child], None)])
            signaled = []

            def scan(*_args):
                return next(scans, ([], RuntimeError("ps process listing timed out")))
            with patch.object(oneshot, "try_find_live_owned", side_effect=scan), \
                 patch.object(oneshot, "signal_verified_process", side_effect=lambda p, s, sig: signaled.append((p, s, sig))):
                with self.assertRaisesRegex(RuntimeError, "ps process listing timed out"):
                    oneshot.terminate_owned_within(reg, None, None, None, set(), oneshot.CleanupDeadline(5.0))
            self.assertIn((100, "1:100", signal.SIGKILL), signaled)
            self.assertIn((200, "1:200", signal.SIGKILL), signaled)

    def test_term_phase_scan_timeout_still_escalates_to_sigkill(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-term-timeout-", dir="/work/tmp") as directory:
            reg = oneshot.OwnedRegistry(Path(directory) / "owned.jsonl")
            reg.record(99999, "1:99999")
            signaled = []
            deadline = oneshot.CleanupDeadline(1.0)
            with patch.object(oneshot, "list_process_rows", side_effect=RuntimeError("ps process listing timed out")), \
                 patch.object(oneshot, "signal_verified_process", side_effect=lambda p, s, sig: signaled.append((p, s, sig))):
                with self.assertRaisesRegex(RuntimeError, "ps process listing timed out"):
                    oneshot.terminate_owned_within(reg, None, None, None, set(), deadline)
            self.assertIn((99999, "1:99999", signal.SIGTERM), signaled)
            self.assertIn((99999, "1:99999", signal.SIGKILL), signaled)

    def test_late_signal_clamps_daemon_wait_and_escalates_to_sigkill(self):
        stubborn = subprocess.Popen(
            [
                sys.executable, "-c",
                "import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); print('READY', flush=True); time.sleep(60)",
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
        try:
            self.assertEqual(stubborn.stdout.readline().strip(), "READY")
            deadline = oneshot.CleanupDeadline(8.0)
            threading.Timer(0.15, lambda: deadline.clamp(1.0)).start()
            started = time.monotonic()
            oneshot.stop_popen(stubborn, grace=5.0, kill_wait=1.0, deadline=deadline)
            elapsed = time.monotonic() - started
            self.assertEqual(stubborn.poll(), -signal.SIGKILL)
        finally:
            if stubborn.poll() is None:
                stubborn.kill()
                stubborn.wait(timeout=2)
            stubborn.stdout.close()
        # Must escalate to SIGKILL well inside the clamped 1.0s deadline (never wait the 5s grace).
        self.assertLess(elapsed, 1.5)

    def test_stage_wrapper_exits_and_cleans_on_sigterm(self):
        with tempfile.TemporaryDirectory(prefix="oneshot-stage-parent-", dir="/work/tmp") as parent:
            scratch = Path(parent) / "mesh-oneshot-bench-stage-test"
            proc = subprocess.Popen(
                [
                    "/bin/sh", "-c", oneshot.REMOTE_STAGE_WRAPPER, "oneshot-stage",
                    str(scratch), sys.executable, "vps", "127.0.0.1", "cid", "10", "/bin/true", "0", "1", "tok-stage", "30",
                ],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            try:
                stream = oneshot.MarkerStream(proc.stdout)
                ready = stream.read_marker(oneshot.STAGE_READY_PREFIX, timeout=5.0)
                self.assertEqual(ready["scratch"], str(scratch))
                self.assertTrue(scratch.is_dir())
                proc.terminate()
                rc = proc.wait(timeout=3.0)
            finally:
                oneshot.stop_popen(proc, grace=1.0)
                proc.stdout.close()
                proc.stderr.close()
            self.assertEqual(rc, 143)
            self.assertFalse(scratch.exists())

    def test_parse_recovery_result_rejects_clean_true_on_nonzero_exit(self):
        bad = subprocess.CompletedProcess([], 1, stdout='{"clean":true,"scratch_removed":true}\n', stderr="rm failed")
        parsed = oneshot.parse_recovery_result(bad, "/root/mesh-oneshot-bench-1")
        self.assertFalse(parsed["clean"])

    def test_cleanup_owned_tree_stops_daemon_even_when_initial_snapshot_raises(self):
        with tempfile.TemporaryDirectory(prefix="mesh-oneshot-bench-snap-", dir="/work/tmp") as directory:
            stopped = self._run_failing_snapshot_cleanup(Path(directory))
            self.assertEqual(stopped, ["owned-daemon"])

    def _run_failing_snapshot_cleanup(self, scratch):
        stopped = []
        with patch.object(oneshot, "list_process_rows", side_effect=RuntimeError("ps exploded")), \
             patch.object(oneshot, "stop_popen", side_effect=lambda p, **_kw: stopped.append(p)):
            with self.assertRaisesRegex(RuntimeError, "ps exploded"):
                oneshot.cleanup_owned_tree(scratch, daemon_proc="owned-daemon")
        return stopped

    def test_recovery_always_uses_fresh_verified_ssh(self):
        fake_res = subprocess.CompletedProcess([], 0, stdout='{"clean":true,"remaining_pids":[],"scratch_removed":true}\n', stderr="")
        spec = oneshot.HOST_SPECS["vps"]
        args = SimpleNamespace(mac_lane="lane", mac_thread="thread")
        with patch.object(oneshot, "run_fresh_verified_ssh", return_value=fake_res) as fresh:
            result = oneshot.remote_recovery_cleanup(
                "100.84.25.68", spec, "/usr/bin/python3", "/root/mesh-oneshot-bench-1-vps", "tok-1", args
            )
        self.assertTrue(result["clean"])
        fresh.assert_called_once()

    def test_missing_stopped_marker_preserves_completed_measurements(self):
        read_fd, write_fd = os.pipe()
        os.write(
            write_fd,
            b'ONESHOT_SERVER_BENCH {"warmups":[],"samples":[{"create_seconds":0.1,"wait_seconds":0.02,"total_seconds":0.12,"exit":0}]}\n',
        )
        os.close(write_fd)
        report_hosts = {}
        with tempfile.TemporaryDirectory(prefix="oneshot-preserve-", dir="/work/tmp") as directory, \
             os.fdopen(read_fd, "r", encoding="utf-8") as reader:
            self._run_missing_stopped_case(Path(directory), reader, report_hosts)
        self.assertEqual(report_hosts["vps"]["status"], "ok")
        self.assertEqual(len(report_hosts["vps"]["pairs"]), 1)
        self.assertEqual(len(report_hosts["vps"]["server_samples"]), 1)
        # record_host_failure must preserve the completed "ok" measurements rather than replacing them.
        oneshot.record_host_failure(report_hosts, "vps", RuntimeError("missing STOPPED"))
        self.assertEqual(report_hosts["vps"]["status"], "ok")
        self.assertIn("missing STOPPED", report_hosts["vps"]["teardown_error"])

    def _run_missing_stopped_case(self, workdir, reader, report_hosts):
        fake_proc = SimpleNamespace(poll=lambda: 1, wait=lambda timeout=30: 1)
        fake_orch = SimpleNamespace(run=lambda _cmd, timeout=20: subprocess.CompletedProcess([], 0, "", ""))
        fake_helpers = SimpleNamespace(
            mcp_native=SimpleNamespace(MCPClient=lambda *_a, **_kw: SimpleNamespace(proc=SimpleNamespace(pid=os.getpid(), poll=lambda: 0))),
            ssh_command=ssh_command,
        )
        args = SimpleNamespace(warmups=0, pairs=1, seed=435)
        probe = {"platform": "Linux", "load_average": [0.1, 0.1, 0.1]}
        ready = {"host_id": "h1", "mesh_identity": "m1", "ip": "100.84.25.68", "port": 9999, "cwd": "/tmp/w", "true_path": "/bin/true"}
        stream = oneshot.MarkerStream(reader)
        registry = oneshot.OwnedRegistry(workdir / "owned.jsonl")
        fake_master = SimpleNamespace(
            master_pid=os.getpid(), control_path=workdir / "c",
            start=lambda: os.getpid(), close=lambda: None,
        )
        with patch.object(oneshot, "SSHMaster", return_value=fake_master), \
             patch.object(oneshot, "collect_mux_proof", return_value=({"check_before": True}, ["1"])), \
             patch.object(oneshot, "finalize_mux_proof"), \
             patch.object(oneshot, "open_mcp_context", return_value="mc1.ctx"), \
             patch.object(oneshot, "run_paired_samples", return_value=([], make_pairs([0.2], [0.1]))):
            with self.assertRaisesRegex(RuntimeError, "ONESHOT_STOPPED"):
                oneshot.execute_remote_measurements(
                    "vps", oneshot.HOST_SPECS["vps"], "100.84.25.68", fake_orch, probe,
                    {"available": True, "summary_ms": {"p50": 80.0}}, "/root/mesh-oneshot-bench-1-vps",
                    fake_proc, stream, lambda: "eof", ready, Path("/bin/true"),
                    {}, workdir, workdir, registry, "tok-1", args, fake_helpers, "run1", report_hosts,
                )

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

        def fake_sample(_sock, _cwd, _true_path, _key, stop_event=None):
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
            task=oneshot.mac_queue_task_label("tok-abc"),
        )
        self.assertIn("mac-queue.ts run", cmd)
        self.assertIn("--lane mesh-oneshot-bench", cmd)
        self.assertIn("Mesh oneshot benchmark (tok-abc)", cmd)
        self.assertIn("-- /usr/bin/python3 /Users/shaul/mesh-oneshot-bench-1/oneshot.py --remote-agent", cmd)

    def test_teardown_remote_cancels_mac_queue_ticket_before_recovery(self):
        spec = oneshot.HOST_SPECS["mac"]
        args = SimpleNamespace(mac_lane="lane", mac_thread="thread")
        report = {}
        calls = []

        def fake_cancel(ip, alias, py, task_label):
            calls.append(("cancel", ip, alias, task_label))
            return {"cancelled": True, "remaining_tickets": []}

        def fake_recover(ip, _spec, _py, scratch, _tok, _args):
            calls.append(("recover", ip, scratch))
            return {"clean": True, "remaining_pids": [], "scratch_removed": True, "remote_scratch": scratch}

        with patch.object(oneshot, "cancel_mac_queue_ticket", side_effect=fake_cancel), \
             patch.object(oneshot, "remote_recovery_cleanup", side_effect=fake_recover):
            oneshot.teardown_remote(
                "mac", spec, "100.92.135.44", "/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1-mac",
                None, None, None, "tok-xyz", False, args, report,
            )
        self.assertTrue(report["mac"]["clean"])
        self.assertEqual(calls[0], ("cancel", "100.92.135.44", "shaul-mac", "Mesh oneshot benchmark (tok-xyz)"))
        self.assertEqual(calls[1][0], "recover")

        report_failed = {}
        with patch.object(oneshot, "cancel_mac_queue_ticket", return_value={"cancelled": False, "error": "still waiting"}), \
             patch.object(oneshot, "remote_recovery_cleanup", side_effect=fake_recover):
            oneshot.teardown_remote(
                "mac", spec, "100.92.135.44", "/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1-mac",
                None, None, None, "tok-xyz", False, args, report_failed,
            )
        self.assertFalse(report_failed["mac"]["clean"])
        self.assertIn("still waiting", report_failed["mac"]["ticket_cancel_error"])

        report_timeout = {}
        timeout_calls = []

        def fake_recover_after_timeout(ip, _spec, _py, scratch, _tok, _args):
            timeout_calls.append(("recover", ip, scratch))
            return {"clean": True, "remaining_pids": [], "scratch_removed": True, "remote_scratch": scratch}

        with patch.object(oneshot, "run_fresh_verified_ssh", side_effect=subprocess.TimeoutExpired("ssh", 20)), \
             patch.object(oneshot, "remote_recovery_cleanup", side_effect=fake_recover_after_timeout):
            oneshot.teardown_remote(
                "mac", spec, "100.92.135.44", "/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1-mac",
                None, None, None, "tok-xyz", False, args, report_timeout,
            )
        self.assertEqual(len(timeout_calls), 1)
        self.assertFalse(report_timeout["mac"]["clean"])
        self.assertIn("timed out", report_timeout["mac"]["ticket_cancel_error"])

        report_both = {}
        with patch.object(oneshot, "run_fresh_verified_ssh", side_effect=subprocess.TimeoutExpired("ssh", 20)), \
             patch.object(oneshot, "remote_recovery_cleanup", side_effect=subprocess.TimeoutExpired("ssh", 60)):
            oneshot.teardown_remote(
                "mac", spec, "100.92.135.44", "/usr/bin/python3", "/Users/shaul/mesh-oneshot-bench-1-mac",
                None, None, None, "tok-xyz", False, args, report_both,
            )
        self.assertFalse(report_both["mac"]["clean"])
        self.assertIn("timed out", report_both["mac"]["error"])
        self.assertIn("timed out", report_both["mac"]["ticket_cancel_error"])

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
